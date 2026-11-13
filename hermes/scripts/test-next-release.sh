#!/usr/bin/env bash
# Test next-release.sh against a throwaway repository.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd -P)
work=$(mktemp -d)
trap 'find "$work" -delete' EXIT

mkdir -p "$work/repo/hermes/scripts" "$work/repo/hermes/patches" "$work/repo/docs"
cp "$here/next-release.sh" "$here/base-version.sh" "$work/repo/hermes/scripts/"
cd "$work/repo"
git init -q
git config user.name test
git config user.email test@example.com
git config commit.gpgsign false
git config tag.gpgsign false
digest=$(printf '0%.0s' $(seq 64))
echo "ARG HERMES_BASE=docker.io/nousresearch/hermes-agent:v0.23.1@sha256:$digest" > hermes/Dockerfile

commit() {
  git add -A
  git commit -q -m "$1"
  git rev-parse HEAD
}

plan() {
  (cd hermes && scripts/next-release.sh "$@")
}

want() {
  local out=$1 key=$2 value=$3 got
  got=$(printf '%s\n' "$out" | sed -n "s/^$key=//p")
  if [ "$got" != "$value" ]; then
    echo "FAIL: $key is '$got', want '$value'" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
}

first=$(commit "base")
out=$(plan "$first")
want "$out" tag v0.23.1-p1
want "$out" release true
want "$out" last none

git tag hermes/v0.23.1-p9 "$first"
git tag hermes/v0.23.1-p10 "$first"
git tag hermes/v0.23.1-p10x "$first"
git tag hermes/v0.22.0-p99 "$first"

# Unrelated files after the tag: nothing to release.
echo x > docs/a.md
unrelated=$(commit "docs")
out=$(plan "$unrelated")
want "$out" release false
want "$out" last hermes/v0.23.1-p10
want "$out" tag v0.23.1-p11

# Force releases it anyway.
want "$(plan "$unrelated" true)" release true

# A patch, then an unrelated commit on top: the tip releases, with the next number.
echo p > hermes/patches/0001-a.patch
patched=$(commit "patch")
echo y > docs/b.md
tip=$(commit "docs again")
out=$(plan "$tip")
want "$out" release true
want "$out" tag v0.23.1-p11
want "$out" sha "$tip"

# The commit that carried the patch gives the same decision as the tip.
want "$(plan "$patched")" release true

# After the tag lands on the tip, later unrelated commits release nothing and
# the next real change takes the next number.
git tag hermes/v0.23.1-p11 "$tip"
echo z > docs/c.md
later=$(commit "docs third")
want "$(plan "$later")" release false
echo q > hermes/scripts/extra.sh
want "$(plan "$(commit "script")")" tag v0.23.1-p12

# A new upstream base starts again at p1.
echo "ARG HERMES_BASE=docker.io/nousresearch/hermes-agent:v0.24.0@sha256:$digest" > hermes/Dockerfile
out=$(plan "$(commit "bump")")
want "$out" upstream v0.24.0
want "$out" tag v0.24.0-p1
want "$out" release true

echo "next-release: ok"
