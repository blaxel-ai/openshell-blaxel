#!/bin/sh
# One-time local setup: gateway PKI, CLI registration, and gateway settings.
# Idempotent; safe to re-run.
set -e
. "$(dirname "$0")/env.sh"

if [ ! -f "$TLS_DIR/ca.crt" ]; then
  "$OPENSHELL_GATEWAY_BIN" generate-certs --output-dir "$TLS_DIR"
fi

# `gateway add --local` copies the Homebrew gateway's certs into the CLI's
# mtls/ dir, so install ours afterwards.
openshell gateway add "https://127.0.0.1:$GATEWAY_PORT" --local --name "$GATEWAY_NAME" >/dev/null 2>&1 || true
MTLS="${XDG_CONFIG_HOME:-$HOME/.config}/openshell/gateways/$GATEWAY_NAME/mtls"
mkdir -p "$MTLS"
install -m 644 "$TLS_DIR/ca.crt" "$MTLS/ca.crt"
install -m 644 "$TLS_DIR/client/tls.crt" "$MTLS/tls.crt"
install -m 600 "$TLS_DIR/client/tls.key" "$MTLS/tls.key"
echo "registered CLI gateway '$GATEWAY_NAME' -> https://127.0.0.1:$GATEWAY_PORT"
