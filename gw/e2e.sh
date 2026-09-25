#!/usr/bin/env bash
# End-to-end test of the Blaxel driver on OpenShell main (RFC 0012), with the
# gateway, driver and supervisors in a Blaxel control sandbox (os-deploy up)
# and the laptop connected through `os-tunnel dial`.
set -uo pipefail
. "$(dirname "$0")/env.sh"
cd "$ROOT"

GW=$GATEWAY_NAME
NAME=${1:-e2e-$(date +%H%M%S)}
pass=0 fail=0

os() { "$OS_BIN" -g "$GW" "$@" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g'; }
ok() { echo "  PASS  $1"; pass=$((pass + 1)); }
ko() { echo "  FAIL  $1"; [ -n "${2:-}" ] && echo "$2" | sed 's/^/        /' | tail -8; fail=$((fail + 1)); }
check() { if grep -qE "$3" <<<"$2"; then ok "$1"; else ko "$1" "$2"; fi; }
xec() {
  local out
  out=$(timeout 90 "$OS_BIN" -g "$GW" sandbox exec -n "$NAME" --no-tty -- sh -c "$1" 2>&1) ||
    out=$(timeout 90 "$OS_BIN" -g "$GW" sandbox exec -n "$NAME" --no-tty -- sh -c "$1" 2>&1)
  sed -E 's/\x1b\[[0-9;]*m//g' <<<"$out"
}
phase() { os sandbox list | awk -v n="$NAME" '$1==n {print $NF}'; }
wait_phase() { for _ in $(seq "$2"); do [ "$(phase)" = "$1" ] && return 0; sleep 1; done; return 1; }

echo "== 1. control plane on Blaxel"
out=$(os status)
check "gateway authenticated over mTLS" "$out" "Authenticated"
check "gateway runs OpenShell main" "$out" "Version: 0\.0\.1[1-9][7-9]|Version: 0\.[1-9]"
check "blaxel compute driver negotiated" "$(os gateway info)" "blaxel"

echo "== 2. create sandbox $NAME"
start=$(date +%s)
# --no-tty streams the main process until it exits; create in the background.
( timeout 600 "$OS_BIN" -g "$GW" sandbox create --name "$NAME" --no-tty -- sleep infinity >/dev/null 2>&1 & )
if wait_phase Ready 300; then ok "Ready after $(( $(date +%s) - start ))s (supervisor session attached)"; else ko "sandbox Ready" "$(os sandbox get "$NAME")"; fi

echo "== 3. isolation"
out=$(xec 'id; uname -r; tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d " "')
check "runs as uid 1500 sandbox" "$out" "uid=1500\(sandbox\)"
check "runs on the landlock kernel" "$out" "^6\.(1[2-9]|[2-9][0-9])"
if [ "$(grep -cvE '^(uid=|6\.|lo$)' <<<"$out")" = 0 ] && grep -qx lo <<<"$out"; then
  ok "network namespace has only loopback (outer fence)"
else
  ko "network namespace has only loopback (outer fence)" "$out"
fi
out=$(xec 'python3 -c "import socket;socket.create_connection((\"1.1.1.1\",443),timeout=5);print(\"DIRECT-OPEN\")" 2>&1 | tail -1')
if grep -q DIRECT-OPEN <<<"$out"; then ko "raw egress blocked" "$out"; else ok "raw egress blocked"; fi
out=$(xec 'touch /var/tmp/landlock-probe 2>&1 && echo VARTMP-WRITABLE; touch /sandbox/probe && echo WORKDIR-OK')
check "workdir writable" "$out" "WORKDIR-OK"
if grep -q VARTMP-WRITABLE <<<"$out"; then ko "Landlock denies world-writable path outside policy" "$out"; else ok "Landlock denies world-writable path outside policy"; fi
out=$(xec 'ls /.openshell/state 2>&1; env | grep -cE "BL_API_KEY|OPENSHELL_TLS_KEY|_TOKEN="')
check "driver state and credentials hidden from the workload" "$out" "Permission denied"

echo "== 4. network policy (enforced by the supervisor in the control sandbox)"
out=$(xec 'curl -sS -m 10 -o /dev/null -w "%{http_code}" https://api.github.com/zen 2>&1 | tail -1')
if grep -q '^200' <<<"$out"; then ko "unlisted host denied by default policy" "$out"; else ok "unlisted host denied by default policy"; fi
# `policy get --base` output has no trailing newline on main.
{ os policy get "$NAME" --base | sed -n '/^---$/,$p' | sed 1d; echo; } > /tmp/os-e2e-main-policy.yaml
cat >> /tmp/os-e2e-main-policy.yaml <<'POL'
network_policies:
  github_api:
    name: github-api-readonly
    endpoints:
      - host: api.github.com
        port: 443
        protocol: rest
        enforcement: enforce
        access: read-only
    binaries:
      - path: /usr/bin/curl
POL
check "policy update loaded" "$(os policy set "$NAME" --policy /tmp/os-e2e-main-policy.yaml --wait)" "[Ll]oaded|✓"
check "allowed host reachable through the supervisor proxy" "$(xec 'curl -sS -m 15 -o /dev/null -w "%{http_code}" https://api.github.com/zen')" "^200"
out=$(xec 'curl -sS -m 15 -o /dev/null -w "%{http_code}" -X POST https://api.github.com/zen')
if grep -q '^200' <<<"$out"; then ko "L7 read-only blocks POST" "$out"; else ok "L7 read-only blocks POST"; fi
sleep 3
check "denials in the audit log" "$(os logs "$NAME" | tail -300)" "DENIED"

echo "== 5. stop / start (new generation, fresh launch credentials)"
os sandbox stop "$NAME" >/dev/null
if wait_phase Stopped 90; then ok "stopped"; else ko "stopped" "$(os sandbox list)"; fi
os sandbox start "$NAME" >/dev/null
if wait_phase Ready 240; then ok "started again"; else ko "started again" "$(os sandbox get "$NAME")"; fi
check "workspace preserved across generations" "$(xec 'ls /sandbox')" "probe"

echo "== 6. delete"
os sandbox delete "$NAME" >/dev/null
sleep 3
check "gone from OpenShell" "$(os sandbox list)" "No sandboxes found|^NAME"

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
