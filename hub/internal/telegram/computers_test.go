package telegram_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	pcDesktop = "d_desktop"
	pcWork    = "d_work"
	homeAlice = "/home/alice"
	sCode     = "s-code"
)

type startCall struct{ User, DeviceID, Dir string }

type moveCall struct{ BackendID, SessionID, DeviceID, Dir string }

// fakeComputers is a hub.Computers over a fakeHub: alice has an online
// desktop and an offline work pc on backend cluster.
type fakeComputers struct {
	hub *fakeHub

	mu       sync.Mutex
	list     map[string][]hub.Computer
	places   map[hub.SessionRef]hub.Place
	starts   []startCall
	moves    []moveCall
	released []hub.SessionRef
}

func newFakeComputers(h *fakeHub) *fakeComputers {
	return &fakeComputers{hub: h, places: map[hub.SessionRef]hub.Place{}, list: map[string][]hub.Computer{
		userAlice: {
			{ID: pcDesktop, Name: "desktop", BackendID: beCluster, Online: true, Home: homeAlice, Roots: []string{homeAlice},
				Recent: []string{homeAlice + "/src/app"}},
			{ID: pcWork, Name: "work pc", BackendID: beCluster},
		},
	}}
}

func (f *fakeComputers) computer(user, id string) (hub.Computer, error) {
	for _, c := range f.list[user] {
		if c.ID == id {
			return c, nil
		}
	}
	return hub.Computer{}, hub.ErrUnknownDevice
}

func (f *fakeComputers) Computers(_ context.Context, user string) ([]hub.Computer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list[user], nil
}

func (f *fakeComputers) Folders(context.Context, string, string, string) (hub.Folder, error) {
	return hub.Folder{}, nil
}

func (f *fakeComputers) Places(_ context.Context, user string, refs []hub.SessionRef) map[hub.SessionRef]hub.Place {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[hub.SessionRef]hub.Place{}
	for _, r := range refs {
		if f.hub.allowed(user, r.BackendID) != nil {
			continue
		}
		if p, ok := f.places[r]; ok {
			out[r] = p
		} else {
			out[r] = hub.Place{Kind: hub.PlaceSandbox, Online: true}
		}
	}
	return out
}

func (f *fakeComputers) Place(_ context.Context, user, backendID, sessionID string) (hub.Place, error) {
	if err := f.hub.allowed(user, backendID); err != nil {
		return hub.Place{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.places[hub.SessionRef{BackendID: backendID, SessionID: sessionID}]; ok {
		return p, nil
	}
	return hub.Place{Kind: hub.PlaceSandbox, Online: true}, nil
}

func (f *fakeComputers) StartOn(ctx context.Context, user, deviceID, dir string) (hub.Session, hub.Place, error) {
	f.mu.Lock()
	c, err := f.computer(user, deviceID)
	f.starts = append(f.starts, startCall{user, deviceID, dir})
	f.mu.Unlock()
	if err != nil {
		return hub.Session{}, hub.Place{}, err
	}
	if !c.Online {
		return hub.Session{}, hub.Place{}, hub.ErrComputerOffline
	}
	if strings.HasPrefix(dir, "/etc") {
		return hub.Session{}, hub.Place{}, fmt.Errorf("%w: the computer does not allow that folder", hub.ErrFolderRefused)
	}
	s, err := f.hub.NewSession(ctx, user, c.BackendID)
	if err != nil {
		return hub.Session{}, hub.Place{}, err
	}
	if dir == "" {
		dir = c.Roots[0]
	}
	p := hub.Place{Kind: hub.PlaceComputer, DeviceID: c.ID, Name: c.Name, Dir: dir, Online: true, Mine: true}
	f.mu.Lock()
	f.places[hub.SessionRef{BackendID: s.BackendID, SessionID: s.ID}] = p
	f.mu.Unlock()
	return s, p, nil
}

func (f *fakeComputers) MoveTo(_ context.Context, user, backendID, sessionID, deviceID, dir string) (hub.Place, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, moveCall{backendID, sessionID, deviceID, dir})
	c, err := f.computer(user, deviceID)
	if err != nil {
		return hub.Place{}, err
	}
	if c.BackendID != backendID {
		return hub.Place{}, hub.ErrOtherBackend
	}
	if !c.Online {
		return hub.Place{}, hub.ErrComputerOffline
	}
	p := hub.Place{Kind: hub.PlaceComputer, DeviceID: c.ID, Name: c.Name, Dir: dir, Online: true, Mine: true}
	f.places[hub.SessionRef{BackendID: backendID, SessionID: sessionID}] = p
	return p, nil
}

func (f *fakeComputers) UseBrowser(context.Context, string, string, string, string) error { return nil }

func (f *fakeComputers) Release(_ context.Context, _, backendID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := hub.SessionRef{BackendID: backendID, SessionID: sessionID}
	if f.places[k].Kind != hub.PlaceComputer {
		return hub.ErrNotOnComputer
	}
	f.places[k] = hub.Place{Kind: hub.PlaceNone, Online: true}
	f.released = append(f.released, k)
	return nil
}

func (f *fakeComputers) snapshot() ([]startCall, []moveCall, []hub.SessionRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]startCall(nil), f.starts...), append([]moveCall(nil), f.moves...), append([]hub.SessionRef(nil), f.released...)
}

func startComputers(t *testing.T) (*harness, *fakeComputers) {
	t.Helper()
	var fc *fakeComputers
	h := startBotWith(t, nil, newFakeState(), func(h *fakeHub) hub.Computers {
		fc = newFakeComputers(h)
		return fc
	})
	return h, fc
}

func TestDevicesCommand(t *testing.T) {
	t.Parallel()
	h, _ := startComputers(t)
	h.tg.say(dm(aliceTG), aliceTG, "/devices")
	c := h.tg.wait(mSend, "Computers")
	if !strings.Contains(c.text(), "desktop: online, last folder <code>/home/alice/src/app</code>") || !strings.Contains(c.text(), "work pc: offline") {
		t.Fatalf("devices = %q", c.text())
	}
	h.tg.say(dm(bobTG), bobTG, "/devices")
	h.tg.wait(mSend, "You have no computers")
	h.tg.say(dm(aliceTG), aliceTG, "/help")
	if c := h.tg.wait(mSend, "Signed in"); !strings.Contains(c.text(), "/new NAME [folder]") || !strings.Contains(c.text(), "/move off") {
		t.Fatalf("help = %q", c.text())
	}
}

func TestNewOnComputer(t *testing.T) {
	t.Parallel()
	h, fc := startComputers(t)
	h.tg.say(dm(aliceTG), aliceTG, "/new desktop")
	h.tg.wait(mSend, "New chat on <b>desktop</b> in <code>/home/alice/src/app</code>")
	h.tg.say(dm(aliceTG), aliceTG, "/new Desktop notes/2026")
	h.tg.wait(mSend, "in <code>/home/alice/notes/2026</code>")
	h.tg.say(dm(aliceTG), aliceTG, "/new desktop ~/src")
	h.tg.wait(mSend, "in <code>/home/alice/src</code>")
	h.tg.say(dm(aliceTG), aliceTG, "/new desktop /etc")
	h.tg.wait(mSend, "The computer does not allow that folder.")
	h.tg.say(dm(aliceTG), aliceTG, "/new work pc")
	h.tg.wait(mSend, "work pc is offline")
	h.tg.say(dm(aliceTG), aliceTG, "/new toaster")
	h.tg.wait(mSend, "Unknown backend or computer")
	h.tg.say(dm(aliceTG), aliceTG, "/new laptop")
	h.tg.wait(mSend, "New session on <b>laptop</b>")
	starts, _, _ := fc.snapshot()
	want := []string{homeAlice + "/src/app", homeAlice + "/notes/2026", homeAlice + "/src", "/etc"}
	if len(starts) != len(want) {
		t.Fatalf("starts = %+v", starts)
	}
	for i, w := range want {
		if starts[i].Dir != w || starts[i].DeviceID != pcDesktop || starts[i].User != userAlice {
			t.Fatalf("start %d = %+v, want dir %s", i, starts[i], w)
		}
	}
	h.tg.say(dm(aliceTG), aliceTG, "hello there")
	p := h.hub.waitPrompt(t, 1)
	if p[0].BackendID != beLaptop {
		t.Fatalf("prompt = %+v", p[0])
	}
}

func TestWhereMoveAndSessions(t *testing.T) {
	t.Parallel()
	h, fc := startComputers(t)
	h.tg.say(dm(aliceTG), aliceTG, "/where")
	h.tg.wait(mSend, "No chat is attached")
	sid := h.startTurn(t)
	h.tg.say(dm(aliceTG), aliceTG, "/where")
	h.tg.wait(mSend, "This chat runs in the sandbox")
	h.tg.say(dm(aliceTG), aliceTG, "/move off")
	h.tg.waitN(mSend, "This chat runs in the sandbox", 2)
	h.tg.say(dm(aliceTG), aliceTG, "/move desktop src")
	h.tg.wait(mSend, "Moved. This chat runs on <b>desktop</b> in <code>/home/alice/src</code>.")
	h.tg.say(dm(aliceTG), aliceTG, "/move work pc")
	h.tg.wait(mSend, "That computer is offline")
	_, moves, _ := fc.snapshot()
	if len(moves) != 2 || moves[0] != (moveCall{beCluster, sid, pcDesktop, homeAlice + "/src"}) || moves[1].Dir != "" {
		t.Fatalf("moves = %+v", moves)
	}
	h.hub.mu.Lock()
	h.hub.sessions = append(h.hub.sessions, hub.Session{ID: sCode, BackendID: beCluster, Title: "From VS Code", Live: true,
		Source: "acp", UpdatedAt: time.Now().Add(-time.Minute)})
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	list := h.tg.wait(mSend, "<b>Sessions</b>")
	if !strings.Contains(list.text(), " · desktop: src") || !strings.Contains(list.text(), "From VS Code") ||
		!strings.Contains(list.text(), "from an editor · sandbox") {
		t.Fatalf("sessions = %q", list.text())
	}
	h.tg.say(dm(aliceTG), aliceTG, "/move off")
	ask := h.tg.wait(mSend, "Stop using <b>desktop</b>")
	btns := ask.buttons()
	if len(btns) != 1 {
		t.Fatalf("buttons = %v", btns)
	}
	data, _ := btns[0]["callback_data"].(string)
	h.tg.press(dm(aliceTG), aliceTG, ask.ResultID, "rl:1")
	h.tg.wait(mAnswer, "")
	h.tg.press(dm(aliceTG), aliceTG, ask.ResultID, data)
	h.tg.wait(mEdit, "no longer uses a computer")
	if _, _, rel := fc.snapshot(); len(rel) != 1 || rel[0].SessionID != sid {
		t.Fatalf("released = %+v", rel)
	}
	h.tg.say(dm(aliceTG), aliceTG, "/where")
	h.tg.wait(mSend, "not on a computer")
}

func TestAttachEditorSession(t *testing.T) {
	t.Parallel()
	h, _ := startComputers(t)
	h.hub.mu.Lock()
	h.hub.sessions = append(h.hub.sessions, hub.Session{ID: sCode, BackendID: beCluster, Title: "From VS Code", Live: true, Source: "acp",
		UpdatedAt: time.Now()}, hub.Session{ID: "s-cli", BackendID: beCluster, Title: "From the CLI", Live: true, Source: "cli",
		UpdatedAt: time.Now().Add(-time.Second)})
	h.hub.mu.Unlock()
	h.tg.say(dm(aliceTG), aliceTG, "/sessions")
	list := h.tg.wait(mSend, "<b>Sessions</b>")
	if !strings.Contains(list.text(), "1. From VS Code <i>(cluster)</i> live, from an editor") ||
		!strings.Contains(list.text(), "2. From the CLI <i>(cluster)</i> live, from the terminal") {
		t.Fatalf("sessions = %q", list.text())
	}
	h.tg.say(dm(aliceTG), aliceTG, "/attach 1")
	h.tg.wait(mSend, "Attached to <b>From VS Code</b>")
	h.hub.emit(t, sCode, hub.KindMessageComplete, map[string]any{"text": "reply from the editor turn"})
	h.tg.wait(mSend, "reply from the editor turn")
	h.tg.say(dm(aliceTG), aliceTG, "and one from Telegram")
	if p := h.hub.waitPrompt(t, 1); p[0].SessionID != sCode {
		t.Fatalf("prompt = %+v", p[0])
	}
}
