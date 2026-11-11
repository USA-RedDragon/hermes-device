package gateway_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// contract lists source lines of Hermes v0.23.1 this client depends on, by
// file. A "#name" suffix lets one file have more than one entry.
// TestContractAgainstSource checks them in a checkout named by
// gatewaytest.HermesSource.
func contract() map[string][]string {
	return map[string][]string{
		"gateway/session_contract.py":  {"CANONICAL_GATEWAY_PROTOCOL = '" + gateway.Subprotocol + "'"},
		"hermes_cli/gateway_client.py": {`GATEWAY_WS_TICKET_PREFIX = "` + gateway.TicketPrefix + `"`},
		"tui_gateway/ws.py":            {`"type": "` + gateway.ReadyEvent + `"`, `{"session_authority": True}`},
		"hermes_cli/dashboard_auth/routes.py": {
			`@router.post("/auth/password-login"`,
			"provider: str = Field(max_length=128)", "username: str", "password: str",
		},
		"hermes_cli/web_routers/webapp.py": {
			`@router.post("/api/webapp/socket-ticket")`, "GATEWAY_SOCKET: (\"gateway_ticket\", True),",
			`return JSONResponse({"ticket": ticket, "ttl_seconds": TTL_SECONDS}`,
		},
		"hermes_cli/web_session_sockets.py": {`GATEWAY_SOCKET = "gateway"`},
		"tui_gateway/contracts/canonical.py": {
			`canonical_method("` + gateway.MethodPromptCancel + `", params=PromptCancelParams`,
			`canonical_method("` + gateway.MethodResolveLost + `", params=PromptResolveUnknownParams`,
			`status: Literal["queued", "started", "unknown", "terminal"]`,
			`canonical_method("` + gateway.MethodClarify + `", params=ClarifyRespondParams`,
			"    admission_id: str\n    execution_generation: int\n",
		},
		"gateway/run_api.py": {
			"native_peer = bool(ws.client) and ws.client.host in {'127.0.0.1', '::1'}",
			"if reason == 'none' or ws.headers.get('origin') or (reason == 'ok' and not native_peer):",
		},
		"tui_gateway/contracts/canonical_sessions.py": {
			"('session.create', CanonicalCreateParams, CanonicalSnapshot)",
			"('session.resume', CanonicalResumeParams, CanonicalSnapshot)",
			"('session.list', CanonicalListParams, CanonicalListResult)",
			"('session.interrupt', CanonicalControlParams, CanonicalSessionHandle)",
			"('session.events.since', CanonicalEventsParams, CanonicalEventsResult)",
			"('prompt.submit', CanonicalSubmitParams, CanonicalSubmitResult)",
			"('approval.respond', CanonicalApprovalParams, PromptResponseResult)",
			"source: Literal['cli', 'tui', 'gui', 'acp', 'tool', 'oneshot'] = 'cli'",
			"scope: Literal['live', 'stored'] = 'live'",
			"last_sequence: int = 0", "replay_epoch: str | None = None", "snapshot_required: bool",
			"subscription_id: str", "execution_generation: int", "prompts: list[CanonicalPendingPrompt]",
			"pending: list[CanonicalPendingAdmission]", "class CanonicalPendingAdmission(AdmissionStatus):\n    input_id: str\n    text: str",
		},
		"gateway/session_controls.py": {
			`if set(params) != {"session_id", "execution_generation", "prompt_id", "choice"}:`,
			`if set(params) != {"session_id", "execution_generation", "prompt_id", "answer"}:`,
			"if set(params) - {'end_reason'} != {'session_id', 'subscription_id'}:",
			"if ref.session_id not in self.subscriptions:\n            raise RuntimeStoreError('permission_denied')",
			"request_id = params.get('submission_id') or params.get('input_id')",
		},
		"gateway/session_automation.py": {
			"identity = json.dumps([producer, key, uuid.uuid4().hex], separators=(',', ':'))",
			"session_id=ref.session_id, request_id=identity, payload=deepcopy(payload),",
		},
		"gateway/session_authority.py":        {"input_id=row['request_id'], text=text,"},
		"gateway/session_completion_notes.py": {"_ALWAYS_NOTED = frozenset({'cron'})"},
		"gateway/session_events.py": {
			"'type': event_type, 'session_id': self._key,", "'payload': deepcopy(payload), 'replay_epoch': self.epoch}}",
		},
		"gateway/session_pending_controls.py": {
			"choices = ['" + gateway.ChoiceOnce + "', '" + gateway.ChoiceDeny + "']",
			"prompt = {'kind': 'approval', 'prompt_id': prompt_id,", "event_type='approval.request'",
			`prompt = {"kind": "clarify", "prompt_id": entry.clarify_id,`, `"multi_select": entry.multi_select}`,
			`event_type="clarify.request")`, "event_type=prompt['kind'] + '.settled')",
		},
		// The hub works out the outcome of an approval answered where it cannot
		// see the choice from the tool call that asked (internal/service/outcome.go).
		"agent/tool_executor.py": {
			"_advance_start_order(lambda: _begin_tool_execution(agent, ref, display_index))",
			`_safe_callback(agent.tool_start_callback, "Tool start", tool_call_id, function_name, display_args)`,
		},
		"gateway/run_turn_progress.py#tools": {
			`payload = {"tool_id": str(call_id or ""), "name": name, "context": build_tool_preview(name, args, max_len=80) or "",`,
			`"result": bounded_result(result, is_error)}`,
		},
		"tools/approval.py#outcome": {
			`return deny(spec.gateway_refused, "denied", reason="denied by user",`,
			`gateway_refused="BLOCKED: Command {reason}.{reason_addendum}"`,
			`cli_denied="BLOCKED: User denied this command."`,
		},
		"tools/terminal_tool.py#outcome":   {`raise _Rejected(_error_json(approval.get("message", fallback_msg), status="blocked",`},
		"tools/file_tools_write_guards.py": {`denied = blocked.format(why="was denied by the user.")`},
		"gateway/session_busy_controls.py": {
			"if params.get('key') == 'show_reasoning' and not set(params) - allowed and ref.session_id:",
			"_YOLO_WORDS = {'1': True, 'on': True,",
		},
		"gateway/local_viewer_events.py": {
			`_CONFIG_DEFAULT_SOURCES = frozenset({"tui", "gui"})`,
			`publish("reasoning.delta", {"text": str(text or "")}) if show_reasoning(turn) else None)`,
		},
		"tui_gateway/contracts/canonical.py#mutate": {
			`canonical_method("` + gateway.MethodMutate + `", params=SessionMutateParams, result=SessionMutateResult,`,
			"    expected_revision: int\n    operation: str\n    payload: dict[str, JsonValue]\n    expected_generation: int | None = None",
		},
		"gateway/session_mutations.py": {
			"_FIELDS = frozenset({'session_id', 'request_id', 'expected_revision', 'operation', 'payload'})",
			"live.event_stream.publish(ref.session_id, result, event_type='" + gateway.UpdatedEvent + "')",
		},
		"hermes_state_mutations.py": {
			"required = {'" + gateway.OpRename + "': {'title'}, '" + gateway.OpArchive + "': {'archived'}}",
			"if operation in {'" + gateway.OpDelete + "', 'reset'} and not payload:",
			"if (operation == '" + gateway.OpModel + "' and 'model' in payload and not set(payload) - {'model', 'provider', 'confirm', 'reasoning'}",
		},
		"hermes_state_runtime.py": {
			"if operation in {'delete', 'rewind', 'reset', 'branch', 'model', 'compress'} and expected_generation is None:",
			"raise RuntimeStoreError('" + gateway.ReasonRevisionConflict + "')",
		},
		"gateway/session_controls.py#refusals": {
			"'code': getattr(exc, 'wire_code', 4001), 'message': getattr(exc, 'wire_message', exc.reason),",
			"if exc.reason in {'" + gateway.ReasonRevisionConflict + "', '" + gateway.ReasonStaleGeneration + "'}:",
			"for key in ('revision', 'execution_generation'):",
			"not params.get('include_hidden') and (row.get('hidden') or row.get('archived'))):",
		},
		"hermes_state_mutation_guards.py": {
			"raise RuntimeStoreError('" + gateway.ReasonUnknownExecution + "')",
			"raise RuntimeStoreError('" + gateway.ReasonSessionBusy + "')",
		},
		"hermes_state_runtime.py#dedupe": {
			"WHERE principal_id=? AND target_session_id=? AND request_id=?''', (principal_id, session_id, request_id)).fetchone()",
			"raise RuntimeStoreError('admission_conflict', **ADMITTED_REQUEST_CONFLICT)",
		},
		"gateway/owner_identity.py": {"return host_owner_subject() if _login_provider(provider) else subject"},
		"gateway/run_api.py#restart": {
			"await send({'type': 'websocket.close', 'code': 1013})",
			"await JSONResponse({'error': 'gateway_not_ready', 'state': descriptor['state']},",
			"return {'type': 'websocket.disconnect', 'code': 1013}",
		},
		"gateway/session_controls.py#draining": {"exc = RuntimeStoreError('" + gateway.ReasonRuntimeDraining + "')"},
		"gateway/drain_control.py":             {`"session.info", "session.list", "prompt.submit", "prompt.receipt", "session.interrupt",`},
		"hermes_state.py":                      {"MAX_TITLE_LENGTH = 100"},
		"hermes_state_titles.py":               {`raise ValueError(f"Title '{title}' is already in use by session {conflict_id}")`},
		"tui_gateway/ws.py#internal":           {`await _reply(_error(-32603, "internal error", req_id),`},
		"gateway/session_mutation_model.py": {
			"return {'status': 'confirmation_required', 'confirm_required': True, 'confirm': token,",
			"'confirm_message': combined_message(warnings),",
		},
		"gateway/session_mutation_model.py#resolve": {"raise RuntimeStoreError('" + gateway.ReasonModelResolution + "')"},
		"gateway/session_deferred_pick.py":          {"'status': 'deferred', **picked,"},
		"gateway/session_config.py": {
			"'" + gateway.MethodModelOptions + "': partial(model_options, connection)}",
			"flags = {'explicit_only', 'include_unconfigured', 'refresh'}",
		},
		"tui_gateway/contracts/config_free_tier_control.py": {
			"    slug: str\n    name: str\n    models: list[str] = Field(default_factory=list)",
			"    authenticated: bool | None = None", "    unavailable_models: list[str] | None = None",
			"    reasoning: bool\n    can_disable_reasoning: bool | None = None",
			"    providers: list[ModelOptionProvider]\n    model: str = \"\"\n    provider: str = \"\"",
		},
		"gateway/session_tui_config.py": {
			"if key == '" + gateway.ReasoningKey + "' or (key == 'yolo' and 'scope' in params):",
			"if value not in _REASONING_DISPLAY_TOGGLES and value != '" + gateway.ReasoningReset + "' and parse_reasoning_effort(value) is None:",
		},
		"hermes_constants.py": {
			`VALID_REASONING_EFFORTS = ("` + strings.Join(hub.ReasoningEfforts()[1:], `", "`) + `")`,
			`if effort in {"` + hub.ReasoningEfforts()[0] + `", "false", "disabled"}:`,
		},
		"tui_gateway/contracts/canonical_sessions.py#info": {
			"    subscription_id: str\n    revision: int\n    execution_generation: int",
			"include_hidden: bool = False",
			"    model: str | None = None", "    cwd: str | None = None", "    reasoning_effort: str | None = None",
		},
		"gateway/session_local.py": {
			"info = {'source': source,\n            'model':", "**({'cwd': policy.cwd} if policy else {}),",
			"return {'reasoning_effort': effort, 'reasoning_effort_wire': wire,",
		},
		"gateway/session_local_inventory.py":           {"info['usage'] = usage", `info["context_length"] = compressor.context_length`},
		"tui_gateway/contracts/canonical.py#inventory": {"    provider: str | None = None", "    context_length: int | None = None"},
		"gateway/session_usage.py": {
			"usage.update(context_used=used, context_max=maximum,",
			"context_percent=max(0, min(100, round(used / maximum * 100))),",
			`for key, source_key in (("avg_latency_s", "avg_latency_s"), ("avg_tps", "tokens_per_s")):`,
			`usage["compressions"] = stats.get("compression_count") or 0`,
			`turn._publish_execution("session.usage", usage_frame(stats))`,
		},
		// The context readout's compaction note: a local viewer gets the raw status kinds.
		"agent/conversation_compression.py": {`status_callback("compacted", COMPACTION_DONE_STATUS)`},
		"gateway/run.py#compaction":         {`_GATEWAY_RAW_TEXT_PLATFORMS = frozenset({"local", "api_server", "webhook", "msgraph_webhook"})`},
		"gateway/run_turn_progress.py#compaction": {
			`return "compacting" if kind == "lifecycle" and is_compaction_progress_status(text) else str(kind)`,
		},
		"gateway/session_settlement_recovery.py": {"complete['usage'] = tui_usage(captured['session_usage'])"},
		"gateway/run_turn_progress.py":           {`payload = {**_tool_lifecycle_payload(call_id, tool_name, args), "is_error": bool(is_error),`},
		"gateway/local_viewer_events.py#tools": {
			`fields = {"duration_s": (time.monotonic() - started) if started is not None else None}`,
			`fields["inline_diff"] = "\n".join(rendered) if rendered else None`,
		},
		"agent/display.py":       {`rendered.append(f"{_diff_ansi()[color]}{raw_line}{_ANSI_RESET}")`},
		"tools/terminal_tool.py": {`body: dict[str, Any] = {"output": "", "exit_code": exit_code, "error": error}`},
		// The device gateway leg's config.set rule (internal/device): these keys change one session,
		// every other key writes the profile.
		"gateway/session_busy_controls.py#device": {
			"return {'key': 'busy', 'value': value, 'scope': 'session'}",
			"return {'key': 'verbose', 'value': current, 'scope': 'session'}",
			"return {'key': 'show_reasoning', 'value': '1' if shown is True else '0', 'scope': 'session'}",
			"return {'key': 'service_tier', 'value': tier or 'normal', 'scope': 'session'}",
			"return {'key': 'yolo', 'value': '1' if enabled else '0', 'scope': 'session'}",
		},
		"gateway/session_tui_config.py#device": {
			"_SESSION_CONTROL_KEYS = frozenset({'busy', 'verbose', 'yolo', 'show_reasoning', 'service_tier', 'model'})",
			"if key == 'fast':\n        return await _set_fast(connection, ref, policy, value)",
			"lambda cfg: _set_display(cfg, key, value))",
			"if params['key'] == 'yolo':\n        if scope != 'global':",
			"value, scope = _word(params.get('value')), params.get('scope', 'session')",
		},
		"gateway/slash_commands_model.py": {
			`_REASONING_DISPLAY_TOGGLES = {"show": True, "on": True, "hide": False, "off": False}`,
			`self._save_gateway_config_key(f"display.platforms.{platform_key}.show_reasoning", show)`,
			`if self._save_gateway_config_key("agent.reasoning_effort", value):`,
		},
		"tui_gateway/contracts/canonical_sessions.py#device": {
			"    row_cwd: str | None = None", "    safe_mode: bool = False", "    ignore_user_config: bool = False",
			"    editor: dict[str, JsonValue] | None = None", "    session_id: str = ''",
		},
		// Sessions the hub starts on a computer run in the plugin placeholder and record the device
		// directory as the row's cwd.
		"hermes_state_local.py#row_cwd": {
			"value = request.get('row_cwd') if isinstance(request, dict) else None",
			"return policy['cwd'] if not isinstance(value, str) else (value or None)",
		},
		// Attachments: images go through image.attach_bytes into the profile's cache/images and
		// ride on prompt.submit; other files go through file.attach; a session without the vision
		// tool cannot read images.
		"gateway/session_controls.py#attach": {
			"handlers['" + gateway.MethodAttachBytes + "'] = partial(attach_bytes, self)",
			"forbidden = set(params) - {'session_id', 'text', 'submission_id', 'input_id', 'queued', 'attachments',",
		},
		"gateway/session_images.py": {
			"set(params) - {'session_id', 'profile', 'content_base64', 'data', 'filename', 'ext'}",
			"directory = Path(connection.authority.profile_id) / 'cache' / 'images'",
			"return {'attached': True, 'path': str(path), 'mime': mime,",
		},
		"gateway/session_ancillary.py": {
			"'" + gateway.MethodFileAttach + "': partial(attach_file, connection)",
			"if (set(params) - {'session_id', 'profile', 'path', 'data_url', 'name', 'staged_upload'}",
			"return {'attached': True, 'name': stored.name, 'path': str(stored),",
		},
		"gateway/session_ingress_media.py": {
			"_ATTACHMENT_MIMES = frozenset({'" + strings.Join(hub.ImageMIMEs(), "', '") + "'})",
			"_ATTACHMENT_LIMIT = " + strconv.Itoa(hub.MaxImages),
			"or any(not isinstance(item, dict) or set(item) != {'path', 'mime'}",
		},
		"gateway/platforms/base.py":                          {`"image", "IMAGE_CACHE_DIR", "cache/images", "image_cache")`},
		"tui_gateway/prompt_attachments.py":                  {"_ATTACH_BYTES_MAX_BYTES = " + strconv.Itoa(hub.MaxImageBytes>>20) + " * 1024 * 1024"},
		"tui_gateway/contracts/canonical_sessions.py#attach": {"    attachments: list[CanonicalAttachment] | None = None", "class CanonicalAttachment(Params):\n    path: str\n    mime: str"},
		"tools/vision_tools.py":                              {`"name": "` + gateway.VisionTool + `",`, "return _should_use_native_vision_fast_path() or check_video_requirements()"},
		"gateway/session_local_inventory.py#tools": {`info["tools"] = _grouped(getattr(agent, "tools", None))`,
			`grouped.setdefault(get_toolset_for_tool(name) or "other", []).append(name)`},
		// Sessions that VS Code (acp) and the CLI started are in the live feed with their source.
		"tui_gateway/contracts/canonical_sessions.py#list": {
			"class CanonicalSessionListRow(Result):\n    session_id: str\n    id: str\n    title: str\n    source: str | None",
		},
		"hermes_state_sessions.py": {`INTERNAL_LISTING_SOURCES = ("kanban", "tool", "oneshot")`},
		"gateway/session_controls.py#live": {
			"if row.get('source') in INTERNAL_LISTING_SOURCES or entry.closed or (",
			"'source': row.get('source'), 'started_at': row.get('started_at'),",
		},
		// The device gateway leg's allowlist (internal/device gatewayRules): where the gateway registers
		// every method it lets through. TestClientMethodsAgainstSource in internal/device checks the
		// other side, the methods the TUI and CLI send.
		"gateway/session_controls.py#device-allow": {
			"'runtime.describe': self.describe", "'setup.status': self.setup_status", "'session.create': self.create",
			"'session.resume': self.resume", "'session.mutate': self.mutate", "'session.detach': self.detach",
			"'session.list': self.list_sessions", "'session.info': self.info", "'prompt.submit': self.submit",
			"'prompt.receipt': self.receipt", "'prompt.cancel': self.cancel", "'prompt.resolve_unknown': self.resolve_unknown",
			"'session.interrupt': self.interrupt", "'approval.respond': self.respond", "'clarify.respond': self.respond_clarify",
			"'slash.exec': self.slash_exec", "'command.dispatch': self.command_dispatch", "'command.observe': self.command_observe",
			"'commands.catalog': self.command_catalog", "'complete.slash': self.slash_completions", "'plugins.list': self.plugins_list",
			"handlers['complete.path'] = partial(complete_path, self)", "handlers['session.stats'] = partial(stats, self)",
			"handlers['session.active_list'] = partial(active_list, self)",
			"handlers['image.attach_bytes'] = partial(attach_bytes, self)", "handlers['clarify.lock'] = partial(clarify_lock, self)",
		},
		"gateway/session_busy_controls.py#device-allow": {
			"'session.steer': partial(correct, connection, verb='steer'),",
			"'session.redirect': partial(correct, connection, verb='redirect'),",
		},
		"gateway/session_config.py#device-allow":          {"'config.get': partial(config_get, connection)"},
		"gateway/session_tui_config.py#device-allow":      {"'config.set': partial(config_set, connection)"},
		"gateway/session_queued_controls.py#device-allow": {"'prompt.replace_queued': partial(replace_queued, connection)"},
		"gateway/session_ancillary.py#device-allow": {
			"'session.control.read': 'control'", "'subagent.list': 'subagents'", "'subagent.tail': 'tail'",
			"for name in ('subagent.steer', 'subagent.interrupt')", "'file.attach': partial(attach_file, connection)",
			"'tools.show': partial(show_tools, connection)",
		},
		"gateway/session_tui_ops.py#device-allow": {
			"'session.export': partial(export_session, connection)",
			"'session.context_breakdown': partial(context_breakdown, connection)",
		},
		"gateway/session_tui_side.py#device-allow": {"'prompt.btw': partial(btw, connection)"},
		"tui_gateway/ws.py#device-allow":           {`if isinstance(req, dict) and req.get("method") == "gateway.ping":`},
		"apps/shared/src/json-rpc-channel.ts":      {"method: 'gateway.ping', params: {} }"},
		// Methods the gateway serves through its legacy handlers, which it lets through only outside
		// these namespaces and for the listed exceptions.
		"tui_gateway/ws_legacy_fallback.py": {
			`_SESSION_NAMESPACES = ("session.", "prompt.", "message.")`,
			`"session.most_recent", "session.active_list", "session.events.stats",`,
		},
		"tui_gateway/methods_session.py#device-allow": {
			`@method("session.most_recent")`, `@method("delegation.status")`, `@_pet_method("pet.cells"`,
			`@_pet_method("pet.gallery"`, `@_pet_method("pet.info.meta"`, `@method("spawn_tree.list")`,
			`@method("spawn_tree.load")`, `@_session_method("terminal.resize")`,
		},
		"tui_gateway/methods_tools.py#device-allow": {
			`@method("system.battery")`, `@_rpc("learning.frames"`, `("detail", "node_detail", ("id",)),`,
		},
		"tui_gateway/methods_prompt.py#device-allow": {`@method("image.detach")`},
		"tui_gateway/methods_i18n.py#device-allow":   {`@method("i18n.catalog")`},
		"tui_gateway/methods_shared_metrics.py#device-allow": {
			`@method("shared_metrics.slash_command")`, `@method("shared_metrics.startup_latency")`,
		},
		// session.mutate operations: the leg allows all but editor_mcp and import.
		"gateway/session_mutations.py#device-ops": {
			"_METADATA = frozenset({'rename', 'archive', 'sidebar'})",
			"_POLICY = frozenset({'model', 'reasoning', 'workspace', 'editor_mcp', 'edit_policy'})",
			"validate_action('import', params['payload'])",
		},
		// The device ACP leg (internal/device acpEditorRules and acpAgentRules): hermes acp runs the
		// gateway ACP agent, whose handlers are these, on ACP SDK 0.9.0.
		"acp_adapter/entry.py#device": {"agent = HermesACPAgent()"},
		"acp_adapter/server.py#device": {
			"if session_manager is None:\n            from acp_adapter.gateway_server import GatewayACPAgent\n            return GatewayACPAgent()",
		},
		"acp_adapter/gateway_server.py#device": {
			"async def initialize(self, **kwargs):", "async def authenticate(self, method_id, **kwargs):",
			"async def new_session(self, cwd, mcp_servers=None, **kwargs):",
			"async def load_session(self, cwd, session_id, mcp_servers=None, **kwargs):",
			"async def resume_session(self, cwd, session_id, mcp_servers=None, **kwargs):",
			"async def cancel(self, session_id, **kwargs):", "async def fork_session(self, cwd, session_id, mcp_servers=None, **kwargs):",
			"async def set_session_model(self, model_id, session_id, **kwargs):",
			"async def set_session_mode(self, mode_id, session_id, **kwargs):",
			"async def set_config_option(self, config_id, session_id, value=None, **kwargs):",
			"async def list_sessions(self, cursor=None, cwd=None, **kwargs):", "async def prompt(self, prompt, session_id, **kwargs):",
			"await self._conn.request_permission(session_id=self._editor_id(session_id),",
			"await self._conn._conn.send_request(\n                    ELICITATION_METHOD,",
		},
		"acp_adapter/elicitation.py": {`ELICITATION_METHOD = "elicitation/create"`},
		"pyproject.toml":             {`acp = ["agent-client-protocol==0.9.0"]`},
		"tui_gateway/contracts/events.py": {
			`event("message.start"`, `event("message.delta"`, `event("reasoning.delta"`, `event("thinking.delta"`,
			`event("message.complete"`, `event("tool.start"`, `event("tool.progress"`, `event("tool.complete"`,
		},
		// The Activity sidebar: the owner's session-scoped verbs, which act only on the named
		// session's children and processes, and the frames and counters it reads.
		"gateway/session_ancillary.py#activity": {
			"'" + gateway.MethodSubagentList + "': 'subagents', '" + gateway.MethodSubagentTail + "': 'tail',",
			"'" + gateway.MethodProcessKill + "': partial(kill_process, connection),",
			"fields = {'session_id', 'profile', 'subagent_id'} | ({'text'} if steer else set())",
			"return {'status': 'queued' if queued else 'rejected', 'subagent_id': sub, 'text': params['text']}",
			"return {'found': child is not None and bool(request_hard_interrupt(child, f'Interrupted via Desktop ({sub})')),",
			"if (set(params) - {'session_id', 'process_id', 'profile'} or not ref.session_id",
			"raise RuntimeStoreError('process_not_found')",
			"return {'subagents': [subagent_row(record) for record in records], 'delegations': []}",
			"'started_at', 'status', 'tool_count', 'last_tool', 'accepting_steer',",
			"return [{'session_id': p.id, 'command': p.command[:200],",
			"'status': 'exited' if p.exited else 'running', 'exit_code': p.exit_code,",
			"'started_epoch': p.started_at, 'exited_at': p.exited_at or None,",
			"'completion_reason': p.completion_reason if p.exited else None,",
			"'output_tail': p.output_buffer[-_PROCESS_TAIL_CHARS:]}",
		},
		"gateway/session_tui_side.py": {
			"return {'" + gateway.MethodPromptBackground + "': partial(background, connection),",
			"live, policy, text, task_id = _admit(connection, ref, params, 'bg')",
			"return {'task_id': task_id}",
			"return _spawn(authority, ref, task_id, '" + gateway.BackgroundCompleteEvent + "', body, holder=holder)",
		},
		"gateway/session_tui_ops.py": {
			"'" + gateway.MethodDelegationPause + "': partial(pause_delegation, connection),",
			"if set(params) - {'paused', 'profile', 'session_id'} or type(paused) is not bool:",
			"return {'paused': set_spawn_paused(paused)}",
		},
		"tui_gateway/ws_legacy_fallback.py#activity": {
			`_PROCESS_GLOBAL = frozenset({"process.stop", "delegation.pause", "reload.mcp", "reload.env", "agents.list"})`,
		},
		"tui_gateway/methods_session.py#activity": {
			`@method("` + gateway.MethodDelegationStatus + `")`,
			`return _ok(rid, {"active": dt.list_active_subagents(), "paused": dt.is_spawn_paused(),`,
		},
		"tui_gateway/methods_subagents.py": {
			`@method("` + gateway.MethodSubagentRoster + `")`,
			`counts.setdefault(sid, {"subagents": 0, "background": 0})[kind] += 1`,
			`return _ok(rid, {"sessions": [{"session_id": sid, **c} for sid, c in counts.items()]})`,
		},
		"tui_gateway/contracts/events.py#activity": {
			"    input_tokens: int | None = None\n    output_tokens: int | None = None",
			`event("` + gateway.BackgroundCompleteEvent + `", SideAgentCompletePayload`,
			`("subagent.complete", "A child finished (status + observability rollup)."),`,
		},
		"tools/delegate_tool_child_run.py": {`"input_tokens": _num(getattr(child, "session_prompt_tokens", 0)),`},
		"gateway/session_usage.py#turn": {
			`last_turn_tokens=max(0, stats["output_tokens"] - base) if isinstance(base, int) else 0,`,
			`"reasoning": stats.get("reasoning_tokens") or 0, "prompt": stats.get("prompt_tokens") or 0,`,
			`return {"usage": tui_usage(stats), **stats}`,
		},
		// Telegram topics: session.info refuses a deleted session with not_found, without a subscribe.
		"gateway/session_controls.py#info": {
			"        if set(params) != {'session_id'}:\n            raise RuntimeStoreError('invalid_params')\n        await self.authority.resolve(self.actor, ref)",
		},
		"gateway/session_authority.py#exists": {"            row = self.db.get_session(ref.session_id)\n            if row is None:\n                raise RuntimeStoreError('not_found')"},
		"gateway/run_turn_runner.py":          {`setattr(agent, TURN_BASE_ATTR, int(getattr(agent, "session_output_tokens", 0) or 0))`},
	}
}

func TestContractAgainstSource(t *testing.T) {
	t.Parallel()
	src := gatewaytest.HermesSource(t, filepath.Join("gateway", "session_contract.py"))
	for key, lines := range contract() {
		file, _, _ := strings.Cut(key, "#")
		b, err := os.ReadFile(filepath.Join(src, file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		for _, line := range lines {
			if !strings.Contains(string(b), line) {
				t.Errorf("%s no longer contains %q", file, line)
			}
		}
	}
}

const (
	kExecGen   = "execution_generation"
	kSessionID = "session_id"
	kRequestID = "request_id"
)

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestParamsUseOnlyKeysTheGatewayAccepts(t *testing.T) {
	t.Parallel()
	listKeys := []string{"include_hidden", "limit", "scope", "title"}
	cases := []struct {
		params  any
		allowed []string
		exact   bool
	}{
		{gateway.ApprovalParams{SessionID: "s", PromptID: "p", Choice: "once"}, []string{"choice", kExecGen, "prompt_id", kSessionID}, true},
		{gateway.ClarifyParams{SessionID: "s", PromptID: "p"}, []string{"answer", kExecGen, "prompt_id", kSessionID}, true},
		{gateway.DetachParams{SessionID: "s", SubscriptionID: "x"}, []string{kSessionID, "subscription_id"}, true},
		{gateway.SubmitParams{SessionID: "s", Text: "t", SubmissionID: "id"}, []string{kSessionID, "submission_id", "text"}, true},
		{gateway.ListParams{Scope: gateway.ScopeLive, Limit: 200}, listKeys, false},
		{gateway.ConfigSetParams{SessionID: "s", Key: "show_reasoning", Value: "on"}, []string{"key", "profile", kSessionID, "value"}, false},
		{gateway.InterruptParams{SessionID: "s"}, []string{kExecGen, kSessionID}, true},
		{gateway.EventsSinceParams{SessionID: "s", ReplayEpoch: "e"}, []string{"last_sequence", "replay_epoch", kSessionID}, true},
		{gateway.MutateParams{SessionID: "s", RequestID: "r", Operation: "rename", Payload: map[string]string{"title": "t"}},
			[]string{"expected_revision", "operation", "payload", kRequestID, kSessionID}, true},
		{gateway.MutateParams{SessionID: "s", ExpectedGeneration: new(int64)},
			[]string{"expected_generation", "expected_revision", "operation", "payload", kRequestID, kSessionID}, true},
		{gateway.ModelPayload{Model: "m", Provider: "p", Confirm: "c"}, []string{"confirm", "model", "provider"}, true},
		{gateway.ModelOptionsParams{SessionID: "s"}, []string{"explicit_only", "include_unconfigured", "profile", "refresh", kSessionID}, false},
		{gateway.ListParams{Scope: gateway.ScopeLive, Limit: 200, IncludeHidden: true}, listKeys, false},
		{gateway.CreateParams{RequestID: "r", Source: gateway.SourceGUI, CWD: "/p", RowCWD: "/home/alice"},
			[]string{"cwd", kRequestID, "row_cwd", "source"}, true},
	}
	for _, c := range cases {
		got := jsonKeys(t, c.params)
		if c.exact && !slices.Equal(got, c.allowed) {
			t.Errorf("%T keys = %v, want exactly %v", c.params, got, c.allowed)
		}
		for _, k := range got {
			if !slices.Contains(c.allowed, k) {
				t.Errorf("%T sends %q, which the gateway refuses", c.params, k)
			}
		}
	}
}

func TestSnapshotInfoSkipsFieldsOfAnotherType(t *testing.T) {
	t.Parallel()
	var i gateway.SnapshotInfo
	err := json.Unmarshal([]byte(`{"model":7,"cwd":"/w","context_length":131072.0,"usage":"x","title":null}`), &i)
	if err != nil || i.Model != nil || i.CWD == nil || *i.CWD != "/w" || i.ContextLength == nil || *i.ContextLength != 131072 ||
		i.Usage != nil || i.Title != nil {
		t.Fatalf("info = %+v, %v", i, err)
	}
}
