package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// draftGap is the least time between two drafts of a turn.
const draftGap = time.Second

// draftKeep is how often the bot sends a draft again when nothing changed
// it: Telegram drops a draft after about 30 seconds.
const draftKeep = 10 * time.Second

// workingHTML is the status line of a turn that streams into a draft. The
// draft shows what runs now; the status line changes only when the turn
// ends.
const workingHTML = "<b>Working…</b>"

// draftState is the draft of the running turn: a message that Telegram
// shows while it is written (sendMessageDraft) and drops when the bot
// sends a message.
type draftState struct {
	id int64
	at time.Time
	// next is when Telegram takes a draft again after a 429.
	next  time.Time
	dirty bool
	// shown is the HTML of the draft that shows, "" when none shows.
	shown string
	// placeholder is set while the draft is the empty "Thinking…" one.
	placeholder bool
}

// draftID is a new draft ID: random, so that turns in different topics of
// one private chat never share one.
func draftID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:])>>2) + 1
}

// draftUnsupported reports whether Telegram refused a draft because this
// chat or server cannot take drafts at all, rather than this one frame.
func draftUnsupported(e *APIError) bool {
	if e.Code == http.StatusNotFound {
		return true
	}
	d := strings.ToUpper(e.Description)
	for _, s := range []string{"METHOD NOT FOUND", "PEER", "CHAT NOT FOUND", "THREAD", "NOT SUPPORTED", "PRIVATE CHAT"} {
		if strings.Contains(d, s) {
			return true
		}
	}
	return false
}

// newDraft starts the draft of a new turn.
func (r *renderer) newDraft() {
	r.draft = draftState{id: draftID(), dirty: true}
}

// markDraft notes that the draft changed and sends it when its gap
// allows.
func (r *renderer) markDraft(ctx context.Context) {
	if !r.drafts {
		return
	}
	if r.draft.id == 0 {
		r.newDraft()
	}
	r.draft.dirty = true
	r.flushDraft(ctx)
}

// draftLost notes that the bot sent a message, which drops the draft from
// the chat, so the next flush sends it again.
func (r *renderer) draftLost() {
	if r.drafts && r.draft.id != 0 && r.busy {
		r.draft.shown, r.draft.dirty = "", true
	}
}

// draftHTML is the draft: the end of the reply so far, else the status
// line of the running turn. It is "" before either exists, which Telegram
// shows as a "Thinking…" placeholder. The reply goes without the status
// line: Telegram animates a draft that only grows, but redraws all of it
// when its start changes, and the status line's clock changes every frame.
func (r *renderer) draftHTML() string {
	_, text := hub.MediaDirectives(r.answer.String())
	text = strings.TrimSpace(text)
	if text == "" {
		if r.turn == nil {
			return ""
		}
		return "<i>" + r.statusHTML(false) + "</i>"
	}
	room := MaxMessageLen
	chunks := RenderMarkdown(text, room)
	if len(chunks) == 0 {
		return ""
	}
	body := chunks[len(chunks)-1]
	if len(chunks) > 1 {
		body = "…\n" + body
		if utf16Len(body) > room {
			body = chunks[len(chunks)-1]
		}
	}
	return body
}

// flushDraft sends the draft when it changed and its gap allows.
func (r *renderer) flushDraft(ctx context.Context) {
	d := &r.draft
	now := r.now()
	if !r.drafts || !d.dirty || d.id == 0 || now.Before(d.at.Add(draftGap)) || now.Before(d.next) {
		return
	}
	html := r.draftHTML()
	d.dirty = false
	if html == d.shown && html != "" && now.Sub(d.at) < draftKeep {
		return
	}
	if html == "" && (r.noEmptyDraft || d.placeholder && now.Sub(d.at) < draftKeep) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	err := r.api.SendDraft(cctx, r.to.chat, r.to.thread, d.id, html)
	cancel()
	d.at = now
	var apiErr *APIError
	switch {
	case err == nil:
		d.shown, d.placeholder = html, html == ""
	case errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests:
		d.next, d.dirty = now.Add(max(apiErr.RetryAfter, time.Second)), true
	case html == "":
		// An empty draft is the "Thinking…" placeholder of Bot API 10.0;
		// an older server refuses it, and gets none again.
		d.placeholder, r.noEmptyDraft = true, true
	case errors.As(err, &apiErr) && draftUnsupported(apiErr):
		r.logger.Info("Telegram refused a draft, editing the status line instead", "error", err)
		r.stopDrafts(ctx)
	case errors.As(err, &apiErr):
		// A bad frame, such as half-written Markdown that Telegram cannot
		// parse: the next change sends the draft again.
		r.logger.Debug("Telegram refused a draft frame", "error", err)
	default:
		r.logger.Debug("Failed to send a draft", "error", err)
		d.dirty = true
	}
}

// stopDrafts turns drafts off for this chat, so the turn shows on its
// status line, which the bot edits.
func (r *renderer) stopDrafts(ctx context.Context) {
	r.drafts = false
	r.draft = draftState{}
	if t := r.turn; t != nil {
		t.msg.dirty = true
		r.flushDue(ctx)
	}
}

// draftDue is how long until the draft is due, false when none is.
func (r *renderer) draftDue() (time.Duration, bool) {
	d := r.draft
	if !r.drafts || d.id == 0 || !r.busy || len(r.prompts) > 0 {
		return 0, false
	}
	at := d.at.Add(draftKeep)
	if d.dirty {
		at = d.at.Add(draftGap)
	}
	if d.next.After(at) {
		at = d.next
	}
	return at.Sub(r.now()), true
}

// draftTick sends the draft when it is due: when it changed, or to keep it
// in the chat while the turn runs.
func (r *renderer) draftTick(ctx context.Context) {
	if !r.drafts || r.draft.id == 0 || !r.busy || len(r.prompts) > 0 {
		return
	}
	if r.now().Sub(r.draft.at) >= draftKeep {
		r.draft.dirty = true
	}
	r.flushDraft(ctx)
}
