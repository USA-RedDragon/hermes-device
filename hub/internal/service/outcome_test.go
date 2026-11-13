package service_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

const (
	promptAsked = "ap-1"
	kToolID     = "tool_id"
	kResult     = "result"
	kCommand    = "command"
	evSettled   = "approval.settled"
	toolTerm    = "terminal"
	promptP9    = "p9"
	cmdP9       = "rm -rf x"
)

// memAnswers is an AnswerStore in memory.
type memAnswers struct {
	mu      sync.Mutex
	answers map[string]hub.PromptAnswer
}

func (m *memAnswers) RecordAnswer(_ context.Context, backendID, promptID string, a hub.PromptAnswer) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.answers == nil {
		m.answers = map[string]hub.PromptAnswer{}
	}
	if _, ok := m.answers[backendID+"/"+promptID]; ok {
		return false, nil
	}
	m.answers[backendID+"/"+promptID] = a
	return true, nil
}

func (m *memAnswers) ForgetAnswer(_ context.Context, backendID, promptID string, a hub.PromptAnswer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.answers[backendID+"/"+promptID] == a {
		delete(m.answers, backendID+"/"+promptID)
	}
	return nil
}

func (m *memAnswers) PromptAnswer(_ context.Context, backendID, promptID string) (hub.PromptAnswer, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.answers[backendID+"/"+promptID]
	return a, ok, nil
}

func (m *memAnswers) ClaimOutcome(context.Context, string, string, bool) (bool, error) {
	return true, nil
}

func (m *memAnswers) ClaimedOutcome(context.Context, string, string) (hub.ApprovalOutcome, bool, error) {
	return hub.ApprovalOutcome{}, false, nil
}

func outcomeOf(t *testing.T, evs []hub.Event) (hub.ApprovalOutcome, bool) {
	t.Helper()
	for _, e := range evs {
		if e.Kind == hub.KindApprovalOutcome {
			var o hub.ApprovalOutcome
			if err := json.Unmarshal(e.Data, &o); err != nil {
				t.Fatal(err)
			}
			return o, true
		}
	}
	return hub.ApprovalOutcome{}, false
}

// attachAsked attaches to the live session and has it ask for approval
// ap-1 from its terminal call t-approve.
func attachAsked(t *testing.T, e *env) <-chan hub.Event {
	t.Helper()
	ctx := context.Background()
	ch, cancel, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	next(t, ch)
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "approve me"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, hub.KindApprovalRequest)
	return ch
}

type published struct {
	kind    string
	payload any
}

func terminalStart(id, cmd string) published {
	return published{hub.KindToolStart, map[string]any{kToolID: id, kName: toolTerm, "args": map[string]any{kCommand: cmd}}}
}

func toolDone(id, result string) published {
	return published{hub.KindToolComplete, map[string]any{kToolID: id, kName: toolTerm, kResult: result}}
}

func asks() published {
	cmd := cmdP9
	return published{hub.KindApprovalRequest, gateway.Prompt{Kind: gateway.PromptApproval, PromptID: promptP9, Command: &cmd,
		Choices: []string{hub.ChoiceOnce, hub.ChoiceDeny}}}
}

func settles() published { return published{evSettled, map[string]any{"prompt_id": promptP9}} }

// attachPublish attaches to the live session, publishes evs and a turn
// end, and returns the events up to it.
func attachPublish(t *testing.T, e *env, evs ...published) []hub.Event {
	t.Helper()
	ch, cancel, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	next(t, ch)
	for _, ev := range evs {
		e.gw.Publish(sLive, ev.kind, ev.payload)
	}
	e.gw.Publish(sLive, hub.KindMessageComplete, map[string]any{kText: "done"})
	out := until(t, ch, hub.KindMessageComplete)
	// The hub reads an approval's answer after its settle; a second turn
	// end lets an outcome that follows the first one arrive.
	e.gw.Publish(sLive, hub.KindMessageComplete, map[string]any{kText: "fence"})
	return append(out, until(t, ch, hub.KindMessageComplete)...)
}

func TestOutcomeOfHubAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		via    string
		choice string
	}{{hub.AnsweredInHub, hub.ChoiceAlways}, {hub.AnsweredInTelegram, hub.ChoiceDeny}} {
		t.Run(tc.via, func(t *testing.T) {
			t.Parallel()
			answers := &memAnswers{}
			e := setup(t, service.Options{Answers: answers})
			log := &noticeLog{}
			e.svc.SetNotify(log.add)
			ch := attachAsked(t, e)
			ctx := context.Background()
			if tc.via != hub.AnsweredInHub {
				ctx = hub.WithAnsweredVia(ctx, tc.via)
			}
			if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, promptAsked, hub.Answer{Choice: tc.choice}); err != nil {
				t.Fatal(err)
			}
			o, _ := outcomeOf(t, until(t, ch, hub.KindApprovalOutcome))
			want := hub.ApprovalOutcome{PromptID: promptAsked, Allowed: tc.choice != hub.ChoiceDeny, Choice: tc.choice, Via: tc.via}
			if o != want {
				t.Fatalf("outcome = %+v, want %+v", o, want)
			}
			log.mu.Lock()
			defer log.mu.Unlock()
			for _, n := range log.ns {
				if n.Kind == hub.NoticeOutcome {
					t.Fatalf("outcome notice for a recorded answer: %+v", n)
				}
			}
		})
	}
}

func TestOutcomeOfAnswerElsewhere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		choice  string
		allowed bool
	}{{hub.ChoiceAlways, true}, {hub.ChoiceDeny, false}} {
		t.Run(tc.choice, func(t *testing.T) {
			t.Parallel()
			for _, answers := range []hub.AnswerStore{nil, &memAnswers{}} {
				e := setup(t, service.Options{Answers: answers})
				log := &noticeLog{}
				e.svc.SetNotify(log.add)
				ch := attachAsked(t, e)
				e.gw.AnswerElsewhere(sLive, promptAsked, tc.choice)
				o, _ := outcomeOf(t, until(t, ch, hub.KindApprovalOutcome))
				want := hub.ApprovalOutcome{PromptID: promptAsked, Allowed: tc.allowed, Via: hub.AnsweredElsewhere}
				if o != want {
					t.Fatalf("outcome = %+v, want %+v", o, want)
				}
				n := log.wait(t, hub.NoticeOutcome)
				if n.Outcome == nil || *n.Outcome != want || n.SessionID != sLive || n.BackendID != backendPC {
					t.Fatalf("notice = %+v", n)
				}
			}
		})
	}
}

func TestNoOutcomeWithoutItsToolCall(t *testing.T) {
	t.Parallel()
	const cmd = cmdP9
	for name, evs := range map[string][]published{
		"two calls": {terminalStart("t1", "ls"), terminalStart("t2", "pwd"), asks(), settles(),
			toolDone("t1", "ok"), toolDone("t2", "ok")},
		"subagent runs": {{"subagent.start", map[string]any{"subagent_id": "sa1"}}, terminalStart("t1", "ls"), asks(),
			settles(), toolDone("t1", "ok")},
		"execute_code": {{hub.KindToolStart, map[string]any{kToolID: "t1", kName: "execute_code"}}, asks(),
			settles(), toolDone("t1", "ok")},
		"plugin rule": {terminalStart("t1", "ls"),
			{hub.KindApprovalRequest, gateway.Prompt{Kind: gateway.PromptApproval, PromptID: promptP9,
				Command: ptr("<terminal> (plugin approval rule)"), Choices: []string{hub.ChoiceOnce, hub.ChoiceDeny}}},
			settles(), toolDone("t1", "ok")},
		"not a gated tool": {{hub.KindToolStart, map[string]any{kToolID: "t1", kName: "delegate_task"}}, asks(),
			settles(), toolDone("t1", "ok")},
		"cancelled": {terminalStart("t1", cmd), asks(),
			{"approval.cancelled", map[string]any{"request_ids": []string{promptP9}}}, toolDone("t1", "ok")},
		"timed out": {terminalStart("t1", cmd), asks(), settles(),
			toolDone("t1", `{"error": "BLOCKED: Command timed out without user response."}`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if o, ok := outcomeOf(t, attachPublish(t, setup(t, service.Options{}), evs...)); ok {
				t.Fatalf("outcome %+v", o)
			}
		})
	}
}

func TestOutcomeMatchesTheCommand(t *testing.T) {
	t.Parallel()
	const cmd = cmdP9
	evs := attachPublish(t, setup(t, service.Options{}), published{"subagent.start", map[string]any{"subagent_id": "sa1"}},
		terminalStart("t1", "ls"), terminalStart("t2", cmd), asks(), settles(),
		toolDone("t1", `{"error": "BLOCKED: User denied this command."}`), toolDone("t2", `{"output": "", "exit_code": 0}`))
	o, _ := outcomeOf(t, evs)
	if want := (hub.ApprovalOutcome{PromptID: promptP9, Allowed: true, Via: hub.AnsweredElsewhere}); o != want {
		t.Fatalf("outcome = %+v, want %+v", o, want)
	}
}
