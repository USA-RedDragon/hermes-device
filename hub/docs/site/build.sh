#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."
out=${1:-artifacts/docs-site}
rm -rf "$out"
mkdir -p "$out"
pandoc lua docs/site/pages.lua >"$out/.pages"
while IFS="$(printf '\t')" read -r src page; do
	mkdir -p "$out/$(dirname "$page")"
	pandoc -f gfm -t html5 --wrap=none --standalone --template docs/site/template.html --lua-filter docs/site/site.lua -M outdir="$out" -M page="$page" -o "$out/$page" "$src"
done <"$out/.pages"
rm "$out/.pages"
pandoc lua docs/site/index.lua "$out"
echo "$out/index.html"
