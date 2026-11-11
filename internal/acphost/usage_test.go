package acphost_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/internal/acphost"
	"github.com/USA-RedDragon/hermes-device/internal/claudeusage"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

type fakeUsage struct{ notReady bool }

func (f fakeUsage) Ready() bool { return !f.notReady }

func (fakeUsage) Report() claudeusage.Report {
	return claudeusage.Report{Rows: []claudeusage.Row{{Hour: 3600, Model: "claude-opus-5-5", Input: 7, Requests: 1}},
		Accounts: []claudeusage.Account{{Label: "Claude (work)", Slot: "work"}}}
}

func (h *harness) getUsage(header string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.url+protocol.ACPHostUsagePath, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if header != "" {
		req.Header.Set(assertion.Header, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestUsageNeedsAUsageAssertion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *acphost.Options) { o.Usage = fakeUsage{} })
	c := h.claims()
	c.Leg, c.Path = assertion.LegUsage, protocol.ACPHostUsagePath
	resp := h.getUsage(h.sign(c))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var rep claudeusage.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil || len(rep.Rows) != 1 || rep.Rows[0].Input != 7 || rep.Accounts[0].Slot != "work" {
		t.Fatalf("report %+v %v", rep, err)
	}
	for name, header := range map[string]string{
		"none":    "",
		"acp leg": h.sign(h.claims()),
		"replay":  h.sign(c),
	} {
		if name == "replay" {
			_ = h.getUsage(header)
		}
		if resp := h.getUsage(header); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
}

func TestUsageBeforeFirstScan(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *acphost.Options) { o.Usage = fakeUsage{notReady: true} })
	c := h.claims()
	c.Leg, c.Path = assertion.LegUsage, protocol.ACPHostUsagePath
	if resp := h.getUsage(h.sign(c)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestUsageOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	c := h.claims()
	c.Leg, c.Path = assertion.LegUsage, protocol.ACPHostUsagePath
	if resp := h.getUsage(h.sign(c)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
