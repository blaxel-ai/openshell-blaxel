# Claude Code on Blaxel via OpenShell

The driver installs Claude Code at `/usr/local/bin/claude` during bootstrap (`-install-claude`, on by default). OpenShell's provider profile trusts that path.

> Status on OpenShell main: the profile imports and the binary is installed and confined. A full Claude session through the supervisor hasn't been validated on main yet. It worked on v0.0.116 (tag `openshell-v0.0.116`).

## Run

```sh
make configure            # imports gw/claude-code-blaxel.yaml (once per control plane)

read -s "ANTHROPIC_API_KEY?Anthropic API key: " && export ANTHROPIC_API_KEY
make os ARGS='provider create --name my-claude --type claude-code-blaxel --from-existing'
unset ANTHROPIC_API_KEY

make os ARGS='sandbox create --name claude --provider my-claude --no-tty -- sleep infinity'
make os ARGS='sandbox exec -n claude --tty -- claude'
```

The key is sent to the gateway in the control sandbox and stays there.

With `-- claude` as the create command instead, Claude is the sandbox's main process, and exiting it ends the sandbox.

Use an Anthropic **API key** (console.anthropic.com). Logging in with a Claude subscription isn't supported.

## What protects what

| | |
|---|---|
| Key | The workload only sees a placeholder. The supervisor, in the control sandbox, injects `x-api-key` on egress. |
| Network | Only `/usr/bin/claude` and `/usr/local/bin/claude` may reach `api.anthropic.com`, `statsig.anthropic.com`, `platform.claude.com` and `sentry.io`. Other binaries and hosts are denied. |
| Filesystem | Landlock allows writes to `/sandbox` (`$HOME`) and `/tmp` only. |
| Audit | `make os ARGS='logs claude' \| grep -E 'ALLOWED\|DENIED'` |

## Why a custom profile

OpenShell main ships no built-in provider profiles. `gw/claude-code-blaxel.yaml` is the v0.0.116 `claude-code` profile plus `platform.claude.com`, which Claude Code 2.1+ calls at startup. Without that host, startup fails with `ERR_PROXY_TUNNEL`.

The v0.0.116 `providers_v2_enabled` setting doesn't exist on main, so `make configure` only imports the profile.
