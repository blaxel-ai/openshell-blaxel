#!/bin/sh
# Starts an OpenShell v0.0.116 gateway on 127.0.0.1:$GATEWAY_PORT backed by the
# Blaxel driver socket.
. "$(dirname "$0")/env.sh"
export OPENSHELL_LOCAL_TLS_DIR="$TLS_DIR"
exec "$OPENSHELL_GATEWAY_BIN" \
  --name "$GATEWAY_NAME" --bind-address 127.0.0.1 --port "$GATEWAY_PORT" \
  --db-url "sqlite:$GATEWAY_DB?mode=rwc" --log-level info \
  --enable-mtls-auth true \
  --drivers blaxel --compute-driver-socket "$DRIVER_SOCKET"
