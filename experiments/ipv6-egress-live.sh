#!/usr/bin/env bash
# Live test of NVIDIA/OpenShell feat/policy-dns-ipv6-egress on Blaxel.
#   ipv6-egress-live.sh ipv6only   control VM with no IPv4 route and DNS64 first
#   ipv6-egress-live.sh dualstack  control VM with the CLAT (IPv4 default route)
# Uses the patched supervisor at $SUP (linux gnu build of the branch).
set -uo pipefail
cd "$(dirname "$0")/.."
MODE=$1
SUP=${SUP:?path to patched openshell-supervisor}
export XDG_CONFIG_HOME=$HOME/.openshell-blaxel/cli-config
OS="bin/main/openshell -g blaxel-main"
NAME=v6-$MODE-$(date +%H%M%S)
set -a; . ./.env; set +a
run() { (cd experiments && node run.mjs os-control "$1"); }

echo "== control network: $MODE"
node experiments/upload.mjs os-control "$SUP" /opt/openshell/bin/openshell-supervisor >/dev/null
if [ "$MODE" = ipv6only ]; then
  run 'ip route del default dev clat 2>/dev/null; grep -v "^nameserver 127.0.0.2$" /etc/resolv.conf > /tmp/r && cat /tmp/r > /etc/resolv.conf'
else
  run 'ip route replace default dev clat mtu 1480; grep -q "^nameserver 127.0.0.2$" /etc/resolv.conf || { printf "nameserver 127.0.0.2\n"; cat /etc/resolv.conf; } > /tmp/r && cat /tmp/r > /etc/resolv.conf'
fi
run 'chmod 755 /opt/openshell/bin/openshell-supervisor; echo "IPv4 default: $(ip -4 route show default | head -1)"; echo "IPv6 default: $(ip -6 route show default | head -1)"; echo "resolver: $(grep -m1 ^nameserver /etc/resolv.conf)"' | grep -v '^\[exit'

echo "== sandbox $NAME"
( timeout 600 $OS sandbox create --name "$NAME" --no-tty -- sleep infinity >/dev/null 2>&1 & )
for _ in $(seq 150); do [ "$($OS sandbox list 2>/dev/null | sed -E 's/\x1b\[[0-9;]*m//g' | awk -v n="$NAME" '$1==n{print $NF}')" = Ready ] && break; sleep 2; done
$OS sandbox list 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | awk -v n="$NAME" 'NR==1||$1==n'
{ $OS policy get "$NAME" --base 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | sed -n '/^---$/,$p' | sed 1d; echo; cat <<'POL'
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
} > /tmp/v6-policy.yaml
$OS policy set "$NAME" --policy /tmp/v6-policy.yaml --wait 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | tail -1
echo "== inside the sandbox"
timeout 120 $OS sandbox exec -n "$NAME" --no-tty -- sh -c '
  echo "resolve: $(getent ahosts api.github.com | awk "{print \$1}" | sort -u | tr "\n" " ")"
  echo "GET  allowed : $(curl -sS -m 20 -o /dev/null -w "%{http_code} via %{remote_ip}" https://api.github.com/zen 2>&1)"
  echo "POST (L7 deny): $(curl -sS -m 10 -o /dev/null -w "%{http_code}" -X POST https://api.github.com/zen 2>&1 | tail -c 60)"
  echo "unlisted host : $(curl -sS -m 10 -o /dev/null -w "%{http_code}" https://example.com 2>&1 | tail -c 60)"' 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g'
echo "== supervisor evidence"
$OS logs "$NAME" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | grep -E 'Policy DNS connected|ipv6_egress|upstream_no_data|api.github.com' | tail -6 | cut -c1-230
$OS sandbox delete "$NAME" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | tail -1
