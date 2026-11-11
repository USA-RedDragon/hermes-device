package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.AnswerStore = (*Store)(nil)

// Choices a claimed outcome stores.
const (
	claimAllowed = "allowed"
	claimDenied  = "denied"
)

// RecordAnswer records an answer to an approval before it goes to Hermes.
// The first answer recorded wins: Hermes takes only the first, so a later
// one is false.
func (s *Store) RecordAnswer(ctx context.Context, backendID, promptID string, a hub.PromptAnswer) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO prompt_answers (backend_id, prompt_id, choice, via, device, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`), backendID, promptID, a.Choice, a.Via, a.Device, s.now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ForgetAnswer drops the answer a recorded, when Hermes did not take it.
func (s *Store) ForgetAnswer(ctx context.Context, backendID, promptID string, a hub.PromptAnswer) error {
	return s.exec(ctx, `DELETE FROM prompt_answers WHERE backend_id = ? AND prompt_id = ? AND choice = ? AND via = ? AND device = ?`,
		backendID, promptID, a.Choice, a.Via, a.Device)
}

// PromptAnswer returns the recorded answer to an approval. A claimed
// outcome is no answer.
func (s *Store) PromptAnswer(ctx context.Context, backendID, promptID string) (hub.PromptAnswer, bool, error) {
	var a hub.PromptAnswer
	err := s.row(ctx, `SELECT choice, via, device FROM prompt_answers WHERE backend_id = ? AND prompt_id = ? AND via <> ?`,
		backendID, promptID, hub.AnsweredElsewhere).Scan(&a.Choice, &a.Via, &a.Device)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.PromptAnswer{}, false, nil
	}
	return a, err == nil, err
}

// ClaimOutcome claims the outcome of an approval that has no recorded
// answer, so one replica shows it, and keeps the outcome.
func (s *Store) ClaimOutcome(ctx context.Context, backendID, promptID string, allowed bool) (bool, error) {
	choice := claimDenied
	if allowed {
		choice = claimAllowed
	}
	return s.RecordAnswer(ctx, backendID, promptID, hub.PromptAnswer{Choice: choice, Via: hub.AnsweredElsewhere})
}

// ClaimedOutcome returns the outcome a replica claimed for an approval.
func (s *Store) ClaimedOutcome(ctx context.Context, backendID, promptID string) (hub.ApprovalOutcome, bool, error) {
	var choice string
	err := s.row(ctx, `SELECT choice FROM prompt_answers WHERE backend_id = ? AND prompt_id = ? AND via = ?`,
		backendID, promptID, hub.AnsweredElsewhere).Scan(&choice)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.ApprovalOutcome{}, false, nil
	}
	if err != nil {
		return hub.ApprovalOutcome{}, false, err
	}
	return hub.ApprovalOutcome{PromptID: promptID, Allowed: choice == claimAllowed, Via: hub.AnsweredElsewhere}, true, nil
}
