# AGENTS.md

Orientation for an AI coding agent, or a human, working in this repo. Read this first for the fastest correct run.

## What this is

OpenShell **main** (RFC 0012 isolation contract) running entirely on Blaxel.
- A control sandbox (`os-control`) runs:
  - the OpenShell gateway;
  - this repo's compute driver;
  - one `openshell-supervisor` per agent;
  - a CLAT and a DoH forwarder.
- Each agent runs in its own Blaxel workload sandbox under `openshell-sandbox launch-capability-free`, in a network namespace that only has loopback.
- The laptop runs the `openshell` CLI and one WebSocket tunnel.

- `driver/`: Go.
  - `cmd/openshell-driver-blaxel`: gRPC `ComputeDriver` on a Unix socket.
  - `cmd/os-tunnel`:
    - `serve`: in workloads;
    - `dial`: on the laptop;
    - `dns`: the DoH forwarder in the control sandbox.
  - `cmd/os-deploy`: creates and updates the control sandbox.
  - `internal/boundary`: main's wire formats and per-session TLS.
  - `internal/driver`: lifecycle.
  - `internal/blaxel`: adapter over the Blaxel Go SDK `sdk-go`, the SDK used by `bl` in blaxel-ai/toolkit.
  - `internal/tunnel`: yamux over WebSocket.
  - `internal/doh`.
  - `gen/`: generated from `proto/`.
- `gw/`: laptop operation.
  - `env.sh`: settings.
  - `connect.sh`, `status.sh`, `cleanup.sh`, `e2e.sh`.
  - `claude-code-blaxel.yaml`.
- `experiments/`: feasibility probes. They aren't part of the product.

Public quickstart: `README.md`. Design and operations: `GUIDE.md`. Machine summary: `llms.txt`.

## Prerequisites

- Go 1.25+, `gh`, `bl` logged in (`bl login <workspace>`).
- `cp .env.example .env`, then set the workspace, env, region and `BL_API_KEY` (service account). The region must support the `landlock`, `tun` and `iptables` kernel variants.

## Setup, in order

1. `make test`: unit tests and script syntax, local.
2. `make deploy`:
   - fetches OpenShell main (`dev` release) and builds;
   - creates or updates `os-control`;
   - fetches the CLI bundle to `~/.openshell-blaxel/mtls`.
3. `make connect`: laptop tunnel on `127.0.0.1:17690` and CLI gateway `blaxel-main`.
4. `make status`: expect every control process `running`, `Connected`, `Authenticated`.
5. `make e2e`: creates and deletes one workload sandbox, 20 checks.

## Commands

| Command | What it does | Side effects |
| -- | -- | -- |
| `make test` | `go test ./...` and `sh -n` on scripts | local, safe |
| `make build` | driver + `os-tunnel` (linux), `os-tunnel` + `os-deploy` (host) | local, safe |
| `make fetch` | downloads and checksum-verifies OpenShell main binaries | network read, local files |
| `make deploy` | creates or updates the control sandbox | **creates a Blaxel sandbox; a redeploy restarts gateway, driver and supervisors, dropping sessions** |
| `make connect` | laptop tunnel + CLI registration | writes `~/.openshell-blaxel/cli-config` |
| `make configure` | imports the Claude Code provider profile | mutates the gateway |
| `make status` | health and sandbox mapping | read-only |
| `make e2e` | full end-to-end run | **creates and deletes a Blaxel sandbox** |
| `make os ARGS='sandbox create …'` | new sandbox | **creates a Blaxel sandbox** |
| `./gw/cleanup.sh <n>` / `--orphans` | prints an exact plan | read-only |
| `./gw/cleanup.sh … --apply` | deletes the named sandboxes or orphan VMs | **deletes Blaxel sandboxes** |
| `make destroy` | deletes the control sandbox | **destroys the gateway and its state** |
| `node experiments/*.mjs`, `experiments/*.sh` | experiments | **most create and delete Blaxel sandboxes** |
| `make proto` | regenerates `driver/gen` | local, needs `protoc` + Go plugins |

## Where to look

| Path | Responsibility |
| -- | -- |
| `driver/internal/driver/driver.go` | RPCs, capability negotiation, admission ack, recovery |
| `driver/internal/driver/launch.go` | provisioning, VM bootstrap, fence launcher, supervisor spawn, watch, teardown |
| `driver/internal/boundary/` | `BoundaryConfig`, runtime descriptor, outer fence, workload identity, TLS material. `fixture/` runs them against the real binaries |
| `driver/internal/blaxel/client.go` | Blaxel operations through `github.com/blaxel-ai/sdk-go` v0.27.2. Don't hand-roll HTTP; extend the adapter |
| `driver/internal/tunnel/tunnel.go` | `Serve` (workload side), `Listen`/`Endpoint.Run` (control and laptop side) |
| `driver/cmd/os-deploy/main.go` | control sandbox: kernel variants, uploads, `gateway.toml`, CLAT script, process set |
| `driver/proto/` | OpenShell main protos (Apache-2.0, NVIDIA) at `08548713c`, copied verbatim |
| `gw/e2e.sh` | the acceptance test; add checks here for behavior changes |
| `docs/KERNEL.md` | kernel requirements and qualification |

## Invariants

- Use the Blaxel Go SDK for every Blaxel call. The only non-SDK requests are the tunnel WebSockets, which take their auth from the adapter's `Headers()`.
- Start long-lived Blaxel processes with `keepAlive: true` **and** `timeout: 0` (`blaxel.Forever`). The zero must reach the wire (`client_test.go` checks it). Without it they die at 600 s.
- Every sandbox carries `openshell.ai/driver-owner=<GATEWAY_NAME>`. Recovery, `status` and `cleanup` filter on it. Unlabeled sandboxes belong to `blaxel`, the v0.0.116 local gateway. A wrong `GATEWAY_NAME` makes other people's sandboxes look like orphans.
- `BoundaryConfig` and the runtime descriptor are `deny_unknown_fields` on the Rust side. Change them only together with `boundary_test.go`, and re-run `fixture/` against the real binaries.
- The supervisor auth bundle is written verbatim. No JWT goes into the workload VM.
- The gateway owns readiness (`driver_reports_runtime_readiness=false`). A sandbox pins its first supervisor, so Start must relaunch both sides.
- Unix socket paths must fit `SUN_LEN` (108): SSH sockets are `/run/openshell-blaxel/<hash>/ssh.sock`.
- The admission acknowledgement must match the gateway's default string byte for byte (`DefaultAdmissionPolicy`).
- Never let user environment override `OPENSHELL_*`.
- Kernel variants (`extraArgs`) are immutable after creation.
- The gateway runs with `BL_API_KEY` unset. Only the driver reads `driver.env`.
- `policy get --base` on main prints no trailing newline. Add one before appending YAML.
- The command after `--` in `sandbox create` is the sandbox's lifetime.

## Safe vs. company-facing

- Local edits, `make test`, `make build` and plan-only cleanup are allowed.
- Get explicit human approval, naming the side effect, before anything that:
  - creates or deletes Blaxel sandboxes (`make deploy`, `make e2e`, `sandbox create`, experiments, `cleanup --apply`, `make destroy`);
  - restarts the control plane (a redeploy drops open sessions);
  - changes gateway settings.
- Never commit `.env`, `gw/tls/`, `*.db`, logs or `experiments/exp3.json` (preview tokens). `.gitignore` covers them. Check before staging.
- Don't push, open PRs, change repo visibility, file upstream issues (NVIDIA/OpenShell) or post publicly without explicit human approval.
