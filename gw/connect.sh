#!/bin/sh
# Connects the laptop to the Blaxel-hosted gateway: starts `os-tunnel dial`
# on 127.0.0.1:$LOCAL_PORT (if not already running) and registers the CLI
# gateway with the client bundle fetched by `os-deploy up`. Idempotent.
set -e
. "$(dirname "$0")/env.sh"

PAT="os-tunnel dial -sandbox $CONTROL_SANDBOX -listen 127.0.0.1:$LOCAL_PORT"
if ! pgrep -f "$PAT" >/dev/null; then
  mkdir -p "$(dirname "$TUNNEL_LOG")"
  nohup "$ROOT/driver/bin/os-tunnel" dial -sandbox "$CONTROL_SANDBOX" -listen "127.0.0.1:$LOCAL_PORT" \
    -workspace "$BL_WORKSPACE" -env "$BL_ENV" > "$TUNNEL_LOG" 2>&1 &
  for _ in $(seq 50); do grep -q "tunnel up" "$TUNNEL_LOG" 2>/dev/null && break; sleep 0.2; done
fi
grep -q "tunnel up" "$TUNNEL_LOG" || { echo "tunnel did not come up; see $TUNNEL_LOG" >&2; exit 1; }

# `gateway add --local` seeds mtls/ with local defaults, so install the
# control plane's bundle afterwards.
"$OS_BIN" gateway add "https://127.0.0.1:$LOCAL_PORT" --local --name "$GATEWAY_NAME" >/dev/null 2>&1 || true
MTLS="$OS_CONFIG_HOME/openshell/gateways/$GATEWAY_NAME/mtls"
mkdir -p "$MTLS"
install -m 600 "$CLIENT_BUNDLE/ca.crt" "$CLIENT_BUNDLE/tls.crt" "$CLIENT_BUNDLE/tls.key" "$MTLS/"
oscli status | grep -E 'Status:|Authentication:|Version:'
echo "use: XDG_CONFIG_HOME=$OS_CONFIG_HOME $OS_BIN -g $GATEWAY_NAME <command>   (or: make os ARGS='sandbox list')"
