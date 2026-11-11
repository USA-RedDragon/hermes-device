package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
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
	if err := e.svc.SubmitAgent(ctx, backendPC, sLive, "hermes-hub-agent-am_1", "[Agent message am_1 from x]\nhi"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SubmitAgent(ctx, backendPC, "s-yolo", "hermes-hub-agent-am_2", "x"); !errors.Is(err, service.ErrAgentYolo) {
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
