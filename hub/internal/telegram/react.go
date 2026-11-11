package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The reactions the bot sets on the message that started a turn. A bot
// may set only the emoji of ReactionTypeEmoji (no ✅, ⚠️ or ❌), and one
// at a time, which replaces the one before.
const (
	// reactWorking: Hermes picked the message up.
	reactWorking = "👀"
	// reactDone: the turn ended well.
	reactDone = "👍"
	// reactNeedsYou: an approval or a question waits for the user.
	reactNeedsYou = "🙏"
	// reactFailed: the turn failed.
	reactFailed = "👎"
	// reactStopped: the turn was interrupted.
	reactStopped = "👌"
)

// typingGap is how often the bot shows "typing" again while a turn runs:
// Telegram shows it for 5 seconds at most.
const typingGap = 4 * time.Second

// setReaction sets the bot's reaction on the message of the running turn,
// once per change. A chat that refuses reactions, such as a group that
// allows only some, gets none from then on.
func (r *renderer) setReaction(ctx context.Context, emoji string) {
	if r.reactTo == 0 || r.reacted == emoji || r.noReact {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	err := r.api.SetReaction(cctx, r.to.chat, r.reactTo, emoji)
	var apiErr *APIError
	switch {
	case err == nil:
		r.reacted = emoji
	case errors.As(err, &apiErr) && refusesReactions(apiErr):
		r.logger.Info("Telegram refused a reaction, setting none in this chat", "error", err)
		r.noReact = true
	default:
		r.logger.Debug("Failed to set a reaction", "error", err)
	}
}

// refusesReactions reports whether Telegram refused a reaction because the
// chat takes none of the bot's, not because of the one message.
func refusesReactions(e *APIError) bool {
	d := strings.ToUpper(e.Description)
	return e.Code == http.StatusForbidden || strings.Contains(d, "REACTION_INVALID") || strings.Contains(d, "REACTIONS_TOO_MANY") ||
		strings.Contains(d, "REACTION_EMPTY")
}

// reactNeeds shows on the turn's message whether a prompt waits for the
// user.
func (r *renderer) reactNeeds(ctx context.Context) {
	if !r.busy {
		return
	}
	if len(r.prompts) > 0 {
		r.setReaction(ctx, reactNeedsYou)
		return
	}
	r.setReaction(ctx, reactWorking)
}

// endReaction shows how the turn ended and forgets its message.
func (r *renderer) endReaction(ctx context.Context, emoji string) {
	r.setReaction(ctx, emoji)
	r.reactTo, r.reacted = 0, ""
}

// typingDue is how long until "typing" is due again, false when no turn
// runs.
func (r *renderer) typingDue() (time.Duration, bool) {
	if !r.busy || r.noTyping || len(r.prompts) > 0 {
		return 0, false
	}
	return r.typedAt.Add(typingGap).Sub(r.now()), true
}

// typingTick shows "typing" in the chat or topic while a turn runs.
func (r *renderer) typingTick(ctx context.Context) {
	if d, ok := r.typingDue(); !ok || d > 0 {
		return
	}
	r.typedAt = r.now()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	err := r.api.SendTyping(cctx, r.to.chat, r.to.thread)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Code == http.StatusBadRequest || apiErr.Code == http.StatusForbidden) {
		r.logger.Info("Telegram refused the typing action, showing none in this chat", "error", err)
		r.noTyping = true
	} else if err != nil {
		r.logger.Debug("Failed to show typing", "error", err)
	}
}
