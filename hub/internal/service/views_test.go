package service_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// views is a hub.ViewCache in memory, shared by the services of a test as
// the store is by replicas.
type views struct {
	mu sync.Mutex
	m  map[string]json.RawMessage
	at map[string]time.Time
}

func (v *views) SaveView(_ context.Context, backendID, key string, data []byte, at time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil {
		v.m, v.at = map[string]json.RawMessage{}, map[string]time.Time{}
	}
	if at.Before(v.at[backendID+"\n"+key]) {
		return nil
	}
	v.m[backendID+"\n"+key], v.at[backendID+"\n"+key] = append(json.RawMessage(nil), data...), at
	return nil
}

func (v *views) View(_ context.Context, backendID, key string) ([]byte, time.Time, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	d, ok := v.m[backendID+"\n"+key]
	return d, v.at[backendID+"\n"+key], ok, nil
}

func (v *views) has(backendID, key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.m[backendID+"\n"+key]
	return ok
}

// listsTheCopy checks that svc lists the hub's copy of the chats at once,
// marked as refreshing. A replica that listed the chats moments ago
// (recent) answers from that list, which needs no mark.
func listsTheCopy(ctx context.Context, t *testing.T, name string, svc *service.Service, recent bool) {
	t.Helper()
	started := time.Now()
	list, err := svc.Sessions(ctx, userAlice)
	ue := hub.Unreachable(err)
	marked := ue != nil && ue.Pending[backendPC] && ue.Backends[backendPC] == ""
	if (!marked && (!recent || err != nil)) || time.Since(started) > 2*time.Second {
		t.Fatalf("%s: list while silent = %v after %s", name, err, time.Since(started))
	}
	for _, s := range list {
		if s.BackendID == backendPC && s.Title == liveTitle {
			return
		}
	}
	t.Fatalf("%s: list while silent = %+v", name, list)
}

// A Hermes that stops answering, as one does while a rollout hands its
// sessions to the next gateway, does not keep a page waiting: the chat list
// and the chat come from the hub's copy at once, on a replica that never
// saw them too, marked as refreshing; the page is told the agent is away
// only after AwayNotice; and the fresh chat follows when Hermes answers.
func TestSilentBackendServesTheCopy(t *testing.T) {
	t.Parallel()
	shared := &views{}
	opts := service.Options{Views: shared, ListFast: 200 * time.Millisecond, AttachFast: 200 * time.Millisecond, AwayNotice: 1500 * time.Millisecond}
	e := setup(t, opts)
	ctx := hub.WithCached(context.Background())
	if _, err := e.svc.Sessions(ctx, userAlice); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot || ev.Provisional {
		t.Fatalf("first open = %+v", ev)
	}
	stop()
	for i := 0; i < 50 && (!shared.has(backendPC, hub.ViewListKey) || !shared.has(backendPC, hub.ViewSnapshotKey(sLive))); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !shared.has(backendPC, hub.ViewSnapshotKey(sLive)) {
		t.Fatal("the hub kept no copy of the chat")
	}

	release := e.gw.Silence()
	defer release()
	// Another replica, which has seen nothing of this backend.
	other := service.New(e.cfg.Backends, groups{userAlice: {"admins"}}, opts)
	t.Cleanup(other.Close)
	listsTheCopy(ctx, t, "same replica", e.svc, true)
	listsTheCopy(ctx, t, "other replica", other, false)
	started := time.Now()
	ch, stop, err = other.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ev := next(t, ch)
	var snap hub.Snapshot
	if ev.Kind != hub.KindSnapshot || !ev.Provisional || json.Unmarshal(ev.Data, &snap) != nil || snap.Title != liveTitle ||
		len(snap.Messages) != 1 || time.Since(started) > 2*time.Second {
		t.Fatalf("open while silent = %+v after %s", ev, time.Since(started))
	}
	// Past AwayNotice the viewer and the list say the agent is away.
	ev = next(t, ch)
	if ev.Kind != hub.KindBackendStatus || string(ev.Data) != `{"connected":false}` || time.Since(started) < time.Second {
		t.Fatalf("event while still silent = %+v after %s", ev, time.Since(started))
	}
	if _, err := other.Sessions(ctx, userAlice); hub.Unreachable(err) == nil || hub.Unreachable(err).Backends[backendPC] == "" {
		t.Fatalf("list after AwayNotice = %v", err)
	}
	release()
	for _, ev := range until(t, ch, hub.KindSnapshot) {
		if ev.Kind == hub.KindSnapshot && ev.Provisional {
			t.Fatalf("snapshot after Hermes answered is provisional: %+v", ev)
		}
	}
	if _, err := other.Sessions(ctx, userAlice); err != nil {
		t.Fatalf("list after Hermes answered = %v", err)
	}
}
