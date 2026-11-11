#!/usr/bin/env bash
# Apply patches/*.patch, in name order, to a Hermes tree.
#
#   apply-patches.sh <tree> <patch dir>
#     Applies every patch to a full source checkout (CI). Fails on the first
#     patch that does not apply.
#   apply-patches.sh --image <tree> <patch dir> <out dir>
#     Applies to the source in the published image, which has no tests/ or
#     website/. Hunks for files that are not in the image and whose directory
#     is not either are skipped. Every file a patch changes or adds is copied
#     to <out dir> under the same relative path.
set -euo pipefail

image=false
if [ "${1:-}" = --image ]; then
  image=true
  shift
fi
tree=$(cd "$1" && pwd -P)
dir=$(cd "$2" && pwd -P)
out=${3:-}
if $image && [ -z "$out" ]; then
  echo "apply-patches: --image needs an out dir" >&2
  exit 2
fi

shopt -s nullglob
patches=("$dir"/*.patch)
if [ "${#patches[@]}" -eq 0 ]; then
  echo "apply-patches: no patches in $dir" >&2
  exit 1
fi

version=${HERMES_VERSION:-the base release}
changed=()
cd "$tree"
for p in "${patches[@]}"; do
  name=$(basename "$p")
  paths=()
  while IFS=$'\t' read -r _ _ path; do
    paths+=("$path")
  done < <(git apply --numstat "$p")
  args=()
  if $image; then
    for f in "${paths[@]}"; do
      if [ -e "$f" ] || [ -d "$(dirname "$f")" ]; then
        args+=("--include=$f")
        changed+=("$f")
      fi
    done
    if [ "${#args[@]}" -eq 0 ]; then
      echo "skip $name: it changes no file in the image"
      continue
    fi
  fi
  if ! git apply --check "${args[@]}" "$p" 2>"${TMPDIR:-/tmp}/apply.err"; then
    cat "${TMPDIR:-/tmp}/apply.err" >&2
    echo "::error file=patches/$name::$name does not apply to Hermes $version. Rebase it onto that release (see README.md, Rebasing)." >&2
    exit 1
  fi
  git apply "${args[@]}" "$p"
  echo "applied $name (${#paths[@]} files)"
done

if $image; then
  mkdir -p "$out"
  for f in $(printf '%s\n' "${changed[@]}" | sort -u); do
    if [ -e "$f" ]; then
      mkdir -p "$out/$(dirname "$f")"
      cp -p "$f" "$out/$f"
    fi
  done
  echo "files for the image:"
  (cd "$out" && find . -type f | sort)
fi
