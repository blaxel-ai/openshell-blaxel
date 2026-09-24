#!/bin/sh
# Restarts the Blaxel driver and its gateway together (the v0.0.116 gateway
# does not reconnect to a restarted external driver).
. "$(dirname "$0")/env.sh"
cd "$GW_DIR"
# Match this instance only: other gateways/drivers may run side by side.
GW_PAT="openshell-gateway --name $GATEWAY_NAME "
DRV_PAT="openshell-driver-blaxel -socket $DRIVER_SOCKET "
pkill -f "$GW_PAT"
pkill -f "$DRV_PAT"
while pgrep -f "$DRV_PAT" >/dev/null || pgrep -f "$GW_PAT" >/dev/null; do sleep 0.2; done
nohup ./run-driver.sh > "$DRIVER_LOG" 2>&1 &
while [ ! -S "$DRIVER_SOCKET" ]; do sleep 0.2; done
nohup ./run-gateway.sh > "$GATEWAY_LOG" 2>&1 &
for i in $(seq 50); do openshell -g "$GATEWAY_NAME" status >/dev/null 2>&1 && echo "gateway ready" && exit 0; sleep 0.3; done
echo "gateway not ready; see $GATEWAY_LOG and $DRIVER_LOG" >&2; exit 1
