#!/bin/sh
# Exact cleanup. Prints the plan; changes nothing without --apply.
#
#   gw/cleanup.sh <sandbox>... [--apply]   delete OpenShell sandboxes (and their Blaxel VMs)
#   gw/cleanup.sh --orphans [--apply]      delete Blaxel sandboxes the gateway no longer knows
. "$(dirname "$0")/env.sh"

apply=false orphans=false names=""
for a in "$@"; do
  case "$a" in
    --apply) apply=true ;;
    --orphans) orphans=true ;;
    -*) echo "unknown flag $a" >&2; exit 2 ;;
    *) names="$names $a" ;;
  esac
done
[ -z "$names" ] && [ "$orphans" = false ] && { sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

plan=""
for n in $names; do
  plan="$plan
XDG_CONFIG_HOME=$OS_CONFIG_HOME $OS_BIN -g $GATEWAY_NAME sandbox delete $n"
done
if [ "$orphans" = true ]; then
  known=$(oscli sandbox list | awk 'NR>1 {print $1}')
  for b in $(bl get sandboxes -w "$BL_WORKSPACE" -o json 2>/dev/null | KNOWN="$known" python3 -c '
import json, os, sys
known = set(os.environ["KNOWN"].split())
for s in json.load(sys.stdin):
    l = s.get("metadata", {}).get("labels") or {}
    # Only sandboxes owned by this instance: another gateway sharing the workspace
    # would otherwise look like orphans.
    if (l.get("openshell.ai/managed-by") == "openshell-driver-blaxel" and l.get("openshell.ai/driver-owner", "blaxel") == os.environ["GATEWAY_NAME"]
            and l.get("openshell.ai/sandbox-name") not in known):
        print(s["metadata"]["name"])'); do
    plan="$plan
bl delete sandbox $b -w $BL_WORKSPACE"
  done
fi

plan=$(printf '%s\n' "$plan" | sed '/^$/d')
[ -z "$plan" ] && { echo "nothing to clean up"; exit 0; }
echo "plan:"
printf '%s\n' "$plan" | sed 's/^/  /'
if [ "$apply" != true ]; then
  echo "(dry run; re-run with --apply)"
  exit 0
fi
printf '%s\n' "$plan" | while read -r cmd; do
  echo "+ $cmd"
  sh -c "$cmd" 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | tail -1
done
