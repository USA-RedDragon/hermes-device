#!/bin/sh
# pin.sh IMAGE TAG: pins one image at TAG and its digest in every kit (compose, the chart, the all-in-one image).
# IMAGE is hub, hermes, device, sandbox or searxng. Pinning the hub also sets the chart's appVersion.
#   hack/pin.sh hub v0.26.71
# Needs crane or docker buildx to read the digest. hack/check-sync.sh checks the result.
set -eu
[ $# -eq 2 ] || { echo "usage: $0 hub|hermes|device|sandbox|searxng TAG" >&2; exit 2; }
cd "$(dirname "$0")/.."
case $1 in
  hub) repo=ghcr.io/usa-reddragon/talaria-hub ;;
  hermes) repo=ghcr.io/usa-reddragon/talaria-hermes ;;
  device) repo=ghcr.io/usa-reddragon/talaria ;;
  sandbox) repo=ghcr.io/usa-reddragon/talaria-sandbox ;;
  searxng) repo=docker.io/searxng/searxng ;;
  *) echo "unknown image $1" >&2; exit 2 ;;
esac
tag=$2
if command -v crane > /dev/null; then
  digest=$(crane digest "$repo:$tag")
else
  digest=$(docker buildx imagetools inspect --format '{{json .Manifest.Digest}}' "$repo:$tag" | tr -d '"')
fi
case $digest in sha256:*) ;; *) echo "no digest for $repo:$tag" >&2; exit 1 ;; esac
ref="$repo:$tag@$digest"
# The old pin: repository, then a tag and digest up to a quote, brace or space
old="$repo:[^\"}[:space:]@]+@sha256:[0-9a-f]+"
for f in compose/compose.yaml charts/talaria/values.yaml aio/Dockerfile; do
  sed -i -E "s#$old#$ref#g" "$f"
done
if [ "$1" = hermes ]; then
  sed -i -E "s#^(\# TALARIA_HERMES_IMAGE=)$repo:.*#\1$repo:$tag#" compose/.env.example
fi
if [ "$1" = hub ]; then
  sed -i -E "s#^appVersion: .*#appVersion: \"${tag#v}\"#" charts/talaria/Chart.yaml
fi
echo "$ref"
hack/check-sync.sh
