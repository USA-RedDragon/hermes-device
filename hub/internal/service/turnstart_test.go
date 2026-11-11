package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	subTerminal = "term-1"
	sTerm       = "s-term"
	kRole       = "role"
	fromTerm    = "from the terminal"
)

type startData struct {
	Text         string `json:"text"`
	Note         string `json:"note"`
	SubmissionID string `json:"submission_id"`
	AdmissionID  string `json:"admission_id"`
	StartedAt    int64  `json:"started_at"`
}

func lastStart(t *testing.T, evs []hub.Event) startData {
	t.Helper()
	var p startData
	if err := json.Unmarshal(evs[len(evs)-1].Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTurnFromElsewhereCarriesItsPrompt(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	e.gw.StartElsewhere(sLive, subTerminal, fromTerm, false, true)
	p := lastStart(t, until(t, ch, hub.KindMessageStart))
	if p.Text != fromTerm || p.SubmissionID != subTerminal || p.AdmissionID == "" || p.StartedAt == 0 || p.Note != "" {
		t.Fatalf("message.start = %+v", p)
	}
}

func TestHubPromptCarriesHubSubmissionID(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "from the hub")
	if err != nil {
		t.Fatal(err)
	}
	p := lastStart(t, until(t, ch, hub.KindMessageStart))
	if !hub.FromHub(res.ID) || p.SubmissionID != res.ID || p.Text != "from the hub" {
		t.Fatalf("prompt %q, message.start = %+v", res.ID, p)
	}
}

// A viewer that attaches after a turn started elsewhere gets the prompt on
// the replayed message.start only when the snapshot does not show it, so
// the web UI shows it once.
func TestAttachMidTurnAddsPromptOnce(t *testing.T) {
	t.Parallel()
	for _, stored := range []bool{false, true} {
		e := setup(t, service.Options{})
		e.gw.AddSession(&gatewaytest.Session{ID: sTerm, Live: true, Source: "tui",
			Messages: []map[string]any{{kRole: roleUser, kText: "hi"}, {kRole: "assistant", kText: "hello"}}})
		e.gw.StartElsewhere(sTerm, subTerminal, fromTerm, true, stored)
		ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sTerm, hub.SinceNow)
		if err != nil {
			t.Fatal(err)
		}
		evs := until(t, ch, hub.KindMessageStart)
		stop()
		var snap hub.Snapshot
		if evs[0].Kind != hub.KindSnapshot || json.Unmarshal(evs[0].Data, &snap) != nil || !snap.Running {
			t.Fatalf("first event = %s %s", evs[0].Kind, evs[0].Data)
		}
		open, shown := hub.OpenTurnPrompt(snap.Messages)
		p := lastStart(t, evs)
		if shown != stored || shown && open.Text != fromTerm || p.SubmissionID != subTerminal {
			t.Fatalf("stored %v: open %+v %v, message.start %+v", stored, open, shown, p)
		}
		if want := map[bool]string{false: fromTerm, true: ""}[stored]; p.Text != want {
			t.Fatalf("stored %v: message.start text = %q, want %q", stored, p.Text, want)
		}
	}
}
