#!/bin/sh
# Restarts the Blaxel driver and its gateway together (the v0.0.116 gateway
# does not reconnect to a restarted external driver).
. "$(dirname "$0")/env.sh"
cd "$GW_DIR"
pkill -f "openshell-gateway --name $GATEWAY_NAME"
pkill -f 'bin/openshell-driver-blaxel'
while pgrep -f 'bin/openshell-driver-blaxel' >/dev/null || pgrep -f "openshell-gateway --name $GATEWAY_NAME" >/dev/null; do sleep 0.2; done
nohup ./run-driver.sh > driver.log 2>&1 &
while [ ! -S "$DRIVER_SOCKET" ]; do sleep 0.2; done
nohup ./run-gateway.sh > gateway.log 2>&1 &
for i in $(seq 50); do openshell -g "$GATEWAY_NAME" status >/dev/null 2>&1 && echo "gateway ready" && exit 0; sleep 0.3; done
echo "gateway not ready; see gw/gateway.log and gw/driver.log" >&2; exit 1
