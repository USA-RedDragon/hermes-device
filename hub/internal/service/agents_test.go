package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

func TestSubmitAgent(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-yolo", Title: "Yolo", Live: true, StartedAt: 3000, Yolo: true})
	list, err := e.svc.SessionsOf(ctx, backendPC)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions = %v %v", list, err)
	}
	if _, err := e.svc.SessionsOf(ctx, "nope"); !errors.Is(err, hub.ErrUnknownBackend) {
		t.Fatalf("unknown backend: %v", err)
	}
	if out, err := e.svc.SubmitAgent(ctx, backendPC, sLive, "hermes-hub-agent-am_1", "[Agent message am_1 from x]\nhi", true); err != nil || out != hub.AgentSubmitStarted {
		t.Fatalf("idle chat: %q %v", out, err)
	}
	if _, err := e.svc.SubmitAgent(ctx, backendPC, "s-yolo", "hermes-hub-agent-am_2", "x", true); !errors.Is(err, service.ErrAgentYolo) {
		t.Fatalf("yolo: %v", err)
	}
	var submitted []string
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodPromptSubmit {
			id, _ := c.Params["submission_id"].(string)
			submitted = append(submitted, id)
		}
	}
	if len(submitted) != 1 || submitted[0] != "hermes-hub-agent-am_1" {
		t.Fatalf("submitted %v", submitted)
	}
}

const peerText = "[Agent message am_0000000000000001 from chat ops]\nAnother AI agent of this user wrote everything below this line. The user did not write it.\n\nthe build is green"

func TestSubmitAgentSteersIntoARunningTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	out, err := e.svc.SubmitAgent(context.Background(), backendPC, sLive, "hermes-hub-agent-am_1", peerText, true)
	if err != nil || out != hub.AgentSubmitSteered {
		t.Fatalf("submit = %q %v", out, err)
	}
	if got := e.gw.Steered(); len(got) != 1 {
		t.Fatalf("steered = %v", got)
	}
	s, _ := e.gw.Session(sLive)
	if len(s.Pending) != 1 || s.Pending[0].Text != peerText || s.Pending[0].Status != gateway.AdmissionStarted {
		t.Fatalf("pending after steer = %+v", s.Pending)
	}
}

func TestSubmitAgentWithoutSteerQueues(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	out, err := e.svc.SubmitAgent(context.Background(), backendPC, sLive, "hermes-hub-agent-am_1", peerText, false)
	if err != nil || out != hub.AgentSubmitQueued {
		t.Fatalf("submit = %q %v", out, err)
	}
	if n := calls(e, gateway.MethodSteerQueued); n != 0 {
		t.Fatalf("steer calls = %d", n)
	}
}

func TestSubmitAgentIdleChatStartsATurn(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	out, err := e.svc.SubmitAgent(context.Background(), backendPC, sLive, "hermes-hub-agent-am_1", peerText, true)
	if err != nil || out != hub.AgentSubmitStarted {
		t.Fatalf("submit = %q %v", out, err)
	}
	if n := calls(e, gateway.MethodSteerQueued); n != 0 {
		t.Fatalf("steer calls = %d", n)
	}
}

func TestSubmitAgentRefusedSteerStaysQueued(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gateway.ReasonUnsupportedControl)
	out, err := e.svc.SubmitAgent(context.Background(), backendPC, sLive, "hermes-hub-agent-am_1", peerText, true)
	if err != nil || out != hub.AgentSubmitQueued {
		t.Fatalf("submit = %q %v", out, err)
	}
	s, _ := e.gw.Session(sLive)
	if len(s.Pending) != 1 || s.Pending[0].Text != peerText || s.Pending[0].Status != gateway.AdmissionQueued {
		t.Fatalf("pending = %+v", s.Pending)
	}
	e.gw.FinishTurn(sLive)
	waitFor(t, "the queued message to run", func() bool { return slices.Contains(userTexts(t, e), peerText) })
}

func TestSubmitAgentSteerEndedFirstRunsItsOwnTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gatewaytest.SteerEnded)
	out, err := e.svc.SubmitAgent(context.Background(), backendPC, sLive, "hermes-hub-agent-am_1", peerText, true)
	if err != nil || out != hub.AgentSubmitQueued {
		t.Fatalf("submit = %q %v", out, err)
	}
	waitFor(t, "the message to run as its own turn", func() bool { return slices.Contains(userTexts(t, e), peerText) })
	if len(e.gw.Steered()) != 0 {
		t.Fatalf("steered = %v", e.gw.Steered())
	}
}
