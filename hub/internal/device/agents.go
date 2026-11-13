package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AgentRequest is one agent message request a tool call of the plugin's
// device-agents toolset put on its bus (docs/protocol.md section 12).
type AgentRequest struct {
	ID     string          `json:"id"`
	Op     string          `json:"op"`
	Caller AgentCaller     `json:"caller"`
	Args   json.RawMessage `json:"args"`
}

// AgentCaller is the agent whose tool call made a request, as the plugin
// knows it from the call itself: its session, and for a subagent its id
// and parent. Tree is the live subagents of its delegation tree.
type AgentCaller struct {
	SessionID        string          `json:"session_id"`
	RootSessionID    string          `json:"root_session_id"`
	SubagentID       string          `json:"subagent_id"`
	ParentSessionID  string          `json:"parent_session_id"`
	ParentSubagentID string          `json:"parent_subagent_id"`
	Tree             []AgentSubagent `json:"tree"`
	// CronJob is set when the caller runs for a scheduled job. An older
	// plugin leaves it out, and the hub reads the job id from the cron
	// session id instead (see agents.CronJobOf).
	CronJob *AgentCronJob `json:"cron_job,omitempty"`
}

// AgentCronJob names the scheduled job whose run made a request.
type AgentCronJob struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AgentSubagent is one live subagent of a delegation tree.
type AgentSubagent struct {
	SubagentID string `json:"subagent_id"`
	SessionID  string `json:"session_id"`
	ParentID   string `json:"parent_id"`
	Depth      int    `json:"depth"`
	Goal       string `json:"goal"`
}

// AgentDelivery asks the plugin to deliver a message into a running agent.
type AgentDelivery struct {
	Mode       string `json:"mode"`
	MessageID  string `json:"message_id"`
	SessionID  string `json:"session_id"`
	SubagentID string `json:"subagent_id,omitempty"`
	Text       string `json:"text"`
}

// Plugin delivery outcomes.
const (
	DeliverySteered    = "steered"
	DeliveryQueued     = "queued"
	DeliveryNotRunning = "not_running"
	DeliveryGone       = "gone"
	DeliveryFull       = "full"
)

// AgentRequests waits up to wait for agent message requests. Only ctx
// bounds it, so give ctx a deadline beyond wait.
func (p *Plugin) AgentRequests(ctx context.Context, wait time.Duration) ([]AgentRequest, error) {
	var out struct {
		Requests []AgentRequest `json:"requests"`
	}
	send := p.r.Request
	if s, ok := p.r.(slowRequester); ok {
		send = s.RequestSlow
	}
	q := "/agents/requests?wait=" + strconv.FormatFloat(wait.Seconds(), 'f', 1, 64)
	return out.Requests, p.callWith(ctx, send, http.MethodGet, q, nil, &out)
}

// AgentAnswer answers a request: with result, or with an error code and
// message when code is not "".
func (p *Plugin) AgentAnswer(ctx context.Context, id string, result any, code, message string) error {
	body := map[string]any{"result": result}
	if code != "" {
		body = map[string]any{"error": map[string]string{"code": code, "message": message}}
	}
	return p.call(ctx, http.MethodPost, "/agents/requests/"+url.PathEscape(id), body, nil)
}

// AgentDeliver delivers a message into a running agent and returns the
// plugin's outcome.
func (p *Plugin) AgentDeliver(ctx context.Context, d AgentDelivery) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	return out.Status, p.call(ctx, http.MethodPost, "/agents/deliver", d, &out)
}
