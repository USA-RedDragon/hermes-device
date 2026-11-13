package service

import (
	"encoding/json"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// counts are a session's cumulative token counters: Prompt is every token
// sent in, cached or not (prompt_tokens), Output every token sent back.
type counts struct {
	Prompt, Output int64
}

// usageCounters is the part of a usage payload turn tokens come from: the
// top-level counters of session.usage (gateway/session_usage.py
// usage_stats) and the status-bar usage object that session.usage,
// message.complete and snapshots carry (tui_usage).
type usageCounters struct {
	PromptTokens   *int64 `json:"prompt_tokens"`
	OutputTokens   *int64 `json:"output_tokens"`
	LastTurnTokens *int64 `json:"last_turn_tokens"`
	Usage          *struct {
		Prompt *int64 `json:"prompt"`
		Output *int64 `json:"output"`
	} `json:"usage"`
}

// cumulative is the counters of a usage payload, from the top level when
// it has them, else from its usage object.
func (u usageCounters) cumulative() (counts, bool) {
	if u.PromptTokens != nil && u.OutputTokens != nil {
		return counts{Prompt: *u.PromptTokens, Output: *u.OutputTokens}, true
	}
	if u.Usage != nil && u.Usage.Prompt != nil && u.Usage.Output != nil {
		return counts{Prompt: *u.Usage.Prompt, Output: *u.Usage.Output}, true
	}
	return counts{}, false
}

// usageObject reads a bare status-bar usage object, as a snapshot's info
// carries it.
func usageObject(raw json.RawMessage) (counts, bool) {
	var u struct {
		Prompt *int64 `json:"prompt"`
		Output *int64 `json:"output"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil || u.Prompt == nil || u.Output == nil {
		return counts{}, false
	}
	return counts{Prompt: *u.Prompt, Output: *u.Output}, true
}

// turnTokens works out the tokens of the running turn from a session.usage
// payload with top-level counters. Hermes counts output per turn itself
// (last_turn_tokens, from the agent's output count at the turn's start);
// the input of the turn is the growth of prompt_tokens since base, the
// counters the hub saw before the turn. base is used only when its output
// count is the one the agent had at the turn's start: when Hermes built a
// new agent for the turn, its counters started at zero, and when the hub
// missed the start, base is wrong.
func turnTokens(u usageCounters, base *counts) (hub.TurnTokens, bool) {
	cur, ok := u.cumulative()
	if !ok || u.LastTurnTokens == nil || u.PromptTokens == nil {
		return hub.TurnTokens{}, false
	}
	out := hub.TurnTokens{Output: max(0, *u.LastTurnTokens)}
	start := cur.Output - out.Output
	var in int64
	switch {
	case start == 0:
		in = cur.Prompt
	case base != nil && base.Output == start && cur.Prompt >= base.Prompt:
		in = cur.Prompt - base.Prompt
	default:
		return out, true
	}
	out.Input = &in
	return out, true
}

// withMember returns payload, a JSON object, with key set to v.
func withMember(payload json.RawMessage, key string, v any) json.RawMessage {
	var p map[string]json.RawMessage
	if json.Unmarshal(payload, &p) != nil || p == nil {
		return payload
	}
	b, err := json.Marshal(v)
	if err != nil {
		return payload
	}
	p[key] = b
	out, err := json.Marshal(p)
	if err != nil {
		return payload
	}
	return out
}

// trackTurn follows the token counters of a session's turns and adds the
// turn's tokens to session.usage and message.complete payloads. ls.mu is
// held.
func (ls *live) trackTurn(ev gateway.Event) json.RawMessage {
	switch ev.Type {
	case hub.KindMessageStart:
		ls.turnAdmission = ev.AdmissionID
		ls.turnBase, ls.turn = nil, nil
		if ls.usage != nil {
			base := *ls.usage
			ls.turnBase = &base
		}
	case gateway.UsageEvent:
		var u usageCounters
		if json.Unmarshal(ev.Payload, &u) != nil {
			return ev.Payload
		}
		if c, ok := u.cumulative(); ok {
			ls.usage = &c
		}
		t, ok := turnTokens(u, ls.turnBase)
		if !ok {
			return ev.Payload
		}
		ls.turn = &t
		return withMember(ev.Payload, hub.TurnKey, t)
	case hub.KindMessageComplete:
		if ev.AdmissionID == "" || ev.AdmissionID == ls.turnAdmission {
			ls.turnAdmission = ""
		}
		var u usageCounters
		if json.Unmarshal(ev.Payload, &u) == nil {
			if c, ok := u.cumulative(); ok {
				ls.usage = &c
			}
		}
		t := ls.turn
		ls.turnBase, ls.turn = nil, nil
		if t != nil {
			return withMember(ev.Payload, hub.TurnKey, *t)
		}
	}
	return ev.Payload
}

// subscribedUsage sets the counters the hub knows from a fresh snapshot.
// For a running session they are those of the last turn end in evs, the
// frames before the running turn, so the turn's tokens can be told apart;
// for an idle one, the snapshot's usage object. ls.mu is held.
func (ls *live) subscribedUsage(running bool, usage json.RawMessage, evs []gateway.Event) {
	ls.usage, ls.turnBase, ls.turn = nil, nil, nil
	if !running {
		if c, ok := usageObject(usage); ok {
			ls.usage = &c
		}
		return
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type != hub.KindMessageComplete {
			continue
		}
		var u usageCounters
		if json.Unmarshal(evs[i].Payload, &u) == nil {
			if c, ok := u.cumulative(); ok {
				ls.usage, ls.turnBase = &c, &c
			}
		}
		return
	}
}

// BackgroundStartEvent is the event the hub sends a session's viewers when
// it starts a /bg prompt: Data is {"task_id", "text", "started_at"}.
const BackgroundStartEvent = "background.start"

// backgroundStarted tells the session's viewers about a /bg prompt the hub
// started.
func (ls *live) backgroundStarted(taskID, text string) {
	data, err := json.Marshal(map[string]any{"task_id": taskID, "text": text,
		"started_at": float64(time.Now().UnixMilli()) / 1000})
	if err != nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.emit(BackgroundStartEvent, data)
}
