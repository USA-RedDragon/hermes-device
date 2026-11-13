# Talaria plugins (`plugins/`)

Hermes Agent plugins that Talaria uses, one directory each. Each is a normal Hermes plugin
(`plugin.yaml`, `__init__.py`, and `dashboard/` with the routes the hub calls) and has no dependency
on the other.

| Plugin | What it does | Needs |
|---|---|---|
| [`subagent-routes`](subagent-routes/README.md) | The model routes a chat's subagents may use, chosen per chat in the hub (`delegation.backend: model_map`). Optional guard against waiting by `sleep` polling. | Hermes with pluggable delegation providers (v0.22.5 or later) |
| [`project-memory`](project-memory/README.md) | One curated `MEMORY.md` per git repository of a chat's bound device folder: a `project_memory` tool, a prompt block, and the hub's Memory page. | Hermes v0.22.5 or later and the Talaria device plugin |

Tested against Hermes v0.23.1 (`.hermes-version`).

## Install

Copy a plugin directory into `$HERMES_HOME/plugins/` and enable it:

```sh
scripts/install-plugin.sh subagent-routes "$HERMES_HOME/plugins/subagent-routes"
hermes plugins enable subagent-routes
```

`install-plugin.sh` replaces the destination only when its content changed, by two renames, so a gateway that
still runs can keep loading the old copy while the new one is put in place. Run it again on every start.
Release tarballs (`talaria-hermes-plugins-vX.Y.Z.tar.gz` on the GitHub release `plugins/vX.Y.Z`) hold the same directories.

Settings go under `plugins.entries.<plugin>.settings` in `config.yaml`, as each plugin's README says.

## Develop

```sh
# a Hermes checkout at the release in .hermes-version, with its dependencies installed
scripts/run-tests.sh /path/to/hermes-agent
```

CI (`.github/workflows/plugins-test.yaml`) does the same (`uv sync --locked --python 3.14 --extra all --extra anthropic --group dev --group test`
in the Hermes tree), plus `ruff check`.

Release: bump `version` in the plugin's `plugin.yaml`, push a `plugins/v<major>.<minor>.<patch>` tag (`.github/workflows/plugins-release.yaml`). Bare `v*` tags are talaria daemon releases.

No personal data in these plugins: no names, hostnames, addresses or account ids, in code, tests or docs.
Routes, models and texts come from the user's Hermes config.
