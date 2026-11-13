package service

import (
	"context"
	"encoding/json"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.Pages = (*Service)(nil)

// Page returns a page block of a reply of the session. See hub.Pages.
func (s *Service) Page(ctx context.Context, user, backendID, sessionID, id string) (string, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return "", err
	}
	if !hub.PageIDOK(id) {
		return "", hub.ErrNoPage
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return "", refusal(err)
	}
	page, ok := ls.page(id)
	ls.unpin()
	if !ok {
		return "", hub.ErrNoPage
	}
	return page, nil
}

// page finds the page block with that id in the replies of the snapshot's
// transcript or the events since. A reply that came in pieces cut inside a
// fenced block is read as one: the piece after the cut goes on from it.
func (ls *live) page(id string) (string, bool) {
	ls.mu.Lock()
	snap := ls.snapshot.Data
	ring := ls.ring[:len(ls.ring):len(ls.ring)]
	ls.mu.Unlock()
	find := func(text string) (string, bool) {
		pages, _ := hub.PageBlocks(text)
		for _, p := range pages {
			if hub.PageID(p) == id {
				return p, true
			}
		}
		return "", false
	}
	var s struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if len(snap) > 0 && json.Unmarshal(snap, &s) == nil {
		joined := ""
		for _, raw := range s.Messages {
			var r row
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			if r.Role == "user" {
				joined = ""
			}
			if r.Role != "assistant" {
				continue
			}
			var content string
			if r.Text != nil {
				content = *r.Text
			} else {
				_ = json.Unmarshal(r.Content, &content)
			}
			if content == "" {
				continue
			}
			if hub.FenceOpen(joined) {
				content = joined + content
			}
			joined = content
			if p, ok := find(content); ok {
				return p, true
			}
		}
	}
	var reply []byte
	for _, e := range ring {
		var d struct {
			Text *string `json:"text"`
		}
		_ = json.Unmarshal(e.Data, &d)
		text := ""
		if d.Text != nil {
			text = *d.Text
		}
		switch e.Kind {
		case hub.KindMessageDelta:
			reply = append(reply, text...)
		case hub.KindMessageStart, hub.KindMessageComplete, hub.KindMessageInterim:
			if e.Kind != hub.KindMessageStart {
				if p, ok := find(text); ok {
					return p, true
				}
			}
			if p, ok := find(string(reply)); ok {
				return p, true
			}
			// A message handed to the running turn starts while a reply is
			// inside a fenced block: the reply goes on after it.
			if e.Kind == hub.KindMessageStart && hub.FenceOpen(string(reply)) {
				continue
			}
			reply = reply[:0]
		}
	}
	return find(string(reply))
}
