#!/usr/bin/env bash
# Run the tests against a Hermes source tree at the release in .hermes-version.
#
#   scripts/run-tests.sh <hermes tree>      (default: $HERMES_AGENT_SRC)
#
# The tree needs its dependencies installed (CI runs `uv sync --locked --python 3.14 --extra all --extra anthropic
# --group dev --group test` in it); PYTHON overrides the interpreter (default: the tree's .venv).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd -P)
tree=$(cd "${1:-${HERMES_AGENT_SRC:?pass the Hermes source tree}}" && pwd -P)
python=${PYTHON:-$tree/.venv/bin/python}

want=$(tr -d '[:space:]' <"$root/.hermes-version")
have=$(git -C "$tree" describe --tags --exact-match 2>/dev/null || echo unknown)
[ "$have" = "$want" ] || echo "warning: the tree is at $have, the plugins are tested against $want" >&2

export TZ=UTC LANG=C.UTF-8 PYTHONHASHSEED=0 PYTHONPATH=$tree
export OPENROUTER_API_KEY= OPENAI_API_KEY= NOUS_API_KEY= ANTHROPIC_API_KEY=
cd "$root"
exec "$python" -m pytest -q -p no:cacheprovider tests "${@:2}"
