# feat(plugins): agent_turn_start/end hooks and a running-subagent snapshot

## What

Two small additions to the plugin API, so a plugin can tell which sessions have a turn running and which delegated children are alive without reading Hermes internals.

1. Two observer hooks, `agent_turn_start` and `agent_turn_end`, that bracket every admitted `AIAgent.run_conversation()` turn. The start fires after turn admission and before the conversation loop. The end fires in the facade's `finally` block, before the session turn lease is released, so it fires for early returns and exceptions too. Payload: `session_id`, `turn_id`, `task_id`, `platform`, `model`; the end adds `outcome` (`success`, `failed`, `cancelled`, `timed_out`, the same values the relay turn outcome already uses). The end repeats the start's `session_id` even when compaction gave the session a new id during the turn, so the pair always matches. Persistence-disabled internal forks (the background review) share their parent's session id and fire neither, the same rule `pre_llm_call` and `on_session_end` follow. Both hooks are timeout-bounded like `on_session_start` and `on_session_end`.

2. `SubagentLifecycleService.list_active()` (reached as `ctx.subagent_lifecycle.list_active()`) returns immutable `ActiveSubagent` snapshots of every delegated child running in the process, whether `delegate_task` or `launch` started it: `subagent_id`, `session_id` (the child's own), `parent_subagent_id`, `parent_session_id`, `depth`, `goal`, `model`, `status`, `started_at`, `steerable`. `steer_active(subagent_id, text)` steers a listed child by id, the same in-process contract `delegate_task` steering uses; the plugin decides who may steer. Both work outside an agent turn, for example from a plugin dashboard route, and work under `plugins.isolation: host` through the existing remote facade.

## Why

A plugin that delivers messages to running agents needs to know whether a session's turn is running. Today the only per-turn hooks are `pre_llm_call`, `post_llm_call` and `on_session_end`, and the last two fire only for turns that reach `finalize_turn`. Several loop exits return before that: a non-retryable provider error (`nonretryable_client_error_result`), exhausted retries (`max_retries_exhausted_result`), an interrupt during the API call (`abort_turn_on_interrupt`), process shutdown, and the repetition stop. `post_llm_call` is also skipped for any interrupted or empty-response turn. A plugin that pairs `pre_llm_call` with `post_llm_call` therefore keeps a session marked as running after the turn ended. The context engine docs already note this gap for `on_turn_complete` and call unifying the terminal paths a separate follow-up; this change gives plugins one bracket that always closes, without touching the loop.

The E2E test in this PR shows it with a real `hermes -z` process: against a provider that answers 400, `pre_llm_call` fires, `post_llm_call` and `on_session_end` do not, and `agent_turn_start` / `agent_turn_end` (outcome `failed`) both fire with the same `turn_id`.

For subagents, the only way to map a session id to a running child (and its parent session) is to read `tools.delegate_tool_registry._active_subagents` under `_active_subagents_lock`, because `list_active_subagents()` leaves out the child's session id and its steering state. That module was split out of `tools/delegate_tool.py` recently and the record shape changed several times (owner session id, steer ownership), so plugins that read it break across releases. `list_active()` gives them a stable, frozen view in the module the docs already call the public subagent API.

Concrete consumer: a device-bridge plugin that lets one chat or subagent message another; it uses the turn bracket to decide whether a message can ride the target's next tool result, and the snapshot to resolve the caller's subagent tree.

## Config and defaults

No config. Without a plugin that registers the new hooks or calls the new methods, behavior is unchanged: the hooks dispatch to no callbacks, and nothing in core calls the new methods.

## Docs

- `website/docs/user-guide/features/hooks.md`: catalog row and a section for `agent_turn_start` / `agent_turn_end`; the `on_session_end` section no longer says it always fires and points at `agent_turn_end`.
- `website/docs/developer-guide/plugins/index.md`: hook table row, and a row in "Where each hook fires".
- `website/docs/developer-guide/subagent-lifecycle-api.md`: a "Running children" section for `list_active` and `steer_active`.

## Tests

- `tests/agent/test_turn_lifecycle_events.py` (9): start before the loop and end after it for success, failed and interrupted results; end on an exception, which still propagates; end keeps the start session id after a mid-turn rotation; nothing for a persistence-disabled fork or a turn that never got the lease; a raising hook never breaks the turn; both names are in `VALID_HOOKS` and timeout-bounded.
- `tests/agent/test_subagent_lifecycle_active.py` (5): snapshot fields and immutability, the child session id follows a rotation, a finished child leaves the list, `steer_active` reaches a running child and refuses closed, unknown and empty input.
- `tests/e2e/core/providers/test_turn_lifecycle_hooks.py` (2): real `hermes -z` with a loopback provider, for a 400 early return and a normal turn.
- Neighbors pass unchanged: `test_plugins.py`, `test_plugin_host.py`, `test_plugin_api_compat.py`, `test_subagent_lifecycle.py`, `test_subagent_steer.py`, `test_cross_process_turn_lease.py`, `test_turn_facade_lease.py`, `test_detached_fork_lifecycle_hooks.py`, `test_relay_atof_cwd.py`, `test_review_idle_queue.py`, `test_shell_hooks.py`, `test_lifecycle.py`.

## Relation to #150143

#150143 proposes `pre_agent_turn` / `post_agent_turn` at the same place in `turn_facade.py`, as a fail-closed admission gate bundled with Desktop project authority: `post_agent_turn` fires only for plugins whose `pre_agent_turn` returned `allow`, and a raising callback refuses the user's turn. That fits policy plugins but is the wrong contract for an observer. These hooks are pure observers and do not overlap with it; if both land, the end hook here can share the same `finally`.
