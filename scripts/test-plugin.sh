#!/bin/sh
# Run the device plugin's unit and contract tests in the Hermes runtime.
#
# The interpreter and the hermes-agent source come from `hermes --print-runtime-command`.
# Tests run with python -I and a throwaway HERMES_HOME. Hermes bootstrap prepares
# dependencies per data root, so the runtime's own data root is used for the bootstrap
# (HERMES_DEVICE_RUNTIME_HOME overrides it); HERMES_HOME is switched to the throwaway
# directory before any test imports Hermes.
#
# Usage: scripts/test-plugin.sh [unittest discover args...]
# Env: HERMES (launcher, default: hermes on PATH), HERMES_DEVICE_RUNTIME_HOME, TMPDIR.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd -P)
hermes=${HERMES:-hermes}
argv=$("$hermes" --print-runtime-command)
python=$(printf '%s' "$argv" | sed -n 's/^\["\([^"]*\)".*/\1/p')
agent=$(printf '%s' "$argv" | sed -n "s/.*sys\.path\.insert(0, '\([^']*\)').*/\1/p")
if [ -z "$python" ] || [ -z "$agent" ]; then
  echo "test-plugin: cannot read the interpreter and source dir from: $argv" >&2
  exit 2
fi

home=$(mktemp -d "${TMPDIR:-/tmp}/hermes-device-test.XXXXXX")
trap 'rm -rf "$home"' EXIT INT TERM

HERMES_AGENT_SRC=${HERMES_AGENT_SRC:-$agent} HERMES_DEVICE_REPO=$repo HERMES_DEVICE_TEST_HOME=$home \
  "$python" -I -c '
import os, sys
agent, repo, home = os.environ["HERMES_AGENT_SRC"], os.environ["HERMES_DEVICE_REPO"], os.environ["HERMES_DEVICE_TEST_HOME"]
for name in ("PYTHONHOME", "PYTHONPATH", "VIRTUAL_ENV"):
    os.environ.pop(name, None)
sys.path.insert(0, agent)
runtime = os.environ.get("HERMES_DEVICE_RUNTIME_HOME")
if runtime:
    os.environ["HERMES_HOME"] = runtime
import hermes_bootstrap
os.environ["HERMES_HOME"] = home
import unittest
tests = os.path.join(repo, "plugin", "device", "tests")
args = sys.argv[1:] or ["-s", tests, "-t", tests]
unittest.main(module=None, argv=["test-plugin", "discover", *args])
' "$@"
