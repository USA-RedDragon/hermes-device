#!/bin/sh
# The chart and the all-in-one image carry copies of the setup scripts in compose/files; they must match. Every
# kit pins the same images, and the chart's appVersion is the hub release it pins.
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

# pin FILE IMAGE: the first reference to IMAGE (repository:tag@digest) in FILE
pin() {
  grep -oE "$2:[^\"}[:space:]]+" "$1" | head -n 1 || true
}
# same IMAGE FILE...: every FILE pins IMAGE, and all the same
same() {
  img=$1
  shift
  want=$(pin charts/talaria/values.yaml "$img")
  [ -n "$want" ] || { echo "charts/talaria/values.yaml pins no $img"; rc=1; return; }
  for f in "$@"; do
    got=$(pin "$f" "$img")
    [ "$got" = "$want" ] || { echo "$f pins ${got:-no $img}, charts/talaria/values.yaml pins $want"; rc=1; }
  done
}
same ghcr.io/usa-reddragon/talaria-hub compose/compose.yaml aio/Dockerfile
same ghcr.io/usa-reddragon/talaria-hermes compose/compose.yaml aio/Dockerfile
same ghcr.io/usa-reddragon/talaria compose/compose.yaml aio/Dockerfile
same ghcr.io/usa-reddragon/talaria-sandbox compose/compose.yaml
same docker.io/searxng/searxng compose/compose.yaml aio/Dockerfile
# .env.example shows the Hermes tag without its digest
hermes=$(pin charts/talaria/values.yaml ghcr.io/usa-reddragon/talaria-hermes)
grep -qxF "# TALARIA_HERMES_IMAGE=${hermes%%@*}" compose/.env.example || { echo "compose/.env.example does not show ${hermes%%@*}"; rc=1; }

# appVersion is the pinned hub release without its v
hub=$(pin charts/talaria/values.yaml ghcr.io/usa-reddragon/talaria-hub)
tag=${hub%%@*}
tag=${tag##*:}
app=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/talaria/Chart.yaml)
[ "$app" = "${tag#v}" ] || { echo "charts/talaria/Chart.yaml appVersion is $app, images.hub pins $tag (run hack/pin.sh hub $tag)"; rc=1; }
exit $rc
