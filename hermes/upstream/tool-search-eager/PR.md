feat(tool-search): tools.tool_search.eager keeps named tools and toolsets direct

## What

A new option, `tools.tool_search.eager` (default `[]`): a list of tool names or toolset names that are never deferred behind the `tool_search` / `tool_describe` / `tool_call` bridge. Their full schemas stay in the tools array, so the model calls them directly. A tool that `defer` names itself still defers when only its toolset is listed in `eager`. A non-list value logs a warning and is ignored.

## Why

With tool search active, every MCP and plugin tool is deferred. For a toolset the model uses on most turns, such as a plugin's browser toolset, each use then costs a `tool_search` and a `tool_describe` round trip and an indirect `tool_call`. Small local models also trip over the per-call caps of the bridge (seven queries, ten names) and call through `tool_call` without required arguments they never saw. There is no way today to keep one plugin toolset or one MCP tool loaded: `defer` only adds names to the deferred set. A tool's own availability check (`check_fn`) still applies, so an eager tool that is unavailable in a session is not sent at all.

## How

- `ToolSearchConfig.eager_tools`, parsed from `tools.tool_search.eager`.
- `is_deferrable_tool_name` takes `eager_tools` (None reads the config) and returns False for a named tool, or for a tool whose registry toolset is named, unless `defer` names the tool itself. `classify_tools`, the assembly, the search and describe catalogs, `scoped_deferrable_names` and `resolve_underlying_call` all use the same set, so `tool_call` on an eager tool gets the existing "directly-listed tool, call it directly" correction.
- `eager: []` in `DEFAULT_CONFIG` and in the tool-search docs.

Related: #110714 (tool names only) and #86620.

## Tests

`tests/tools/test_tool_search.py`: parsing (list, blanks, scalar warning, default), classification by tool and toolset name, precedence over `defer`, and the configured list reaching the assembled tools array and the bridge scope. `pytest tests/tools/test_tool_search*.py tests/tools/test_deferral_fixes.py tests/tools/test_connectors_catalog.py tests/tools/test_connectors_tool.py tests/tools/test_model_tools.py`: 150 passed.
