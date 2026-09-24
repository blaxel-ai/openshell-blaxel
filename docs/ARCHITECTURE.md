# Architecture

## Components

| Component | Where | Role |
|---|---|---|
| `openshell` CLI | laptop | User interface |
| `openshell-gateway` v0.0.116 | laptop, `127.0.0.1:17680` | Control plane, mTLS, sandbox JWTs, policy, relay |
| `openshell-driver-blaxel` | laptop, Unix socket | Implements `openshell.compute.v1.ComputeDriver` on the Blaxel API |
| `os-tunnel` | in each VM, port 9000 | In-VM end of the reverse tunnel |
| `openshell-sandbox` v0.0.116 | in each VM, root | Supervisor: netns, policy proxy, Landlock, seccomp, SSH relay |
| workload | in each VM, uid 1500 | The agent (`sleep`, `bash`, `claude`, …) |

## Why a reverse tunnel

Blaxel ingress is HTTPS/WebSocket only, and the preview proxy talks HTTP/1.1
to the workload, so gRPC can't be exposed directly. The sandbox also must not
hold credentials that can reach the gateway host. So:

1. The driver dials `wss://<sandbox>/port/9000/tunnel` with its Blaxel token.
2. `os-tunnel` runs a yamux client over that WebSocket and listens on
   `127.0.0.1:17680` inside the VM.
3. `openshell-sandbox` dials `OPENSHELL_ENDPOINT=https://127.0.0.1:17680`.
   Each TCP connection becomes a yamux stream, which the driver connects to the
   real gateway.

The gateway's mTLS is end to end inside the tunnel, so Blaxel's edge only sees
opaque bytes. yamux keepalives (15 s) keep the WebSocket under Blaxel's idle
cutoff.

## Lifecycle mapping

| OpenShell RPC | Blaxel |
|---|---|
| `CreateSandbox` | `POST /sandboxes` (labels `openshell.ai/*`, `extraArgs` kernel variant) → wait `DEPLOYED` → upload runtime, TLS, token, launch script → bootstrap (packages, `sandbox` user, Claude Code) → start `os-tunnel` → dial tunnel → start `openshell-sandbox` |
| Ready condition | `/run/openshell/ssh.sock` exists (same signal as the Podman driver's healthcheck) |
| `StopSandbox` | kill `openshell-sandbox`, close tunnel, report `Suspended=True`; the VM idles into standby |
| `StartSandbox` | restart tunnel endpoint + `openshell-sandbox` from the stored launch script |
| `DeleteSandbox` | `DELETE /sandboxes/{name}` |
| driver restart | rebuild state from Blaxel labels and re-attach tunnels **without** restarting workloads |

Blaxel processes are started with `keepAlive: true, timeout: 0`. Without
an explicit `timeout: 0`, keepAlive processes are killed after 600 s.

## Bootstrap contract (v0.0.116, mirrors the Podman driver)

Launch environment written to `/opt/openshell/launch.sh` (root, 0700):
`OPENSHELL_SANDBOX{,_ID}`, `OPENSHELL_ENDPOINT`, `OPENSHELL_SSH_SOCKET_PATH`,
`OPENSHELL_MAIN_PROCESS_SPEC` (base64url JSON), `OPENSHELL_TLS_{CA,CERT,KEY}`,
`OPENSHELL_SANDBOX_TOKEN_FILE`, `OPENSHELL_OCI_IMAGE_USER=sandbox`,
`OPENSHELL_NETWORK_RUNTIME_CAPABILITIES=policy-dns-transparent-tcp`.
User environment can't override `OPENSHELL_*` names.

Files: `/opt/openshell/bin/{openshell-sandbox,os-tunnel}`,
`/etc/openshell/tls/client/{ca.crt,tls.crt,tls.key}`,
`/etc/openshell/auth/sandbox.jwt`. The filesystem API ignores upload modes, so
the bootstrap script sets them.
