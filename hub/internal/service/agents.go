package service

import (
	"context"
	"errors"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
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
func (s *Service) SubmitAgent(ctx context.Context, backendID, sessionID, id, text string) error {
	b, ok := s.backends[backendID]
	if !ok {
		return hub.ErrUnknownBackend
	}
	release := b.gate(sessionID)
	defer release()
	if b.heldCount(sessionID) > 0 {
		return errAgentHeld
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return refusal(err)
	}
	defer ls.unpin()
	var yolo struct {
		Value string `json:"value"`
	}
	if err := b.call(ctx, ls, gateway.MethodConfigGet, gateway.ConfigGetParams{SessionID: ls.id, Key: gateway.KeyYolo}, &yolo); err != nil {
		return refusal(err)
	}
	if yolo.Value != "0" {
		return ErrAgentYolo
	}
	return b.call(ctx, ls, gateway.MethodPromptSubmit, gateway.SubmitParams{SessionID: ls.id, Text: text, SubmissionID: id}, nil)
}
