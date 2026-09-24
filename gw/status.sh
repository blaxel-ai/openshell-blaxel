#!/bin/sh
# Read-only status: gateway and driver health, OpenShell sandboxes joined with
# their Blaxel sandboxes, and orphans on either side.
. "$(dirname "$0")/env.sh"

echo "== gateway '$GATEWAY_NAME' (127.0.0.1:$GATEWAY_PORT)"
openshell -g "$GATEWAY_NAME" status 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | grep -E 'Status:|Authentication:|Version:|Error' | sed 's/^ */  /'
printf '  driver process: '; pgrep -f "openshell-driver-blaxel -socket $DRIVER_SOCKET " >/dev/null && echo running || echo "NOT RUNNING"
printf '  driver socket:  '; [ -S "$DRIVER_SOCKET" ] && echo "$DRIVER_SOCKET" || echo "MISSING ($DRIVER_SOCKET)"
printf '  providers_v2:   '; openshell -g "$GATEWAY_NAME" settings get --global 2>/dev/null | sed -E 's/\x1b\[[0-9;]*m//g' | grep -q 'providers_v2_enabled.*true' && echo enabled || echo "disabled (run: make configure)"

echo "== sandboxes (workspace $BL_WORKSPACE, $BL_ENV)"
OS_LIST=$(openshell -g "$GATEWAY_NAME" sandbox list 2>/dev/null | sed -E 's/\x1b\[[0-9;]*m//g')
BL_JSON=$(bl get sandboxes -w "$BL_WORKSPACE" -o json 2>/dev/null)
OS_LIST="$OS_LIST" BL_JSON="$BL_JSON" python3 - <<'EOF'
import json, os
phases = {}
for line in os.environ["OS_LIST"].splitlines()[1:]:
    parts = line.split()
    if len(parts) >= 2:
        phases[parts[0]] = parts[-1]
try:
    blx = json.loads(os.environ["BL_JSON"] or "[]")
except json.JSONDecodeError:
    blx = []
managed = {}
for s in blx:
    labels = s.get("metadata", {}).get("labels") or {}
    l = labels
    if labels.get("openshell.ai/managed-by") == "openshell-driver-blaxel" and l.get("openshell.ai/driver-owner", "blaxel") == os.environ["GATEWAY_NAME"]:
        managed[labels.get("openshell.ai/sandbox-name", "?")] = (
            s["metadata"]["name"], s.get("status", "?"),
            (s.get("spec", {}).get("runtime", {}).get("extraArgs") or {}))
rows = sorted(set(phases) | set(managed))
if not rows:
    print("  none")
fmt = "  {:<20} {:<13} {:<40} {:<11} {}"
print(fmt.format("OPENSHELL", "PHASE", "BLAXEL SANDBOX", "STATUS", "KERNEL"))
for n in rows:
    bname, bstatus, extra = managed.get(n, ("-", "-", {}))
    kernel = ",".join(k for k, v in extra.items() if v == "enabled") or ("default" if n in managed else "-")
    note = ""
    if n in phases and n not in managed:
        note = "  <- no Blaxel sandbox"
    elif n not in phases:
        note = "  <- orphan (not in gateway)"
    print(fmt.format(n, phases.get(n, "-"), bname, bstatus, kernel) + note)
EOF
