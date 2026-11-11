## feat(gateway): opt-in turns for background completions with no viewer

### What

Adds `sessions.unviewed_completion_turns` (integer, default `0`). When it is above zero, a `terminal(background=true, notify_on_complete=true)` process that exits, or a background `delegate_task` subagent that finishes, while its CLI, TUI or Desktop session has no viewer attached runs as an ordinary agent turn, the same turn a viewer would have got, instead of being written into the session as a note. The value is how many such turns one session may run in a row with nobody watching; process exits and subagent results share the count. A viewer attaching or a user message starts the count again; past the limit, completions are notes as before.

### Why

Since #134889 (LC-7) a local completion that reaches the head of the session's queue with no viewer is recorded as a note with no model call. That is a safe default, but it means an agent that starts a long job or a background subagent and ends its turn to wait for it never resumes unless someone is watching. Example: alice asks the agent to run a 12 minute build in the background and report, closes her laptop, and comes back an hour later to find the build finished and nothing done with it. Issue #143649 asks for an explicit opt-in for this. The same happens to a scheduled orchestrator that dispatches background subagents with nobody attached: the delegate_task result says the subagent's result re-enters the conversation, the result is written as a note, and the orchestrator never continues. This PR is that opt-in, kept narrow: process exits and subagent results. Watch-pattern notices (which can fire many times per process) keep their current rules, and a failed child's wake (`wakes_parent`) keeps its own rule and `UNATTENDED_WAKE_LIMIT`.

### Behavior with the option set

| Process exit or subagent result, no viewer attached | Before | After |
|---|---|---|
| Session left by a disconnect (laptop asleep, dropped socket, closed tab) | note | turn |
| Row already ended by the orphan reaper (`ws_orphan_reap`, `startup_orphan_reap`) | note | turn; the row is reopened, and the orphan close is armed again so it ends `orphan_end_grace_s` after the turn starts, or when it settles if later |
| Row open after a plain `session.detach` (a client handed the session back) | note | turn; no orphan close is armed, so the row stays open as before |
| Chat the user closed (exit, `/new`): refusal in memory, a close still pending after a restart, or a non-recoverable `end_reason` | note | note |
| Subagent whose dispatching session is refused (its record's route or parent row) | note | note |
| More than N completions in a row with nobody watching (exits and subagent results together) | note | note |
| Failed-child wake (`wakes_parent`) | turn, up to `UNATTENDED_WAKE_LIMIT` | unchanged |
| Watch-pattern notice, local cron result | unchanged | unchanged |
| A viewer attached | turn | turn |
| Messaging chats (Telegram, Discord, ...) | turn | turn (they never reach this path) |

The decision is made where the existing viewer check is (`note_if_unviewed`), at the FIFO head, so queue order, coalescing, restart survival and exactly-once stay the admission contract's. A turn already running holds the FIFO; an exit that arrives meanwhile waits behind it, and a second one folds into the queued admission, so both reach the agent in one turn.

The config and the row's end reason are read off the loop before the claim, while the admission is still queued, and only for a process exit or subagent result with no viewer. So a cancelled read loses nothing, the claimed path gains no await, and the viewer check and the decision still share one loop step. An unreadable config keeps the note. Process exits and subagent results share one count: it bounds the model calls nobody watches, whatever woke them, so an agent that alternates a job and a subagent stops at the same limit as one that only relaunches a job. The in-a-row count lives on the session authority, not on the in-memory session, so retiring an idle session between exits does not reset it; a gateway restart does.

### Config

```yaml
sessions:
  unviewed_completion_turns: 20   # default 0: every unviewed completion is a note
```

Booleans and invalid values fall back to the default. Documented in `cli-config.yaml.example`, `website/docs/user-guide/sessions.md` and the no-viewer bullet in `website/docs/user-guide/features/delegation.md`.

### Tests

New `tests/gateway/test_unviewed_completion_turns.py` (real `GatewayRunner`, `SessionAuthority` and `SessionDB`; the turn handler is a stand-in, no model call):

- default (unset, `0`, a boolean) keeps the note and starts no turn
- option set, no viewer: the exit runs as one turn, no note is written, the admission completes, and a viewer that attaches afterward sees the turn's output in its history snapshot
- a viewer attached: unchanged
- busy session: a human input runs first, then two exits coalesced into one turn
- the in-a-row limit, a user message resetting it, a viewer attaching resetting it, and the count surviving the session leaving memory
- an unreadable config keeps the note
- a plain detach: the turn runs and no orphan close is armed
- a chat the user closed (refusal, a pending close after a restart, an ended row) keeps the note, for a process exit and for a subagent result
- a background subagent dispatched through `dispatch_async_delegation` and delivered through `_deliver_async_delegation_group` with no viewer: a turn with the option set, a note with `0`
- a subagent whose dispatching session is refused by its own record's parent row keeps the note
- process exits and subagent results share one count
- a failed child's wake (`wakes_parent`) does not use up the count
- an orphan-reaped row is reopened for the turn and its orphan close is re-armed
- a watch-pattern notice stays a note

The turn cases fail without the change (the subagent cases without `async_delegation` in the producer set); the refused-dispatcher case fails without its guard; the three closed-chat cases each fail when their guard is removed, and the attach case fails without the attach reset. The neighboring suites (`test_unviewed_completion_notes.py`, `test_unviewed_delegation_failure_turn.py`, `test_session_end_delegations.py`, `test_local_automation.py`, `test_local_session_orphan_end.py`, `test_viewer_close_lifecycle.py`, the completion admission and delivery suites, `tests/tools/test_async_delegation.py`) pass.

### Trade-offs

Model calls happen with nobody watching, at most N per session between visits. The default is off, so nothing changes unless an operator sets it.
