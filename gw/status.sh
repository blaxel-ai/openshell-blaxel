#!/bin/sh
# Read-only status: control-plane processes, gateway health, and OpenShell
# sandboxes joined with their Blaxel workload sandboxes (orphans flagged).
. "$(dirname "$0")/env.sh"

"$ROOT/driver/bin/os-deploy" status -name "$CONTROL_SANDBOX" -workspace "$BL_WORKSPACE" -env "$BL_ENV" 2>&1
echo "== gateway '$GATEWAY_NAME' via 127.0.0.1:$LOCAL_PORT"
printf '  laptop tunnel: '
pgrep -f "os-tunnel dial -sandbox $CONTROL_SANDBOX -listen 127.0.0.1:$LOCAL_PORT" >/dev/null && echo running || echo "NOT RUNNING (make connect)"
oscli status | grep -E 'Status:|Authentication:|Version:|Error' | sed 's/^ */  /'

echo "== sandboxes (workspace $BL_WORKSPACE, $BL_ENV)"
OS_LIST=$(oscli sandbox list)
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
owner = os.environ["GATEWAY_NAME"]
managed = {}
for s in blx:
    l = s.get("metadata", {}).get("labels") or {}
    if l.get("openshell.ai/managed-by") == "openshell-driver-blaxel" and l.get("openshell.ai/driver-owner", "blaxel") == owner:
        managed[l.get("openshell.ai/sandbox-name", "?")] = (
            s["metadata"]["name"], s.get("status", "?"),
            ",".join(k for k, v in (s.get("spec", {}).get("runtime", {}).get("extraArgs") or {}).items() if v == "enabled") or "default")
rows = sorted(set(phases) | set(managed))
if not rows:
    print("  none")
    raise SystemExit
fmt = "  {:<22} {:<13} {:<36} {:<11} {}"
print(fmt.format("OPENSHELL", "PHASE", "BLAXEL SANDBOX", "STATUS", "KERNEL"))
for n in rows:
    b, st, k = managed.get(n, ("-", "-", "-"))
    note = "  <- no Blaxel sandbox" if n in phases and n not in managed else ("  <- orphan (not in gateway)" if n not in phases else "")
    print(fmt.format(n, phases.get(n, "-"), b, st, k) + note)
EOF
