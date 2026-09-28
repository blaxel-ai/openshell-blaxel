#!/bin/sh
# Shared settings for the Blaxel-hosted OpenShell control plane. Override any
# of these in .env (see .env.example) or in the environment.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ -f "$ROOT/.env" ]; then
  set -a
  . "$ROOT/.env"
  set +a
fi

: "${BL_WORKSPACE:=my-workspace}"
: "${BL_ENV:=prod}"
: "${BL_REGION:=us-was-1}"
# Control sandbox running the gateway, the driver and the supervisors.
: "${CONTROL_SANDBOX:=os-control}"
# Driver owner label and CLI gateway name (one per control plane).
: "${GATEWAY_NAME:=blaxel-main}"
# Laptop port the CLI uses; os-tunnel dial forwards it to the gateway.
: "${LOCAL_PORT:=17690}"
# OpenShell main CLI and an isolated CLI config (keeps other OpenShell
# installs on the laptop untouched).
: "${OS_BIN:=$ROOT/bin/main/openshell}"
: "${OS_CONFIG_HOME:=$HOME/.openshell-blaxel/cli-config}"
: "${CLIENT_BUNDLE:=$HOME/.openshell-blaxel/mtls}"

export BL_WORKSPACE BL_ENV BL_REGION CONTROL_SANDBOX GATEWAY_NAME LOCAL_PORT OS_BIN OS_CONFIG_HOME CLIENT_BUNDLE
export XDG_CONFIG_HOME="$OS_CONFIG_HOME"
GW_DIR="$ROOT/gw"
TUNNEL_LOG="$HOME/.openshell-blaxel/tunnel-$GATEWAY_NAME.log"

oscli() { "$OS_BIN" -g "$GATEWAY_NAME" "$@" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g'; }
