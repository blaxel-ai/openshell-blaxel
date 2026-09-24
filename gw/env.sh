#!/bin/sh
# Shared settings for the local Blaxel-backed OpenShell gateway. Override any
# of these in the environment before running the scripts in this directory.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Optional local overrides (gitignored); see .env.example.
if [ -f "$ROOT/.env" ]; then
  set -a
  . "$ROOT/.env"
  set +a
fi

: "${BL_WORKSPACE:=charlou-dev}"
: "${BL_ENV:=dev}"
: "${BL_REGION:=us-was-1}"
: "${OPENSHELL_GATEWAY_BIN:=$(command -v openshell-gateway || echo /opt/homebrew/opt/openshell/bin/openshell-gateway)}"
: "${GATEWAY_NAME:=blaxel}"
: "${GATEWAY_PORT:=17680}"
: "${DRIVER_SOCKET:=/tmp/openshell-blaxel/driver.sock}"
: "${KERNEL_VARIANT:=landlock}"
: "${SANDBOX_PACKAGES:=curl,git,ca-certificates}"

export BL_WORKSPACE BL_ENV BL_REGION OPENSHELL_GATEWAY_BIN GATEWAY_NAME GATEWAY_PORT DRIVER_SOCKET KERNEL_VARIANT SANDBOX_PACKAGES
GW_DIR="$ROOT/gw"
TLS_DIR="$GW_DIR/tls"
