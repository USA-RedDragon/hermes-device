#!/bin/sh
# install-pandoc.sh DIR downloads the pinned pandoc release for linux-amd64,
# checks its SHA-256, extracts it under DIR, and prints the directory of the
# pandoc binary. GitHub Actions and the Cloudflare build both use it.
set -eu

version=3.13
sha256=38aa33b325a2bc96a4c39409631212db55702f564250bcdbead180b3bf646389

dir=${1:?usage: install-pandoc.sh DIR}
mkdir -p "$dir"
curl -fsSL -o "$dir/pandoc.tar.gz" "https://github.com/jgm/pandoc/releases/download/$version/pandoc-$version-linux-amd64.tar.gz"
echo "$sha256  $dir/pandoc.tar.gz" | sha256sum -c - >&2
tar -xzf "$dir/pandoc.tar.gz" -C "$dir"
rm "$dir/pandoc.tar.gz"
echo "$dir/pandoc-$version/bin"
