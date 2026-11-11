feat(terminal): move long foreground commands to the background

## What

A new option, `terminal.auto_background_seconds` (default `0`, off). When it is set, a foreground `terminal` command that is still running after that many seconds is handed to the process registry as a tracked background session with `notify_on_complete`, the same as if the agent had started it with `background=true, notify=true`. The command is not killed and not started again.

The tool result returned at that point has:

- `status: "auto_backgrounded"`, `exit_code: null`
- `session_id` and `pid` of the background session
- `output`: the output captured so far
- `notify_on_complete: true`
- `note`: the command is still running in the background as that session, the agent will be notified when it exits, it must not re-run it, and it can use `process_manage` poll/wait/log to check on it

The completion notice (exit code and output tail) then arrives through the normal `notify_on_complete` delivery path: the completion queue in the CLI/TUI and the per-process watcher in the messaging gateway.

## Why

Today a foreground command runs until it exits or reaches its timeout, and a command that times out is killed. Long builds, test suites and downloads that the agent did not think to background hold the turn the whole time, and when they pass the timeout the work is lost and usually re-run. Moving the command to the background after a set time (like Ctrl+B in other coding agents) keeps the work, frees the turn, and still gives the agent the result.

## How

It reuses the yield-to-background path that already moves a local foreground command to the background when a user message arrives mid-command (`yield_to_background_handler` and `ProcessRegistry.adopt_local`). No new process tracking or notification mechanism is added.

- `BaseEnvironment._wait_for_process` and `execute` take an optional `yield_after` (seconds). With a `yield_handler`, the wait hands the process to the handler once it has run that long, exactly as for a user-message yield, and tags the result with `yield_reason="auto_background"` and `yielded_after_s`. It is tried once: if the handler declines or fails, the wait continues to the normal timeout, so there is no second copy and no orphan.
- `_yield_kwargs` in `tools/terminal_tool.py` adds `yield_after` only when a yield handler exists (local backend) and the configured value is below the command's timeout.
- `auto_background_seconds()` in `tools/terminal_tool_background.py` reads the option from the effective config and returns nothing unless a completion can reach the agent, using the same predicate as `terminal.process_wait_auto_notify` (`_can_deliver_completion`). One-shot runs, cron jobs, Kanban workers, stateless API sessions, delegated subagents and profiles with background notifications off keep today's behavior.
- `_run_foreground` returns the `auto_backgrounded` result with its own note. A user-message yield keeps its existing `yielded_to_background` result and note.

## Scope and fallback

Only the local backend can hand a running foreground process to the registry today (`yield_to_background_handler` returns no handler for other backends). For docker, ssh, modal, daytona, plugin backends and anything else, no handler and no `yield_after` are passed, so `env.execute` receives the same arguments as before and the command runs in the foreground until it exits or times out, as today. A backend or plugin that wants this later can pass its own `yield_handler` and `yield_after` to `BaseEnvironment.execute`; `_run_foreground` already formats the result from `yield_reason`.

## Config

```yaml
terminal:
  auto_background_seconds: 120   # 0 (default) = off
```

A value at or above the command's timeout has no effect. Documented in `website/docs/user-guide/features/tools.md` and `cli-config.yaml.example`; default added to `DEFAULT_CONFIG`.

## Tests

`tests/tools/test_terminal_auto_background.py` (about 7 s; thresholds are fractions of a second):

- the default is 0 and reads as off, as do unset and invalid values
- off: a foreground command runs to its exit with a normal result
- a command that finishes before the threshold gets a normal foreground result
- a command that runs past the threshold returns `auto_backgrounded` in well under its runtime, with the session id, the output so far, `notify_on_complete`, a registered running session with `notify_on_complete` set, a note with the session id and the do-not-rerun instruction, and a completion event after the cleanup kill
- a threshold at or above the timeout is ignored
- docker, ssh, modal and a plugin backend get no handler and no `yield_after`
- a session that cannot receive a completion stays off
- a handler that declines or raises is tried once and the command runs to its natural exit exactly once
- `yield_after` without a handler does nothing
- a user-message yield still reports its own reason

Existing yield-to-background, foreground timeout, trailing-ampersand, base environment, local environment and process wait tests pass unchanged.
