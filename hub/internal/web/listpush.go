package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// kindSessionsChanged tells a page that the chat list changed since it last
// looked: it fetches the list again. It carries no id, so a reconnect still
// resumes from the last chat event.
const kindSessionsChanged = "sessions.changed"

// How often an open stream looks at the user's chat list, and how long one
// look is shared among the user's streams.
const (
	listWatchEvery = 4 * time.Second
	listMemoFor    = 3 * time.Second
)

// listFingerprints keeps each user's latest chat list fingerprint, so a few
// open tabs cost the backend one list read per listMemoFor, not one each.
type listFingerprints struct {
	mu sync.Mutex
	by map[string]*listMemo
}

type listMemo struct {
	mu sync.Mutex
	at time.Time
	fp string
}

func (l *listFingerprints) of(user string) *listMemo {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*listMemo{}
	}
	m := l.by[user]
	if m == nil {
		m = &listMemo{}
		l.by[user] = m
	}
	return m
}

// listFP is a hash of the user's chat list as /api/sessions would show it
// ("" when it cannot be read now).
func (s *Server) listFP(ctx context.Context, user string) string {
	m := s.listFPs.of(user)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fp != "" && time.Since(m.at) < listMemoFor {
		return m.fp
	}
	ss, err := s.svc.Sessions(hub.WithCached(ctx), user)
	ue := hub.Unreachable(err)
	if err != nil && ue == nil {
		return ""
	}
	data, err := json.Marshal(s.marked(ctx, user, ss, ue == nil))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	m.fp, m.at = hex.EncodeToString(sum[:8]), time.Now()
	return m.fp
}

// watchList sends kindSessionsChanged on w each time the user's chat list
// differs from the last one this stream saw, until ctx ends.
func (s *Server) watchList(ctx context.Context, user string, changed chan<- struct{}) {
	last := s.listFP(ctx, user)
	t := time.NewTicker(listWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fp := s.listFP(ctx, user)
			if fp == "" || fp == last {
				continue
			}
			last = fp
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
}

func writeListChanged(w io.Writer) error {
	_, err := fmt.Fprintf(w, "data: {\"kind\":%q}\n\n", kindSessionsChanged)
	return err
}
