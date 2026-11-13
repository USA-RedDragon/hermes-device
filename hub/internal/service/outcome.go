package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// Hermes v0.23.1 asks for an approval from inside the tool call it holds:
// tool.start comes first (agent/tool_executor.py _begin_tool_execution), the
// tool then waits for the answer, and the call ends with tool.complete. A
// denied call ends with a result whose error starts with "BLOCKED:" and says
// the user denied it (tools/approval.py, tools/file_tools_write_guards.py,
// tools/computer_use/tool.py). approval.request names no tool call, so the
// hub picks the running call that asked, and works out the outcome of an
// approval answered where the hub could not see the choice from how that
// call ends.

// gatedTool reports whether name is a tool that asks for approval while it
// runs and ends with the block when it is denied. Other approvals come
// before their call starts (plugin rules) or from inside tools whose
// results the hub cannot read: execute_code runs terminal and file calls of
// its own with no tool.start (tools/code_execution_rpc.py) and returns their
// blocks to the script, and MCP tools ask for elicitations.
func gatedTool(name string) bool {
	switch name {
	case "terminal", "write_file", "patch", "computer_use":
		return true
	}
	return false
}

// pluginRule ends the command of an approval that a plugin's pre_tool_call
// rule asks for before its call starts (tools/approval.py
// request_tool_approval).
const pluginRule = "(plugin approval rule)"

// outcomeLookup bounds reading a recorded answer.
const outcomeLookup = 5 * time.Second

// runningTool is a tool call that started and has not ended.
type runningTool struct {
	name    string
	command string
}

// gate is an approval and the tool call that asked for it.
type gate struct {
	tool string
	// looking is set while the hub reads the approval's recorded answer,
	// and settled once it settled with none.
	looking bool
	settled bool
	// verdict is how the call ended: nil while it runs.
	verdict *bool
}

type toolEvent struct {
	ToolID string `json:"tool_id"`
	Name   string `json:"name"`
	Args   struct {
		Command any `json:"command"`
	} `json:"args"`
	Result string `json:"result"`
}

type subagentEvent struct {
	SubagentID string `json:"subagent_id"`
}

// trackOutcomes follows the tool calls and subagents of the session, and
// the approvals they ask for. ls.mu is held.
func (ls *live) trackOutcomes(ev gateway.Event) {
	switch ev.Type {
	case hub.KindToolStart:
		var p toolEvent
		if json.Unmarshal(ev.Payload, &p) == nil && p.ToolID != "" {
			cmd, _ := p.Args.Command.(string)
			ls.running[p.ToolID] = runningTool{name: p.Name, command: strings.TrimSpace(cmd)}
		}
	case hub.KindToolComplete:
		var p toolEvent
		if json.Unmarshal(ev.Payload, &p) != nil || p.ToolID == "" {
			return
		}
		delete(ls.running, p.ToolID)
		for id, g := range ls.gates {
			if g.tool != p.ToolID {
				continue
			}
			allowed, ok := toolVerdict(p.Result)
			if !ok {
				delete(ls.gates, id)
				continue
			}
			g.verdict = &allowed
			if g.settled {
				delete(ls.gates, id)
				ls.inferred(id, allowed)
			}
		}
	case "subagent.spawn_requested", "subagent.start":
		var p subagentEvent
		if json.Unmarshal(ev.Payload, &p) == nil && p.SubagentID != "" {
			ls.subagents[p.SubagentID] = true
		}
	case "subagent.complete":
		var p subagentEvent
		if json.Unmarshal(ev.Payload, &p) == nil {
			delete(ls.subagents, p.SubagentID)
		}
	case hub.KindMessageComplete:
		clear(ls.running)
		for id, g := range ls.gates {
			if !g.looking {
				delete(ls.gates, id)
			}
		}
	}
}

// gateApproval links a new approval to the tool call that asked for it:
// the only running terminal call with its command, or else the only
// running call, when it is a tool that asks while it runs and no subagent
// runs that could have asked instead. ls.mu is held.
func (ls *live) gateApproval(p gateway.Prompt) {
	if p.Kind != gateway.PromptApproval {
		return
	}
	var cmd string
	if p.Command != nil {
		cmd = strings.TrimSpace(*p.Command)
	}
	var match []string
	for id, t := range ls.running {
		if cmd != "" && t.name == "terminal" && t.command == cmd {
			match = append(match, id)
		}
	}
	if len(match) == 0 && len(ls.subagents) == 0 && len(ls.running) == 1 && !strings.HasSuffix(cmd, pluginRule) {
		for id, t := range ls.running {
			if gatedTool(t.name) {
				match = append(match, id)
			}
		}
	}
	if len(match) == 1 {
		ls.gates[p.PromptID] = &gate{tool: match[0]}
	}
}

// settleOutcome works out what became of an approval that settled: the
// answer the hub recorded, or else how its tool call ends. ls.mu is held.
func (ls *live) settleOutcome(id string, cancelled bool) {
	if cancelled {
		delete(ls.gates, id)
		return
	}
	if ls.b.svc.opts.Answers == nil {
		// Nothing is recorded without a store: settle in event order, so
		// the outcome cannot trail the turn end that follows.
		ls.unanswered(id)
		return
	}
	if g, ok := ls.gates[id]; ok {
		g.looking = true
	}
	go ls.lookupOutcome(ls.b.svc.opts.Answers, id)
}

// lookupOutcome reads the recorded answer of an approval that settled and
// shows its outcome, or else waits for its tool call. It runs after the
// event, with no request behind it, so the outcome follows approval.settled
// also without a store.
//
//nolint:contextcheck // see above
func (ls *live) lookupOutcome(answers hub.AnswerStore, id string) {
	var (
		a   hub.PromptAnswer
		ok  bool
		err error
	)
	if answers != nil {
		ctx, cancel := context.WithTimeout(context.Background(), outcomeLookup)
		a, ok, err = answers.PromptAnswer(ctx, ls.b.cfg.ID, id)
		cancel()
	}
	if err != nil {
		ls.b.log.Warn("Failed to read an approval's answer", "session", ls.id, "error", err.Error())
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ok {
		delete(ls.gates, id)
		ls.outcome(a.Outcome(id))
		return
	}
	ls.unanswered(id)
}

// unanswered marks an approval settled with no recorded answer, and shows
// its outcome when its tool call already ended. ls.mu is held.
func (ls *live) unanswered(id string) {
	g, ok := ls.gates[id]
	if !ok {
		return
	}
	g.looking, g.settled = false, true
	if g.verdict != nil {
		delete(ls.gates, id)
		ls.inferred(id, *g.verdict)
	}
}

// inferred shows the outcome of an approval answered where the hub could
// not see the choice. ls.mu is held.
func (ls *live) inferred(id string, allowed bool) {
	o := hub.ApprovalOutcome{PromptID: id, Allowed: allowed, Via: hub.AnsweredElsewhere}
	ls.outcome(o)
	ls.b.svc.notice(hub.Notice{Kind: hub.NoticeOutcome, BackendID: ls.b.cfg.ID, SessionID: ls.id, PromptIDs: []string{id}, Outcome: &o})
}

// outcome tells viewers what became of an approval. ls.mu is held.
func (ls *live) outcome(o hub.ApprovalOutcome) {
	data, err := json.Marshal(o)
	if err != nil {
		return
	}
	ls.emit(hub.KindApprovalOutcome, data)
}

// toolVerdict reads how a tool call that waited for an approval ended:
// allowed unless its error is a block that says the user denied it. ok is
// false for a block for another reason, such as a timeout.
func toolVerdict(result string) (allowed, ok bool) {
	text := strings.TrimSpace(result)
	var p struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(text), &p) == nil {
		text, _ = p.Error.(string)
	}
	if !strings.HasPrefix(strings.TrimSpace(text), "BLOCKED:") {
		return true, true
	}
	for _, s := range []string{"denied by user", "denied by the user", "User denied"} {
		if strings.Contains(text, s) {
			return false, true
		}
	}
	return false, false
}
