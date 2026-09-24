# Production guide: OpenShell on Blaxel

Use [README.md](README.md) for the shortest runnable path. This guide explains the design, security model, operational controls and failure handling.

## System boundary

| Owner | Responsibilities |
| --- | --- |
| OpenShell gateway | CLI API, mTLS, sandbox JWTs, policy storage and revisions, providers and credential resolution, SSH/exec relay, audit log aggregation |
| `openshell-driver-blaxel` | `openshell.compute.v1.ComputeDriver` over a Unix socket: Blaxel sandbox lifecycle (through the Blaxel Go SDK `sdk-go`, as used by `bl`), bootstrap, reverse tunnel, readiness, recovery after restart |
| Blaxel | microVM (mk3), kernel variant, process API, filesystem API, `/port/N` WebSocket ingress, standby |
| `openshell-sandbox` (in VM, root) | Network namespace and veth, policy proxy (L4 + L7), TLS interception for credential injection, Landlock, seccomp, SSH server, process supervision |
| Workload (in VM, uid 1500) | The agent: `claude`, `bash`, … |

The driver never runs agent code. The in-VM supervisor never holds Blaxel credentials.

## Request lifecycle

```mermaid
sequenceDiagram
    actor User
    participant GW as OpenShell gateway
    participant D as Blaxel driver
    participant B as Blaxel API
    participant VM as Blaxel microVM

    User->>GW: sandbox create --provider … -- claude
    GW->>D: CreateSandbox (spec, sandbox JWT)
    D-->>GW: accepted; Ready=False Starting
    D->>B: POST /sandboxes (labels, extraArgs landlock, port 9000)
    B-->>D: DEPLOYED + sandbox URL
    D->>VM: upload runtime, os-tunnel, client TLS, JWT, launch script
    D->>VM: bootstrap (packages, sandbox user, Claude Code)
    D->>VM: start os-tunnel (keepAlive, timeout 0)
    D->>VM: WebSocket /port/9000/tunnel (yamux)
    D->>VM: start openshell-sandbox (keepAlive, timeout 0)
    VM->>GW: supervisor dials 127.0.0.1:17680 through the tunnel (mTLS)
    GW-->>VM: policy, provider env, inference bundle
    D->>VM: poll /run/openshell/ssh.sock
    D-->>GW: Ready=True HealthCheckPassed
    User->>GW: sandbox exec / connect (relayed over the supervisor session)
```

The first boot of a fresh VM takes about 16–24 s, mostly package installation. Later operations (exec, policy updates) go over the existing session.

## Configuration

All settings come from `gw/env.sh`. It loads `.env` when present, and the environment overrides both.

| Variable | Default | Purpose |
| --- | --- | --- |
| `BL_WORKSPACE`, `BL_ENV`, `BL_REGION` | `charlou-dev`, `dev`, `us-was-1` | Where sandboxes are created |
| `BL_API_KEY` | unset | Service-account key instead of the `bl login` session. Without it, the SDK reads `~/.blaxel/config.yaml` like `bl` and refreshes the token |
| `KERNEL_VARIANT` | `landlock` | `spec.runtime.extraArgs` key set to `enabled`. Empty means the default kernel |
| `SANDBOX_PACKAGES` | `curl,git,ca-certificates` | Debian/Alpine packages installed as root at bootstrap |
| `GATEWAY_NAME`, `GATEWAY_PORT` | `blaxel`, `17680` | CLI gateway name, and a loopback port used on the host and in each VM. The name is also the driver owner (`-owner`) |
| `DRIVER_SOCKET` | `/tmp/openshell-blaxel/driver.sock` | Driver socket. Its directory is 0700 and the socket 0600 |
| `OPENSHELL_GATEWAY_BIN` | PATH, then Homebrew | v0.0.116 `openshell-gateway` |

Driver flags not exposed in `env.sh` (see `driver/bin/openshell-driver-blaxel -h`): `-image` (default `blaxel/py-app:latest`), `-memory` (MiB, default 4096), `-install-claude` (default true), `-tunnel-port` (default 9000).

### Several gateways in one workspace

Each sandbox carries `openshell.ai/driver-owner=<GATEWAY_NAME>`. A driver only recovers its own sandboxes, and `status`/`cleanup` only list them. Sandboxes without the label (created before it existed) belong to the default owner `blaxel`. To run a second instance next to the default one:

```bash
export GATEWAY_NAME=blaxel-2 GATEWAY_PORT=17690 DRIVER_SOCKET=/tmp/openshell-blaxel-2/driver.sock
./gw/setup.sh && ./gw/restart.sh && make configure
```

Its DB and logs go to `gw/blaxel-2-*`, and `restart.sh`/`make down` only stop processes matching its own gateway name and socket.

### Gateway settings applied by `make configure`

| Setting | Why |
| --- | --- |
| `providers_v2_enabled=true` | Without it, providers inject credentials but add no network policy, and every agent request is denied |
| `claude-code-blaxel` provider profile | The built-in `claude-code` profile adds a network policy for Claude Code. This one also allows `platform.claude.com`, which Claude Code 2.1+ checks at startup |

## Images and kernel

The driver uses `blaxel/py-app:latest` (Debian 13), because it has glibc and apt. Requests for other Blaxel images (`blaxel/*`, `sandbox/*` templates) pass through. Any other OCI reference is replaced, and the driver emits an `ImageSubstituted` warning event. Blaxel images must embed Blaxel's `sandbox-api`.

The kernel variant is chosen at creation and can't change afterwards. Recreate sandboxes to move them onto a new variant. See [docs/KERNEL.md](docs/KERNEL.md) for the required config and how to qualify a kernel.

## Network model

1. The workload runs in a network namespace whose only route leads to the supervisor's proxy (`10.200.0.1:3128`) over a veth. Direct connections fail (`direct egress blocked` in the E2E suite).
2. The proxy identifies the calling binary (path after symlink resolution, plus a SHA-256 of the executable, trust on first use) and evaluates OPA policy per binary and host.
3. For `protocol: rest` endpoints, the proxy terminates TLS with a per-sandbox ephemeral CA and enforces method and path rules (`access: read-only`).
4. Provider credentials are injected at step 3 as headers. The workload environment only holds `openshell:resolve:env:v4…` placeholders.
5. Every decision is an OCSF event (`NET:OPEN`, `HTTP:POST`, … `ALLOWED`/`DENIED`) that flows to `openshell logs`.

The VM itself keeps Blaxel's outbound network, which the supervisor uses for approved egress. Blaxel's own domain filtering relies on `HTTP_PROXY` and isn't used as a fence.

## Security boundaries

- The workload runs as uid 1500 with an empty capability bounding set, `no_new_privs`, a seccomp filter and Landlock (writes only to `/sandbox` and `/tmp`). It has no `sudo` and can't install packages. Install tools at bootstrap (`SANDBOX_PACKAGES`) or in an image.
- The supervisor (v0.0.116) runs as root in the same VM and holds the gateway client certificate and the sandbox JWT, both in root-only files. A workload escaping to root would get them. OpenShell `main` fixes this by moving the supervisor out of the VM.
- The VM holds no Blaxel credentials. The driver dials `wss://<sandbox>/port/9000/tunnel` with its own token. `os-tunnel` accepts one host session and closes local connections while none is attached.
- The gateway's mTLS runs end to end inside the tunnel.
- The driver socket is protected by file permissions (directory 0700, socket 0600). Any process running as your user can reach it.
- The driver rejects user environment that tries to override `OPENSHELL_*` names, shell-quotes every value and drops invalid names (`driver/internal/driver/driver_test.go`).
- Keep provider keys out of shell history (`read -s`), and delete throwaway providers.

## Operations

```bash
make status                    # health + OpenShell <-> Blaxel mapping, orphans flagged
openshell -g blaxel logs <n>   # sandbox + gateway logs, OCSF decisions
./gw/cleanup.sh <n>            # exact plan; --apply to execute
./gw/cleanup.sh --orphans      # Blaxel sandboxes unknown to the gateway
./gw/restart.sh                # restart driver + gateway together
```

Restart semantics: the v0.0.116 gateway doesn't reconnect to a restarted external driver, so both restart together. On start the driver lists Blaxel sandboxes labeled `openshell.ai/managed-by=openshell-driver-blaxel`. It re-attaches tunnels to those whose `openshell-sandbox` process is still running, without restarting them. Every open `exec`/`connect` session drops. The workload and its files survive.

Logs: `gw/driver.log` and `gw/gateway.log` on the host. Blaxel-side, `bl logs sandbox <os-…>` shows process logs. For VMs that never boot, the Firecracker guest console is the source of truth (in Blaxel's SigNoz: `service.name = 'firecracker-vm'`, filtered by `instance_id`).

## Failure handling

Every row below was observed while building this.

| Signal | Meaning | Action |
| --- | --- | --- |
| Sandbox stuck `Provisioning`, `WORKLOAD_UNAVAILABLE` from Blaxel, no logs | The VM doesn't boot (for example a kernel panic) | Read the Firecracker console. One `landlock` build panicked with `FIPS140 loader: module loading error` |
| `ProvisionFailed: … Permission denied` on `os-tunnel` | The filesystem API ignored upload modes | Bootstrap `chmod`s explicitly. Rebuild the driver if you changed upload paths |
| Sandbox killed ~10 min after start | A `keepAlive` process was started without `timeout: 0` | Use `blaxel.Forever` for long-lived processes |
| `MainProcessExited` | The command after `--` finished | Use `-- sleep infinity` and `exec` for sessions |
| `canonical main process already has an input owner; attached read-only` | `connect` attaches to the main process, which already has an owner | Use `sandbox exec -n <n> --tty -- bash -l` |
| `ERR_PROXY_TUNNEL` or `CONNECT tunnel failed, response 403` | The host isn't allowed for that binary | `openshell logs <n> \| grep DENIED`, then extend the policy or profile |
| All agent requests denied although a provider is attached | `providers_v2_enabled` is off | `make configure` |
| `OCI USER is required because run_as_user is omitted` | The image declares no `USER` | The driver sets `OPENSHELL_OCI_IMAGE_USER=sandbox` |
| `operation was canceled` on every CLI call after a driver restart | The gateway lost its driver channel | `gw/restart.sh` |
| `Connection to sandbox closed by remote host` | Driver/gateway restart, or the sandbox ended | Reconnect with `exec`. The workload keeps running across restarts |
| `invalid peer certificate: BadSignature` | `openshell gateway add --local` copied the Homebrew certs | `make setup` reinstalls this gateway's certs |
| `unsupported extraArgs key "landlock"` | The control plane doesn't expose the variant | Needs a control-plane release that allows it |
| Your sandboxes drop to `Provisioning` right after another driver starts | A second driver with the same owner adopted them and took over their tunnels (`os-tunnel` accepts one host session) | Give each instance its own `GATEWAY_NAME`. Stop the intruder, and the original tunnels reconnect within ~30 s |
| `no Blaxel credentials for workspace …` at driver start | No `bl login` session for that workspace and no `BL_API_KEY` | `bl login <workspace>` |

## Verification ladder

Local, no network:

```bash
make test
make build
```

Live, in this order:

1. `make status`: the gateway is authenticated, the driver is running and `providers_v2` is enabled.
2. `experiments/kab.mjs`: the `landlock` variant boots and reports `lsm=…landlock`.
3. `experiments/kcheck2.mjs`: `openshell-sandbox` from OpenShell `main` prints `"qualified": true`. This checks the kernel, not this driver.
4. `make e2e`: 22 checks, all passing.
5. Claude Code with a real key ([docs/CLAUDE-CODE.md](docs/CLAUDE-CODE.md)).
6. `./gw/cleanup.sh --orphans`: nothing to clean up.

## Teardown

- Delete sandboxes through OpenShell (`./gw/cleanup.sh <n> --apply`). That deletes their Blaxel VMs.
- Run `./gw/cleanup.sh --orphans --apply` for VMs whose gateway record is gone.
- Delete throwaway providers (`openshell -g blaxel provider delete <p>`).
- Stop the local processes with `make down`.
- Remove the CLI registration with `openshell gateway remove blaxel` if you're done. `gw/tls/` and `gw/gateway.db` are local state; delete them to start from scratch.

## Primary references

- [OpenShell](https://github.com/NVIDIA/OpenShell) and its [compute driver contract](driver/proto/compute_driver.proto) (v0.0.116)
- [OpenShell providers](https://github.com/NVIDIA/OpenShell/blob/main/docs/sandboxes/manage-providers.mdx) and [providers v2](https://github.com/NVIDIA/OpenShell/blob/main/docs/sandboxes/providers-v2.mdx)
- [RFC 0012: isolation backend](https://github.com/NVIDIA/OpenShell/tree/main/rfc/0012-isolation-backend), [issue #3361](https://github.com/NVIDIA/OpenShell/issues/3361), [PR #3366](https://github.com/NVIDIA/OpenShell/pull/3366)
- [Blaxel sandboxes](https://docs.blaxel.ai/Sandboxes/Overview), [processes](https://docs.blaxel.ai/Sandboxes/Processes), [ports](https://docs.blaxel.ai/Sandboxes/Ports)
- [Landlock](https://docs.kernel.org/userspace-api/landlock.html)
