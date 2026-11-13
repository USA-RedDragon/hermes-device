package web_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/device/devicetest"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
	"github.com/USA-RedDragon/talaria/protocol"
)

const (
	homeAlice   = "/home/alice"
	srcAlice    = "/home/alice/src"
	sandboxPC   = "sandbox-pc"
	pcName      = "desktop"
	keyDeviceID = "device_id"
	keyDir      = "dir"
)

// late hands the device server the hub service, which setupWith makes
// after the options.
type late struct{ e **hubEnv }

func (l late) Allowed(ctx context.Context, user, backendID string) error {
	return (*l.e).svc.Allowed(ctx, user, backendID)
}

func (l late) NewSessionAt(ctx context.Context, user, backendID, cwd, rowCWD string) (hub.Session, error) {
	return (*l.e).svc.NewSessionAt(ctx, user, backendID, cwd, rowCWD)
}

func (l late) DeleteSession(ctx context.Context, user, backendID, sessionID string) error {
	return (*l.e).svc.DeleteSession(ctx, user, backendID, sessionID)
}

type computersEnv struct {
	*hubEnv
	plugin *devicetest.Plugin
	id     string
}

// setupComputers is a hub with devices on for backend pc, a device server
// over the fake plugin, and one paired computer of alice's that is online.
func setupComputers(t *testing.T) *computersEnv {
	t.Helper()
	var e *hubEnv
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	pub, _ := key.Public().(ed25519.PublicKey)
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Backends[0].Devices = true
		cfg.Backends[0].ServiceDevices = []config.ServiceDevice{{ID: sandboxPC, Name: "Sandbox", Token: "hdt_" + strings.Repeat("s", 40)}}
		b := cfg.Backends[0]
		d, err := device.New(device.Options{Store: st, Access: late{&e}, Sessions: late{&e}, Backends: []config.Backend{b},
			Dialers: map[string]device.Dialer{"pc": gateway.NewDialer(gateway.Credentials{BaseURL: b.URL, Provider: "basic", Username: b.Username, Password: b.Password})},
			Key:     key, PublicURL: cfg.HTTP.PublicURL, Config: config.Device{Enabled: true}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Drain)
		opts.Devices, opts.DeviceAPI, opts.Computers = st, d.Handler(), d
	})
	plugin := devicetest.NewPlugin(e.gw, "pc", pub)
	ctx := context.Background()
	dc, code, err := e.store.StartPairing(ctx, hub.PairingRequest{Name: pcName, Hostname: "desktop.example.com"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.store.ApprovePairing(ctx, code, userAlice, "pc", pcName)
	if err != nil {
		t.Fatal(err)
	}
	if poll, err := e.store.PollPairing(ctx, dc, 0); err != nil || poll.Status != hub.PairApproved {
		t.Fatalf("poll = %+v, %v", poll, err)
	}
	plugin.Connect(d.ID, pcName, protocol.Hello{Home: homeAlice, Roots: []string{homeAlice}})
	plugin.SetDirs(d.ID, map[string][]string{homeAlice: {"src"}, srcAlice: {}})
	plugin.SetSandbox(sandboxPC, "/work")
	plugin.Connect(sandboxPC, "Sandbox", protocol.Hello{Roots: []string{"/work"}})
	return &computersEnv{hubEnv: e, plugin: plugin, id: d.ID}
}

func apiJSON(t *testing.T, c *http.Client, method, u, csrf string, body, out any) int {
	t.Helper()
	var resp *http.Response
	var text string
	if method == http.MethodGet {
		resp, text = get(t, c, u)
	} else {
		resp, text = post(t, c, u, csrf, "", body)
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, u, err, text)
		}
	}
	if resp.StatusCode >= 300 && out != nil {
		if m, ok := out.(*map[string]any); ok {
			_ = json.Unmarshal([]byte(text), m)
		}
	}
	return resp.StatusCode
}

func errText(m map[string]any) string {
	s, _ := m["error"].(string)
	return s
}

type startedJSON struct {
	ID    string    `json:"id"`
	Place hub.Place `json:"place"`
}

type placesJSON []struct {
	BackendID string    `json:"backend_id"`
	SessionID string    `json:"session_id"`
	Place     hub.Place `json:"place"`
}

func TestComputersAPI(t *testing.T) {
	t.Parallel()
	e := setupComputers(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	computersAndFolders(t, e, c)
	started, plainID := startChats(t, e, c, m.CSRF)
	moveAndRelease(t, e, c, m.CSRF, plainID)
	e.plugin.Disconnect(e.id)
	base := e.url + "/api/sessions/pc/" + plainID
	var off map[string]any
	if s := apiJSON(t, c, http.MethodPost, base+"/move", m.CSRF, map[string]string{keyDeviceID: e.id, keyDir: homeAlice}, &off); s != http.StatusConflict ||
		!strings.Contains(errText(off), "offline") {
		t.Fatalf("move to an offline computer = %d %v", s, off)
	}
	var now hub.Place
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions/pc/"+started+"/place", "", nil, &now); s != http.StatusOK || now.Online {
		t.Fatalf("place on an offline computer = %d %+v", s, now)
	}
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/"+started+"/move", "", map[string]string{keyDeviceID: e.id}, nil); s != http.StatusForbidden {
		t.Fatalf("move without CSRF = %d", s)
	}
	bobRefused(t, e, started, plainID)
}

func computersAndFolders(t *testing.T, e *computersEnv, c *http.Client) {
	t.Helper()
	var computers []map[string]any
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/computers", "", nil, &computers); s != http.StatusOK || len(computers) != 1 ||
		computers[0]["id"] != e.id || computers[0]["online"] != true || computers[0]["home"] != homeAlice {
		t.Fatalf("computers = %d %v", s, computers)
	}
	var folders struct {
		Path    string   `json:"path"`
		Folders []string `json:"folders"`
	}
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/computers/"+e.id+"/folders", "", nil, &folders); s != http.StatusOK ||
		folders.Path != homeAlice || !slices.Equal(folders.Folders, []string{"src"}) {
		t.Fatalf("folders = %d %+v", s, folders)
	}
	var bad map[string]any
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/computers/"+e.id+"/folders?path=/etc", "", nil, &bad); s != http.StatusBadRequest ||
		!strings.Contains(errText(bad), "does not allow") {
		t.Fatalf("refused folder = %d %v", s, bad)
	}
}

// startChats starts a chat on the computer and a plain one, checks their
// places, and returns their ids.
func startChats(t *testing.T, e *computersEnv, c *http.Client, csrf string) (string, string) {
	t.Helper()
	var started startedJSON
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions", csrf, map[string]string{keyDeviceID: e.id, keyDir: srcAlice}, &started); s != http.StatusCreated ||
		started.Place.Kind != hub.PlaceComputer || started.Place.Dir != srcAlice || started.Place.Name != pcName {
		t.Fatalf("start on computer = %d %+v", s, started)
	}
	if b := e.plugin.Bindings()[started.ID]; b.DeviceID != e.id {
		t.Fatalf("binding = %+v", b)
	}
	if row, _ := e.gw.Session(started.ID); row.RowCWD != srcAlice {
		t.Fatalf("row cwd = %q", row.RowCWD)
	}
	var plain startedJSON
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions", csrf, map[string]string{keyBackendID: "pc"}, &plain); s != http.StatusCreated {
		t.Fatalf("plain new chat = %d", s)
	}
	var places placesJSON
	refs := []hub.SessionRef{{BackendID: "pc", SessionID: started.ID}, {BackendID: "pc", SessionID: plain.ID}, {BackendID: backendHome, SessionID: "s1"}}
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/places", csrf, map[string]any{"sessions": refs}, &places); s != http.StatusOK || len(places) != 2 {
		t.Fatalf("places = %d %+v", s, places)
	}
	for _, p := range places {
		want := hub.PlaceComputer
		if p.SessionID == plain.ID {
			want = hub.PlaceSandbox
		}
		if p.Place.Kind != want {
			t.Fatalf("place of %s = %+v", p.SessionID, p.Place)
		}
	}
	return started.ID, plain.ID
}

func moveAndRelease(t *testing.T, e *computersEnv, c *http.Client, csrf, sid string) {
	t.Helper()
	base := e.url + "/api/sessions/pc/" + sid
	var moved hub.Place
	if s := apiJSON(t, c, http.MethodPost, base+"/move", csrf, map[string]string{keyDeviceID: e.id, keyDir: homeAlice}, &moved); s != http.StatusOK ||
		moved.Kind != hub.PlaceComputer || moved.Dir != homeAlice {
		t.Fatalf("move = %d %+v", s, moved)
	}
	if s := apiJSON(t, c, http.MethodPost, base+"/release", csrf, map[string]string{}, nil); s != http.StatusAccepted {
		t.Fatalf("release = %d", s)
	}
	var now hub.Place
	if s := apiJSON(t, c, http.MethodGet, base+"/place", "", nil, &now); s != http.StatusOK || now.Kind != hub.PlaceNone {
		t.Fatalf("place after release = %d %+v", s, now)
	}
	var again map[string]any
	if s := apiJSON(t, c, http.MethodPost, base+"/release", csrf, map[string]string{}, &again); s != http.StatusConflict ||
		!strings.Contains(errText(again), "not on a computer") {
		t.Fatalf("release again = %d %v", s, again)
	}
	var cs []computerView
	apiJSON(t, c, http.MethodGet, e.url+"/api/computers", "", nil, &cs)
	if len(cs) != 1 || !slices.Equal(cs[0].Recent, []string{homeAlice, srcAlice}) {
		t.Fatalf("computers after moves = %+v", cs)
	}
}

type computerView struct {
	ID     string   `json:"id"`
	Recent []string `json:"recent"`
}

func bobRefused(t *testing.T, e *computersEnv, started, plain string) {
	t.Helper()
	e.is.as("sub-bob", userBob)
	bob := client(t)
	bm := login(t, e.hubEnv, bob)
	var none []map[string]any
	if s := apiJSON(t, bob, http.MethodGet, e.url+"/api/computers", "", nil, &none); s != http.StatusOK || len(none) != 0 {
		t.Fatalf("bob computers = %d %v", s, none)
	}
	var refused map[string]any
	if s := apiJSON(t, bob, http.MethodPost, e.url+"/api/sessions", bm.CSRF, map[string]string{keyDeviceID: e.id}, &refused); s != http.StatusNotFound {
		t.Fatalf("bob starts on alice's computer = %d %v", s, refused)
	}
	if s := apiJSON(t, bob, http.MethodGet, e.url+"/api/computers/"+e.id+"/folders", "", nil, &refused); s != http.StatusNotFound {
		t.Fatalf("bob browses alice's computer = %d %v", s, refused)
	}
	var places placesJSON
	refs := []hub.SessionRef{{BackendID: "pc", SessionID: started}, {BackendID: "pc", SessionID: plain}}
	if s := apiJSON(t, bob, http.MethodPost, e.url+"/api/places", bm.CSRF, map[string]any{"sessions": refs}, &places); s != http.StatusOK || len(places) != 0 {
		t.Fatalf("bob places = %d %+v", s, places)
	}
	if s := apiJSON(t, bob, http.MethodPost, e.url+"/api/sessions/pc/"+started+"/release", bm.CSRF, map[string]string{}, nil); s != http.StatusForbidden {
		t.Fatalf("bob releases alice's chat = %d", s)
	}
}

func TestSessionsCarrySource(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s-code", Title: "From VS Code", Live: true, StartedAt: 200, Source: gateway.SourceACP,
		Messages: []map[string]any{{"role": "user", "text": "hi"}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "s-cli", Title: "From the CLI", Live: true, StartedAt: 300, Source: gateway.SourceCLI,
		Messages: []map[string]any{{"role": "user", "text": "hi"}}})
	c := client(t)
	login(t, e, c)
	var list []map[string]any
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions", "", nil, &list); s != http.StatusOK {
		t.Fatalf("sessions = %d", s)
	}
	src := map[string]any{}
	for _, x := range list {
		if x["backend_id"] == "pc" {
			id, _ := x["id"].(string)
			src[id] = x["source"]
		}
	}
	if src["s-code"] != gateway.SourceACP || src["s-cli"] != gateway.SourceCLI {
		t.Fatalf("sources = %v", src)
	}
}

func TestBackgroundRefusedOnAComputer(t *testing.T) {
	t.Parallel()
	e := setupComputers(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	started, plain := startChats(t, e, c, m.CSRF)
	var refused map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/"+started+"/background", m.CSRF, map[string]string{kText: "x"}, &refused); s != http.StatusConflict ||
		!strings.Contains(errText(refused), pcName) {
		t.Fatalf("bg on a computer = %d %v", s, refused)
	}
	var ok map[string]string
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/"+plain+"/background", m.CSRF, map[string]string{kText: "x"}, &ok); s != http.StatusAccepted ||
		!strings.HasPrefix(ok["task_id"], "bg_") {
		t.Fatalf("bg in the sandbox = %d %v", s, ok)
	}
}
