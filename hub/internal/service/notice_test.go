package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	kName = "name"
	sCron = "s-cron"
	late  = "late"
)

type noticeLog struct {
	mu sync.Mutex
	ns []hub.Notice
}

func (l *noticeLog) add(n hub.Notice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ns = append(l.ns, n)
}

// wait returns the first notice of kind, waiting for it.
func (l *noticeLog) wait(t *testing.T, kind string) hub.Notice {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, n := range l.ns {
			if n.Kind == kind {
				l.mu.Unlock()
				return n
			}
		}
		l.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s notice", kind)
	return hub.Notice{}
}

func TestQuestionsAnswerAndSettle(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	log := &noticeLog{}
	e.svc.SetNotify(log.add)
	ctx := context.Background()
	ch, cancel, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	next(t, ch)
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "pick many"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, hub.KindClarifyRequest)
	n := log.wait(t, hub.NoticeQuestion)
	if n.Prompt == nil || n.Prompt.ID != "cl-1" || !n.Prompt.Multi || n.Title != liveTitle || n.Key != "prompt:"+sLive+":cl-1" {
		t.Fatalf("notice = %+v %+v", n, n.Prompt)
	}
	p, err := e.svc.PendingPrompt(ctx, userAlice, backendPC, sLive, "cl-1")
	if err != nil || p.Question != "Which one?" || len(p.Choices) != 3 {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	if _, err := e.svc.PendingPrompt(ctx, userBob, backendPC, sLive, "cl-1"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob read alice's prompt: %v", err)
	}
	if err := e.svc.AnswerPrompt(ctx, userBob, backendPC, sLive, "cl-1", hub.Answer{Picks: []string{"red"}}); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob answered alice's prompt: %v", err)
	}
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "cl-1", hub.Answer{Picks: []string{"purple"}}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("unknown pick: %v", err)
	}
	if err := e.svc.RespondApproval(ctx, userAlice, backendPC, sLive, "cl-1", true); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("approval answer to a question: %v", err)
	}
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "cl-1", hub.Answer{Picks: []string{"red", "green"}}); err != nil {
		t.Fatal(err)
	}
	evs := until(t, ch, hub.KindMessageComplete)
	if got := text(t, evs[len(evs)-1]); got != `answered: ["red","green"]` {
		t.Fatalf("answer = %q", got)
	}
	if s := log.wait(t, hub.NoticeSettled); len(s.PromptIDs) != 1 || s.PromptIDs[0] != "cl-1" {
		t.Fatalf("settled = %+v", s)
	}
}

func TestApprovalChoicesAndFreeText(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, cancel, _ := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	defer cancel()
	next(t, ch)
	_, _ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "approve me")
	until(t, ch, hub.KindApprovalRequest)
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "ap-1", hub.Answer{Choice: "never"}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("bad choice: %v", err)
	}
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "ap-1", hub.Answer{Choice: hub.ChoiceSession}); err != nil {
		t.Fatal(err)
	}
	evs := until(t, ch, hub.KindMessageComplete)
	if got := text(t, evs[len(evs)-1]); got != "approved: session" {
		t.Fatalf("result = %q", got)
	}
	_, _ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "ask free")
	until(t, ch, hub.KindClarifyRequest)
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "cl-1", hub.Answer{Text: "  "}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("empty answer: %v", err)
	}
	if err := e.svc.AnswerPrompt(ctx, userAlice, backendPC, sLive, "cl-1", hub.Answer{Text: " teal "}); err != nil {
		t.Fatal(err)
	}
	evs = until(t, ch, hub.KindMessageComplete)
	if got := text(t, evs[len(evs)-1]); got != "answered: teal" {
		t.Fatalf("result = %q", got)
	}
}

func TestWatchSeesPromptsOfChatsNobodyHasOpen(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{IdleDetach: 20 * time.Millisecond})
	q := "Deploy now?"
	e.gw.AddSession(&gatewaytest.Session{ID: sCron, Title: "Nightly", Live: true, Running: true, StartedAt: 4000,
		Prompts: []gateway.Prompt{{Kind: "clarify", PromptID: "early", Question: &q, Choices: []string{"yes", "no"}}}})
	log := &noticeLog{}
	e.svc.SetNotify(log.add)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.svc.Watch(ctx, 20*time.Millisecond)
	n := log.wait(t, hub.NoticeQuestion)
	if n.SessionID != sCron || n.Prompt.ID != "early" || n.Title != "Nightly" {
		t.Fatalf("notice = %+v", n)
	}
	time.Sleep(100 * time.Millisecond)
	cmd := "curl example.com"
	e.gw.Publish(sCron, "approval.request", gateway.Prompt{Kind: "approval", PromptID: late, Command: &cmd, Choices: []string{"once", "deny"}})
	a := log.wait(t, hub.NoticeApproval)
	if a.Prompt.ID != late || a.Prompt.Command != cmd {
		t.Fatalf("approval = %+v", a.Prompt)
	}
	e.gw.Publish(sCron, "tool.complete", map[string]any{kName: "read_page",
		"result_text": "A page that says: browser_click was not run: and no user is present in this run"})
	e.gw.Publish(sCron, "tool.complete", map[string]any{kName: "browser_click",
		"result_text": "browser_click was not run: it needs the user's approval, and no user is present in this run"})
	f := log.wait(t, hub.NoticeFailure)
	if f.SessionID != sCron || !strings.Contains(f.Detail, "browser_click") {
		t.Fatalf("failure = %+v", f)
	}
}
