#!/bin/sh
# Starts the Blaxel compute driver for the local gateway.
. "$(dirname "$0")/env.sh"
exec "$ROOT/driver/bin/openshell-driver-blaxel" \
  -socket "$DRIVER_SOCKET" \
  -workspace "$BL_WORKSPACE" -env "$BL_ENV" -region "$BL_REGION" \
  -gateway-addr "127.0.0.1:$GATEWAY_PORT" -vm-gateway-port "$GATEWAY_PORT" \
  -tls-dir "$TLS_DIR" \
  -sandbox-bin "$ROOT/bin/openshell-sandbox" \
  -tunnel-bin "$ROOT/driver/bin/os-tunnel-linux-amd64" \
  -kernel-variant "$KERNEL_VARIANT" -packages "$SANDBOX_PACKAGES"
