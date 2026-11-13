package hub

import "context"

// Where an approval got its answer. Hermes does not say which choice
// answered an approval, so the hub records the answers that pass through
// it, and works out the others from the tool call that the approval held.
const (
	// AnsweredInHub is an answer given on the hub's page.
	AnsweredInHub = "hub"
	// AnsweredInTelegram is an answer given with the Telegram bot.
	AnsweredInTelegram = "telegram"
	// AnsweredOnDevice is an answer given in a client of a paired device,
	// such as the Hermes TUI through talaria.
	AnsweredOnDevice = "device"
	// AnsweredElsewhere is an answer the hub did not see: from a client
	// that talks to Hermes directly.
	AnsweredElsewhere = "other"
)

// KindApprovalOutcome is a hub-made event that says what became of an
// approval after its approval.settled: data is an ApprovalOutcome.
const KindApprovalOutcome = "approval.outcome"

// NoticeOutcome tells front ends what became of approvals that a
// NoticeSettled settled: Outcome says it.
const NoticeOutcome = "outcome"

// PromptAnswer is an answer to an approval that passed through the hub.
type PromptAnswer struct {
	Choice string
	// Via is AnsweredInHub, AnsweredInTelegram or AnsweredOnDevice.
	Via string
	// Device is the device's name, for AnsweredOnDevice.
	Device string
}

// ApprovalOutcome is what became of an approval.
type ApprovalOutcome struct {
	PromptID string `json:"prompt_id"`
	// Allowed is true when the approval let the tool run.
	Allowed bool `json:"allowed"`
	// Choice is the choice that answered, "" when the hub worked the
	// outcome out from the tool call and cannot tell the scope.
	Choice string `json:"choice,omitempty"`
	// Via says where the answer came from: one of the Answered* values.
	Via string `json:"via"`
	// Device is the device's name, for AnsweredOnDevice.
	Device string `json:"device,omitempty"`
}

// Outcome is the outcome of an approval that a recorded answer settled.
func (a PromptAnswer) Outcome(promptID string) ApprovalOutcome {
	return ApprovalOutcome{PromptID: promptID, Allowed: a.Choice != ChoiceDeny, Choice: a.Choice, Via: a.Via, Device: a.Device}
}

// AnswerStore records the answers to approvals that pass through the hub,
// so every replica can tell what became of them.
type AnswerStore interface {
	// RecordAnswer records an answer before it goes to Hermes. The first
	// answer wins, as in Hermes: recorded is false when the approval
	// already has one.
	RecordAnswer(ctx context.Context, backendID, promptID string, a PromptAnswer) (recorded bool, err error)
	// ForgetAnswer drops a recorded answer a that Hermes did not take, and
	// no other answer.
	ForgetAnswer(ctx context.Context, backendID, promptID string, a PromptAnswer) error
	// PromptAnswer returns the recorded answer to a prompt.
	PromptAnswer(ctx context.Context, backendID, promptID string) (PromptAnswer, bool, error)
	// ClaimOutcome claims the outcome of an approval with no recorded
	// answer for one replica to show, and keeps it: false when an answer
	// is recorded or another replica claimed it.
	ClaimOutcome(ctx context.Context, backendID, promptID string, allowed bool) (bool, error)
	// ClaimedOutcome returns the outcome a replica claimed.
	ClaimedOutcome(ctx context.Context, backendID, promptID string) (ApprovalOutcome, bool, error)
}

type answerViaKey struct{}

// WithAnsweredVia marks answers given with ctx as coming from via, one of
// the Answered* values. The default is AnsweredInHub.
func WithAnsweredVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, answerViaKey{}, via)
}

// AnsweredVia is where answers given with ctx come from.
func AnsweredVia(ctx context.Context) string {
	if v, ok := ctx.Value(answerViaKey{}).(string); ok && v != "" {
		return v
	}
	return AnsweredInHub
}
