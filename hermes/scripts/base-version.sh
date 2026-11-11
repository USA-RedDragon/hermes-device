#!/usr/bin/env bash
# Print the upstream Hermes release the Dockerfile builds on, such as v0.23.1,
# from its pinned HERMES_BASE.
set -euo pipefail

file=${1:-Dockerfile}
versions=$(sed -n 's|^ARG HERMES_BASE=.*nousresearch/hermes-agent:\(v[0-9][0-9.]*\)@sha256:[0-9a-f]\{64\}$|\1|p' "$file" | sort -u)
count=$(printf '%s\n' "$versions" | grep -c . || true)
if [ "$count" -ne 1 ]; then
  echo "base-version: want one pinned nousresearch/hermes-agent release in ARG HERMES_BASE of $file, found: ${versions:-none}" >&2
  exit 1
fi
echo "$versions"
