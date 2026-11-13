// Package agents routes messages between the agents of one backend: its
// chats and the subagents they delegate to. The talaria device plugin's
// device-agents tools put each call on a bus that the hub takes with a
// long poll (docs/protocol.md section 12). The hub checks the
// call, records it and delivers the message: as a new input of a chat
// (prompt.submit), or into a running agent through the plugin.
package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// Limits on agent messages.
const (
	// MaxText is the longest message, in characters.
	MaxText = 8000
	// ShortWindow, MaxFromTree and MaxPair: one delegation tree (a chat
	// and its subagents) may send at most MaxFromTree messages, and at
	// most MaxPair to one target, in ShortWindow.
	ShortWindow = 10 * time.Minute
	MaxFromTree = 20
	MaxPair     = 8
	// LongWindow and MaxBackend: all agents of a backend may send at most
	// MaxBackend messages in LongWindow.
	LongWindow = time.Hour
	MaxBackend = 200
	// MaxHop is the longest back-and-forth chain. Only an answer continues
	// a chain: a message counts one hop more than the message it answers
	// (reply_to), or than the newest message in HopWindow from its target
	// that its sender's tree has not answered yet. Any other message
	// starts a chain at 1.
	MaxHop    = 6
	HopWindow = 30 * time.Minute
	// MaxPartnerHop and MaxPartnerPair replace MaxHop and MaxPair for two
	// chats the user marked as working together. The pair limit equals
	// MaxFromTree: a higher one could never apply.
	MaxPartnerHop  = 30
	MaxPartnerPair = 20
)

const partnerHint = " If these chats work together, the user can mark these chats as working together to raise the limit"

// InputPrefix starts the submission ID of each agent message the hub
// submits as a chat's input. Devices may not use it (hermes-hub- ids are
// the hub's).
const InputPrefix = "hermes-hub-agent-"

const (
	opPeers       = "peers"
	opSend        = "send"
	opName        = "name"
	opUndelivered = "undelivered"

	toParent     = "parent"
	keySessionID = "session_id"

	pollWait     = 25 * time.Second
	pollSlack    = 15 * time.Second
	callTimeout  = 20 * time.Second
	maxHandlers  = 8
	maxPeerChats = 30
	peerAge      = 7 * 24 * time.Hour
	maxLabel     = 80

	// cronRoot prefixes the sender identity of a scheduled job's runs in the
	// message log and the limits: "cron:<job id>".
	cronRoot = "cron:"
)

// Sessions lists and feeds a backend's sessions for the hub itself.
// *service.Service is one.
type Sessions interface {
	SessionsOf(ctx context.Context, backendID string) ([]hub.Session, error)
	SubmitAgent(ctx context.Context, backendID, sessionID, id, text string) error
}

// Places says where sessions run. *device.Server is one.
type Places interface {
	Places(ctx context.Context, user string, sessions []hub.SessionRef) map[hub.SessionRef]hub.Place
}

// Plugin is the device plugin of one backend. *device.Plugin is one.
type Plugin interface {
	Info(ctx context.Context) (device.Info, error)
	AgentRequests(ctx context.Context, wait time.Duration) ([]device.AgentRequest, error)
	AgentAnswer(ctx context.Context, id string, result any, code, message string) error
	AgentDeliver(ctx context.Context, d device.AgentDelivery) (string, error)
}

// Options configure a Router.
type Options struct {
	Backends []config.Backend
	Plugin   func(backendID string) Plugin
	Sessions Sessions
	Places   Places
	Store    hub.AgentStore
	Logger   *slog.Logger
	Now      func() time.Time
}

// Router serves the agent message requests of every backend that has
// them on.
type Router struct {
	opts Options
	log  *slog.Logger
	sem  chan struct{}
	// sendMu orders sends, so two sends cannot both pass a
	// limit that only one of them fits. Several replicas can still race.
	sendMu sync.Mutex
	lim    *limiter
}

// Enabled reports whether a backend's agents may message each other.
func Enabled(b config.Backend) bool {
	if !b.Devices {
		return false
	}
	if b.AgentMessages != nil {
		return *b.AgentMessages
	}
	return len(b.Users) == 1 && len(b.Groups) == 0
}

// New returns a Router.
func New(opts Options) *Router {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Router{opts: opts, log: opts.Logger, sem: make(chan struct{}, maxHandlers), lim: newLimiter()}
}

// Run polls each enabled backend's plugin until ctx ends.
func (r *Router) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range r.opts.Backends {
		if !Enabled(b) {
			continue
		}
		p := r.opts.Plugin(b.ID)
		if p == nil {
			continue
		}
		wg.Go(func() { r.poll(ctx, b, p) })
	}
	wg.Wait()
}

func (r *Router) poll(ctx context.Context, b config.Backend, p Plugin) {
	backoff := time.Second
	checked := false
	for ctx.Err() == nil {
		if !checked {
			ictx, cancel := context.WithTimeout(ctx, callTimeout)
			info, err := p.Info(ictx)
			cancel()
			switch {
			case err == nil && info.BackendID == b.ID:
				checked = true
			case err == nil:
				r.log.Error("the device plugin serves another backend; agent messages are off for this one",
					"backend", b.ID, "plugin_backend", info.BackendID)
				return
			default:
				r.log.Debug("read the device plugin's info", "backend", b.ID, "error", err.Error())
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
		}
		pctx, cancel := context.WithTimeout(ctx, pollWait+pollSlack)
		reqs, err := p.AgentRequests(pctx, pollWait)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var pe *device.PluginError
			if errors.As(err, &pe) && pe.Status == 404 {
				r.log.Debug("the device plugin has no agent message routes", "backend", b.ID)
			} else {
				r.log.Debug("poll agent messages", "backend", b.ID, "error", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		for _, req := range reqs {
			select {
			case r.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-r.sem }()
				r.handle(ctx, b, p, req)
			}()
		}
	}
}

// refusal is an answer the agent gets as its tool's error.
type refusal struct{ code, message string }

func (e *refusal) Error() string { return e.message }

func refuse(code, format string, args ...any) error {
	return &refusal{code: code, message: fmt.Sprintf(format, args...)}
}

func (r *Router) handle(ctx context.Context, b config.Backend, p Plugin, req device.AgentRequest) {
	hctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	c := req.Caller
	if c.RootSessionID == "" {
		c.RootSessionID = c.SessionID
	}
	if job := CronJobOf(c); job != nil {
		c.CronJob = job
		// One scheduled job is one sender for the limits, whatever run it is:
		// every run starts a new session, so counting by session would let a
		// job that fires often send without end.
		c.RootSessionID = cronRoot + job.ID
	}
	var (
		result any
		err    error
	)
	switch {
	case c.SessionID == "":
		err = refuse("no_session", "this tool call has no session")
	case (req.Op == opPeers || req.Op == opName) && !r.lim.allow(b.ID+"\x00"+c.RootSessionID, r.opts.Now()):
		err = refuse("rate_limited", "too many agent_peers and agent_set_name calls; wait a few minutes")
	case req.Op == opPeers:
		result, err = r.peers(hctx, b, c)
	case req.Op == opSend:
		result, err = r.send(hctx, b, p, c, req.Args)
	case req.Op == opName:
		result, err = r.name(hctx, b, c, req.Args)
	case req.Op == opUndelivered:
		r.undelivered(hctx, b, req.Args)
		return
	default:
		err = refuse("unknown_op", "unknown request")
	}
	code, msg := "", ""
	if err != nil {
		var rf *refusal
		if errors.As(err, &rf) {
			code, msg = rf.code, rf.message
		} else {
			r.log.Warn("agent message request failed", "backend", b.ID, "op", req.Op, "error", err.Error())
			code, msg = "failed", "the Talaria hub could not do this right now"
		}
		result = nil
	}
	if aerr := p.AgentAnswer(hctx, req.ID, result, code, msg); aerr != nil {
		r.log.Debug("answer agent message request", "backend", b.ID, "error", aerr.Error())
	}
}

// --- peers ---------------------------------------------------------------

type peerChat struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name,omitempty"`
	Title     string `json:"title"`
	Where     string `json:"where,omitempty"`
	Running   bool   `json:"running"`
	StartedBy string `json:"started_by,omitempty"`
	// Partner: the user marked this chat as working with yours, so the
	// two may exchange longer chains.
	Partner bool `json:"partner,omitempty"`
}

type peerSubagent struct {
	SubagentID string `json:"subagent_id"`
	Relation   string `json:"relation"`
	Goal       string `json:"goal"`
}

// cronSessionRE is the session id of a scheduled run: cron_<job id>_<date>_<time>.
var cronSessionRE = regexp.MustCompile(`^cron_([0-9a-f]{6,64})_\d{8}_\d{6}`)

// CronJobOf says which scheduled job a caller's run belongs to, or nil for a
// chat or a subagent of one. A plugin that knows the job sends it in
// cron_job; for an older plugin the job id is in the run's session id (the
// name is then unknown). The id is cleaned either way, since it ends up in
// a message header and the limits.
func CronJobOf(c device.AgentCaller) *device.AgentCronJob {
	id, name := "", ""
	if c.CronJob != nil {
		id, name = c.CronJob.ID, c.CronJob.Name
	}
	if id == "" {
		if m := cronSessionRE.FindStringSubmatch(c.RootSessionID); m != nil {
			id = m[1]
		} else if m := cronSessionRE.FindStringSubmatch(c.SessionID); m != nil {
			id = m[1]
		}
	}
	if id == "" {
		return nil
	}
	id = label(id)
	if len(id) > 64 || strings.ContainsAny(id, " :'") {
		return nil
	}
	return &device.AgentCronJob{ID: id, Name: label(name)}
}

func owner(b config.Backend) string {
	if len(b.Users) > 0 {
		return b.Users[0]
	}
	return ""
}

func (r *Router) peers(ctx context.Context, b config.Backend, c device.AgentCaller) (any, error) {
	sessions, err := r.opts.Sessions.SessionsOf(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	names, err := r.opts.Store.AgentNames(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	inTree := map[string]bool{c.RootSessionID: true, c.SessionID: true}
	for _, s := range c.Tree {
		inTree[s.SessionID] = true
	}
	now := r.opts.Now()
	var you map[string]any
	var list []hub.Session
	for _, s := range sessions {
		if s.ID == c.RootSessionID {
			you = map[string]any{keySessionID: s.ID, "name": names[s.ID], "title": s.Title}
		}
		if inTree[s.ID] || s.Archived || !peerSource(s.Source) || (!s.Live && now.Sub(s.UpdatedAt) > peerAge) {
			continue
		}
		list = append(list, s)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Running != list[j].Running {
			return list[i].Running
		}
		if list[i].Live != list[j].Live {
			return list[i].Live
		}
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
	list = list[:min(len(list), maxPeerChats)]
	refs := make([]hub.SessionRef, 0, len(list))
	for _, s := range list {
		refs = append(refs, hub.SessionRef{BackendID: b.ID, SessionID: s.ID})
	}
	var places map[hub.SessionRef]hub.Place
	if r.opts.Places != nil && len(refs) > 0 {
		places = r.opts.Places.Places(ctx, owner(b), refs)
	}
	partners, err := r.partnerSet(ctx, b, c.RootSessionID)
	if err != nil {
		return nil, err
	}
	chats := make([]peerChat, 0, len(list))
	for _, s := range list {
		chats = append(chats, peerChat{SessionID: s.ID, Name: names[s.ID], Title: label(s.Title), Running: s.Running,
			Partner:   partners[s.ID],
			StartedBy: s.Source, Where: where(places[hub.SessionRef{BackendID: b.ID, SessionID: s.ID}])})
	}
	var subs []peerSubagent
	if c.SubagentID != "" && c.ParentSubagentID == "" {
		subs = append(subs, peerSubagent{SubagentID: toParent, Relation: "parent (the chat that delegated to you)"})
	}
	for _, s := range c.Tree {
		if s.SessionID == c.SessionID {
			continue
		}
		subs = append(subs, peerSubagent{SubagentID: s.SubagentID, Relation: relation(c, s), Goal: label(s.Goal)})
	}
	if you == nil {
		you = map[string]any{keySessionID: c.RootSessionID}
		if c.CronJob != nil {
			you = map[string]any{keySessionID: c.SessionID, "title": cronLabel(c.CronJob)}
		}
	}
	if c.SubagentID != "" {
		you["subagent_id"] = c.SubagentID
	}
	return map[string]any{
		"you": you, "chats": chats, "subagents": subs,
		"note": "Send with agent_send, to a session id, a name, an exact title, a subagent id or parent. " +
			"Chats are the user's other chats on this Hermes; it does not matter which computer they run on.",
	}, nil
}

// peerSources are the surfaces whose sessions are the user's own chats.
// Sessions of messaging platforms (a Telegram group or a direct message
// that Hermes serves itself) may have other people in them, so agents
// never reach them.
func peerSource(s string) bool {
	switch s {
	case "gui", "cli", "tui", "acp", "desktop", "local":
		return true
	}
	return false
}

func relation(c device.AgentCaller, s device.AgentSubagent) string {
	switch {
	case c.SubagentID != "" && s.SubagentID == c.ParentSubagentID:
		return toParent
	case directChild(c, s):
		return "child"
	case c.SubagentID != "" && s.ParentID == c.ParentSubagentID:
		return "sibling"
	}
	return "same delegation tree"
}

func directChild(c device.AgentCaller, s device.AgentSubagent) bool {
	if c.SubagentID == "" {
		return s.ParentID == "" && c.SessionID == c.RootSessionID
	}
	return s.ParentID == c.SubagentID
}

func where(p hub.Place) string {
	off := ""
	if !p.Online {
		off = " (offline)"
	}
	switch p.Kind {
	case hub.PlaceComputer:
		return "computer " + label(p.Name) + ", folder " + label(p.Dir) + off
	case hub.PlaceSandbox:
		if p.Dir != "" {
			return "sandbox, folder " + label(p.Dir) + off
		}
		return "sandbox" + off
	case hub.PlaceServer:
		return "Hermes server"
	case hub.PlaceNone:
		return "no computer: it runs no commands"
	}
	return ""
}

// label makes text from Hermes or an agent safe for one line of a
// message header: no control characters, brackets, quotes or @, at most
// maxLabel characters.
func label(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cf, r):
			continue
		case unicode.IsSpace(r) || unicode.IsControl(r):
			space = b.Len() > 0
			continue
		case strings.ContainsRune("[]\"`@<>", r):
			r = '\''
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		if n >= maxLabel {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// --- names ---------------------------------------------------------------

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)

func (r *Router) name(ctx context.Context, b config.Backend, c device.AgentCaller, raw json.RawMessage) (any, error) {
	var a struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &a)
	if c.SubagentID != "" {
		return nil, refuse("not_a_chat", "only a chat can have a name; a subagent is addressed by its id")
	}
	lower := strings.ToLower(a.Name)
	if a.Name != "" && (!nameRE.MatchString(a.Name) || lower == toParent || strings.HasPrefix(lower, "sa-") ||
		strings.HasPrefix(lower, "am_")) {
		return nil, refuse("bad_name", "a name is 1 to 40 letters, digits, dashes and underscores, starts with a "+
			"letter or digit, and is not parent or starts with sa- or am_")
	}
	if a.Name != "" {
		sessions, err := r.opts.Sessions.SessionsOf(ctx, b.ID)
		if err != nil {
			return nil, err
		}
		for _, s := range sessions {
			if s.ID != c.SessionID && (strings.EqualFold(s.ID, a.Name) || strings.EqualFold(strings.TrimSpace(s.Title), a.Name)) {
				return nil, refuse("name_taken", "another chat has %s as its id or title", a.Name)
			}
		}
	}
	err := r.opts.Store.SetAgentName(ctx, b.ID, c.SessionID, a.Name)
	if errors.Is(err, hub.ErrAgentNameTaken) {
		return nil, refuse("name_taken", "another chat already has the name %s", a.Name)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{keySessionID: c.SessionID, "name": a.Name}, nil
}

// --- send ----------------------------------------------------------------

// target is a resolved recipient.
type target struct {
	session  string
	subagent string
	label    string
	// mode is how to deliver: an input of a chat, the plugin's inbox, or a
	// steer of a direct child. inboxFirst is a chat the sender's own tree
	// belongs to: while its turn runs, the message rides on its next tool
	// result, else it is a new input.
	mode       string
	inboxFirst bool
}

func (r *Router) resolve(ctx context.Context, b config.Backend, c device.AgentCaller, to string) (target, error) {
	to = strings.TrimSpace(to)
	if strings.EqualFold(to, toParent) {
		if c.SubagentID == "" {
			return target{}, refuse("no_parent", "you are a chat, not a subagent, so you have no parent agent")
		}
		if c.ParentSubagentID != "" {
			for _, s := range c.Tree {
				if s.SubagentID == c.ParentSubagentID {
					return target{session: s.SessionID, subagent: s.SubagentID, label: "your parent agent " + s.SubagentID,
						mode: hub.AgentModeInbox}, nil
				}
			}
			return target{}, refuse("gone", "your parent agent is no longer running")
		}
		to = c.ParentSessionID
	}
	for _, s := range c.Tree {
		if s.SubagentID != to {
			continue
		}
		if s.SessionID == c.SessionID {
			return target{}, refuse("self", "you cannot message yourself")
		}
		t := target{session: s.SessionID, subagent: s.SubagentID, label: "subagent " + s.SubagentID,
			mode: hub.AgentModeInbox}
		if directChild(c, s) {
			t.mode = hub.AgentModeSteer
		}
		return t, nil
	}
	if strings.HasPrefix(strings.ToLower(to), "sa-") {
		return target{}, refuse("unknown_peer", "%s is not a running subagent of your delegation tree; "+
			"call agent_peers", label(to))
	}
	sessions, err := r.opts.Sessions.SessionsOf(ctx, b.ID)
	if err != nil {
		return target{}, err
	}
	names, err := r.opts.Store.AgentNames(ctx, b.ID)
	if err != nil {
		return target{}, err
	}
	hit, err := pick(sessions, names, to)
	if err != nil {
		return target{}, err
	}
	if hit.ID == c.SessionID || (c.SubagentID == "" && hit.ID == c.RootSessionID) {
		return target{}, refuse("self", "you cannot message yourself")
	}
	t := target{session: hit.ID, mode: hub.AgentModeInput, label: chatLabel(hit.ID, names[hit.ID], hit.Title)}
	if c.SubagentID != "" && hit.ID == c.RootSessionID {
		t.inboxFirst = true
		if c.ParentSubagentID == "" {
			t.label = "your parent chat " + t.label
		}
	}
	return t, nil
}

// pick finds a chat by exact session id, else by short name or exact
// title. A name and a title of two different chats is ambiguous, so a
// chat cannot take another chat's messages by naming itself after it.
func pick(sessions []hub.Session, names map[string]string, to string) (*hub.Session, error) {
	var byName *hub.Session
	var titled []*hub.Session
	for i := range sessions {
		s := &sessions[i]
		if !peerSource(s.Source) {
			continue
		}
		if s.ID == to {
			return s, nil
		}
		if strings.EqualFold(names[s.ID], to) {
			byName = s
		}
		if !s.Archived && strings.EqualFold(strings.TrimSpace(s.Title), to) {
			titled = append(titled, s)
		}
	}
	if byName != nil {
		for _, s := range titled {
			if s.ID != byName.ID {
				return nil, refuse("ambiguous", "a chat has the name %q and another the title; use a session id: %s or %s",
					label(to), byName.ID, s.ID)
			}
		}
		return byName, nil
	}
	switch len(titled) {
	case 0:
		return nil, refuse("unknown_peer", "no chat has the id, name or title %q; call agent_peers", label(to))
	case 1:
		return titled[0], nil
	}
	ids := make([]string, 0, len(titled))
	for _, s := range titled {
		ids = append(ids, s.ID)
	}
	return nil, refuse("ambiguous", "several chats have that title; use a session id: %s", strings.Join(ids, ", "))
}

func chatLabel(id, name, title string) string {
	shown := name
	if shown == "" {
		shown = title
	}
	if shown == "" {
		shown = "untitled"
	}
	return fmt.Sprintf("chat '%s' (session %s)", label(shown), id)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "am_" + hex.EncodeToString(b)
}

func cronLabel(j *device.AgentCronJob) string {
	if j.Name != "" {
		return fmt.Sprintf("cron job '%s' (id %s)", j.Name, j.ID)
	}
	return fmt.Sprintf("cron job %s", j.ID)
}

func (r *Router) sender(ctx context.Context, b config.Backend, c device.AgentCaller) (string, string, error) {
	sessions, err := r.opts.Sessions.SessionsOf(ctx, b.ID)
	if err != nil {
		return "", "", err
	}
	names, err := r.opts.Store.AgentNames(ctx, b.ID)
	if err != nil {
		return "", "", err
	}
	title := ""
	for _, s := range sessions {
		if s.ID == c.RootSessionID {
			title = s.Title
		}
	}
	chat := chatLabel(c.RootSessionID, names[c.RootSessionID], title)
	if c.CronJob != nil {
		chat = cronLabel(c.CronJob)
	}
	if c.SubagentID == "" {
		return chat, names[c.RootSessionID], nil
	}
	goal := ""
	for _, s := range c.Tree {
		if s.SubagentID == c.SubagentID {
			goal = s.Goal
		}
	}
	desc := "subagent " + c.SubagentID
	if goal != "" {
		desc += " (goal: " + label(goal) + ")"
	}
	return desc + " of " + chat, c.SubagentID, nil
}

func (r *Router) send(ctx context.Context, b config.Backend, p Plugin, c device.AgentCaller, raw json.RawMessage) (any, error) {
	var a struct {
		To      string `json:"to"`
		Text    string `json:"text"`
		ReplyTo string `json:"reply_to"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil, refuse("bad_arguments", "bad arguments")
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, refuse("bad_arguments", "text is empty")
	}
	if n := len([]rune(a.Text)); n > MaxText {
		return nil, refuse("too_long", "the message has %d characters; the limit is %d", n, MaxText)
	}
	if c.SubagentID != "" && !inTree(c, c.SubagentID, c.SessionID) {
		return nil, refuse("no_session", "this subagent is no longer running")
	}
	t, err := r.resolve(ctx, b, c, a.To)
	if err != nil {
		return nil, err
	}
	r.sendMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			r.sendMu.Unlock()
		}
	}
	defer unlock()
	now := r.opts.Now()
	partner, err := r.isPartner(ctx, b, c, t)
	if err != nil {
		return nil, err
	}
	maxPair, maxHop, hint := MaxPair, MaxHop, ""
	if partner {
		maxPair, maxHop = MaxPartnerPair, MaxPartnerHop
	} else if !inTreeSession(c, t.session) {
		hint = partnerHint
	}
	counts, err := r.opts.Store.AgentCounts(ctx, b.ID, c.RootSessionID, t.session, now.Add(-ShortWindow), now.Add(-LongWindow))
	if err != nil {
		return nil, err
	}
	switch {
	case counts.Pair >= maxPair:
		return nil, refuse("rate_limited", "you sent %s %d messages in the last %s; wait, or let the user decide.%s",
			t.label, counts.Pair, ShortWindow, hint)
	case counts.FromRoot >= MaxFromTree:
		return nil, refuse("rate_limited", "your chat and its subagents sent %d messages in the last %s; wait, "+
			"or let the user decide", counts.FromRoot, ShortWindow)
	case counts.Backend >= MaxBackend:
		return nil, refuse("rate_limited", "the user's agents sent %d messages in the last %s, the most allowed",
			counts.Backend, LongWindow)
	}
	hop, err := r.hop(ctx, b, c, t, a.ReplyTo, now)
	if err != nil {
		return nil, err
	}
	if hop > maxHop {
		return nil, refuse("too_many_hops", "this exchange has gone back and forth %d times, the most allowed. "+
			"Stop messaging about it and tell the user where it stands.%s", hop-1, hint)
	}
	from, fromName, err := r.sender(ctx, b, c)
	if err != nil {
		return nil, err
	}
	m := hub.AgentMessage{ID: newID(), BackendID: b.ID, FromSession: c.SessionID, FromRoot: c.RootSessionID,
		FromSubagent: c.SubagentID, FromName: fromName, ToSession: t.session, ToSubagent: t.subagent,
		ToName: t.label, ReplyTo: a.ReplyTo, Hop: hop, Mode: t.mode, Status: hub.AgentStatusSending, CreatedAt: now}
	m.Text = Format(m.ID, from, replyHint(c, t), t.mode == hub.AgentModeSteer, a.Text)
	if err := r.opts.Store.AddAgentMessage(ctx, m); err != nil {
		return nil, err
	}
	unlock()
	mode, status, how, err := r.deliver(ctx, b, p, m, t)
	if serr := r.opts.Store.SetAgentMessageStatus(context.WithoutCancel(ctx), m.ID, mode, status); serr != nil {
		r.log.Warn("record agent message status", "backend", b.ID, "id", m.ID, "error", serr.Error())
	}
	r.log.Info("agent message", "backend", b.ID, "id", m.ID, "from", c.SessionID, "from_subagent", c.SubagentID,
		"to", t.session, "to_subagent", t.subagent, "hop", hop, "mode", mode, "status", status)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message_id": m.ID, "to": t.label, "delivery": how}, nil
}

func inTree(c device.AgentCaller, id, session string) bool {
	for _, s := range c.Tree {
		if s.SubagentID == id && s.SessionID == session {
			return true
		}
	}
	return false
}

func replyHint(c device.AgentCaller, t target) string {
	if c.CronJob != nil && c.SubagentID == "" {
		return "" // a scheduled run ends when it ends: nobody can answer it
	}
	if c.SubagentID == "" {
		return c.RootSessionID
	}
	if t.subagent != "" || t.session == c.RootSessionID {
		return c.SubagentID
	}
	return c.RootSessionID
}

// treeSessions are the sessions of the sender's delegation tree.
func treeSessions(c device.AgentCaller) []string {
	sessions := make([]string, 0, 2+len(c.Tree))
	sessions = append(sessions, c.RootSessionID)
	if c.SessionID != c.RootSessionID {
		sessions = append(sessions, c.SessionID)
	}
	for _, s := range c.Tree {
		sessions = append(sessions, s.SessionID)
	}
	return sessions
}

func inTreeSession(c device.AgentCaller, session string) bool {
	return slices.Contains(treeSessions(c), session)
}

// hop is the new message's place in its back-and-forth chain. Only an
// answer continues a chain, so unrelated messages between two chats never
// pile up:
//   - reply_to names a message the sender's tree got: one more than its hop.
//   - else the newest message in HopWindow that the target's tree sent to
//     the sender's tree, if the sender's tree has not sent the target
//     anything since, counts as the message being answered: one more than
//     its hop. A turn starts on the message it answers, so an agent that
//     never sets reply_to still counts; a subagent counts what its chat
//     got.
//   - else it starts a chain at 1.
//
// A bad reply_to cannot lower the second rule: the highest applies.
func (r *Router) hop(ctx context.Context, b config.Backend, c device.AgentCaller, t target, replyTo string, now time.Time) (int, error) {
	sessions := treeSessions(c)
	hop := 1
	if replyTo != "" {
		m, ok, err := r.opts.Store.AgentMessage(ctx, b.ID, replyTo)
		if err != nil {
			return 0, err
		}
		if ok && slices.Contains(sessions, m.ToSession) {
			hop = m.Hop + 1
		}
	}
	in, ok, err := r.opts.Store.LastAgentIn(ctx, b.ID, sessions, t.session, now.Add(-HopWindow))
	if err != nil || !ok {
		return hop, err
	}
	out, sent, err := r.opts.Store.LastAgentOut(ctx, b.ID, c.RootSessionID, t.session, in.CreatedAt)
	if err != nil {
		return 0, err
	}
	if !sent || !out.CreatedAt.After(in.CreatedAt) {
		hop = max(hop, in.Hop+1)
	}
	return hop, nil
}

// partnerSet is the chats the user marked as working with session.
func (r *Router) partnerSet(ctx context.Context, b config.Backend, session string) (map[string]bool, error) {
	out := map[string]bool{}
	user := PartnerOwner(b)
	if user == "" {
		return out, nil
	}
	pairs, err := r.opts.Store.AgentPartners(ctx, user, b.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range pairs {
		switch session {
		case p.A:
			out[p.B] = true
		case p.B:
			out[p.A] = true
		}
	}
	return out, nil
}

// isPartner reports whether the sender's chat and the target chat work
// together. Subagents count with their chat.
func (r *Router) isPartner(ctx context.Context, b config.Backend, c device.AgentCaller, t target) (bool, error) {
	if inTreeSession(c, t.session) {
		return false, nil
	}
	set, err := r.partnerSet(ctx, b, c.RootSessionID)
	return set[t.session], err
}

// PartnerOwner is the one user whose partner pairs a backend honors: the
// owner of a single-user backend. A backend with several users has none,
// so no one's pairs raise another user's limits.
func PartnerOwner(b config.Backend) string {
	if len(b.Users) == 1 {
		return b.Users[0]
	}
	return ""
}

func (r *Router) deliver(ctx context.Context, b config.Backend, p Plugin, m hub.AgentMessage, t target) (string, string, string, error) {
	if t.mode == hub.AgentModeSteer || t.mode == hub.AgentModeInbox || t.inboxFirst {
		mode := t.mode
		if t.inboxFirst {
			mode = hub.AgentModeInbox
		}
		st, err := p.AgentDeliver(ctx, device.AgentDelivery{Mode: mode, MessageID: m.ID, SessionID: t.session,
			SubagentID: t.subagent, Text: m.Text})
		if err != nil {
			r.log.Warn("deliver an agent message", "backend", b.ID, "id", m.ID, "error", err.Error())
			return mode, hub.AgentStatusNotDelivered, "", refuse("not_delivered", "could not deliver to %s now", t.label)
		}
		switch st {
		case device.DeliverySteered:
			return mode, hub.AgentStatusDelivered, "steered into the subagent: it reads it after its current step", nil
		case device.DeliveryQueued:
			return mode, hub.AgentStatusDelivered, "it arrives with the target's next tool result", nil
		case device.DeliveryNotRunning:
			if !t.inboxFirst {
				return mode, hub.AgentStatusNotDelivered, "", refuse("gone", "%s is not running", t.label)
			}
		case device.DeliveryFull:
			return mode, hub.AgentStatusNotDelivered, "", refuse("busy", "%s has too many unread messages; wait", t.label)
		default:
			return mode, hub.AgentStatusNotDelivered, "", refuse("gone", "%s is no longer running", t.label)
		}
	}
	if err := r.opts.Sessions.SubmitAgent(ctx, b.ID, t.session, InputPrefix+m.ID, m.Text); err != nil {
		if errors.Is(err, service.ErrAgentYolo) {
			return hub.AgentModeInput, hub.AgentStatusNotDelivered, "", refuse("yolo", "%s runs without approvals (yolo), "+
				"so it takes no messages from agents", t.label)
		}
		r.log.Warn("deliver an agent message", "backend", b.ID, "id", m.ID, "error", err.Error())
		return hub.AgentModeInput, hub.AgentStatusNotDelivered, "", refuse("not_delivered", "could not deliver to %s now; "+
			"try again later", t.label)
	}
	return hub.AgentModeInput, hub.AgentStatusDelivered,
		"a new input of the chat: it starts a turn if the chat is idle, else it waits for the current turn", nil
}

// undelivered handles an inbox message the target never read: a chat
// gets it as a new input, a subagent that finished never does.
func (r *Router) undelivered(ctx context.Context, b config.Backend, raw json.RawMessage) {
	var a struct {
		MessageID string `json:"message_id"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &a) != nil || a.MessageID == "" {
		return
	}
	m, ok, err := r.opts.Store.AgentMessage(ctx, b.ID, a.MessageID)
	if err != nil || !ok || m.ToSession != a.SessionID || m.Mode != hub.AgentModeInbox {
		return
	}
	status, mode := hub.AgentStatusNotDelivered, hub.AgentModeInbox
	if m.ToSubagent == "" {
		if err := r.opts.Sessions.SubmitAgent(ctx, b.ID, m.ToSession, InputPrefix+m.ID, m.Text); err != nil {
			r.log.Warn("deliver an unread agent message as an input", "backend", b.ID, "id", m.ID, "error", err.Error())
		} else {
			status, mode = hub.AgentStatusDelivered, hub.AgentModeInput
		}
	}
	if err := r.opts.Store.SetAgentMessageStatus(ctx, m.ID, mode, status); err != nil {
		r.log.Warn("record agent message status", "backend", b.ID, "id", m.ID, "error", err.Error())
	}
	r.log.Info("agent message not read in time", "backend", b.ID, "id", m.ID, "mode", mode, "status", status)
}

// limiter allows each key opLimit calls in opWindow.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const (
	opLimit  = 30
	opWindow = 10 * time.Minute
)

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-opWindow)
	for k, v := range l.hits {
		for len(v) > 0 && v[0].Before(cut) {
			v = v[1:]
		}
		if len(v) == 0 {
			delete(l.hits, k)
		} else {
			l.hits[k] = v
		}
	}
	if len(l.hits[key]) >= opLimit {
		return false
	}
	l.hits[key] = append(l.hits[key], now)
	return true
}
