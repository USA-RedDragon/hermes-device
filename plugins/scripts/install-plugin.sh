#!/bin/sh
# Install one plugin directory into a Hermes home, the way the Talaria init step does.
#
#   install-plugin.sh <plugin dir> <destination>
#
# The destination (for example $HERMES_HOME/plugins/project-memory) is replaced only when its content
# changed, by two renames, because a gateway that is still running may load it while this runs. Directories
# get mode 755 and files 644. __pycache__, test files and README.md are left out.
set -eu

src=${1:?plugin dir} dest=${2:?destination}
[ -f "$src/plugin.yaml" ] || { echo "install-plugin: $src has no plugin.yaml" >&2; exit 1; }

new="$dest.new" old="$dest.old"
rm -rf "$new" "$old"
mkdir -p "$(dirname "$dest")"
mkdir -p "$new"
(cd "$src" && find . -type f ! -path '*/__pycache__/*' ! -name '*.pyc' ! -name 'test_*.py' ! -name README.md | sort) |
while IFS= read -r f; do
  mkdir -p "$new/$(dirname "$f")"
  cp "$src/$f" "$new/$f"
done
find "$new" -type d -exec chmod 755 {} +
find "$new" -type f -exec chmod 644 {} +

if diff -r "$new" "$dest" >/dev/null 2>&1; then
  rm -rf "$new"
  echo "install-plugin: $dest is current"
else
  [ ! -d "$dest" ] || mv "$dest" "$old"
  mv "$new" "$dest"
  rm -rf "$old"
  echo "install-plugin: installed $dest"
fi
