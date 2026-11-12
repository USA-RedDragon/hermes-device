package service

import (
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
)

func TestSteeringRows(t *testing.T) {
	t.Parallel()
	ps := []gateway.PendingAdmission{
		{AdmissionID: "steer", Sequence: 7, Status: gateway.AdmissionStarted},
		{AdmissionID: "turn", Sequence: 5, Status: gateway.AdmissionStarted},
		{AdmissionID: "next", Sequence: 8, Status: gateway.AdmissionQueued},
	}
	turn := runningAdmission(ps, "")
	if turn != "turn" {
		t.Fatalf("turn = %q", turn)
	}
	if runningAdmission(ps, "seen") != "seen" {
		t.Fatal("the turn the hub saw start does not win")
	}
	for _, p := range ps {
		if got, want := steering(p, turn), p.AdmissionID == "steer"; got != want {
			t.Fatalf("steering(%s) = %v", p.AdmissionID, got)
		}
	}
}
