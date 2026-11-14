package telegram

import (
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// The smart approval card's "this source in this chat" choice gets words,
// not its token.
func TestApprovalSourceChoiceWords(t *testing.T) {
	t.Parallel()

	if got := approvalLabel(hub.ChoiceSource); got != "This source in this chat" {
		t.Fatalf("label = %q", got)
	}
	if got := approvalVerdict(hub.ChoiceSource); got != "Approved code from this source in this chat" {
		t.Fatalf("verdict = %q", got)
	}
	o := hub.ApprovalOutcome{Allowed: true, Choice: hub.ChoiceSource, Via: hub.AnsweredInHub}
	if got := outcomeHead(o); got != "Allowed for this source in this chat (in the hub)" {
		t.Fatalf("outcome = %q", got)
	}
}
