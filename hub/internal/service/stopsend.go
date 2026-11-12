package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.Redirector = (*Service)(nil)

// Outcomes of an admission's message.complete, and what StopAndSend
// records when one starts as its own turn.
const (
	outcomeSteered   = "steered"
	outcomeCancelled = "cancelled"
	// outcomeInterrupted is the settled row of a steer whose turn was
	// stopped; Hermes reports it as cancelled, and the hub takes either.
	outcomeInterrupted = "interrupted"
	settledStarted     = "started"
)

// maxSettled bounds the outcomes a session remembers for StopAndSend.
const maxSettled = 256

// trackSettled records what became of each admission, for StopAndSend,
// and adds the submission ID to the end of a steer, so a viewer can tell
// which of its messages a stopped turn never read. It returns ev's
// payload. ls.mu is held.
func (ls *live) trackSettled(ev gateway.Event) json.RawMessage {
	if ev.AdmissionID == "" || (ev.Type != hub.KindMessageStart && ev.Type != hub.KindMessageComplete) {
		return ev.Payload
	}
	if ls.outcomes == nil {
		ls.outcomes = map[string]string{}
	}
	for id := range ls.outcomes {
		if len(ls.outcomes) < maxSettled {
			break
		}
		delete(ls.outcomes, id)
	}
	sub := ls.subs[ev.AdmissionID]
	delete(ls.subs, ev.AdmissionID)
	if ev.Type == hub.KindMessageStart {
		ls.outcomes[ev.AdmissionID] = settledStarted
		return ev.Payload
	}
	var p map[string]any
	if json.Unmarshal(ev.Payload, &p) != nil || p == nil {
		return ev.Payload
	}
	outcome, _ := p["outcome"].(string)
	ls.outcomes[ev.AdmissionID] = outcome
	if _, turn := p["status"]; turn || sub == "" || (outcome != outcomeSteered && outcome != outcomeCancelled) {
		return ev.Payload
	}
	p[hub.StartSubmissionID] = sub
	p[hub.StartAdmissionID] = ev.AdmissionID
	out, err := json.Marshal(p)
	if err != nil {
		return ev.Payload
	}
	return out
}

// runningAdmission is the admission of the running turn among a pending list: the
// one the hub saw start, else the first started one, since a steer only
// takes an input queued after the turn's own. "" when none started.
func runningAdmission(ps []gateway.PendingAdmission, known string) string {
	if known != "" {
		return known
	}
	turn, seq := "", int64(0)
	for _, p := range ps {
		if p.Status == gateway.AdmissionStarted && (turn == "" || p.Sequence < seq) {
			turn, seq = p.AdmissionID, p.Sequence
		}
	}
	return turn
}

// steering reports whether p is an input Hermes hands to the running turn
// as a steer: started, and not the turn's own.
func steering(p gateway.PendingAdmission, turn string) bool {
	return p.Status == gateway.AdmissionStarted && p.AdmissionID != turn
}

// findInput finds a pending input by its admission or submission ID.
// ls.mu is held.
func (ls *live) findInput(id string) (gateway.PendingAdmission, bool) {
	if p, ok := ls.inputs[id]; ok {
		return p, true
	}
	for _, p := range ls.inputs {
		if p.InputID == id {
			return p, true
		}
	}
	return gateway.PendingAdmission{}, false
}

// StopAndSend stops the running turn and makes a waiting input the next
// turn. A queued input runs once the turn stops, as Hermes runs its queue.
// A steer Hermes holds for the running turn ends with the turn (Hermes
// drops a steer on a stop), so the hub waits for that end and sends its
// text again as a new prompt, at the end of the queue; a steer that went
// back to the queue, or that the turn read and finished with, needs
// nothing more. The wait and the new prompt do not end with ctx: the stop
// already dropped the steer.
func (s *Service) StopAndSend(ctx context.Context, user, backendID, sessionID, inputID string) (hub.PromptResult, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.PromptResult{}, err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return hub.PromptResult{}, refusal(err)
	}
	defer ls.unpin()
	ls.mu.Lock()
	p, ok := ls.findInput(inputID)
	inputs := make([]gateway.PendingAdmission, 0, len(ls.inputs))
	for _, in := range ls.inputs {
		inputs = append(inputs, in)
	}
	turn := runningAdmission(inputs, ls.turnAdmission)
	if ok {
		delete(ls.outcomes, p.AdmissionID)
	}
	ls.mu.Unlock()
	switch {
	case !ok || (p.Status != gateway.AdmissionQueued && p.Status != gateway.AdmissionStarted):
		return hub.PromptResult{}, hub.ErrUnknownInput
	case p.AdmissionID == turn:
		return hub.PromptResult{}, fmt.Errorf("%w: that message is the turn that runs", hub.ErrBusy)
	}
	id := p.InputID
	if id == "" || gateway.Producer(id) != "" {
		id = p.AdmissionID
	}
	if err := s.Interrupt(ctx, user, backendID, sessionID); err != nil && !idle(err) {
		return hub.PromptResult{}, refusal(err)
	}
	if !steering(p, turn) {
		return hub.PromptResult{ID: id}, nil
	}
	ctx = context.WithoutCancel(ctx)
	outcome, err := ls.waitSettled(ctx, p.AdmissionID, s.opts.SteerSettle)
	if err != nil {
		return hub.PromptResult{}, err
	}
	if outcome != outcomeCancelled && outcome != outcomeInterrupted {
		return hub.PromptResult{ID: id}, nil
	}
	return s.Prompt(hub.WithSubmissionID(ctx, againID(p)), user, backendID, sessionID, p.Text)
}

// againID is the submission ID a stopped steer is sent again under: the
// same for every try, so a retry never runs it twice.
func againID(p gateway.PendingAdmission) string {
	return hub.SubmissionIDFor("stop-and-send:" + p.AdmissionID)
}

// idle reports a stop refused because no turn runs any more: the turn
// ended on its own, and what waited for it runs now.
func idle(err error) bool {
	var r *gateway.RPCError
	return errors.As(err, &r) && (r.Message == gateway.ReasonStaleGeneration || r.Message == gateway.ReasonSessionIdle)
}

// waitSettled waits until a steer Hermes held for the running turn ends:
// with the outcome its message.complete reports, or "started" when it
// went back to the queue and runs as its own turn.
func (ls *live) waitSettled(ctx context.Context, admissionID string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		ls.mu.Lock()
		outcome, ok := ls.outcomes[admissionID]
		p, pending := ls.inputs[admissionID]
		ls.mu.Unlock()
		switch {
		case ok:
			return outcome, nil
		case pending && p.Status == gateway.AdmissionQueued:
			return gateway.AdmissionQueued, nil
		case time.Now().After(deadline):
			return "", errors.New("the turn did not stop in time; send the message again if it does not run")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// sub is the submission ID a page sent an input under, "" for an input of
// Hermes's own.
func sub(p gateway.PendingAdmission) string {
	if p.InputID == "" || gateway.Producer(p.InputID) != "" {
		return ""
	}
	return p.InputID
}
