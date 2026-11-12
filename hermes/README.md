# hermes-patches

A small set of patches on top of the pinned [Hermes Agent](https://github.com/NousResearch/hermes-agent) release, built into a multi-arch image (`linux/amd64`, `linux/arm64`, the same platforms as upstream's image).

Each patch fixes one limit, sits behind a config option whose default keeps upstream's behavior, has upstream-style tests, and is written so that upstream can take it. When upstream ships a fix, the patch is dropped.

## How it works

- `Dockerfile` starts from `docker.io/nousresearch/hermes-agent:<release>@<digest>` (`ARG HERMES_BASE`), and sets `HERMES_BASE_IMAGE` to that reference in the image. A build stage on the build platform applies `patches/*.patch` in name order to the source in `/opt/hermes` with `scripts/apply-patches.sh --image`, and the final stage copies only the changed files on top of the upstream image. Nothing runs on the target platform, so the arm64 image needs no emulation. Hunks for files that are not in the image (tests, docs) are skipped there.
- New layers are zstd. The upstream layers keep their compression and digests, so nodes reuse the layers they already have from the upstream image and a release adds only the patched files.
- The Hermes version that the image reports is upstream's, so tools that check the Hermes version see the base release.

## Releases and tags

Tags are `v<upstream release>-p<n>`: `v0.23.1-p1`, `v0.23.1-p2`, then `v0.24.0-p1` after the base moves. The upstream release is always the part before `-p`.

The Release workflow runs on every push to `main` that changes the Dockerfile, the patches, the scripts or the workflows. It runs the tests, then builds and pushes `ghcr.io/usa-reddragon/hermes-patches:<tag>` with BuildKit SLSA provenance and an SBOM, signs the image keyless with cosign (GitHub artifact attestations are not available for private repositories of a user account), and creates the tag and a GitHub release that lists the patches. Run it by hand (workflow_dispatch) to release HEAD again.

Verify a release with `cosign verify ghcr.io/usa-reddragon/hermes-patches:<tag> --certificate-identity-regexp '^https://github.com/USA-RedDragon/hermes-patches/' --certificate-oidc-issuer https://token.actions.githubusercontent.com`.

Renovate bumps `HERMES_BASE` in the Dockerfile as soon as upstream releases. A green PR merges on its own, which publishes `v<new release>-p1`. Rolling it out is a separate Renovate PR in the Flux repo that is never merged on its own.

## CI

The Test workflow runs on pull requests and before each release:

- **Patches**: clones upstream at the release in the Dockerfile, applies every patch to the full tree, installs the locked test environment with uv, and runs each patch's tests. A patch that no longer applies fails with `<patch> does not apply to Hermes <release>`. A patch's tests are the test files it adds or changes plus the files in `patches/<name>.tests`.
- **Hub contract**: the hermes-hub contract tests (`TestContractAgainstSource`, `TestClientMethodsAgainstSource`) against the patched tree, with `HERMES_AGENT_VERSION` set to the upstream release.
- **Image**: builds the image for both platforms, then runs the hermes-device plugin tests inside the amd64 image.

## Patches

| Patch | What it does | Upstream status (checked 2026-11-11) |
|---|---|---|
| `0001-compression-main-model-routes` | `auxiliary.compression.main_model_providers` (list) and `main_model_when_larger` (bool): chats on a listed provider, or with a larger window than the compression model, summarize on their own main model at their own threshold. | Not fixed upstream; no PR. |
| `0002-terminal-auto-background` | `terminal.auto_background_seconds` (0 = off): a foreground command still running after N seconds becomes a tracked background process with notify_on_complete instead of running on to its timeout. Local backend; the talaria device plugin (0.19.5 and later) does the same for device commands. | Not fixed upstream; no PR. |
| `0003-image-render-confinement` | `security.image_render_paths` (`any`, `media`, `workspace`): confines `image.render` to images Hermes stored, or also the session's workspace. `runtime.describe` lists `image-render-confined-v1` while confined. | Not fixed upstream; no PR. |
| `0004-session-list-scope` | `session.list` param `audience` and plugin hook `filter_session_list`: a caller can limit a listing to the sessions a plugin keeps for that audience. Fails closed. `runtime.describe` lists `session-list-audience-v1`. | Not fixed upstream; no PR. |
| `0005-plugin-agent-state-api` | Plugin hooks `agent_turn_start` and `agent_turn_end` (with an outcome; fire on every turn ending, unlike `post_llm_call`) and `subagent_lifecycle.list_active()` / `steer_active()`. | Not fixed upstream. Open PR #150143 adds a different, fail-closed turn gate. |
| `0006-ollama-num-ctx-scope` | `model.ollama_num_ctx` applies only while the active route is a self-hosted server, also after `/model`, fallback and restore. No new key. | Not fixed upstream. Open PRs (#134389 and others) gate on `is_local_endpoint()`, which is true for `process://` providers. |
| `0007-background-completion-unattended-turn` | `sessions.unviewed_completion_turns` (0 = off): a notify_on_complete process exit or a background subagent result in a local session with no viewer runs as an agent turn, up to N in a row (one count for both), instead of a note. Not in a chat closed by exit or `/new`, and not for a subagent whose dispatching session is refused. | Not fixed upstream. Open PR #153217 makes it the default and has changes requested; issue #143649 asks for an opt-in. |
| `0008-acp-session-list` | `sessions.acp_list_sources` (default `[acp]`): ACP `session/list` can also offer chats the TUI, Desktop or CLI started, filtered by folder as before. | Not fixed upstream; no PR. |
| `0009-acp-load-running` | ACP `session/load` of a running session from another folder attaches as a viewer instead of failing with `session_busy`; the workspace moves at the next idle prompt. Bug fix, no key. | Not fixed upstream; no PR. |
| `0010-gateway-wait-for-runtime-lock` | `gateway.runtime_lock_wait_seconds` (0 = off): a gateway that finds its profile owned warms up, then waits for the owner to exit and takes over within a poll (0.2 s) instead of exiting 1. Gateway lock records carry the PID namespace, so a holder in a sibling container is not read as stale. While it waits, `$HERMES_HOME/gateway.standby` exists, so a supervisor can stop the owner only once its successor is ready. For rolling updates of pods that share one volume. | Not fixed upstream; no PR. |
| `0011-sigterm-restart-wait` | `gateway.sigterm_restart_wait` (false = off): a supervisor's SIGTERM restarts like SIGUSR1, so running turns finish while approvals and clarify stay answerable and new input is kept for the next gateway. A second SIGTERM stops at once. `gateway.restart_wait_plugin_sockets` (`[]`) lists plugins whose WebSocket routes still accept connections during the restart wait, so a device that drops can reconnect while its turns finish. Sign-in (`/auth/...`) also works during the wait, so a front end without a session cookie can still open chats. `gateway.restart_agent_cleanup_seconds` (0) releases the idle cached agents together within that bound on a restart instead of up to 30 s each, after their queued memory writes and session-end flush. `gateway.restart_wait_delegations` (false) lets running background subagents hold the restart wait. | Not fixed upstream. Open PR #83329 sends every unmarked SIGTERM of a supervised gateway down the after-turn restart path, with no key. |
| `0012-background-process-handoff` | `terminal.process_handoff_backends` (`[]` = off): a graceful restart hands the background processes of the listed backends to the next gateway, which reattaches them from processes.json (same session id, output, notify_on_complete routing). Plugins register reattach providers for their own processes. Also fixes an env poll that read a reconnecting backend's error text as an exit. Local processes still stop with the gateway. | Not fixed upstream; no PR. |
| `0013-tool-search-eager` | `tools.tool_search.eager` (`[]` = off): tool or toolset names that are never deferred behind the tool-search bridge, so the model calls them directly. A tool named in `defer` still defers when only its toolset is eager. | Not fixed upstream. Open PR #110714 takes tool names only. |
| `0014-adopted-command-markers` | A foreground command moved to the background (a user message or `terminal.auto_background_seconds`, on the local or a plugin backend) no longer shows Hermes's `__HERMES_PIPE_`/`__HERMES_CWD_` wrapper lines in its output and completion notice. Bug fix, no key. | Not fixed upstream; no PR. |
| `0015-compaction-keep-last-skill` | `compression.keep_last_skill_chars` (0 = off): compaction keeps the newest loaded skill's body, up to N chars, on the handoff instead of a `[SKILL_PRUNED]` reload marker, and drops the marker when a kept row still holds the body. Older skills and larger bodies keep their markers. | Not fixed upstream; no PR. |
| `0016-delegation-restart-handoff` | `delegation.restart_handoff` (false = off) and `restart_handoff_grace_seconds` (30): a graceful restart stops each background subagent at a clean point and leaves its delegation `handed_off` in the ledger, children that finished keeping their results; the next gateway resumes the others from their stored transcripts on their recorded routes under the same delegation id, and the delegation is delivered once. A tool call cut by the restart reads as interrupted. | Not fixed upstream; no PR. |
| `0017-prompt-product-name` | The system prompt's runtime block is `# Runtime environment` (end marker `<!-- End runtime environment -->`) and the profile line `Active profile: <name>`, a plain rename without a key; readers of stored prompts still accept the old `# Hermes runtime environment` markers. `agent.product_help_guidance` (true) drops the Hermes help paragraph (both variants) when false, and then lets `skills.disabled` list the otherwise essential hermes-agent skill. | Not fixed upstream; no PR. |
| `0018-steer-controls-after-restart` | A chat a surface started (hub, Desktop, TUI, CLI, ACP) keeps steer and replace after a gateway restart instead of `unsupported_control`: the first turn relabels its row `local`, and a `local` row now reads as its `created_source`. Bug fix, no key. | Not fixed upstream; no PR. |
| `0019-steer-current-turn` | A steer sent before the turn's first tool call waits, with its ticket, for a tool result of this turn instead of landing after a tool result of an earlier turn, where the model read it as history. Bug fix, no key. | Not fixed upstream; no PR. |
| `0020-skill-config-managed-overlay` | Skill settings (`skills.disabled`, `skills.platform_disabled`, `agent.product_help_guidance` for 0017) in the managed scope apply even when config.yaml lacks them; before, they held only while config.yaml had a copy, and Hermes writes config.yaml without the managed values. Bug fix, no key. | Not fixed upstream; no PR. |
| `0021-subagent-controls-listed-children` | Stop, Steer and tail reach every background subagent `subagent.list` shows, also after the parent's cached agent was rebuilt or evicted; before, Stop on such a row answered `found: false` (the hub's "no longer running"). Bug fix, no key. | Not fixed upstream; no PR. |
| `0022-stale-breaker-ends-call` | A stale-stream kill that opens the consecutive-stale breaker (`HERMES_STREAM_STALE_GIVEUP`) ends the call instead of reconnecting up to `HERMES_STREAM_RETRIES` more times; a provider that holds the stream open with keepalive comments cost 6 stale windows (18 min at 180 s) before, 5 now. Bug fix, no key. | Not fixed upstream; no PR. |
| `0023-subagent-provider-patience` | `delegation.stream_stale_giveup` and `delegation.auto_recovery_cycles` (null = inherit): a subagent's own stale-breaker threshold and overload/outage wait ladder, so a background child on a queued or overloaded model (DeepInfra 429 `engine_overloaded`) fails fast with the provider's error while the main agent keeps its patience. | Not fixed upstream; no PR. |
| `0024-session-background-tool` | Gateway method `session.background_tool` (session_id, execution_generation, optional tool_call_id) asks the running turn's tool workers to yield, so a foreground terminal command moves to the background registry and the turn goes on; `runtime.describe` advertises `tool-background-v1`. New method, no key. | Not upstream; no PR. |
| `0025-delegation-route-report` | A backend-routed subagent's result carries `ran_on` (`route <label>: <model> via <provider>`, with a fallback provider named), the background dispatch handle lists `runs_on` per task, and every async completion block prints `Ran on ...` for its task instead of one unit-wide model. Reporting only; the default backend's result shape is unchanged. | Not fixed upstream; no PR. |
| `0026-mid-turn-subagent-results` | `sessions.mid_turn_subagent_results` (false = off): a background subagent's result admitted while its local session's turn runs is transferred to that turn (the `prompt.steer_queued` transfer) and rides its next tool result, outside the user steer marker; no running tool is interrupted. A result no tool result took goes back to its queue place at settlement and runs or is noted as before. Only results dispatched from the same conversation. | Not fixed upstream; no PR. |
| `0027-delegation-keep-child-processes` | `delegation.keep_child_processes` (false = off): background processes a subagent leaves running when it ends move to its parent instead of being killed, and a child stopped for a restart handoff keeps them; the parent can poll, wait on and kill them and gets their completion. | Not fixed upstream; no PR. |
| `0028-sandbox-background-tree-kill` | A sandbox (plugin backend) background process kill stops the whole process tree, not only the wrapper shell. Bug fix, no key. | Not fixed upstream; no PR. |
| `0029-adapter-settle-push-only` | The 1 s settle before lifecycle sends at gateway start runs only when a connected adapter pushes (`supports_async_delivery`); the API server alone no longer adds 1 s to every boot and takeover. Bug fix, no key. | Not fixed upstream; no PR. |
| `0030-standby-prewarm-dashboard` | A 0010 standby also imports the dashboard app its Webapp or gated listener mounts at start, so the listener serves within tens of ms of the takeover instead of about 0.5 s. Part of 0010, no key. | Not fixed upstream; no PR. |

## Adding or changing a patch

1. Clone upstream and check out the release in the Dockerfile.
2. Make the change as one commit with its tests, in upstream's style and with a config option that keeps upstream's default.
3. Run its tests: `uv sync --locked --python 3.14 --extra all --extra anthropic --group dev --group test`, then `.venv/bin/python -m pytest <test files>`.
4. Put all patches on one branch in order and export them: `git format-patch --zero-commit --no-signature -o patches <release>..HEAD`. Keep the numbers in the file names; they set the order.
5. List neighboring upstream tests the patch must keep green in `patches/<name>.tests`.
6. Add the patch to the table above.

## Rebasing

When a Renovate PR fails in "Patches" with `does not apply`:

1. Clone upstream at the new release and `git am -3 patches/*.patch`.
2. Resolve each conflict, run the tests, and export again as in step 4 above.
3. If upstream now has the fix, delete the patch, its `.tests` file and its row in the table.
4. Push to the Renovate branch. CI runs again.

## Upstream branches

`upstream/<name>/` holds each patch ported to upstream `main`, as `git format-patch` output with the upstream commit it applies to (`BASE`) and a pull request description (`PR.md`). Apply with `git am` on that commit to get a branch ready for a pull request.
