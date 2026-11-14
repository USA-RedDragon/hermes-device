package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

func bigSnapshot(t *testing.T, rows int) json.RawMessage {
	t.Helper()
	msgs := make([]map[string]any, 0, rows)
	for i := range rows {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "text": fmt.Sprintf("row %d %s", i, strings.Repeat("x", 200))})
	}
	data, err := json.Marshal(struct {
		Title    string           `json:"title"`
		Messages []map[string]any `json:"messages"`
	}{"t", msgs})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The frames of a snapshot are made once for the bytes the service hands
// out, and made again for new bytes even when they read the same.
func TestSnapCacheKeepsFramesPerSnapshot(t *testing.T) {
	t.Parallel()
	s := &Server{instance: "i1"}
	data := bigSnapshot(t, 400)
	e := hub.Event{Seq: 7, Kind: hub.KindSnapshot, Data: data}
	p := s.snaps.prepared(e)
	if !p.split || p.fp == "" || p.fp != snapFP(data) {
		t.Fatalf("prepared = split %v fp %q", p.split, p.fp)
	}
	if again := s.snaps.prepared(e); again != p {
		t.Fatal("the same snapshot was prepared twice")
	}
	if other := s.snaps.prepared(hub.Event{Seq: 7, Kind: hub.KindSnapshot, Data: bytes.Clone(data)}); other == p {
		t.Fatal("a new snapshot got the frames of an old one")
	}
	if prov := s.snaps.prepared(hub.Event{Seq: 7, Kind: hub.KindSnapshot, Data: data, Provisional: true}); prov == p || prov.fp != "" {
		t.Fatal("a provisional copy shares the frames of the snapshot")
	}
	var direct bytes.Buffer
	ew := &eventWriter{s: &Server{instance: "i1"}, split: true}
	if !ew.write(&direct, e) {
		t.Fatal("write failed")
	}
	var cached bytes.Buffer
	ew2 := &eventWriter{s: s, split: true}
	for range 2 {
		if !ew2.write(&cached, e) {
			t.Fatal("write failed")
		}
	}
	head, older, _ := splitSnapshot(data)
	if !bytes.Contains(direct.Bytes(), []byte(`"kind":"snapshot.older"`)) || !bytes.Contains(direct.Bytes(), trimToolRows(head)[:50]) || len(older) == 0 {
		t.Fatalf("split frames: %.200s", direct.String())
	}
	if !bytes.HasPrefix(cached.Bytes(), direct.Bytes()) {
		t.Fatal("kept frames differ from frames made afresh")
	}
	// A page that shows the transcript gets snapshot.same.
	var same bytes.Buffer
	ew3 := &eventWriter{s: s, split: true, fp: p.fp}
	if !ew3.write(&same, e) || !strings.Contains(same.String(), `"kind":"snapshot.same"`) {
		t.Fatalf("with fp = %.200s", same.String())
	}
}

// The cache keeps at most snapCacheEntries snapshots.
func TestSnapCacheIsBounded(t *testing.T) {
	t.Parallel()
	var c snapCache
	for i := range 3 * snapCacheEntries {
		c.prepared(hub.Event{Seq: int64(i), Kind: hub.KindSnapshot, Data: bigSnapshot(t, 10)})
	}
	if len(c.list) > snapCacheEntries || c.bytes > snapCacheBytes {
		t.Fatalf("cache holds %d snapshots, %d bytes", len(c.list), c.bytes)
	}
}
