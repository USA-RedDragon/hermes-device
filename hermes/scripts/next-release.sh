#!/usr/bin/env bash
# Decide the next Hermes image release of a commit and print it as key=value
# lines: upstream, last, tag, release (true or false) and sha.
#
#   next-release.sh <commit> [force]
#
# Run from the hermes directory of a clone with all tags. The release is false
# when Dockerfile, patches and scripts at <commit> match the newest
# hermes/<upstream>-p<n> tag, unless force is "true". The tag is the next number
# after the newest one. Callers pass the tip of main, so a run that started on an
# older commit still releases the newest image inputs, and a later run for a
# commit that did not touch them finds nothing changed.
set -euo pipefail

commit=$(git rev-parse --verify "$1^{commit}")
force=${2:-false}

upstream=$(scripts/base-version.sh <(git show "$commit:./Dockerfile"))
last=$(git tag -l "hermes/$upstream-p*" | grep -E "^hermes/$upstream-p[0-9]+$" | sort -V | tail -n1 || true)
release=true
if [ -n "$last" ] && [ "$force" != true ] && git diff --quiet "$last" "$commit" -- Dockerfile patches scripts; then
  release=false
fi
n=$(( ${last##*-p} + 0 ))
{
  echo "upstream=$upstream"
  echo "last=${last:-none}"
  echo "tag=$upstream-p$(( n + 1 ))"
  echo "release=$release"
  echo "sha=$commit"
}
