package device_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

type usageSink struct {
	mu   sync.Mutex
	got  []string
	devs []hub.Device
	err  error
}

func (u *usageSink) IngestDevice(_ context.Context, d hub.Device, body []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return u.err
	}
	u.got = append(u.got, string(body))
	u.devs = append(u.devs, d)
	return nil
}

func (u *usageSink) fail(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.err = err
}

func postUsage(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/api/device/usage", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestDeviceUsageRoute(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		if got := postUsage(t, e.a.url, tok, `{"rows":[]}`); got != http.StatusNotFound {
			t.Fatalf("without a sink: %d", got)
		}
		sink := &usageSink{}
		e.a.dev.SetUsage(sink)
		if got := postUsage(t, e.a.url, "", `{}`); got != http.StatusUnauthorized {
			t.Fatalf("without a token: %d", got)
		}
		if got := postUsage(t, e.a.url, sandboxToken, `{}`); got != http.StatusForbidden {
			t.Fatalf("sandbox: %d", got)
		}
		if got := postUsage(t, e.a.url, tok, `{"rows":[]}`); got != http.StatusNoContent {
			t.Fatalf("report: %d", got)
		}
		sink.mu.Lock()
		if len(sink.devs) != 1 || sink.devs[0].ID != id || sink.devs[0].User != userAlice || sink.got[0] != `{"rows":[]}` {
			t.Fatalf("sink got %+v %v", sink.devs, sink.got)
		}
		sink.mu.Unlock()
		sink.fail(fmt.Errorf("%w: row", hub.ErrBadClaudeCodeReport))
		if got := postUsage(t, e.a.url, tok, `{}`); got != http.StatusBadRequest {
			t.Fatalf("bad report: %d", got)
		}
		sink.fail(errors.New("disk full"))
		if got := postUsage(t, e.a.url, tok, `{}`); got != http.StatusServiceUnavailable {
			t.Fatalf("store failure: %d", got)
		}
	})
}

func TestHermesUsage(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		if _, err := e.a.dev.HermesUsage(ctx, backendID); err == nil {
			t.Fatal("usage off on acp-host is not an error")
		}
		e.acp.SetUsage([]byte(`{"rows":[]}`))
		got, err := e.a.dev.HermesUsage(ctx, backendID)
		if err != nil || !bytes.Equal(got, []byte(`{"rows":[]}`)) {
			t.Fatalf("usage %q %v", got, err)
		}
		cl := e.acp.UsageClaims()
		if len(cl) != 2 || cl[1].Leg != "usage" || cl[1].Path != "/usage" || cl[1].Aud != backendID {
			t.Fatalf("claims %+v", cl)
		}
		if _, err := e.a.dev.HermesUsage(ctx, "nope"); err == nil {
			t.Fatal("unknown backend")
		}
	})
}
