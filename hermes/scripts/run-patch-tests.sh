#!/usr/bin/env bash
# Run the tests of every patch in a patched Hermes source tree.
#
#   run-patch-tests.sh <tree> <patch dir> [shard/shards]
#
# A patch's tests are the test files it adds or changes, plus the files listed
# in <patch dir>/<patch name without .patch>.tests (one path per line, # for
# comments): existing upstream tests of the code the patch changes. PYTHON is
# the interpreter (default: the tree's .venv).
#
# Many patches list the same upstream files, so the union of all files runs
# once, each file in its own pytest process, several at a time. A failed file is
# mapped back to every patch that lists it, so each of them gets an ::error
# annotation. With shard/shards (such as 2/4) only the files in that
# shard run; the patches-without-tests check always covers all patches.
#   WORKERS       parallel pytest processes (default: the CPUs)
#   FILE_TIMEOUT  seconds one file may run (default 900)
set -euo pipefail

tree=$(cd "$1" && pwd -P)
dir=$(cd "$2" && pwd -P)
shard=${3:-1/1}
python=${PYTHON:-$tree/.venv/bin/python}
work=${RUNNER_TEMP:-$(mktemp -d)}/patch-tests
mkdir -p "$work"

export TZ=UTC LANG=C.UTF-8 PYTHONHASHSEED=0 PYTHONPATH=$tree
export OPENROUTER_API_KEY= OPENAI_API_KEY= NOUS_API_KEY= ANTHROPIC_API_KEY=

shopt -s nullglob
failed=()
map=$work/file-patch.tsv # <test file> <patch name>
: >"$map"
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
  for t in "${tests[@]}"; do printf '%s\t%s\n' "$t" "$name" >>"$map"; done
done

sort -u "$map" -o "$map"
files=$(cut -f1 "$map" | sort -u)
# A node id of a file that also runs whole is already covered.
allfiles=$files
files=$(while IFS= read -r f; do
  case "$f" in *::*) echo "$allfiles" | grep -qxF -- "${f%%::*}" || echo "$f" ;; *) echo "$f" ;; esac
done <<<"$allfiles")
total=$(echo "$files" | grep -c . || true)
echo "$total unique test files from $(cut -f2 "$map" | sort -u | wc -l) patches"

s=${shard%/*}
n=${shard#*/}
mine=$(echo "$files" | awk -v s="$s" -v n="$n" 'NR % n == s % n')
echo "shard $shard: $(echo "$mine" | grep -c . || true) files"

if [ -n "$mine" ]; then
  out=$work/out-$s
  rm -rf "$out"
  mkdir -p "$out"
  export python out
  # One fresh pytest process per file, as upstream's own runner does (a shared
  # xdist worker pool breaks upstream's conftest, which SIGTERMs the processes
  # of the session's temp dirs at the end of a worker). The biggest files go
  # first so the long ones do not end up last.
  run_one() {
    f=$1
    log=$out/$(echo "$f" | tr / _).log
    rc=0
    timeout "${FILE_TIMEOUT:-900}" "$python" -m pytest -q -p no:cacheprovider "$f" >"$log" 2>&1 || rc=$?
    echo "$rc" >"$log.rc"
    {
      flock 9
      echo "::group::$f (exit $rc)"
      cat "$log"
      echo "::endgroup::"
    } 9>"$out/.lock"
  }
  export -f run_one
  # Biggest files first (a node id sorts by its file); the sort key is the size.
  echo "$mine" | while IFS= read -r f; do
    printf '%s %s\n' "$(stat -c %s "${f%%::*}" 2>/dev/null || echo 0)" "$f"
  done | sort -rn | cut -d' ' -f2- |
    xargs -d '\n' -P "${WORKERS:-$(nproc)}" -I{} bash -c 'run_one "$1"' _ {} || true

  # Several tests of upstream wait on a real child process for a fixed second or
  # two, so a loaded runner can fail one that passes alone: run each failed file
  # once more with nothing else running. A real failure fails again.
  for f in $mine; do
    rc=$(cat "$out/$(echo "$f" | tr / _).log.rc" 2>/dev/null || echo missing)
    [ "$rc" = 0 ] && continue
    echo "::warning::$f failed (exit $rc) under load; running it again alone"
    WORKERS=1 run_one "$f"
  done

  named=0
  for f in $mine; do
    rc=$(cat "$out/$(echo "$f" | tr / _).log.rc" 2>/dev/null || echo missing)
    [ "$rc" = 0 ] && continue
    for name in $(awk -F'\t' -v f="$f" '$1 == f {print $2}' "$map"); do
      echo "::error file=patches/$name.patch::the tests of $name failed: $f (pytest exit $rc)"
      failed+=("$name")
      named=1
    done
  done
fi

if [ "${#failed[@]}" -gt 0 ]; then
  echo "failed: $(printf '%s\n' "${failed[@]}" | sort -u | tr '\n' ' ')" >&2
  exit 1
fi
