package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	cmdPrompts = "prompts"
	// cbSavedPrompt is a button of /prompts: "pl:<gen>:<i>".
	cbSavedPrompt = "pl"
	// maxPromptButtons bounds the buttons /prompts shows.
	maxPromptButtons = 50
	// promptHead is room for the line above a prompt's text.
	promptHead = 120
)

// PromptStore is the part of hub.PromptLibrary the bot reads.
type PromptStore interface {
	Prompts(ctx context.Context, user string) ([]hub.SavedPrompt, error)
	Prompt(ctx context.Context, user, name string) (hub.SavedPrompt, bool, error)
}

// promptMenu is what /prompts showed a chat, for its buttons.
type promptMenu struct {
	gen   int
	names []string
}

// listPrompts handles /prompts: a button for each of the user's saved
// prompts. A press sends the prompt's text to copy.
func (b *Bot) listPrompts(ctx context.Context, to dest, user string) {
	if b.prompts == nil {
		b.sendText(ctx, to, "This hub keeps no prompts.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ps, err := b.prompts.Prompts(cctx, user)
	if err != nil {
		b.reportErr(ctx, to, user, "list prompts", err)
		return
	}
	manage := `Add and change them on the hub in <a href="` + escapeHTML(b.public+"/") + `">Settings &gt; Prompts</a>.`
	if len(ps) == 0 {
		b.sendHTML(ctx, to, "You have no prompts yet. "+manage, nil)
		return
	}
	st := b.chat(to)
	st.prompts.gen++
	st.prompts.names = nil
	rows := make([][]InlineButton, 0, min(len(ps), maxPromptButtons))
	for _, p := range ps[:min(len(ps), maxPromptButtons)] {
		st.prompts.names = append(st.prompts.names, p.Name)
		rows = append(rows, []InlineButton{{Text: truncate(p.Name, 60),
			CallbackData: fmt.Sprintf("%s:%d:%d", cbSavedPrompt, st.prompts.gen, len(st.prompts.names)-1)}})
	}
	var sb strings.Builder
	sb.WriteString("<b>Prompts</b>\nTap one to get its text to copy.")
	if len(ps) > maxPromptButtons {
		fmt.Fprintf(&sb, " %d more are not shown.", len(ps)-maxPromptButtons)
	}
	sb.WriteString("\n" + manage)
	b.sendHTML(ctx, to, sb.String(), &InlineKeyboard{InlineKeyboard: rows})
}

// pressSavedPrompt sends the text of the prompt a /prompts button names,
// as it is now, in code blocks of at most a message each.
func (b *Bot) pressSavedPrompt(ctx context.Context, q *CallbackQuery, to dest, user string, parts []string) {
	st := b.chat(to)
	gen, err1 := strconv.Atoi(parts[1])
	i, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || gen != st.prompts.gen || i < 0 || i >= len(st.prompts.names) || b.prompts == nil {
		b.answer(ctx, q.ID, "That list is out of date. Send /prompts again.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p, ok, err := b.prompts.Prompt(cctx, user, st.prompts.names[i])
	if err != nil {
		b.answer(ctx, q.ID, errGeneric)
		b.logger.Error("Failed to read a saved prompt", "user", user, "error", err)
		return
	}
	if !ok {
		b.answer(ctx, q.ID, "That prompt no longer exists. Send /prompts again.")
		return
	}
	b.answer(ctx, q.ID, "")
	for j, chunk := range promptChunks(p.Text) {
		head := "<b>" + escapeHTML(p.Name) + "</b>"
		if j > 0 {
			head += " (continued)"
		}
		b.sendHTML(ctx, to, head+"\n"+chunk, nil)
	}
}

// promptChunks splits a prompt's text into code blocks that each fit in a
// message under its head line.
func promptChunks(text string) []string {
	return codePieces(strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "", MaxMessageLen-promptHead)
}
