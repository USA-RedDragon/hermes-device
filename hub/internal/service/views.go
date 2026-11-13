package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// While a backend restarts (a Hermes rollout hands its sessions to the next
// gateway over about 40 seconds) its list and resume calls can take many
// seconds. A page that opens or reconnects then gets the hub's last copy of
// the chat list and of the chat at once, and the fresh ones when Hermes
// answers. The hub keeps that copy in memory and, with Options.Views, in
// the store, so the other replica has it too.

// Defaults of Options.ListFast, AttachFast and AwayNotice.
const (
	defaultListFast   = 800 * time.Millisecond
	defaultAttachFast = 800 * time.Millisecond
	defaultAwayNotice = 60 * time.Second
	// defaultAwayAlarm is how long a backend may not answer before the
	// page calls it down, not restarting: the whole restart budget.
	defaultAwayAlarm = gateway.RestartBudget
)

// viewMaxAge bounds how old a copy may be to stand in for a backend that is
// slow to answer, and viewMaxChats how many chats each backend keeps in
// memory.
const (
	viewMaxAge   = 24 * time.Hour
	viewMaxChats = 256
	// viewStoreWait bounds each store read or write of a copy.
	viewStoreWait = 2 * time.Second
)

type viewCopy struct {
	data json.RawMessage
	at   time.Time
}

// listCall is one session list in flight, shared by every caller that
// asks while it runs.
type listCall struct {
	done chan struct{}
	list []hub.Session
	err  error
}

// sharedList starts a list of the backend's sessions, or joins the one in
// flight. The list outlives ctx, so a caller that stops waiting still
// leaves a fresh copy for the next one.
func (b *backend) sharedList(ctx context.Context) *listCall {
	b.listMu.Lock()
	defer b.listMu.Unlock()
	if c := b.listing; c != nil {
		return c
	}
	c := &listCall{done: make(chan struct{})}
	b.listing = c
	go func() {
		c.list, c.err = b.listRetry(context.WithoutCancel(ctx))
		b.listMu.Lock()
		b.listing = nil
		b.listMu.Unlock()
		close(c.done)
	}()
	return c
}

// markAway notes that the backend did not answer, markBack that it did.
func (b *backend) markAway() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.awaySince.IsZero() {
		b.awaySince = time.Now()
	}
}

func (b *backend) markBack() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.awaySince = time.Time{}
}

// awayFor is how long the backend has not answered, 0 when it answers.
func (b *backend) awayFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.awaySince.IsZero() {
		return 0
	}
	return time.Since(b.awaySince)
}

// noticeAway is whether the backend has been away long enough to tell the
// user.
func (b *backend) noticeAway() bool { return b.awayFor() >= b.svc.opts.AwayNotice }

// alarmAway is whether the backend has been away so long, or failed in a
// way a restart does not explain, that it is down and not restarting.
func (b *backend) alarmAway(err error) bool {
	if b.awayFor() >= b.svc.opts.AwayAlarm {
		return true
	}
	return err != nil && !gateway.Retryable(err) && !errors.Is(err, gateway.ErrStalled) && !errors.Is(err, context.DeadlineExceeded)
}

// hasListed reports whether the backend listed its sessions since the hub
// started.
func (b *backend) hasListed() bool {
	b.listMu.Lock()
	defer b.listMu.Unlock()
	return b.listed != nil
}

// keepList records a list that worked, in memory and in the store.
func (b *backend) keepList(list []hub.Session) {
	now := time.Now()
	data, err := json.Marshal(list)
	b.listMu.Lock()
	b.listed, b.listedAt = slices.Clone(list), now
	same := err == nil && bytes.Equal(data, b.listSaved)
	if err == nil {
		b.listSaved = data
	}
	b.listMu.Unlock()
	if err != nil || same {
		return
	}
	b.saveView(hub.ViewListKey, data, now)
}

// saveView writes a copy to the store in the background.
//
//nolint:contextcheck // the write outlives the call that made the copy
func (b *backend) saveView(key string, data []byte, at time.Time) {
	v := b.svc.opts.Views
	if v == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), viewStoreWait)
		defer cancel()
		if err := v.SaveView(ctx, b.cfg.ID, key, data, at); err != nil {
			b.log.Warn("keep a copy of the chats", "error", err.Error())
		}
	}()
}

// storedView reads a copy from the store; ok is false when there is none
// recent enough.
func (b *backend) storedView(ctx context.Context, key string) (json.RawMessage, time.Time, bool) {
	v := b.svc.opts.Views
	if v == nil {
		return nil, time.Time{}, false
	}
	sctx, cancel := context.WithTimeout(ctx, viewStoreWait)
	defer cancel()
	data, at, ok, err := v.View(sctx, b.cfg.ID, key)
	if err != nil {
		b.log.Warn("read the copy of the chats", "error", err.Error())
		return nil, time.Time{}, false
	}
	if !ok || time.Since(at) > viewMaxAge {
		return nil, time.Time{}, false
	}
	return data, at, true
}

// cachedList is the newest list the hub kept of the backend, from memory or
// the store; nil when it has none recent enough.
func (b *backend) cachedList(ctx context.Context) []hub.Session {
	b.listMu.Lock()
	list, at := slices.Clone(b.listed), b.listedAt
	b.listMu.Unlock()
	if data, sat, ok := b.storedView(ctx, hub.ViewListKey); ok && (list == nil || sat.After(at)) {
		var got []hub.Session
		if json.Unmarshal(data, &got) == nil {
			list, at = got, sat
		}
	}
	if list == nil || time.Since(at) > viewMaxAge {
		return nil
	}
	return list
}

// keepSnapshot keeps a chat's snapshot as it opens: its newest rows, its
// title, its settings and whether it runs. It leaves out the approvals
// and waiting messages, which may be gone by the time the copy shows.
func (b *backend) keepSnapshot(sessionID string, snap hub.Snapshot) {
	snap.Messages = snap.Messages[hub.HeadCut(snap.Messages):]
	snap.Prompts, snap.Pending = nil, nil
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	now := time.Now()
	b.snapMu.Lock()
	if b.snaps == nil {
		b.snaps = map[string]viewCopy{}
	}
	same := bytes.Equal(b.snaps[sessionID].data, data)
	b.snaps[sessionID] = viewCopy{data: data, at: now}
	if len(b.snaps) > viewMaxChats {
		oldest, at := "", now
		for id, c := range b.snaps {
			if c.at.Before(at) {
				oldest, at = id, c.at
			}
		}
		delete(b.snaps, oldest)
	}
	b.snapMu.Unlock()
	if !same {
		b.saveView(hub.ViewSnapshotKey(sessionID), data, now)
	}
}

// cachedSnapshot is the newest copy of a chat the hub kept, from memory or
// the store; nil when it has none.
func (b *backend) cachedSnapshot(ctx context.Context, sessionID string) json.RawMessage {
	b.snapMu.Lock()
	c := b.snaps[sessionID]
	b.snapMu.Unlock()
	if data, at, ok := b.storedView(ctx, hub.ViewSnapshotKey(sessionID)); ok && (c.data == nil || at.After(c.at)) {
		c = viewCopy{data: data, at: at}
	}
	if c.data == nil || time.Since(c.at) > viewMaxAge {
		return nil
	}
	return c.data
}

// attached is what attachable found.
type attached struct {
	ls      *live
	offline bool
	err     error
}

// attachLater answers an Attach at once with the hub's copy of the chat,
// marked provisional, and then follows the chat as Attach does once the
// backend answers. While it waits past Options.AwayNotice the viewer is
// told the backend is away.
func (s *Service) attachLater(ctx context.Context, b *backend, sinceSeq int64, copyData json.RawMessage, res <-chan attached) (<-chan hub.Event, func()) {
	out := make(chan hub.Event, s.opts.ViewerBuffer)
	out <- hub.Event{Kind: hub.KindSnapshot, Data: copyData, Provisional: true}
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopped) }) }
	go func() {
		defer close(out)
		notice := time.NewTimer(max(s.opts.AwayNotice-b.awayFor(), 0))
		defer notice.Stop()
		var r attached
	wait:
		for {
			select {
			case r = <-res:
				break wait
			case <-notice.C:
				out <- hub.Event{Kind: hub.KindBackendStatus, Data: json.RawMessage(`{"connected":false}`)}
			case <-ctx.Done():
				go release(res)
				return
			case <-stopped:
				go release(res)
				return
			}
		}
		if r.err != nil {
			if data, err := json.Marshal(map[string]string{"message": refusal(r.err).Error()}); err == nil {
				out <- hub.Event{Kind: hub.KindError, Data: data}
			}
			return
		}
		ch, cancel := r.ls.attach(sinceSeq)
		defer cancel()
		if r.offline {
			if b.noticeAway() {
				out <- hub.Event{Kind: hub.KindBackendStatus, Data: json.RawMessage(`{"connected":false}`)}
			}
			b.retry()
		} else {
			out <- hub.Event{Kind: hub.KindBackendStatus, Data: json.RawMessage(`{"connected":true}`)}
		}
		r.ls.unpin()
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				case <-stopped:
					return
				}
			case <-ctx.Done():
				return
			case <-stopped:
				return
			}
		}
	}()
	return out, stop
}

// release unpins what an attach that no one waits for any more found.
func release(res <-chan attached) {
	if r := <-res; r.err == nil {
		r.ls.unpin()
	}
}
