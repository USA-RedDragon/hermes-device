#!/bin/sh
# The chart and the all-in-one image carry copies of the setup scripts in compose/files; they must match.
set -eu
cd "$(dirname "$0")/.."
rc=0
for f in hermes-init.py demo-llm.py; do
  cmp -s "compose/files/$f" "charts/talaria/files/$f" || { echo "charts/talaria/files/$f differs from compose/files/$f"; rc=1; }
done
# The SearXNG settings: compose/files carries a copy, as compose users download only that folder
cmp -s searxng/settings.yml compose/files/searxng/settings.yml || { echo "compose/files/searxng/settings.yml differs from searxng/settings.yml"; rc=1; }
# The chart carries one too, as files outside the chart directory are not part of a Helm chart
cmp -s searxng/settings.yml charts/talaria/files/searxng-settings.yml || { echo "charts/talaria/files/searxng-settings.yml differs from searxng/settings.yml"; rc=1; }
exit $rc
