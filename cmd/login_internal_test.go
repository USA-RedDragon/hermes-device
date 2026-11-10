package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/pairing"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
)

type pairHub struct {
	t     *testing.T
	polls atomic.Int32
	name  atomic.Value
}

func (h *pairHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case hubapi.PathPair:
		var req hubapi.PairRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.name.Store(req.Name)
		writeJSONBody(h.t, w, hubapi.PairStart{
			DeviceCode: "dc", UserCode: "WDJB-MJHT", VerificationURI: "https://hub.example.com/device/pair", ExpiresIn: 600, Interval: 1,
		})
	case hubapi.PathPairToken:
		if h.polls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		writeJSONBody(h.t, w, hubapi.PairResult{Token: pairedToken, DeviceID: pairedID, DeviceName: pairedName, Backend: testUser})
	default:
		http.NotFound(w, r)
	}
}

func TestLoginCommandStoresTokenFile(t *testing.T) {
	t.Parallel()
	hub := &pairHub{t: t}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("log-level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand("v0.2.0", "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"login", "--config", cfgFile, "--hub.url", srv.URL, "--store.keyring=false", "--store.state-dir", stateDir, "--name", pairedName})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := hub.name.Load().(string); got != pairedName {
		t.Errorf("suggested name = %q", got)
	}
	file := filepath.Join(stateDir, "token")
	text := out.String()
	for _, want := range []string{"https://hub.example.com/device/pair\n", "WDJB-MJHT", file} {
		if !strings.Contains(text, want) {
			t.Errorf("output %q lacks %q", text, want)
		}
	}
	rec, err := tokenstore.New(false, file).Load(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := tokenstore.Record{Hub: srv.URL, Token: pairedToken, DeviceID: pairedID, DeviceName: pairedName, Backend: testUser}
	if *rec != want {
		t.Errorf("record = %+v, want %+v", *rec, want)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, %v", fi, err)
	}
}

type memKeyring struct{ m map[string]string }

func (k *memKeyring) Get(s, u string) (string, error) {
	if v, ok := k.m[s+"/"+u]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func (k *memKeyring) Set(s, u, p string) error {
	k.m[s+"/"+u] = p
	return nil
}

func (k *memKeyring) Delete(s, u string) error {
	delete(k.m, s+"/"+u)
	return nil
}

func TestLoginStoresInKeyring(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&pairHub{t: t})
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	kr := &memKeyring{m: map[string]string{}}
	st := &tokenstore.Store{Keyring: kr, File: file}
	p := &pairing.Pairer{
		Hub:   hubapi.New(srv.URL, "", ""),
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
	var out bytes.Buffer
	if err := login(context.Background(), &out, srv.URL, p, st, "", "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "OS keyring") {
		t.Errorf("output = %q", out.String())
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file written: %v", err)
	}
	rec, err := st.Load(srv.URL)
	if err != nil || rec.Token != pairedToken || rec.DeviceID != pairedID {
		t.Errorf("Load = %+v, %v", rec, err)
	}
}
