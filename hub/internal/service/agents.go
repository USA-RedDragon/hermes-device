package service

import (
	"context"
	"errors"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var (
	// errAgentHeld is a chat whose messages the hub holds for a
	// restarting backend.
	errAgentHeld = errors.New("hermes is restarting; send the message again in a minute")
	// ErrAgentYolo is a chat that runs commands without approvals: an
	// agent message there would run with no approval the user sees.
	ErrAgentYolo = errors.New("the target chat runs without approvals (yolo), so it takes no messages from agents")
)

// SessionsOf lists one backend's sessions without a user check. It is for
// the hub's own agent messages, which never come from a user.
func (s *Service) SessionsOf(ctx context.Context, backendID string) ([]hub.Session, error) {
	b, ok := s.backends[backendID]
	if !ok {
		return nil, hub.ErrUnknownBackend
	}
	return b.list(ctx)
}

// SubmitAgent sends an agent message as the next input of a session, under
// the submission ID id, without a user check. It refuses while the
// session has held prompts, so it never passes a user's message, and for a
// session in yolo mode (ErrAgentYolo), or whose mode it cannot read.
//
// With steer, a message that waits behind a running turn is handed to that
// turn as Hermes's steer, as a user's message typed mid-turn is: it
// reaches the model with the turn's next tool result. The answer says what
// happened: hub.AgentSubmitStarted (the session was idle and runs it),
// hub.AgentSubmitSteered or hub.AgentSubmitQueued (it waits for the turn
// to end: no steer was asked for, or Hermes refused it, which leaves the
// input queued).
func (s *Service) SubmitAgent(ctx context.Context, backendID, sessionID, id, text string, steer bool) (string, error) {
	b, ok := s.backends[backendID]
	if !ok {
		return "", hub.ErrUnknownBackend
	}
	receipt, err := s.submitAgent(ctx, b, sessionID, id, text)
	if err != nil {
		return "", err
	}
	switch {
	case receipt.Status == gateway.AdmissionStarted:
		return hub.AgentSubmitStarted, nil
	case steer && receipt.Status == gateway.AdmissionQueued && b.steerOrFind(context.WithoutCancel(ctx), sessionID, receipt):
		return hub.AgentSubmitSteered, nil
	}
	return hub.AgentSubmitQueued, nil
}

// submitAgent is SubmitAgent's prompt.submit, with the session's gate
// held; the steer waits until the gate is free again, as a user's does.
func (s *Service) submitAgent(ctx context.Context, b *backend, sessionID, id, text string) (gateway.Receipt, error) {
	release := b.gate(sessionID)
	defer release()
	if b.heldCount(sessionID) > 0 {
		return gateway.Receipt{}, errAgentHeld
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return gateway.Receipt{}, refusal(err)
	}
	defer ls.unpin()
	var yolo struct {
		Value string `json:"value"`
	}
	if err := b.read(ctx, ls, gateway.MethodConfigGet, gateway.ConfigGetParams{SessionID: ls.id, Key: gateway.KeyYolo}, &yolo); err != nil {
		return gateway.Receipt{}, refusal(err)
	}
	if yolo.Value != "0" {
		return gateway.Receipt{}, ErrAgentYolo
	}
	var receipt gateway.Receipt
	if err := b.call(ctx, ls, gateway.MethodPromptSubmit, gateway.SubmitParams{SessionID: ls.id, Text: text, SubmissionID: id}, &receipt); err != nil {
		return gateway.Receipt{}, err
	}
	return receipt, nil
}
