# AGENTS.md

Orientation for an AI coding agent, or a human, working in this repo. Read this first for the fastest correct run.

## What this is

An external OpenShell (v0.0.116) compute driver that runs each OpenShell sandbox as a Blaxel microVM. The OpenShell gateway runs locally, and each sandbox reaches it through a reverse tunnel dialed by the driver.

- `driver/`: Go. `cmd/openshell-driver-blaxel` (gRPC `ComputeDriver` on a Unix socket), `cmd/os-tunnel` (uploaded into each VM), `internal/blaxel` (REST client), `internal/tunnel` (yamux over WebSocket), `internal/driver` (lifecycle), `gen/computev1` (generated from `proto/`).
- `gw/`: local gateway operation. `env.sh` (settings), `setup.sh`, `restart.sh`, `status.sh`, `cleanup.sh`, `e2e.sh`, `claude-code-blaxel.yaml`.
- `experiments/`: feasibility probes and their scripts. They aren't part of the product.

Public quickstart: `README.md`. Design and operations: `GUIDE.md`. Machine summary: `llms.txt`.

## Prerequisites

- Go 1.25+, `gh`, `bl` logged in (`bl login <workspace>`), OpenShell v0.0.116 CLI and gateway (`brew install nvidia/openshell/openshell`).
- `cp .env.example .env` and set the workspace, env and region. The region must support the `landlock` kernel variant.

## Setup, in order

1. `make test`: local unit tests and script syntax.
2. `make setup`: generates `gw/tls/` and registers CLI gateway `blaxel` (local only).
3. `make up`: fetches `bin/openshell-sandbox`, builds, starts the driver and gateway, runs `make configure`.
4. `make status`: expect `Authenticated`, driver running, `providers_v2: enabled`.
5. `make e2e`: creates and deletes one Blaxel sandbox, 22 checks.

## Commands

| Command | What it does | Side effects |
| -- | -- | -- |
| `make test` | `go test ./...` and `sh -n gw/*.sh` | local, safe |
| `make build` | builds the driver and linux `os-tunnel` | local, safe |
| `make fetch` | downloads and checksum-verifies `openshell-sandbox` v0.0.116 | network read, local file |
| `make setup` | gateway PKI + CLI registration | writes `gw/tls/` and `~/.config/openshell/gateways/blaxel` |
| `make up` / `./gw/restart.sh` | (re)starts the driver + gateway | **drops every open sandbox session**; workloads survive |
| `make configure` | enables `providers_v2_enabled`, imports the Claude profile | mutates gateway settings |
| `make status` | health and sandbox mapping | read-only Blaxel and gateway calls |
| `make e2e` | full end-to-end run | **creates and deletes a Blaxel sandbox** |
| `./gw/cleanup.sh <n>` / `--orphans` | prints an exact plan | read-only |
| `./gw/cleanup.sh … --apply` | deletes the named sandboxes or orphan VMs | **deletes Blaxel sandboxes** |
| `openshell -g blaxel sandbox create …` | new sandbox | **creates a Blaxel sandbox** |
| `node experiments/*.mjs` | experiments | **most create and delete Blaxel sandboxes** |
| `make proto` | regenerates `driver/gen` | local, needs `protoc` + Go plugins |

## Where to look

| Path | Responsibility |
| -- | -- |
| `driver/internal/driver/driver.go` | RPCs, provisioning, bootstrap script, launch env, monitor, recovery |
| `driver/internal/blaxel/client.go` | Blaxel REST calls, mirroring `@blaxel/core` (API `2026-04-28`) |
| `driver/internal/tunnel/tunnel.go` | reverse tunnel both halves, keepalive, reconnect |
| `driver/proto/` | OpenShell v0.0.116 protos (Apache-2.0, NVIDIA), copied verbatim |
| `gw/e2e.sh` | the acceptance test; add checks here for behavior changes |
| `docs/KERNEL.md` | kernel requirements and qualification |
| `docs/ROADMAP.md` | the OpenShell `main` port |

## Invariants

- Start long-lived Blaxel processes with `keepAlive: true` **and** `timeout: 0` (`blaxel.Forever`). `omitempty` must not drop the zero, which is why `ProcessRequest.Timeout` is `*int`. Without it they die at 600 s.
- The Blaxel filesystem API ignores the multipart `permissions` field, so set modes in the bootstrap script.
- In Blaxel's process API `HOME` is `/blaxel`. Pin `HOME` when installers depend on it (see `claudeSetup`).
- Readiness is `/run/openshell/ssh.sock` existing, the same signal as the Podman driver.
- Recovery (`reattach`) must not restart `openshell-sandbox`. Only `Create`/`Start` do.
- Never let user environment override `OPENSHELL_*`. Keep `shellQuote` on every value in `launch.sh`.
- The kernel variant (`extraArgs`) is immutable after creation.
- The v0.0.116 gateway doesn't reconnect to a restarted driver. Restart both (`gw/restart.sh`).
- A process that exits cleanly must not delete a socket path a newer driver has bound (`os.SameFile` check in `main.go`).
- `openshell gateway add --local` overwrites that gateway's `mtls/` with the Homebrew certs. `gw/setup.sh` installs ours afterwards.
- `openshell policy set` on a live sandbox can't remove filesystem paths. Start from `policy get <n> --base`.
- The command after `--` in `sandbox create` is the sandbox's lifetime.

## Safe vs. company-facing

- Local edits, `make test`, `make build` and plan-only cleanup are allowed.
- Before anything that creates or deletes Blaxel sandboxes (`make e2e`, `sandbox create`, experiments, `cleanup --apply`), restarts the driver or gateway (`make up`, `gw/restart.sh`: it drops users' open sessions), or changes gateway settings, get explicit human approval and name the side effect.
- Never commit `gw/tls/`, `gw/*.db`, logs, `.env` or `experiments/exp3.json` (preview tokens). `.gitignore` covers them. Check before staging.
- Do not push, open PRs, change repo visibility, file upstream issues (NVIDIA/OpenShell) or post publicly without explicit human approval.
