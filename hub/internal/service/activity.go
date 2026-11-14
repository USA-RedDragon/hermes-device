package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var (
	_ hub.Activity  = (*Service)(nil)
	_ hub.ToolMover = (*Service)(nil)
)

// maxSteer bounds a message to a subagent, maxBackground a /bg prompt.
const (
	maxSteer      = maxAnswer
	maxBackground = 1 << 20
)

// reasonProcessNotFound is process.kill's refusal of a process the session
// does not own (gateway/session_ancillary.py kill_process).
const reasonProcessNotFound = "process_not_found"

// Activity lists the session's subagents and background commands. The
// gateway answers for the named session only; delegation.status also lists
// every chat's subagents, which the hub drops.
func (s *Service) Activity(ctx context.Context, user, backendID, sessionID string) (hub.ActivityState, error) {
	out := hub.ActivityState{Subagents: []hub.Subagent{}, Processes: []hub.Process{}}
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		subs, err := b.subagents(ctx, ls)
		if err != nil {
			return err
		}
		for _, r := range subs {
			sa := subagent(r)
			s.keepProgress(ctx, hub.SubagentKey{User: user, BackendID: backendID, SessionID: sessionID, SubagentID: sa.ID}, &sa)
			out.Subagents = append(out.Subagents, sa)
		}
		var procs gateway.ProcessList
		if err := b.read(ctx, ls, gateway.MethodProcessList, gateway.SessionParams{SessionID: ls.id}, &procs); err != nil {
			return refusal(err)
		}
		for _, p := range procs.Processes {
			out.Processes = append(out.Processes, process(p))
		}
		var st gateway.DelegationStatus
		if err := b.read(ctx, ls, gateway.MethodDelegationStatus, map[string]any{}, &st); err != nil {
			b.log.Debug("delegation.status", "error", err.Error())
		}
		out.Paused = st.Paused
		return nil
	})
	return out, err
}

// keepProgress keeps a listed subagent's start and tool count from moving
// backward. A restart hands a running subagent to a new Hermes process that
// may list a later start and a lower count; the hub shows the earliest start
// and the highest count it ever saw, stored so a hub restart keeps them.
func (s *Service) keepProgress(ctx context.Context, k hub.SubagentKey, sa *hub.Subagent) {
	if k.SubagentID == "" {
		return
	}
	if log := s.opts.SubagentLog; log != nil {
		started, tools, err := log.KeepSubagentProgress(ctx, k, sa.StartedAt, sa.ToolCount)
		if err == nil {
			sa.StartedAt, sa.ToolCount = started, tools
			return
		}
		s.log.Debug("subagent progress", "error", err.Error())
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	if s.progress == nil {
		s.progress = map[hub.SubagentKey]subagentProgress{}
	}
	p := s.progress[k]
	if sa.StartedAt > 0 && (p.started <= 0 || sa.StartedAt < p.started) {
		p.started = sa.StartedAt
	}
	p.tools = max(p.tools, sa.ToolCount)
	s.progress[k] = p
	sa.StartedAt, sa.ToolCount = p.started, p.tools
}

type subagentProgress struct {
	started float64
	tools   int
}

func (b *backend) subagents(ctx context.Context, ls *live) ([]gateway.SubagentRow, error) {
	var res gateway.SubagentList
	if err := b.read(ctx, ls, gateway.MethodSubagentList, gateway.SessionParams{SessionID: ls.id}, &res); err != nil {
		return nil, refusal(err)
	}
	return res.Subagents, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func subagent(r gateway.SubagentRow) hub.Subagent {
	return hub.Subagent{ID: r.SubagentID, ParentID: deref(r.ParentID), Depth: deref(r.Depth), Goal: r.Goal,
		Model: deref(r.Model), StartedAt: deref(r.StartedAt), Status: deref(r.Status), ToolCount: deref(r.ToolCount),
		LastTool: deref(r.LastTool), LastPreview: deref(r.LastPreview), AcceptingSteer: deref(r.AcceptingSteer), ChildSessionID: deref(r.ChildSessionID)}
}

func process(p gateway.ProcessRow) hub.Process {
	return hub.Process{ID: p.SessionID, Command: p.Command, Running: p.Status == "running", ExitCode: p.ExitCode,
		StartedAt: deref(p.StartedEpoch), ExitedAt: deref(p.ExitedAt), Reason: deref(p.CompletionReason), Output: p.OutputTail}
}

// ownSubagent runs fn when subagentID is a subagent the session runs now.
// The gateway checks this too; the hub does not rely on it alone.
func (s *Service) ownSubagent(ctx context.Context, user, backendID, sessionID, subagentID string,
	fn func(*backend, *live) error) error {
	if subagentID == "" {
		return hub.ErrUnknownSubagent
	}
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		subs, err := b.subagents(ctx, ls)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(subs, func(r gateway.SubagentRow) bool { return r.SubagentID == subagentID }) {
			return hub.ErrUnknownSubagent
		}
		return fn(b, ls)
	})
}

// SubagentTail is the end of a running subagent's transcript.
func (s *Service) SubagentTail(ctx context.Context, user, backendID, sessionID, subagentID string) (hub.SubagentTail, error) {
	var out hub.SubagentTail
	err := s.ownSubagent(ctx, user, backendID, sessionID, subagentID, func(b *backend, ls *live) error {
		var res gateway.SubagentTail
		if err := b.read(ctx, ls, gateway.MethodSubagentTail, gateway.SubagentParams{SessionID: ls.id, SubagentID: subagentID}, &res); err != nil {
			return refusal(err)
		}
		out = hub.SubagentTail{Available: res.Available, Text: res.Text, Truncated: res.Truncated}
		return nil
	})
	return out, err
}

// SteerSubagent queues text for a running subagent.
func (s *Service) SteerSubagent(ctx context.Context, user, backendID, sessionID, subagentID, text string) error {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return fmt.Errorf("%w: the message is empty", hub.ErrBadValue)
	case len(text) > maxSteer:
		return fmt.Errorf("%w: the message is too long", hub.ErrBadValue)
	}
	return s.ownSubagent(ctx, user, backendID, sessionID, subagentID, func(b *backend, ls *live) error {
		var res gateway.SteerResult
		if err := b.call(ctx, ls, gateway.MethodSubagentSteer,
			gateway.SteerParams{SessionID: ls.id, SubagentID: subagentID, Text: text}, &res); err != nil {
			return refusal(err)
		}
		if res.Status != "queued" {
			return hub.ErrSteerRejected
		}
		s.recordSteer(ctx, user, backendID, sessionID, subagentID, text)
		return nil
	})
}

// StopSubagent interrupts a running subagent.
func (s *Service) StopSubagent(ctx context.Context, user, backendID, sessionID, subagentID string) error {
	return s.ownSubagent(ctx, user, backendID, sessionID, subagentID, func(b *backend, ls *live) error {
		var res gateway.InterruptResult
		if err := b.call(ctx, ls, gateway.MethodSubagentInterrupt,
			gateway.SubagentParams{SessionID: ls.id, SubagentID: subagentID}, &res); err != nil {
			return refusal(err)
		}
		if !res.Found {
			return hub.ErrUnknownSubagent
		}
		return nil
	})
}

// StopProcess kills one background command of the session, never a
// process of another chat: the hub checks it in the session's own list
// first, and process.kill checks it again.
func (s *Service) StopProcess(ctx context.Context, user, backendID, sessionID, processID string) error {
	if processID == "" {
		return hub.ErrUnknownProcess
	}
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		var procs gateway.ProcessList
		if err := b.read(ctx, ls, gateway.MethodProcessList, gateway.SessionParams{SessionID: ls.id}, &procs); err != nil {
			return refusal(err)
		}
		if !slices.ContainsFunc(procs.Processes, func(p gateway.ProcessRow) bool { return p.SessionID == processID }) {
			return hub.ErrUnknownProcess
		}
		var res gateway.ProcessKillResult
		err := b.call(ctx, ls, gateway.MethodProcessKill, gateway.ProcessKillParams{SessionID: ls.id, ProcessID: processID}, &res)
		var rerr *gateway.RPCError
		if errors.As(err, &rerr) && rerr.Message == reasonProcessNotFound {
			return hub.ErrUnknownProcess
		}
		return refusal(err)
	})
}

// PauseSubagents sets Hermes's switch for new subagents. The gateway
// checks that the hub may control the session; the switch covers every
// chat of the backend.
func (s *Service) PauseSubagents(ctx context.Context, user, backendID, sessionID string, paused bool) (bool, error) {
	var out gateway.DelegationStatus
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		return refusal(b.call(ctx, ls, gateway.MethodDelegationPause, gateway.DelegationPauseParams{SessionID: ls.id, Paused: paused}, &out))
	})
	return out.Paused, err
}

// Background starts a /bg prompt and reports it to the session's viewers
// as a background.start event, so each of them can show it while it runs.
func (s *Service) Background(ctx context.Context, user, backendID, sessionID, text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", fmt.Errorf("%w: the prompt is empty", hub.ErrBadValue)
	case len(text) > maxBackground:
		return "", fmt.Errorf("%w: the prompt is too long", hub.ErrBadValue)
	}
	var res gateway.BackgroundResult
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		if err := b.call(ctx, ls, gateway.MethodPromptBackground, gateway.BackgroundParams{SessionID: ls.id, Text: text}, &res); err != nil {
			return refusal(err)
		}
		if res.TaskID == "" {
			return errors.New("the backend did not start the prompt")
		}
		ls.backgroundStarted(res.TaskID, text)
		return nil
	})
	return res.TaskID, err
}

// BackgroundTool asks Hermes to move the turn's running terminal command
// to the background. Hermes only asks: the command hands its process over
// when it next checks, and tools that are not commands go on as they are.
func (s *Service) BackgroundTool(ctx context.Context, user, backendID, sessionID, toolCallID string) error {
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		c, err := b.ensure(ctx, ls)
		if err != nil {
			return refusal(err)
		}
		cctx, cancel := b.timeout(ctx)
		capable := b.capable(cctx, c, gateway.CapToolBackground)
		cancel()
		if !capable {
			return hub.ErrNoToolBackground
		}
		ls.mu.Lock()
		gen := ls.generation
		ls.mu.Unlock()
		var res gateway.BackgroundToolResult
		if err := b.call(ctx, ls, gateway.MethodBackgroundTool,
			gateway.BackgroundToolParams{SessionID: ls.id, ExecutionGeneration: gen, ToolCallID: toolCallID}, &res); err != nil {
			return refusal(err)
		}
		if res.Status != gateway.BackgroundToolRequested {
			return hub.ErrNoRunningTool
		}
		return nil
	})
}

// capable reports whether runtime.describe on c lists capability. The
// list is read once per connection; a failed read counts as no.
func (b *backend) capable(ctx context.Context, c *gateway.Client, capability string) bool {
	b.capsMu.Lock()
	defer b.capsMu.Unlock()
	if b.capsOf != c {
		var d gateway.RuntimeDescription
		if err := c.Call(ctx, gateway.MethodRuntimeDescribe, map[string]any{}, &d); err != nil {
			b.log.Debug(gateway.MethodRuntimeDescribe, "error", err.Error())
			return false
		}
		b.capsOf, b.caps = c, d.Capabilities
	}
	return slices.Contains(b.caps, capability)
}
