#!/bin/sh
# Fail when the device plugin's version is not the release tag's version.
#
#   check-plugin-version.sh vX.Y.Z
#
# The plugin reports PLUGIN_VERSION (plugin/device/core.py) to the daemon, and
# plugin/device/plugin.yaml carries the same version. Both are bumped by hand in
# a "plugin: report version X.Y.Z" commit before the tag, so a release made
# without that commit ships a plugin that reports the previous version (v0.25.3
# reported 0.25.2). The Release workflow runs this before it builds anything.
# A pre-release tag (a "-" or "+" in it) is not checked.
set -eu

tag=${1:?usage: check-plugin-version.sh vX.Y.Z}
root=$(cd "$(dirname "$0")/.." && pwd -P)
want=${tag#v}
case "$want" in
  *-* | *+*) echo "check-plugin-version: $tag is a pre-release, not checked"; exit 0 ;;
esac

core=$(sed -n 's/^PLUGIN_VERSION = "\(.*\)"$/\1/p' "$root/plugin/device/core.py")
manifest=$(sed -n 's/^version: *//p' "$root/plugin/device/plugin.yaml")
bad=0
if [ "$core" != "$want" ]; then
  echo "check-plugin-version: PLUGIN_VERSION in plugin/device/core.py is ${core:-missing}, the tag $tag wants $want" >&2
  bad=1
fi
if [ "$manifest" != "$want" ]; then
  echo "check-plugin-version: version in plugin/device/plugin.yaml is ${manifest:-missing}, the tag $tag wants $want" >&2
  bad=1
fi
if [ "$bad" -ne 0 ]; then
  echo "check-plugin-version: commit \"plugin: report version $want\" before tagging $tag" >&2
  exit 1
fi
echo "check-plugin-version: plugin reports $want"
