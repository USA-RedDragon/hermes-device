#!/bin/sh
# Lists the settings of the Unraid template: container target, default, name.
set -eu
xml="$(dirname "$0")/../deploy/aio/unraid/talaria.xml"
[ -f "$xml" ] || { echo "missing $xml" >&2; exit 1; }
printf '%-24s %-34s %s\n' "Setting" "Default" "Name in Unraid"
awk 'match($0, /<Config Name="[^"]*" Target="[^"]*" Default="[^"]*"/) {
	n = split(substr($0, RSTART, RLENGTH), a, "\"")
	printf "%-24s %-34s %s\n", a[4], (a[6] == "" ? "(empty)" : a[6]), a[2]
}' "$xml"
