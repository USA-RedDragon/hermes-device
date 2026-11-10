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
)

// contract lists source lines of Hermes v0.22.7 this client depends on.
// TestContractAgainstSource checks them in a checkout named by
// HERMES_AGENT_SRC (default ~/.hermes/hermes-agent) and skips without one.
var contract = map[string][]string{
	"gateway/session_contract.py":  {"CANONICAL_GATEWAY_PROTOCOL = '" + gateway.Subprotocol + "'"},
	"hermes_cli/gateway_client.py": {`GATEWAY_WS_TICKET_PREFIX = "` + gateway.TicketPrefix + `"`},
	"tui_gateway/ws.py":            {`"type": "` + gateway.ReadyEvent + `"`, `{"session_authority": True}`},
	"hermes_cli/dashboard_auth/routes.py": {
		`@router.post("/auth/password-login"`, `@router.post("/api/auth/ws-ticket"`, `return {"ticket": ticket`,
		"provider: str = Field(max_length=128)", "username: str", "password: str",
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
	"tui_gateway/contracts/events.py": {
		`event("message.start"`, `event("message.delta"`, `event("reasoning.delta"`, `event("thinking.delta"`,
		`event("message.complete"`, `event("tool.start"`, `event("tool.progress"`, `event("tool.complete"`,
	},
}

func hermesSource(t *testing.T) string {
	t.Helper()
	src := os.Getenv("HERMES_AGENT_SRC")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".hermes", "hermes-agent")
	}
	if _, err := os.Stat(filepath.Join(src, "gateway", "session_contract.py")); err != nil {
		t.Skipf("no Hermes checkout at %s", src)
	}
	return src
}

func sourceVersion(src string) string {
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
	out, err := exec.Command("git", "-C", src, "describe", "--tags", "--exact-match").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestContractAgainstSource(t *testing.T) {
	t.Parallel()
	src := hermesSource(t)
	if v := sourceVersion(src); v != gateway.HermesVersion {
		t.Fatalf("Hermes checkout at %s is %q, this client is pinned to %s", src, v, gateway.HermesVersion)
	}
	for file, lines := range contract {
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

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, _ := json.Marshal(v)
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
	cases := []struct {
		params  any
		allowed []string
		exact   bool
	}{
		{gateway.ApprovalParams{SessionID: "s", PromptID: "p", Choice: "once"}, []string{"choice", "execution_generation", "prompt_id", "session_id"}, true},
		{gateway.DetachParams{SessionID: "s", SubscriptionID: "x"}, []string{"session_id", "subscription_id"}, true},
		{gateway.SubmitParams{SessionID: "s", Text: "t", SubmissionID: "id"}, []string{"session_id", "submission_id", "text"}, true},
		{gateway.ListParams{Scope: "live", Limit: 200}, []string{"include_hidden", "limit", "scope", "title"}, false},
		{gateway.ConfigSetParams{SessionID: "s", Key: "show_reasoning", Value: "on"}, []string{"key", "profile", "session_id", "value"}, false},
		{gateway.InterruptParams{SessionID: "s"}, []string{"execution_generation", "session_id"}, true},
		{gateway.EventsSinceParams{SessionID: "s", ReplayEpoch: "e"}, []string{"last_sequence", "replay_epoch", "session_id"}, true},
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
