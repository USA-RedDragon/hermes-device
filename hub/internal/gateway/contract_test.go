package gateway_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// contract lists source lines of Hermes v0.22.7 this client depends on, by
// file. A "#name" suffix lets one file have more than one entry.
// TestContractAgainstSource checks them in a checkout named by
// HERMES_AGENT_SRC (default ~/.hermes/hermes-agent) and skips without one.
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
			"    admission_id: str\n    execution_generation: int\n",
		},
		"gateway/run_api.py": {
			"if reason == 'none' or ws.headers.get('origin'):",
			"or not ws.client or ws.client.host not in {'127.0.0.1', '::1'}):",
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
			"if set(params) - {'end_reason'} != {'session_id', 'subscription_id'}:",
			"if ref.session_id not in self.subscriptions:\n            raise RuntimeStoreError('permission_denied')",
			"request_id = params.get('submission_id') or params.get('input_id')",
		},
		"gateway/session_events.py": {
			"'type': event_type, 'session_id': self._key,", "'payload': deepcopy(payload), 'replay_epoch': self.epoch}}",
		},
		"gateway/session_pending_controls.py": {
			"choices = ['" + gateway.ChoiceOnce + "', '" + gateway.ChoiceDeny + "']",
			"prompt = {'kind': 'approval', 'prompt_id': prompt_id,", "event_type='approval.request'",
		},
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
		},
		"gateway/session_settlement_recovery.py": {"complete['usage'] = tui_usage(captured['session_usage'])"},
		"gateway/run_turn_progress.py":           {`payload = {**_tool_lifecycle_payload(call_id, tool_name, args), "is_error": bool(is_error),`},
		"gateway/local_viewer_events.py#tools": {
			`fields = {"duration_s": (time.monotonic() - started) if started is not None else None}`,
			`fields["inline_diff"] = "\n".join(rendered) if rendered else None`,
		},
		"agent/display.py":       {`rendered.append(f"{_diff_ansi()[color]}{raw_line}{_ANSI_RESET}")`},
		"tools/terminal_tool.py": {`body: dict[str, Any] = {"output": "", "exit_code": exit_code, "error": error}`},
		"tui_gateway/contracts/events.py": {
			`event("message.start"`, `event("message.delta"`, `event("reasoning.delta"`, `event("thinking.delta"`,
			`event("message.complete"`, `event("tool.start"`, `event("tool.progress"`, `event("tool.complete"`,
		},
	}
}

func hermesSource(t *testing.T) string {
	t.Helper()
	src := os.Getenv("HERMES_AGENT_SRC")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".hermes", "hermes-agent")
	}
	if _, err := os.Stat(filepath.Join(src, "gateway", "session_contract.py")); err != nil { //nolint:gosec // a test reads the checkout it is told to
		t.Skipf("no Hermes checkout at %s", src)
	}
	return src
}

func sourceVersion(t *testing.T, src string) string {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(src, "install-stamp.json")); err == nil {
		var stamp struct {
			BaseVersion string `json:"baseVersion"`
			Distance    int    `json:"distance"`
			Dirty       bool   `json:"dirty"`
		}
		if json.Unmarshal(b, &stamp) == nil && stamp.BaseVersion != "" && stamp.Distance == 0 && !stamp.Dirty {
			return "v" + stamp.BaseVersion
		}
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", src, "describe", "--tags", "--exact-match").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestContractAgainstSource(t *testing.T) {
	t.Parallel()
	src := hermesSource(t)
	if v := sourceVersion(t, src); v != gateway.HermesVersion {
		t.Fatalf("Hermes checkout at %s is %q, this client is pinned to %s", src, v, gateway.HermesVersion)
	}
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

const kSessionID = "session_id"

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
		{gateway.ApprovalParams{SessionID: "s", PromptID: "p", Choice: "once"}, []string{"choice", "execution_generation", "prompt_id", kSessionID}, true},
		{gateway.DetachParams{SessionID: "s", SubscriptionID: "x"}, []string{kSessionID, "subscription_id"}, true},
		{gateway.SubmitParams{SessionID: "s", Text: "t", SubmissionID: "id"}, []string{kSessionID, "submission_id", "text"}, true},
		{gateway.ListParams{Scope: gateway.ScopeLive, Limit: 200}, listKeys, false},
		{gateway.ConfigSetParams{SessionID: "s", Key: "show_reasoning", Value: "on"}, []string{"key", "profile", kSessionID, "value"}, false},
		{gateway.InterruptParams{SessionID: "s"}, []string{"execution_generation", kSessionID}, true},
		{gateway.EventsSinceParams{SessionID: "s", ReplayEpoch: "e"}, []string{"last_sequence", "replay_epoch", kSessionID}, true},
		{gateway.MutateParams{SessionID: "s", RequestID: "r", Operation: "rename", Payload: map[string]string{"title": "t"}},
			[]string{"expected_revision", "operation", "payload", "request_id", kSessionID}, true},
		{gateway.MutateParams{SessionID: "s", ExpectedGeneration: new(int64)},
			[]string{"expected_generation", "expected_revision", "operation", "payload", "request_id", kSessionID}, true},
		{gateway.ModelPayload{Model: "m", Provider: "p", Confirm: "c"}, []string{"confirm", "model", "provider"}, true},
		{gateway.ModelOptionsParams{SessionID: "s"}, []string{"explicit_only", "include_unconfigured", "profile", "refresh", kSessionID}, false},
		{gateway.ListParams{Scope: gateway.ScopeLive, Limit: 200, IncludeHidden: true}, listKeys, false},
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
