# Claude Code on Blaxel via OpenShell

The driver installs Claude Code at `/usr/local/bin/claude` during bootstrap
(`-install-claude`, on by default). That's a path OpenShell's provider profile
trusts.

## Run

```sh
read -s "ANTHROPIC_API_KEY?Anthropic API key: " && export ANTHROPIC_API_KEY
openshell -g blaxel provider create --name my-claude --type claude-code-blaxel --from-existing
unset ANTHROPIC_API_KEY

openshell -g blaxel sandbox create --name claude --provider my-claude -- claude
```

`claude` is then the sandbox's main process, and exiting it ends the sandbox. To keep the
sandbox:

```sh
openshell -g blaxel sandbox create --name claude --provider my-claude --no-tty -- sleep infinity
openshell -g blaxel sandbox exec -n claude --tty -- claude
```

Use an Anthropic **API key** (console.anthropic.com). A Claude subscription login isn't supported.

## What protects what

| | |
|---|---|
| Key | The workload only sees a placeholder (`openshell:resolve:env:v4…`). The proxy injects `x-api-key` on egress. |
| Network | Only `/usr/bin/claude` and `/usr/local/bin/claude` may reach `api.anthropic.com`, `statsig.anthropic.com`, `platform.claude.com` and `sentry.io`. `curl` to the same host is denied, and so is Datadog telemetry. |
| Filesystem | Landlock allows writes to `/sandbox` (`$HOME`) and `/tmp` only. |
| Audit | `openshell -g blaxel logs claude \| grep -E 'ALLOWED\|DENIED'` |

## Why a custom profile

The built-in `claude-code` profile (v0.0.116) lacks `platform.claude.com`, which
Claude Code 2.1+ calls at startup, so it fails with `ERR_PROXY_TUNNEL`.
`gw/claude-code-blaxel.yaml` is the built-in profile plus that host. `make configure`
imports it and enables `providers_v2_enabled`. Without that setting,
providers inject credentials but add no network policy.
