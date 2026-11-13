package device

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// The hub records the answers a device gives to approvals, so the hub's
// page and Telegram can say which choice answered one: Hermes does not
// say. A device answers with approval.respond, or with the answer to an
// "approval" server request once it advertised server_requests.

const (
	methodApproval = "approval.respond"
	// serverApproval is the server request that asks a client for an
	// approval (tui_gateway/server.py _emit_approval_request).
	serverApproval = "approval"
	keyPromptID    = "prompt_id"
	keyChoice      = "choice"
	// maxServerApprovals bounds the approval server requests a leg keeps
	// to match answers to.
	maxServerApprovals = 256
	answerTimeout      = 2 * time.Second
)

// answers is the store's answer half, nil when it has none.
func (g *guard) answers() hub.AnswerStore {
	as, _ := g.s.store.(hub.AnswerStore)
	return as
}

// deviceAnswer is the answer choice from this device.
func (g *guard) deviceAnswer(choice string) hub.PromptAnswer {
	return hub.PromptAnswer{Choice: choice, Via: hub.AnsweredOnDevice, Device: g.dev.Name}
}

// recordAnswer records an approval answer of the device. It reports
// whether it recorded it: false also when the approval already has one.
func (g *guard) recordAnswer(promptID, choice string) bool {
	as := g.answers()
	if as == nil || promptID == "" || choice == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(g.l.ctx, answerTimeout)
	defer cancel()
	recorded, err := as.RecordAnswer(ctx, g.dev.BackendID, promptID, g.deviceAnswer(choice))
	if err != nil {
		g.s.log.Warn("Failed to record a device's approval answer", "device", g.dev.ID, "error", err.Error())
	}
	return recorded
}

// recordRespond records the answer of an approval.respond request. It
// reports whether it recorded one. The session check passed already, and
// one with no session_id is refused by the gateway, so it records none.
func (g *guard) recordRespond(method string, params map[string]json.RawMessage) bool {
	sid, ok := rawStringOK(params[keySessionID])
	return method == methodApproval && ok && sid != "" &&
		g.recordAnswer(rawString(params[keyPromptID]), rawString(params[keyChoice]))
}

// approvalAnswered forgets a recorded answer when the gateway did not
// take it.
func (g *guard) approvalAnswered(h *held, f *frame) {
	if !f.isError() {
		var res struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(f.members["result"], &res) != nil || res.Status != "already_resolved" {
			return
		}
	}
	as := g.answers()
	if as == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
	defer cancel()
	if err := as.ForgetAnswer(ctx, g.dev.BackendID, h.requestID, g.deviceAnswer(h.choice)); err != nil {
		g.s.log.Warn("Failed to forget a device's approval answer", "device", g.dev.ID, "error", err.Error())
	}
}

// noteServerApproval remembers which prompt an approval server request
// asks about, so the device's answer to it can be recorded, and forgets
// one the gateway withdrew (request.cancel), whose answer it ignores.
func (g *guard) noteServerApproval(f *frame) {
	if f.Method == "event" {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(f.Params, &ev) == nil && ev.Type == "request.cancel" {
			g.mu.Lock()
			delete(g.srq, ev.Payload.ID)
			g.mu.Unlock()
		}
		return
	}
	id, ok := rawStringOK(f.ID)
	if f.Method != serverApproval || !ok || !strings.HasPrefix(id, serverRequestPrefix) {
		return
	}
	var p struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(f.Params, &p) != nil || p.RequestID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.srq == nil {
		g.srq = map[string]string{}
	}
	if len(g.srq) >= maxServerApprovals {
		return
	}
	g.srq[id] = p.RequestID
}

// serverApprovalAnswered records the device's answer to an approval
// server request. Hermes reads a missing choice as deny.
func (g *guard) serverApprovalAnswered(f *frame) {
	id, _ := rawStringOK(f.ID)
	g.mu.Lock()
	promptID, ok := g.srq[id]
	delete(g.srq, id)
	g.mu.Unlock()
	raw, hasResult := f.members["result"]
	if !ok || !hasResult || isNull(raw) {
		return
	}
	var res struct {
		Choice string `json:"choice"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return
	}
	if res.Choice == "" {
		res.Choice = hub.ChoiceDeny
	}
	g.recordAnswer(promptID, res.Choice)
}
