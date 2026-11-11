# fix(acp): attach to a running session loaded from another folder

## What

`session/load` (and `session/resume`) on the gateway-attached `hermes acp` moves the session to the editor's folder when the session's cwd differs (`_move_workspace`, the owner's `workspace` mutation). The owner accepts that mutation only on an idle session. When another surface (TUI, Desktop, a messaging chat) is running a turn in that session, the load fails:

```
{"code": -32603, "message": "Internal error", "data": {"details": "session_busy"}}
```

The same load from the session's own folder succeeds and streams the running turn, because no move is needed.

With this change, a load that finds the session busy (`session_busy`, or `unknown_execution` for a lost turn) attaches as a viewer without moving it: the editor gets the history and the live turn, plus a short notice that the session stays in its current folder for now. The move is remembered and runs before this editor's next prompt that finds the session idle. A later load re-evaluates it. The deferred move is skipped while a lost submit's retry identity is held, so the retry still reuses its admission.

## Why

Opening a running conversation from an editor is a normal way to watch or join it. Failing with an internal error gives the user no way in until the other surface's turn ends, and the error does not say why.

## Config and defaults

None. The change only affects a load that failed before; a load of an idle session moves the workspace exactly as today.

## Tests

- `tests/acp_adapter/test_gateway_load_running.py` (new, Linux, real gateway and `hermes acp` over stdio): a TUI session runs a turn held at the model; an editor loads it from another folder. Before the fix the load fails with `-32603` / `session_busy`. After it, the load succeeds, the session keeps its folder during the turn, the running turn's reply streams to the editor, and the editor's next prompt moves the session to the editor's folder before it runs.
- Existing `tests/acp_adapter` suite passes.
