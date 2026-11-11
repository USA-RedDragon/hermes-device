#!/usr/bin/env bash
# Run each patch's tests in a patched Hermes source tree.
#
#   run-patch-tests.sh <tree> <patch dir>
#
# A patch's tests are the test files it adds or changes, plus the files listed
# in <patch dir>/<patch name without .patch>.tests (one path per line, # for
# comments): existing upstream tests of the code the patch changes. PYTHON is
# the interpreter (default: the tree's .venv).
set -euo pipefail

tree=$(cd "$1" && pwd -P)
dir=$(cd "$2" && pwd -P)
python=${PYTHON:-$tree/.venv/bin/python}

export TZ=UTC LANG=C.UTF-8 PYTHONHASHSEED=0 PYTHONPATH=$tree
export OPENROUTER_API_KEY= OPENAI_API_KEY= NOUS_API_KEY= ANTHROPIC_API_KEY=

shopt -s nullglob
failed=()
cd "$tree"
for p in "$dir"/*.patch; do
  name=$(basename "$p" .patch)
  tests=()
  while IFS=$'\t' read -r _ _ path; do
    case "$path" in
      tests/*/test_*.py | tests/test_*.py) tests+=("$path") ;;
    esac
  done < <(git apply --numstat "$p")
  if [ -f "$dir/$name.tests" ]; then
    while read -r line; do
      line=${line%%#*}
      line=$(echo "$line" | xargs)
      [ -n "$line" ] && tests+=("$line")
    done <"$dir/$name.tests"
  fi
  if [ "${#tests[@]}" -eq 0 ]; then
    echo "::error file=patches/$name.patch::$name has no tests"
    failed+=("$name")
    continue
  fi
  echo "::group::$name: ${tests[*]}"
  if "$python" -m pytest -q -p no:cacheprovider "${tests[@]}"; then
    echo "::endgroup::"
  else
    echo "::endgroup::"
    echo "::error file=patches/$name.patch::the tests of $name failed"
    failed+=("$name")
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo "failed: ${failed[*]}" >&2
  exit 1
fi
