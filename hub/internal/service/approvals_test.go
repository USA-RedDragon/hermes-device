package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

func approvalPrompt(id string) gateway.Prompt {
	cmd := "cleanup " + id
	return gateway.Prompt{Kind: gateway.PromptApproval, PromptID: id, Command: &cmd,
		Choices: []string{gateway.ChoiceOnce, gateway.ChoiceDeny}}
}

const freshApproval = "fresh-one"

func concurrentIDs() []string { return []string{"ap-x", "ap-y", "ap-z"} }

// Every approval pending at once reaches the viewer as an event of its own,
// and each one stays pending until it is answered.
func TestConcurrentApprovalsEachGetARequest(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, cancel, _ := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	defer cancel()
	next(t, ch)
	for _, id := range concurrentIDs() {
		e.gw.Ask(sLive, approvalPrompt(id))
	}
	var got []string
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-ch:
			if ev.Kind != hub.KindApprovalRequest {
				continue
			}
			var p struct {
				ID string `json:"prompt_id"`
			}
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatal(err)
			}
			got = append(got, p.ID)
		case <-deadline:
			t.Fatalf("requests = %v", got)
		}
	}
	if !slices.Equal(got, concurrentIDs()) {
		t.Fatalf("requests = %v", got)
	}
	for _, id := range []string{"ap-z", "ap-x", "ap-y"} {
		if _, err := e.svc.PendingPrompt(ctx, userAlice, backendPC, sLive, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// Approvals that Hermes dropped while the hub was cut off (a restart) are
// settled as lost, so no card keeps buttons that cannot work, and one the
// new gateway lists that no event announced is sent as a request.
func TestResumeSettlesLostApprovalsAndOffersNewOnes(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	log := &noticeLog{}
	e.svc.SetNotify(log.add)
	ctx := context.Background()
	ch, cancel, _ := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	defer cancel()
	next(t, ch)
	for _, id := range []string{"old-1", "old-2", "keep"} {
		e.gw.Ask(sLive, approvalPrompt(id))
	}
	seen := 0
	for deadline := time.After(5 * time.Second); seen < 3; {
		select {
		case ev := <-ch:
			if ev.Kind == hub.KindApprovalRequest {
				seen++
			}
		case <-deadline:
			t.Fatal("requests did not arrive")
		}
	}
	e.gw.Withdraw(sLive, "old-1", "old-2")
	e.gw.AskSilently(sLive, approvalPrompt(freshApproval))
	e.gw.DropConnections()
	var lost, asked []string
	deadline := time.After(5 * time.Second)
	for len(lost) < 2 || len(asked) < 1 {
		select {
		case ev := <-ch:
			var p struct {
				IDs []string `json:"prompt_ids"`
				ID  string   `json:"prompt_id"`
			}
			_ = json.Unmarshal(ev.Data, &p)
			switch ev.Kind {
			case hub.KindPromptLost:
				lost = append(lost, p.IDs...)
			case hub.KindApprovalRequest:
				asked = append(asked, p.ID)
			}
		case <-deadline:
			t.Fatalf("lost = %v, asked = %v", lost, asked)
		}
	}
	slices.Sort(lost)
	if !slices.Equal(lost, []string{"old-1", "old-2"}) || !slices.Equal(asked, []string{freshApproval}) {
		t.Fatalf("lost = %v, asked = %v", lost, asked)
	}
	if n := log.wait(t, hub.NoticeSettled); !n.Cancelled || len(n.PromptIDs) != 2 {
		t.Fatalf("settled notice = %+v", n)
	}
	if _, err := e.svc.PendingPrompt(ctx, userAlice, backendPC, sLive, "old-1"); !errors.Is(err, hub.ErrUnknownApproval) {
		t.Fatalf("lost prompt still pending: %v", err)
	}
	for _, id := range []string{"keep", freshApproval} {
		if _, err := e.svc.PendingPrompt(ctx, userAlice, backendPC, sLive, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// An answer Hermes refuses as stale (it restarted since it asked) drops the
// prompt, so the chat list stops showing an approval no card can answer.
func TestStaleApprovalAnswerClearsPromptAndBadge(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-stale", Title: "Stale", Live: true, StartedAt: 1200, Running: true,
		Prompts: []gateway.Prompt{approvalPrompt("ap-old")}})
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-stale", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	needs := func() string {
		ss, err := e.svc.Sessions(ctx, userAlice)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			if s.ID == "s-stale" {
				return s.Needs
			}
		}
		return "missing"
	}
	if got := needs(); got != hub.NeedsApproval {
		t.Fatalf("needs = %q before the answer", got)
	}
	e.gw.StaleApprovals = true
	if err := e.svc.RespondApproval(ctx, userAlice, backendPC, "s-stale", "ap-old", true); !errors.Is(err, hub.ErrUnknownApproval) {
		t.Fatalf("answer = %v, want unknown approval", err)
	}
	if got := needs(); got != "" {
		t.Fatalf("needs = %q after a stale answer, want none", got)
	}
}

// An approval a subagent asked for in a turn that has ended still has a
// card for a viewer that opens the chat later.
func TestNewViewerGetsApprovalOfEndedTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	first, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, first)
	goal := "tidy the repo"
	sub := approvalPrompt("ap-sub")
	sub.Subagent = &goal
	e.gw.Ask(sLive, sub)
	e.gw.Ask(sLive, approvalPrompt("ap-turn"))
	e.gw.AnswerElsewhere(sLive, "ap-turn", gateway.ChoiceOnce)
	for deadline := time.After(5 * time.Second); ; {
		select {
		case ev := <-first:
			if ev.Kind != hub.KindMessageComplete {
				continue
			}
		case <-deadline:
			t.Fatal("the turn did not end")
		}
		break
	}
	second, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, hub.SinceNow)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	ev := next(t, second)
	var snap struct {
		Prompts []struct {
			ID string `json:"prompt_id"`
		} `json:"prompts"`
	}
	if ev.Kind != hub.KindSnapshot || json.Unmarshal(ev.Data, &snap) != nil {
		t.Fatalf("first event = %s", ev.Kind)
	}
	if len(snap.Prompts) != 1 || snap.Prompts[0].ID != "ap-sub" {
		t.Fatalf("snapshot prompts = %+v, want only the subagent's ap-sub", snap.Prompts)
	}
}
