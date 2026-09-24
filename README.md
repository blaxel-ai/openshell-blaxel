# OpenShell on Blaxel

Run [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandboxes as Blaxel
microVMs. An external OpenShell compute driver creates one Blaxel sandbox per
OpenShell sandbox. OpenShell's supervisor runs inside it and enforces policy:
per-binary egress rules, L7 REST rules, credential injection and Landlock
filesystem confinement.

Status: working proof of concept on the **OpenShell v0.0.116** driver contract,
end-to-end tested (22/22) against Blaxel dev. The port to OpenShell `main` is
next; see [docs/ROADMAP.md](docs/ROADMAP.md).

```
laptop                                   Blaxel microVM (mk3, landlock kernel)
openshell CLI ──mTLS──► gateway :17680   ┌──────────────────────────────────────┐
                           │ UDS         │ os-tunnel :9000 ◄─┐                  │
                  openshell-driver-blaxel│   127.0.0.1:17680 │ reverse tunnel   │
                           │ Blaxel API  │ openshell-sandbox (supervisor, root) │
                           └────────────►│   netns + proxy + Landlock          │
                  WebSocket (yamux) ────►│   └─ agent (uid 1500)               │
                                         └──────────────────────────────────────┘
```

## Requirements

- macOS or Linux with Go ≥ 1.24, `gh`, `bl` (logged in to the target workspace)
- OpenShell CLI + gateway **v0.0.116** (`brew install nvidia/openshell/openshell`)
- A Blaxel workspace whose region supports the `landlock` kernel variant
  (`spec.runtime.extraArgs: {landlock: enabled}`), see [docs/KERNEL.md](docs/KERNEL.md)

## Quick start

```sh
export BL_WORKSPACE=charlou-dev BL_ENV=dev BL_REGION=us-was-1   # defaults
make setup        # gateway PKI + register CLI gateway "blaxel" (once)
make up           # fetch runtime, build, start driver + gateway, configure
make e2e          # end-to-end test against real Blaxel sandboxes (~3 min)
```

Use it:

```sh
openshell -g blaxel sandbox create --name dev --no-tty -- sleep infinity
openshell -g blaxel sandbox exec -n dev --tty -- bash -l
openshell -g blaxel logs dev              # OCSF allow/deny audit trail
openshell -g blaxel sandbox delete dev
```

Claude Code: see [docs/CLAUDE-CODE.md](docs/CLAUDE-CODE.md).

## Layout

| Path | Contents |
|---|---|
| `driver/` | Go external compute driver (`cmd/openshell-driver-blaxel`), in-VM tunnel endpoint (`cmd/os-tunnel`), Blaxel REST client, reverse tunnel, generated v0.0.116 protos |
| `gw/` | Local gateway scripts (`env.sh`, `setup.sh`, `restart.sh`), `e2e.sh`, `claude-code-blaxel.yaml` provider profile |
| `docs/` | Architecture, kernel requirements, Claude Code guide, roadmap, experiment results |
| `experiments/` | Feasibility experiments: kernel probe, WS relay, gRPC-through-proxy, kernel variant checks |

## Configuration

All settings live in `gw/env.sh` and can be overridden from the environment:

| Variable | Default | Meaning |
|---|---|---|
| `BL_WORKSPACE` / `BL_ENV` / `BL_REGION` | `charlou-dev` / `dev` / `us-was-1` | Where sandboxes are created |
| `KERNEL_VARIANT` | `landlock` | `spec.runtime.extraArgs` key enabled on create (empty: default kernel) |
| `SANDBOX_PACKAGES` | `curl,git,ca-certificates` | Installed as root at bootstrap |
| `GATEWAY_NAME` / `GATEWAY_PORT` | `blaxel` / `17680` | Local gateway identity |
| `OPENSHELL_GATEWAY_BIN` | Homebrew path | v0.0.116 `openshell-gateway` |

## Known limitations

- **v0.0.116 architecture:** the supervisor runs as root in the VM with the
  gateway client cert and sandbox JWT. OpenShell `main` moves it out of the
  VM; porting is tracked in the roadmap.
- **The first boot of each sandbox takes ~20 s** (uploads plus apt install). A
  Blaxel template image with the runtime baked in would remove it.
- Non-Blaxel images (for example OpenShell's community image) are replaced by
  `blaxel/py-app`, and the driver emits an `ImageSubstituted` event.
- The v0.0.116 gateway doesn't reconnect to a restarted driver. `gw/restart.sh`
  restarts both.
