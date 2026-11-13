package web_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A big chat opened fresh streams its newest turns first, then the older
// rows; a reconnect between the two gets the whole snapshot again.
func TestBigChatOpensWithNewestTurnsFirst(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ms := make([]map[string]any, 0, 1200)
	for i := range 600 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("answer %d ", i), 200)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "bigchat", Title: "Big", Live: true, StartedAt: 900, Messages: ms})
	c := client(t)
	login(t, e, c)
	start := time.Now()
	r, stop := stream(t, c, e.url+"/api/sessions/pc/bigchat/events", "")
	defer stop()
	first := readEvents(t, r, "snapshot")
	took := time.Since(start)
	var head struct {
		Older    int               `json:"older"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(first[len(first)-1].data, &head); err != nil {
		t.Fatal(err)
	}
	if head.Older == 0 || len(head.Messages) == 0 || len(head.Messages) > 200 {
		t.Fatalf("first snapshot has %d rows and older = %d, want the newest turns", len(head.Messages), head.Older)
	}
	if !strings.HasSuffix(first[len(first)-1].id, ".0") {
		t.Fatalf("first snapshot id = %q, want one no stream covers", first[len(first)-1].id)
	}
	rest := readEvents(t, r, "snapshot.older")
	var older struct {
		Messages []json.RawMessage `json:"messages"`
	}
	last := rest[len(rest)-1]
	if err := json.Unmarshal(last.data, &older); err != nil || len(older.Messages) != head.Older || head.Older+len(head.Messages) != len(ms) {
		t.Fatalf("older rows = %d, want %d of %d", len(older.Messages), head.Older, len(ms))
	}
	if strings.HasSuffix(last.id, ".0") {
		t.Fatal("the older rows carry no resume point")
	}
	t.Logf("first snapshot event after %v: %d of %d rows", took, len(head.Messages), len(ms))
	// A reconnect after only the first part gets the whole snapshot.
	r2, stop2 := stream(t, c, e.url+"/api/sessions/pc/bigchat/events", first[len(first)-1].id)
	defer stop2()
	again := readEvents(t, r2, "snapshot")
	var whole struct {
		Older    int               `json:"older"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(again[len(again)-1].data, &whole); err != nil || whole.Older != 0 || len(whole.Messages) != len(ms) {
		t.Fatalf("reconnect got %d rows, older %d, want all %d", len(whole.Messages), whole.Older, len(ms))
	}
}
