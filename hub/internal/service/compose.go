package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.ChatActions = (*Service)(nil)

// errCommandAway is a slash command sent while the backend is away: Hermes
// runs a command at once, so the hub cannot hold it.
var errCommandAway = fmt.Errorf("%w: Hermes is restarting or cannot be reached. Run the command again when it is back",
	hub.ErrBackendAway)

// command runs a slash command through slash.exec, as Hermes' own clients
// do: Hermes runs only the commands it reviewed for a local session and
// refuses the rest. A command that becomes a prompt ("send" or "skill")
// is submitted as one, under id.
func (s *Service) command(ctx context.Context, b *backend, user, sessionID, id, text string) (hub.PromptResult, error) {
	name, arg, _ := hub.SlashCommand(text)
	if strings.EqualFold(name, "diff") {
		return hub.PromptResult{}, fmt.Errorf("%w: /diff reads the Hermes server's folder, not the chat's folder", hub.ErrUnknownCommand)
	}
	if strings.EqualFold(name, compressCommand) || strings.EqualFold(name, compactAlias) {
		return s.compress(ctx, b, sessionID, arg)
	}
	if b.heldCount(sessionID) > 0 {
		return hub.PromptResult{}, errCommandAway
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		if gateway.Retryable(err) {
			return hub.PromptResult{}, errCommandAway
		}
		return hub.PromptResult{}, refusal(err)
	}
	var res gateway.SlashExecResult
	err = b.call(ctx, ls, gateway.MethodSlashExec,
		gateway.SlashExecParams{SessionID: ls.id, Command: strings.TrimPrefix(strings.TrimSpace(text), "/")}, &res)
	ls.unpin()
	var r *gateway.RPCError
	switch {
	case errors.As(err, &r) && (r.Message == gateway.ReasonUnsupportedCommand || r.Message == "command_unavailable"):
		var data struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(r.Data, &data)
		if data.Detail != "" {
			return hub.PromptResult{}, fmt.Errorf("%w: %s", hub.ErrUnknownCommand, data.Detail)
		}
		return hub.PromptResult{}, hub.ErrUnknownCommand
	case err != nil && gateway.Retryable(err):
		return hub.PromptResult{}, errCommandAway
	case err != nil:
		return hub.PromptResult{}, refusal(err)
	}
	if (res.Type != gateway.SlashSend && res.Type != gateway.SlashSkill) || strings.TrimSpace(res.Message) == "" {
		return hub.PromptResult{Command: true, Output: res.Output}, nil
	}
	release := b.gate(sessionID)
	defer release()
	out, _, err := s.promptLocked(ctx, b, user, sessionID, id, res.Message, nil)
	if err == nil && res.Notice != "" {
		out.Notice = res.Notice
	}
	return out, err
}

// steer hands a queued admission to the session's running turn with
// prompt.steer_queued. Hermes refuses with steer_unavailable while the
// turn has no agent to take it yet, so it tries again for SteerWindow. Every
// other refusal leaves the admission queued, or started as its own turn.
func (b *backend) steer(ctx context.Context, sessionID, admissionID string) bool {
	deadline := time.Now().Add(b.svc.opts.SteerWindow)
	for {
		ls, err := b.session(ctx, sessionID)
		if err != nil {
			return false
		}
		err = b.call(ctx, ls, gateway.MethodSteerQueued, gateway.SteerQueuedParams{SessionID: ls.id, AdmissionID: admissionID}, nil)
		ls.unpin()
		if err == nil {
			return true
		}
		var r *gateway.RPCError
		if !errors.As(err, &r) || r.Message != gateway.ReasonSteerUnavailable || time.Now().After(deadline) {
			b.log.Debug("steer not taken", "session", sessionID, "error", err.Error())
			return false
		}
		t := time.NewTimer(b.svc.opts.SteerRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

// steerOrFind hands a queued admission to the running turn. An admission
// that already started answers a prompt sent again under the same ID: it
// is a steer when it is not the running turn's own.
func (b *backend) steerOrFind(ctx context.Context, sessionID string, r gateway.Receipt) bool {
	if r.Status == gateway.AdmissionQueued {
		return b.steer(ctx, sessionID, r.AdmissionID)
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return false
	}
	defer ls.unpin()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.turnAdmission != "" && ls.turnAdmission != r.AdmissionID
}

// busy reports whether the session runs a turn or has inputs waiting, as
// far as the hub saw.
func (ls *live) busy() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !ls.turnStart.IsZero() {
		return true
	}
	for _, in := range ls.inputs {
		if in.Status == gateway.AdmissionQueued || in.Status == gateway.AdmissionUnknown {
			return true
		}
	}
	return false
}

// lastUserMessage finds the session's last message from its user in its
// stored transcript, newest pages first.
func (b *backend) lastUserMessage(ctx context.Context, sessionID string) (hub.UserMessage, bool, error) {
	cursor := ""
	for range maxRewindPages {
		var r gateway.TranscriptResult
		err := b.readConn(ctx, gateway.MethodTranscript, gateway.TranscriptParams{SessionID: sessionID, Cursor: cursor,
			Limit: rewindPage, Order: gateway.OrderLatest}, &r)
		if err != nil {
			return hub.UserMessage{}, false, refusal(err)
		}
		if m, ok := hub.LastUserMessage(r.Rows); ok {
			return m, true, nil
		}
		if r.NextCursor == nil || len(r.Rows) == 0 {
			break
		}
		cursor = *r.NextCursor
	}
	return hub.UserMessage{}, false, nil
}

// How far back a rewind looks for the user's last message.
const (
	rewindPage     = 100
	maxRewindPages = 10
)

// Rewind takes back the session's last user message and what followed it
// (session.mutate rewind, as Hermes' /undo and /retry), then sends r's text.
func (s *Service) Rewind(ctx context.Context, user, backendID, sessionID string, r hub.Rewind) (hub.RewindResult, error) {
	if strings.TrimSpace(r.Text) == "" && !r.Retry {
		return hub.RewindResult{}, fmt.Errorf("%w: the message is empty", hub.ErrBadValue)
	}
	if _, _, ok := hub.SlashCommand(r.Text); ok {
		return hub.RewindResult{}, fmt.Errorf("%w: send a slash command as a new message", hub.ErrBadValue)
	}
	if len(r.Text) > maxPrompt {
		return hub.RewindResult{}, fmt.Errorf("%w: the message is too long", hub.ErrBadValue)
	}
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.RewindResult{}, err
	}
	id := hub.SubmissionID(ctx)
	if id == "" {
		id = hub.NewSubmissionID()
	}
	release := b.gate(sessionID)
	defer release()
	if b.heldCount(sessionID) > 0 {
		return hub.RewindResult{}, fmt.Errorf("%w: messages are waiting for the backend", hub.ErrBusy)
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return hub.RewindResult{}, refusal(err)
	}
	defer ls.unpin()
	if ls.busy() {
		return hub.RewindResult{}, fmt.Errorf("%w: the chat runs", hub.ErrBusy)
	}
	m, ok, err := b.lastUserMessage(ctx, ls.id)
	if err != nil {
		return hub.RewindResult{}, err
	}
	if !ok {
		return hub.RewindResult{}, hub.ErrNoUserMessage
	}
	text := r.Text
	if strings.TrimSpace(text) == "" && strings.TrimSpace(m.Text) == "" {
		return hub.RewindResult{}, fmt.Errorf("%w: your last message has no text to send again", hub.ErrBadValue)
	}
	var out hub.RewindResult
	if strings.TrimSpace(r.Model) != "" {
		change, err := s.SetModel(ctx, user, backendID, sessionID, r.Provider, r.Model, r.Confirm)
		if err != nil {
			return hub.RewindResult{}, err
		}
		out.Model = &change
		if change.Status == hub.ModelConfirm {
			return out, nil
		}
	}
	raw, err := b.mutateRaw(ctx, ls, gateway.OpRewind, gateway.RewindPayload{TargetMessageID: m.RowID}, true)
	if err != nil {
		return out, refusal(err)
	}
	var res gateway.RewindResult
	_ = json.Unmarshal(raw, &res)
	out.Removed = res.RewoundCount
	ls.mu.Lock()
	ls.resnapshotLocked()
	ls.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		text = m.Text
		if t, _ := hub.MessageText(res.TargetMessage.Content); strings.TrimSpace(t) != "" {
			text = t
		}
	}
	out.Prompt, _, err = s.promptLocked(ctx, b, user, sessionID, id, text, nil)
	if err == nil && r.Text == "" && m.Images > 0 {
		out.Prompt.Notice = noticeNoImages
	}
	return out, err
}

// noticeNoImages tells the user that a retry sent only the text.
const noticeNoImages = "The images of your message were not sent again: attach them again if the agent needs them."

// maxPrompt bounds the text of a rewind's message, as the web API bounds
// a prompt's.
const maxPrompt = 1 << 20

// Branch copies a session into a new one (session.mutate branch), through
// row rowID, or all of it for 0. The new session follows its parent's
// place: Hermes records the parent as the new session's parent and copies
// its policy (hermes_state_mutation_branch.py), so the device plugin
// resolves its binding through the parent.
func (s *Service) Branch(ctx context.Context, user, backendID, sessionID string, rowID int64, title string) (hub.Session, error) {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxTitle || strings.ContainsAny(title, "\r\n") {
		return hub.Session{}, fmt.Errorf("%w: a title has at most %d characters on one line", hub.ErrBadValue, maxTitle)
	}
	if rowID < 0 {
		return hub.Session{}, fmt.Errorf("%w: bad row", hub.ErrBadValue)
	}
	var out hub.Session
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		if ls.busy() {
			return fmt.Errorf("%w: the chat runs", hub.ErrBusy)
		}
		raw, err := b.mutateRaw(ctx, ls, gateway.OpBranch, gateway.BranchPayload{Title: title, ThroughMessageID: rowID}, true)
		var r *gateway.RPCError
		if errors.As(err, &r) && r.Code == codeInternal && title != "" {
			return fmt.Errorf("%w: %s", hub.ErrTitleInUse, r.Message)
		}
		if err != nil {
			return refusal(err)
		}
		var res gateway.BranchResult
		if err := json.Unmarshal(raw, &res); err != nil || res.BranchedSessionID == "" {
			return errors.New("the gateway did not name the new chat")
		}
		out = hub.Session{ID: res.BranchedSessionID, BackendID: backendID, Title: title, UpdatedAt: time.Now(),
			Source: gateway.SourceGUI}
		return nil
	})
	return out, err
}

// maxTranscriptPages bounds a transcript read: 100,000 rows.
const maxTranscriptPages = 200

// Transcript reads a session's stored transcript with session.transcript,
// which needs no subscription, and its title with session.info.
func (s *Service) Transcript(ctx context.Context, user, backendID, sessionID string) (hub.Transcript, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.Transcript{}, err
	}
	out := hub.Transcript{Messages: []json.RawMessage{}}
	var info gateway.SnapshotInfo
	if err := b.readConn(ctx, gateway.MethodSessionInfo, gateway.SessionParams{SessionID: sessionID}, &info); err != nil {
		return hub.Transcript{}, refusal(err)
	}
	if info.Title != nil {
		out.Title = *info.Title
	}
	cursor, restarts := "", 0
	for range maxTranscriptPages {
		var r gateway.TranscriptResult
		err := b.readConn(ctx, gateway.MethodTranscript, gateway.TranscriptParams{SessionID: sessionID, Cursor: cursor,
			Limit: gateway.MaxTranscriptPage, Order: gateway.OrderOldest}, &r)
		if err != nil {
			return hub.Transcript{}, refusal(err)
		}
		if cursor != "" && r.Reset {
			if restarts++; restarts > 3 {
				return hub.Transcript{}, errors.New("the transcript kept changing while the hub read it")
			}
			out.Messages = out.Messages[:0]
		}
		out.Messages = append(out.Messages, r.Rows...)
		if len(r.Rows) == 0 || r.NextCursor == nil {
			break
		}
		cursor = *r.NextCursor
	}
	return out, nil
}

// hubCommands are the registry commands slash.exec runs for a local
// session (gateway/session_commands.py: _READ_COMMANDS, _CONTROL_COMMANDS,
// _HANDLER_CONTROLS, _PROMPT_COMMANDS, _SETTING_COMMANDS, _INFO_READS and
// _OWNER_COMMANDS), without /diff, which runs git in the Hermes server's
// folder and not where a chat on a computer works, and /approve and /deny,
// which the page answers with its buttons. It adds /compress, which
// slash.exec refuses and the hub runs as session.mutate compress.
func hubCommands() []string {
	return []string{compressCommand,
		"help", "commands", "status", "context", "version", "whoami", "usage", "insights", "profile", "memory", "kanban",
		"topup", "suggestions", "bundles", "skills",
		"title",
		"goal", "subgoal", "loop", "heartbeat", "review", "refine", "debug", "curator", "login", "autoprune",
		"plan", "learn", "init", "moa", "blueprint", "egress",
		"reasoning", "voice", "approvals", "codex-runtime", "footer", "busy", "verbose", "yolo", "fast", "personality",
		"toolsets", "platforms", "cron",
	}
}

// compressCommand and its alias run as session.mutate compress, with a
// bound for the summarizing call, which takes a model's time.
const (
	compressCommand = "compress"
	compactAlias    = "compact"
	compressTimeout = 10 * time.Minute
)

// compress runs /compress as Hermes' own clients do: session.mutate
// compress with the argument as the focus, which Hermes parses ("here N",
// --preview or a topic). Hermes writes the summary into the chat, and the
// answer is its report.
func (s *Service) compress(ctx context.Context, b *backend, sessionID, arg string) (hub.PromptResult, error) {
	if b.heldCount(sessionID) > 0 {
		return hub.PromptResult{}, errCommandAway
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		if gateway.Retryable(err) {
			return hub.PromptResult{}, errCommandAway
		}
		return hub.PromptResult{}, refusal(err)
	}
	defer ls.unpin()
	if ls.busy() {
		return hub.PromptResult{}, fmt.Errorf("%w: the chat runs", hub.ErrBusy)
	}
	raw, err := b.mutateRawWithin(ctx, ls, compressTimeout, gateway.OpCompress, gateway.CompressPayload{Focus: arg}, true)
	if err != nil {
		if gateway.Retryable(err) {
			return hub.PromptResult{}, errCommandAway
		}
		return hub.PromptResult{}, refusal(err)
	}
	var res gateway.CompressResult
	_ = json.Unmarshal(raw, &res)
	ls.mu.Lock()
	ls.resnapshotLocked()
	ls.mu.Unlock()
	return hub.PromptResult{Command: true, Output: compressReport(res)}, nil
}

// compressReport is the text Hermes' clients print after /compress.
func compressReport(r gateway.CompressResult) string {
	if len(r.Lines) > 0 {
		return strings.Join(r.Lines, "\n")
	}
	if r.Summary == nil {
		return "Compressed"
	}
	return strings.Join(slices.DeleteFunc([]string{r.Summary.Headline, r.Summary.TokenLine, r.Summary.Note},
		func(l string) bool { return l == "" }), "\n")
}

// Catalog categories whose commands slash.exec runs as they are.
const (
	categoryUser   = "User commands"
	categoryPlugin = "Plugin commands"
)

// usageHint splits a catalog description from its English usage hint
// (locales/en.yaml slash.shared.usage_suffix).
var usageHint = regexp.MustCompile(`^(.*) \(usage: /(\S+) (.+)\)$`)

// backgroundCommands are the commands the hub runs itself (see Activity in
// the README).
func backgroundCommands() []hub.Command {
	return []hub.Command{{Name: "/background", Description: "Run a prompt in the background, with this chat's settings",
		Args: "<prompt>", Aliases: []string{"/bg"}}}
}

// Commands lists the slash commands a session can run from the hub: those
// of commands.catalog that slash.exec runs, and /bg and /background. The
// catalog is cached per backend for CommandsTTL.
func (s *Service) Commands(ctx context.Context, user, backendID, _ string) ([]hub.Command, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	if b.cmds != nil && time.Since(b.cmdsAt) < s.opts.CommandsTTL {
		return slices.Clone(b.cmds), nil
	}
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	var res gateway.CatalogResult
	if err := c.Call(cctx, gateway.MethodCommandsCatalog, map[string]any{}, &res); err != nil {
		return nil, refusal(err)
	}
	b.cmds, b.cmdsAt = catalogCommands(res), time.Now()
	return slices.Clone(b.cmds), nil
}

func catalogCommands(res gateway.CatalogResult) []hub.Command {
	allowed := hubCommands()
	out := []hub.Command{}
	seen := map[string]bool{}
	aliases := map[string][]string{}
	for alias, name := range res.Canon {
		if alias != strings.ToLower(name) {
			aliases[name] = append(aliases[name], alias)
		}
	}
	add := func(key, desc string, skill bool) {
		if seen[key] || !strings.HasPrefix(key, "/") {
			return
		}
		seen[key] = true
		cmd := hub.Command{Name: key, Description: desc, Aliases: slices.Sorted(slices.Values(aliases[key])), Skill: skill}
		if m := usageHint.FindStringSubmatch(desc); m != nil && "/"+m[2] == key {
			cmd.Description, cmd.Args = m[1], m[3]
		}
		out = append(out, cmd)
	}
	for _, cat := range res.Categories {
		for _, p := range cat.Pairs {
			if cat.Name == categoryUser || cat.Name == categoryPlugin || slices.Contains(allowed, strings.TrimPrefix(p[0], "/")) {
				add(p[0], p[1], false)
			}
		}
	}
	for _, p := range res.Pairs {
		if _, ok := res.Skills[p[0]]; ok {
			add(p[0], p[1], true)
		}
	}
	for _, c := range backgroundCommands() {
		if !seen[c.Name] {
			seen[c.Name] = true
			out = append(out, c)
		}
	}
	return out
}
