package service

import (
	"encoding/json"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func i64(v int64) *int64 { return &v }

func TestTurnTokens(t *testing.T) {
	t.Parallel()
	usage := func(prompt, output, last int64) usageCounters {
		return usageCounters{PromptTokens: i64(prompt), OutputTokens: i64(output), LastTurnTokens: i64(last)}
	}
	for _, tc := range []struct {
		name   string
		u      usageCounters
		base   *counts
		in     *int64
		out    int64
		counts bool
	}{
		{"growth since the turn started", usage(71526, 563, 82), &counts{Prompt: 47220, Output: 481}, i64(24306), 82, true},
		{"a new agent for the turn", usage(23388, 375, 375), &counts{Prompt: 900000, Output: 9000}, i64(23388), 375, true},
		{"no base", usage(71526, 563, 82), nil, nil, 82, true},
		{"a base from another agent", usage(71526, 563, 82), &counts{Prompt: 1000, Output: 12}, nil, 82, true},
		{"a base above the counters", usage(500, 563, 82), &counts{Prompt: 1000, Output: 481}, nil, 82, true},
		{"no last_turn_tokens", usageCounters{PromptTokens: i64(5), OutputTokens: i64(5)}, nil, nil, 0, false},
	} {
		got, ok := turnTokens(tc.u, tc.base)
		if ok != tc.counts || got.Output != tc.out || (got.Input == nil) != (tc.in == nil) || (got.Input != nil && *got.Input != *tc.in) {
			t.Errorf("%s: got %+v (input %v), %v", tc.name, got, got.Input, ok)
		}
	}
}

func turnOf(t *testing.T, payload json.RawMessage) *hub.TurnTokens {
	t.Helper()
	var p struct {
		Turn *hub.TurnTokens `json:"hub_turn"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	return p.Turn
}

// TestTrackTurn feeds the frames of a turn as Hermes v0.23.1 sent them in
// a live check: two API calls, the second one mostly from the cache.
func TestTrackTurn(t *testing.T) {
	t.Parallel()
	ls := &live{}
	ls.subscribedUsage(false, json.RawMessage(`{"model":"q","input":0,"output":0,"prompt":0,"completion":0}`), nil)
	ev := func(typ, payload string) json.RawMessage {
		return ls.trackTurn(gateway.Event{Type: typ, Payload: json.RawMessage(payload)})
	}
	ev(hub.KindMessageStart, `{}`)
	p := ev(gateway.UsageEvent, `{"usage":{"prompt":23388,"output":375},"prompt_tokens":23388,"output_tokens":375,"last_turn_tokens":375}`)
	if tt := turnOf(t, p); tt == nil || *tt.Input != 23388 || tt.Output != 375 {
		t.Fatalf("first usage: %s", p)
	}
	p = ev(hub.KindMessageComplete, `{"text":"done","usage":{"prompt":23388,"output":375}}`)
	if tt := turnOf(t, p); tt == nil || *tt.Input != 23388 {
		t.Fatalf("complete: %s", p)
	}
	ev(hub.KindMessageStart, `{}`)
	p = ev(gateway.UsageEvent, `{"usage":{"prompt":47220,"output":481},"prompt_tokens":47220,"output_tokens":481,"last_turn_tokens":106}`)
	if tt := turnOf(t, p); tt == nil || *tt.Input != 47220-23388 || tt.Output != 106 {
		t.Fatalf("second turn: %s", p)
	}
	p = ev(hub.KindMessageComplete, `{"usage":{"prompt":47220,"output":481}}`)
	if tt := turnOf(t, p); tt == nil || *tt.Input != 47220-23388 || tt.Output != 106 {
		t.Fatalf("second complete: %s", p)
	}
	p = ev(hub.KindMessageComplete, `{"usage":{"prompt":47220,"output":481}}`)
	if turnOf(t, p) != nil {
		t.Fatalf("a complete with no turn usage got turn tokens: %s", p)
	}
}

func TestSubscribedUsageOfARunningTurn(t *testing.T) {
	t.Parallel()
	ls := &live{}
	evs := []gateway.Event{
		{Type: hub.KindMessageComplete, Payload: json.RawMessage(`{"usage":{"prompt":100,"output":10}}`)},
		{Type: hub.KindMessageComplete, Payload: json.RawMessage(`{"usage":{"prompt":400,"output":30}}`)},
	}
	ls.subscribedUsage(true, json.RawMessage(`{"prompt":999,"output":99}`), evs)
	p := ls.trackTurn(gateway.Event{Type: gateway.UsageEvent,
		Payload: json.RawMessage(`{"prompt_tokens":700,"output_tokens":50,"last_turn_tokens":20}`)})
	if tt := turnOf(t, p); tt == nil || tt.Input == nil || *tt.Input != 300 || tt.Output != 20 {
		t.Fatalf("usage = %s", p)
	}
}
