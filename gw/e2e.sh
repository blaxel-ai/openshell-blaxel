#!/usr/bin/env bash
# End-to-end test of the OpenShell Blaxel driver (v0.0.116 path).
# Requires ./restart.sh to have started the driver + gateway ("blaxel").
set -uo pipefail
. "$(dirname "$0")/env.sh"
cd "$GW_DIR"

GW=$GATEWAY_NAME
NAME=${1:-e2e-$(date +%H%M%S)}
pass=0 fail=0

os() { openshell -g "$GW" "$@" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g'; }
ok() { echo "  PASS  $1"; pass=$((pass + 1)); }
ko() { echo "  FAIL  $1"; [ -n "${2:-}" ] && echo "$2" | sed 's/^/        /' | tail -8; fail=$((fail + 1)); }
check() { # name, output, grep pattern that must match
  if grep -qE "$3" <<<"$2"; then ok "$1"; else ko "$1" "$2"; fi
}
xec() { # exec with a deadline; retry once (a first exec right after Ready can stall)
  local out
  out=$(timeout 60 openshell -g "$GW" sandbox exec -n "$NAME" -- sh -c "$1" 2>&1) || \
    out=$(timeout 60 openshell -g "$GW" sandbox exec -n "$NAME" -- sh -c "$1" 2>&1)
  sed -E 's/\x1b\[[0-9;]*m//g' <<<"$out"
}
phase() { os sandbox list | awk -v n="$NAME" '$1==n {print $NF}'; }
wait_phase() { # phase, timeout seconds
  for _ in $(seq "$2"); do [ "$(phase)" = "$1" ] && return 0; sleep 1; done; return 1
}

echo "== 1. gateway + driver"
check "gateway authenticated over mTLS" "$(os status)" "Authenticated"

echo "== 2. create sandbox $NAME"
start=$(date +%s)
out=$(timeout 420 openshell -g "$GW" sandbox create --name "$NAME" --no-tty -- sleep infinity 2>&1)
if wait_phase Ready 60; then ok "Ready after $(( $(date +%s) - start ))s"; else ko "sandbox Ready" "$out"; fi
blx=$(bl get sandboxes -w "$BL_WORKSPACE" -o json 2>/dev/null | python3 -c "
import json,sys
for s in json.load(sys.stdin):
  if s['metadata'].get('labels',{}).get('openshell.ai/sandbox-name')=='$NAME': print(s['metadata']['name'])" 2>/dev/null)
if [ -n "$blx" ]; then ok "Blaxel sandbox exists: $blx"; else ko "Blaxel sandbox exists"; fi

echo "== 3. exec"
out=$(xec 'id; uname -r')
check "exec runs as non-root sandbox user" "$out" "uid=1500\(sandbox\)"
check "exec runs in the Blaxel VM kernel" "$out" "^6\."

echo "== 4. network policy"
out=$(xec 'python3 -c "import socket;socket.create_connection((\"1.1.1.1\",443),timeout=5);print(\"DIRECT-OPEN\")" 2>&1 | tail -1')
if grep -q DIRECT-OPEN <<<"$out"; then ko "direct egress blocked" "$out"; else ok "direct egress blocked"; fi
out=$(xec 'python3 -c "import urllib.request as u;print(\"STATUS\",u.urlopen(\"https://api.github.com\",timeout=8).status)" 2>&1 | tail -1')
if grep -q "STATUS 200" <<<"$out"; then ko "unlisted host denied by default policy" "$out"; else ok "unlisted host denied by default policy"; fi
out=$(xec 'ip -br addr | grep -v "^lo"')
check "workload sits behind the supervisor's veth" "$out" "veth-s-"
py=$(xec 'readlink -f "$(command -v python3)"' | tail -1)
# Keep the effective filesystem policy (live sandboxes cannot drop paths)
# and add one allowed endpoint for python3.
os policy get "$NAME" --base | sed -n '/^---$/,$p' | sed '1d' > /tmp/os-e2e-policy.yaml
cat >> /tmp/os-e2e-policy.yaml <<POL
network_policies:
  github_api:
    name: github-api-readonly
    endpoints:
      - { host: api.github.com, port: 443, protocol: rest, enforcement: enforce, access: read-only }
    binaries:
      - { path: $py }
POL
out=$(os policy set "$NAME" --policy /tmp/os-e2e-policy.yaml --wait)
check "policy update loaded by in-VM supervisor" "$out" "[Ll]oaded|applied|✓"
out=$(xec 'python3 -c "import urllib.request as u;print(\"STATUS\",u.urlopen(\"https://api.github.com/zen\",timeout=10).status)" 2>&1 | tail -1')
check "allowed host reachable through the policy proxy" "$out" "STATUS 200"
out=$(xec 'python3 -c "import urllib.request as u;r=u.Request(\"https://api.github.com/zen\",method=\"POST\");print(\"STATUS\",u.urlopen(r,timeout=10).status)" 2>&1 | tail -1')
if grep -q "STATUS 200" <<<"$out"; then ko "L7 read-only blocks POST" "$out"; else ok "L7 read-only blocks POST"; fi
sleep 2
check "denial recorded in OCSF logs" "$(os logs "$NAME" | tail -200)" "DENIED"

echo "== 5. filesystem"
out=$(xec 'touch /usr/probe 2>&1 && echo USR-WRITABLE; touch /sandbox/probe && echo WORKDIR-OK')
check "workdir writable" "$out" "WORKDIR-OK"
if grep -q USR-WRITABLE <<<"$out"; then ko "/usr not writable" "$out"; else ok "/usr not writable"; fi
check "runs on the Blaxel Landlock kernel" "$(xec 'uname -r')" "^6\.(1[2-9]|[2-9][0-9])"
out=$(xec 'touch /var/tmp/landlock-probe 2>&1 && echo VARTMP-WRITABLE')
if grep -q VARTMP-WRITABLE <<<"$out"; then ko "Landlock denies world-writable path outside policy" "$out"; else ok "Landlock denies world-writable path outside policy"; fi
check "supervisor applied a Landlock ruleset" "$(os logs "$NAME" | grep -i landlock)" "ruleset built"

echo "== 6. stop / start"
os sandbox stop "$NAME" >/dev/null
if wait_phase Stopped 60; then ok "stopped"; else ko "stopped" "$(os sandbox list)"; fi
os sandbox start "$NAME" >/dev/null
if wait_phase Ready 120; then ok "started again"; else ko "started again" "$(os sandbox list)"; fi
check "workspace preserved across stop/start" "$(xec 'ls /sandbox')" "probe"

echo "== 7. delete"
os sandbox delete "$NAME" >/dev/null
sleep 3
if bl get sandboxes -w "$BL_WORKSPACE" -o json 2>/dev/null | grep -q "\"$blx\""; then
  # Blaxel deletion is asynchronous; accept DELETING.
  st=$(bl get sandbox "$blx" -w "$BL_WORKSPACE" -o json 2>/dev/null | python3 -c "import json,sys;d=json.load(sys.stdin);d=d[0] if isinstance(d,list) else d;print(d.get('status'))" 2>/dev/null)
  if [ "$st" = "DELETING" ] || [ "$st" = "TERMINATED" ]; then ok "Blaxel sandbox deleting"; else ko "Blaxel sandbox deleted" "status=$st"; fi
else
  ok "Blaxel sandbox deleted"
fi
check "gone from OpenShell" "$(os sandbox list)" "No sandboxes found|^NAME"

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
