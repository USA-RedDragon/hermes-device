# feat(acp): configurable session sources for session/list

## What

ACP `session/list` (the gateway-attached `hermes acp`) offers only sessions an editor started: rows with `source='acp'`, or `source='local'` with `created_source='acp'`. A chat started in the TUI, Desktop or the CLI never appears, so an editor cannot open it, although `session/load` of that chat works.

This adds `sessions.acp_list_sources`, a list of session source labels (the same labels as `hermes sessions list --source`). `acp_adapter/catalog.py` passes the list to `SurfaceLabelReader.list_sessions_rich(sources=...)`, so a group name (`gui`/`desktop`, `tui`, `cli`, `acp`) also matches gateway rows stored as `local` that the surface created, in SQL before the LIMIT. The folder filter, the logical-id projection and the one-row-per-conversation rule are unchanged.

## Why

An editor user who starts a conversation in the TUI or Desktop and wants to continue it in the editor has no way to find it from the editor. Some users want the editor to show the same conversations as their other surfaces.

## Config and defaults

```yaml
sessions:
  acp_list_sources: [acp]          # default: today's behavior
  # acp_list_sources: [acp, tui, gui, cli]
```

The default `[acp]` keeps today's listing exactly. A missing, empty or unreadable value falls back to the default. The key is read from the profile home's own config.yaml (the gateway profile the ACP process attaches to), like `sessions.show_subagents`.

Why opt-in: an editor list scoped to editor threads is the documented behavior, and other surfaces' chats can carry a different cwd, policy or provenance than an editor expects. Users who want the wider list turn it on.

## Tests

- `tests/acp_adapter/test_gateway_catalog_sources.py` (new): the default (unset, unrelated key, empty list) lists editor threads only; `[acp, tui, gui, cli]` lists TUI, Desktop, gui and CLI chats in the editor's folder, and still excludes a messaging chat and other folders when a cwd is given. The second test fails without the change.
- Existing `tests/acp_adapter` suite passes.

Docs: `website/docs/user-guide/sessions.md` (Session Sources), `cli-config.yaml.example`, and the default in `hermes_cli/config_defaults.py`.
