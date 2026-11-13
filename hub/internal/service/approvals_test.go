package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
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
