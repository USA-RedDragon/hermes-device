package telegram

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/google/uuid"
)

const (
	cbRelease    = "rl"
	msgNoDevices = "You have no computers. Run talaria setup on one to connect it."
	msgNoAttach  = "No chat is attached. Send /sessions or /new first."
)

// pendingRelease is a "/move off" waiting for its confirmation button.
type pendingRelease struct {
	nonce                string
	backendID, sessionID string
}

func (b *Bot) computerList(ctx context.Context, to dest, user string) ([]hub.Computer, bool) {
	if b.computers == nil {
		b.sendText(ctx, to, "Computers are not turned on in this hub.")
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := b.computers.Computers(cctx, user)
	if err != nil {
		b.reportErr(ctx, to, user, "list computers", err)
		return nil, false
	}
	if len(cs) == 0 {
		b.sendText(ctx, to, msgNoDevices)
		return nil, false
	}
	return cs, true
}

func (b *Bot) listComputers(ctx context.Context, to dest, user string) {
	cs, ok := b.computerList(ctx, to, user)
	if !ok {
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Computers</b>")
	for _, c := range cs {
		state := "offline"
		if c.Online {
			state = "online"
		}
		fmt.Fprintf(&sb, "\n%s: %s", escapeHTML(c.Name), state)
		if len(c.Recent) > 0 {
			fmt.Fprintf(&sb, ", last folder <code>%s</code>", escapeHTML(c.Recent[0]))
		}
	}
	sb.WriteString("\n\n/new NAME [folder] starts a chat on one.")
	b.sendHTML(ctx, to, sb.String(), nil)
}

// matchComputer finds the computer whose name starts arg, longest name
// first, and returns the rest of arg.
func matchComputer(cs []hub.Computer, arg string) (hub.Computer, string, bool) {
	words := strings.Fields(arg)
	for n := len(words); n >= 1; n-- {
		name := strings.Join(words[:n], " ")
		for _, c := range cs {
			if strings.EqualFold(c.Name, name) || strings.EqualFold(c.ID, name) {
				return c, strings.Join(words[n:], " "), true
			}
		}
	}
	return hub.Computer{}, "", false
}

// folderFor turns a folder argument into a directory on c: "" is the last
// folder used there, else "" for the computer's first root; "~" is its
// home; a relative folder is under its first root.
func folderFor(c hub.Computer, arg string) string {
	switch {
	case arg == "":
		if len(c.Recent) > 0 {
			return c.Recent[0]
		}
		return ""
	case arg == "~":
		return c.Home
	case strings.HasPrefix(arg, "~/"):
		return path.Join(c.Home, arg[2:])
	case path.IsAbs(arg):
		return path.Clean(arg)
	}
	root := c.Home
	if len(c.Roots) > 0 {
		root = c.Roots[0]
	}
	return path.Join(root, arg)
}

// startOnComputer handles "/new NAME [folder]": it starts a chat on the
// computer arg names. matched is false when arg names no computer, so the
// caller can say so; ok is false when it failed and said why.
func (b *Bot) startOnComputer(ctx context.Context, to dest, user, arg string) (s hub.Session, p hub.Place, matched, ok bool) {
	if b.computers == nil {
		return s, p, false, false
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := b.computers.Computers(cctx, user)
	if err != nil {
		b.reportErr(ctx, to, user, "list computers", err)
		return s, p, true, false
	}
	c, rest, found := matchComputer(cs, arg)
	if !found {
		return s, p, false, false
	}
	if !c.Online {
		b.sendText(ctx, to, c.Name+" is offline. Turn it on and check that talaria runs on it.")
		return s, p, true, false
	}
	s, p, err = b.computers.StartOn(cctx, user, c.ID, folderFor(c, rest))
	if err != nil {
		b.reportErr(ctx, to, user, "start session on a computer", err)
		return s, p, true, false
	}
	if p.Name == "" {
		p.Name = c.Name
	}
	return s, p, true, true
}

// computerStarted says where a chat that /new started on a computer runs.
func computerStarted(p hub.Place) string {
	return "New chat on <b>" + escapeHTML(p.Name) + "</b> in <code>" + escapeHTML(p.Dir) + "</code>. Send a message to begin."
}

// placeLine says where a chat runs, for /where.
func placeLine(p hub.Place) string {
	switch p.Kind {
	case hub.PlaceComputer:
		name := "another person's computer"
		if p.Mine {
			name = "<b>" + escapeHTML(p.Name) + "</b>"
		}
		line := "This chat runs on " + name + "."
		if p.Dir != "" {
			line = "This chat runs on " + name + " in <code>" + escapeHTML(p.Dir) + "</code>."
		}
		if p.Inherited {
			line += " It uses the computer of the chat it came from."
		}
		if !p.Online {
			line += "\nThat computer is offline: messages will fail until it is back."
		}
		return line
	case hub.PlaceSandbox:
		if !p.Online {
			return "This chat runs in the sandbox, which is not running right now: commands will fail until it is back."
		}
		return "This chat runs in the sandbox. /move NAME [folder] moves it to one of your computers."
	case hub.PlaceNone:
		return "This chat is not on a computer, so Talaria cannot run commands for it. /move NAME [folder] fixes that."
	case hub.PlaceServer:
		return "This chat runs on the Hermes server."
	}
	return "This chat's Hermes has no computers."
}

// placeTag is the short form of a place in /sessions.
func placeTag(p hub.Place, ok bool) string {
	if !ok {
		return ""
	}
	off := ""
	if !p.Online {
		off = " (offline)"
	}
	switch p.Kind {
	case hub.PlaceComputer:
		if !p.Mine || p.Dir == "" {
			return " · another computer" + off
		}
		return " · " + escapeHTML(p.Name) + ": " + escapeHTML(path.Base(p.Dir)) + off
	case hub.PlaceSandbox:
		return " · sandbox" + off
	case hub.PlaceNone:
		return " · no computer"
	}
	return ""
}

func (b *Bot) attached(ctx context.Context, to dest, user string) (*attachment, bool) {
	st := b.chat(to)
	if st.att == nil || st.user != user {
		b.sendText(ctx, to, msgNoAttach)
		return nil, false
	}
	return st.att, true
}

func (b *Bot) where(ctx context.Context, to dest, user string) {
	if b.computers == nil {
		b.sendText(ctx, to, "Computers are not turned on in this hub.")
		return
	}
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p, err := b.computers.Place(cctx, user, a.backendID, a.sessionID)
	if err != nil {
		b.reportErr(ctx, to, user, "look up place", err)
		return
	}
	b.sendHTML(ctx, to, placeLine(p), nil)
}

// move handles "/move NAME [folder]" and "/move off".
func (b *Bot) move(ctx context.Context, to dest, user, arg string) {
	if arg == "" {
		b.sendText(ctx, to, "Send /move NAME [folder] to run this chat on one of your computers, or /move off to stop using its computer.")
		return
	}
	if strings.EqualFold(arg, "off") {
		b.askRelease(ctx, to, user)
		return
	}
	cs, ok := b.computerList(ctx, to, user)
	if !ok {
		return
	}
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	c, rest, ok := matchComputer(cs, arg)
	if !ok {
		b.sendText(ctx, to, "Unknown computer. Send /devices to list yours.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p, err := b.computers.MoveTo(cctx, user, a.backendID, a.sessionID, c.ID, folderFor(c, rest))
	if err != nil {
		b.reportErr(ctx, to, user, "move session", err)
		return
	}
	b.sendHTML(ctx, to, "Moved. "+placeLine(p), nil)
}

func (b *Bot) askRelease(ctx context.Context, to dest, user string) {
	if b.computers == nil {
		b.sendText(ctx, to, "Computers are not turned on in this hub.")
		return
	}
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p, err := b.computers.Place(cctx, user, a.backendID, a.sessionID)
	if err != nil {
		b.reportErr(ctx, to, user, "look up place", err)
		return
	}
	if p.Kind != hub.PlaceComputer || p.Inherited || !p.Mine {
		b.sendHTML(ctx, to, placeLine(p), nil)
		return
	}
	st := b.chat(to)
	st.release = &pendingRelease{nonce: uuid.NewString()[:12], backendID: a.backendID, sessionID: a.sessionID}
	b.sendHTML(ctx, to, "Stop using <b>"+escapeHTML(p.Name)+"</b> for this chat? Talaria then cannot run commands or change files "+
		"for it anywhere, also not in the sandbox, until you /move it to a computer again.",
		&InlineKeyboard{InlineKeyboard: [][]InlineButton{{{Text: "Stop using " + truncate(p.Name, 30), CallbackData: cbRelease + ":" + st.release.nonce}}}})
}

func (b *Bot) confirmRelease(ctx context.Context, q *CallbackQuery, to dest, user, nonce string) {
	st := b.chat(to)
	r := st.release
	if r == nil || r.nonce != nonce || st.user != user {
		b.answer(ctx, q.ID, "That question is out of date. Send /move off again.")
		return
	}
	st.release = nil
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := b.computers.Release(cctx, user, r.backendID, r.sessionID); err != nil {
		b.answer(ctx, q.ID, "")
		b.reportErr(ctx, to, user, "release session", err)
		return
	}
	b.answer(ctx, q.ID, "Done")
	b.editHTML(ctx, q.Message.Chat.ID, q.Message.MessageID,
		"This chat no longer uses a computer. Talaria cannot run commands for it until you /move it to one again.")
}

// computerErr reports the errors of computer commands. It returns false
// for any other error.
func (b *Bot) computerErr(ctx context.Context, to dest, err error) bool {
	reason := func() string {
		_, msg, _ := strings.Cut(err.Error(), ": ")
		if msg == "" {
			return "Hermes did not accept that."
		}
		return strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	var text string
	switch {
	case errors.Is(err, hub.ErrUnknownDevice):
		text = "Unknown computer. Send /devices to list yours."
	case errors.Is(err, hub.ErrComputerOffline):
		text = "That computer is offline. Turn it on and check that talaria runs on it."
	case errors.Is(err, hub.ErrFolderRefused), errors.Is(err, hub.ErrCannotMove):
		text = reason()
	case errors.Is(err, hub.ErrOtherBackend):
		text = "That computer works with another Hermes than this chat. Send /new NAME to start a chat on it."
	case errors.Is(err, hub.ErrNotOnComputer):
		text = "This chat is not on a computer."
	case errors.Is(err, hub.ErrInheritedPlace):
		text = "This chat uses the computer of the chat it came from. /move NAME moves it to a computer of its own."
	default:
		return false
	}
	b.sendText(ctx, to, text)
	return true
}
