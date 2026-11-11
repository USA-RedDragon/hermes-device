package device_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	homeDir   = "/home/alice"
	userBob   = "bob"
	laptop    = "laptop"
	refused   = "/home/alice/secret"
	sandboxFS = "/work"
	srcName   = "src"
)

// computer pairs a device of alice's under name and connects it to the
// plugin with a directory tree under /home/alice.
func (e *env) computer(name string) string {
	e.t.Helper()
	_, id := e.pairAs(e.a, name)
	e.plugin.Connect(id, name, protocol.Hello{Hostname: name + ".example.com", OS: osLinux, Home: homeDir, Roots: []string{homeDir}})
	e.plugin.SetDirs(id, map[string][]string{homeDir: {"notes", srcName}, deviceDir: {"app"}, otherDir: {}, deviceDir + "/app": {}})
	return id
}

func TestComputersAndFolders(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		e.plugin.Connect(sandboxID, sandboxName, protocol.Hello{Roots: []string{sandboxFS}})
		cs, err := e.a.dev.Computers(ctx, userAlice)
		if err != nil || len(cs) != 1 {
			t.Fatalf("computers = %+v, %v", cs, err)
		}
		if c := cs[0]; c.ID != id || c.Name != deviceName || !c.Online || c.Home != homeDir || !slices.Equal(c.Roots, []string{homeDir}) ||
			c.BackendID != backendID || len(c.Recent) != 0 {
			t.Fatalf("computer = %+v", c)
		}
		f, err := e.a.dev.Folders(ctx, userAlice, id, "")
		if err != nil || f.Path != homeDir || !slices.Equal(f.Dirs, []string{"notes", srcName}) {
			t.Fatalf("folders = %+v, %v", f, err)
		}
		if f, err := e.a.dev.Folders(ctx, userAlice, id, deviceDir); err != nil || !slices.Equal(f.Dirs, []string{"app"}) {
			t.Fatalf("src folders = %+v, %v", f, err)
		}
		for _, dir := range []string{refused, srcName} {
			if _, err := e.a.dev.Folders(ctx, userAlice, id, dir); !errors.Is(err, hub.ErrFolderRefused) {
				t.Fatalf("folders %s = %v", dir, err)
			}
		}
		if cs, _ := e.a.dev.Computers(ctx, userBob); len(cs) != 0 {
			t.Fatalf("bob computers = %+v", cs)
		}
		for _, x := range []struct{ user, id string }{{userBob, id}, {userAlice, sandboxID}, {userAlice, "d_none"}} {
			if _, err := e.a.dev.Folders(ctx, x.user, x.id, ""); !errors.Is(err, hub.ErrUnknownDevice) {
				t.Fatalf("folders of %s as %s = %v", x.id, x.user, err)
			}
		}
		e.plugin.Disconnect(id)
		if cs, _ := e.a.dev.Computers(ctx, userAlice); cs[0].Online || cs[0].Roots != nil {
			t.Fatalf("offline computer = %+v", cs[0])
		}
		if _, err := e.a.dev.Folders(ctx, userAlice, id, ""); !errors.Is(err, hub.ErrComputerOffline) {
			t.Fatalf("offline folders = %v", err)
		}
		e.access.denied.Store(true)
		if cs, _ := e.a.dev.Computers(ctx, userAlice); len(cs) != 0 {
			t.Fatalf("computers of a backend alice lost = %+v", cs)
		}
	})
}

func TestStartOn(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		s, p, err := e.a.dev.StartOn(ctx, userAlice, id, deviceDir+"/")
		if err != nil {
			t.Fatal(err)
		}
		row, ok := e.gw.Session(s.ID)
		if !ok || row.CWD != placeholder || row.RowCWD != deviceDir {
			t.Fatalf("session = %+v", row)
		}
		if b := e.plugin.Bindings()[s.ID]; b.DeviceID != id || b.Dir != deviceDir {
			t.Fatalf("binding = %+v", b)
		}
		want := hub.Place{Kind: hub.PlaceComputer, DeviceID: id, Name: deviceName, Dir: deviceDir, Online: true, Mine: true}
		if p != want {
			t.Fatalf("place = %+v", p)
		}
		if got, err := e.b.dev.Place(ctx, userAlice, backendID, s.ID); err != nil || got != want {
			t.Fatalf("place on the other replica = %+v, %v", got, err)
		}
		s2, p, err := e.a.dev.StartOn(ctx, userAlice, id, "")
		if err != nil || p.Dir != homeDir || e.plugin.Bindings()[s2.ID].Dir != homeDir {
			t.Fatalf("start in the first root = %+v, %v", p, err)
		}
		if cs, _ := e.a.dev.Computers(ctx, userAlice); !slices.Equal(cs[0].Recent, []string{homeDir, deviceDir}) {
			t.Fatalf("recent = %v", cs[0].Recent)
		}
		before := len(e.gw.SessionIDs())
		if _, _, err := e.a.dev.StartOn(ctx, userAlice, id, refused); !errors.Is(err, hub.ErrFolderRefused) {
			t.Fatalf("start in a refused folder = %v", err)
		}
		if n := len(e.gw.SessionIDs()); n != before {
			t.Fatalf("a session whose bind failed stayed: %d -> %d", before, n)
		}
		if _, _, err := e.a.dev.StartOn(ctx, userAlice, id, srcName); !errors.Is(err, hub.ErrFolderRefused) {
			t.Fatalf("relative folder = %v", err)
		}
		if _, _, err := e.a.dev.StartOn(ctx, userBob, id, deviceDir); !errors.Is(err, hub.ErrUnknownDevice) {
			t.Fatalf("bob starts on alice's computer = %v", err)
		}
		e.plugin.Disconnect(id)
		if _, _, err := e.a.dev.StartOn(ctx, userAlice, id, deviceDir); !errors.Is(err, hub.ErrComputerOffline) {
			t.Fatalf("offline start = %v", err)
		}
		if n := len(e.gw.SessionIDs()); n != before {
			t.Fatalf("an offline start made a session: %d -> %d", before, n)
		}
		if p, _ := e.a.dev.Place(ctx, userAlice, backendID, s.ID); p.Online || p.Kind != hub.PlaceComputer {
			t.Fatalf("place of an offline computer = %+v", p)
		}
	})
}

func TestPlaces(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		e.plugin.SetSandbox(sandboxID, sandboxFS)
		e.plugin.Connect(sandboxID, sandboxName, protocol.Hello{Roots: []string{sandboxFS}})
		e.plugin.SetBinding("s-pc", id, deviceDir)
		e.plugin.SetParent("s-pc-child", "s-pc")
		e.plugin.SetBinding("s-off", id, deviceDir)
		if err := e.a.dev.Release(ctx, userAlice, backendID, "s-off"); err != nil {
			t.Fatal(err)
		}
		refs := []hub.SessionRef{{BackendID: backendID, SessionID: "s-pc"}, {BackendID: backendID, SessionID: "s-pc-child"},
			{BackendID: backendID, SessionID: "s-new"}, {BackendID: backendID, SessionID: "s-off"}, {BackendID: "elsewhere", SessionID: "x"}}
		got := e.a.dev.Places(ctx, userAlice, refs)
		pc := hub.Place{Kind: hub.PlaceComputer, DeviceID: id, Name: deviceName, Dir: deviceDir, Online: true, Mine: true}
		child := pc
		child.Inherited = true
		want := map[hub.SessionRef]hub.Place{refs[0]: pc, refs[1]: child,
			refs[2]: {Kind: hub.PlaceSandbox, DeviceID: sandboxID, Name: sandboxName, Dir: sandboxFS + "/s-new", Online: true},
			refs[3]: {Kind: hub.PlaceNone}}
		if len(got) != len(want) {
			t.Fatalf("places = %+v", got)
		}
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("place of %s = %+v, want %+v", k.SessionID, got[k], w)
			}
		}
		resolves := len(e.plugin.Calls())
		e.plugin.Disconnect(sandboxID)
		got = e.a.dev.Places(ctx, userAlice, refs)
		if p := got[refs[2]]; p.Kind != hub.PlaceSandbox || p.Online {
			t.Fatalf("sandbox offline = %+v", p)
		}
		if n := len(e.plugin.Calls()) - resolves; n != 2 {
			t.Fatalf("second Places sent %d plugin requests, want 2 (devices and bindings)", n)
		}
		e.plugin.SetBinding("s-new", id, otherDir)
		if p := e.a.dev.Places(ctx, userAlice, refs[2:3])[refs[2]]; p.Kind != hub.PlaceComputer || p.Dir != otherDir {
			t.Fatalf("place after a bind elsewhere = %+v", p)
		}
		if got := e.a.dev.Places(ctx, userBob, refs); len(got) != 0 {
			t.Fatalf("bob places = %+v", got)
		}
		e.plugin.SetSandbox("", "")
		if p, _ := e.a.dev.Place(ctx, userAlice, backendID, "s-plain"); p.Kind != hub.PlaceServer {
			t.Fatalf("place with fallback local = %+v", p)
		}
		if p, err := e.a.dev.Place(ctx, userAlice, "elsewhere", "x"); err == nil || p.Kind != "" {
			t.Fatalf("place on an unknown backend = %+v, %v", p, err)
		}
	})
}

func TestUseBrowser(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		if err := e.a.dev.UseBrowser(ctx, userAlice, backendID, "s1", id); err != nil {
			t.Fatal(err)
		}
		if got := e.plugin.Browsers()["s1"]; got != id {
			t.Fatalf("browser device = %q, want %q", got, id)
		}
		if err := e.a.dev.UseBrowser(ctx, userBob, backendID, "s2", id); err == nil {
			t.Error("bob set alice's computer as a browser device")
		}
		for _, device := range []string{sandboxID, "d_none"} {
			if err := e.a.dev.UseBrowser(ctx, userAlice, backendID, "s2", device); !errors.Is(err, hub.ErrUnknownDevice) {
				t.Errorf("UseBrowser %s = %v", device, err)
			}
		}
		e.plugin.Disconnect(id)
		if err := e.a.dev.UseBrowser(ctx, userAlice, backendID, "s2", id); !errors.Is(err, hub.ErrComputerOffline) {
			t.Errorf("UseBrowser offline = %v", err)
		}
		if _, ok := e.plugin.Browsers()["s2"]; ok {
			t.Error("a refused UseBrowser set a browser device")
		}
	})
}

func TestMoveAndRelease(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		id2 := e.computer(laptop)
		p, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id, deviceDir)
		if err != nil || p.Kind != hub.PlaceComputer || p.DeviceID != id || p.Dir != deviceDir {
			t.Fatalf("move from the sandbox = %+v, %v", p, err)
		}
		if p, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id, otherDir); err != nil || e.plugin.Bindings()["s1"].Dir != otherDir || p.Dir != otherDir {
			t.Fatalf("move to another folder = %+v, %v", p, err)
		}
		if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id2, refused); !errors.Is(err, hub.ErrFolderRefused) {
			t.Fatalf("move to a refused folder = %v", err)
		}
		if b := e.plugin.Bindings()["s1"]; b.DeviceID != id || b.Dir != otherDir {
			t.Fatalf("binding after a refused move = %+v", b)
		}
		if p, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id2, deviceDir); err != nil || p.Name != laptop {
			t.Fatalf("move to the laptop = %+v, %v", p, err)
		}
		if b := e.plugin.Bindings()["s1"]; b.DeviceID != id2 || b.Dir != deviceDir {
			t.Fatalf("binding after the move = %+v", b)
		}
		e.plugin.SetParent("s1-child", "s1")
		if err := e.a.dev.Release(ctx, userAlice, backendID, "s1-child"); !errors.Is(err, hub.ErrInheritedPlace) {
			t.Fatalf("release a child = %v", err)
		}
		if err := e.a.dev.Release(ctx, userBob, backendID, "s1"); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("bob releases = %v", err)
		}
		if err := e.a.dev.Release(ctx, userAlice, backendID, "s1"); err != nil {
			t.Fatal(err)
		}
		if p, _ := e.a.dev.Place(ctx, userAlice, backendID, "s1"); p.Kind != hub.PlaceNone {
			t.Fatalf("place after release = %+v", p)
		}
		if err := e.a.dev.Release(ctx, userAlice, backendID, "s1"); !errors.Is(err, hub.ErrNotOnComputer) {
			t.Fatalf("release again = %v", err)
		}
		if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id, deviceDir); err != nil {
			t.Fatalf("move after release = %v", err)
		}
		e.plugin.RefuseBinds("s-old", "not_device_session")
		if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s-old", id, deviceDir); !errors.Is(err, hub.ErrCannotMove) {
			t.Fatalf("move a session that cannot run on a device = %v", err)
		}
		bobsComputer(t, e, id)
	})
}

func TestReleaseAndMoveEdges(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		id2 := e.computer(laptop)

		// A branch with a binding of its own under a bound parent would
		// follow the parent after an unbind, so Release refuses it.
		e.plugin.SetBinding("parent", id, deviceDir)
		e.plugin.SetBinding("branch", id, otherDir)
		e.plugin.SetParent("branch", "parent")
		if err := e.a.dev.Release(ctx, userAlice, backendID, "branch"); !errors.Is(err, hub.ErrInheritedPlace) {
			t.Fatalf("release under a bound parent = %v", err)
		}
		if b := e.plugin.Bindings()["branch"]; b.DeviceID != id || b.Dir != otherDir {
			t.Fatalf("branch binding after the refused release = %+v", b)
		}

		// Releasing a parent changes the cached place of its children.
		e.plugin.SetParent("child", "parent")
		ref := hub.SessionRef{BackendID: backendID, SessionID: "child"}
		if p := e.a.dev.Places(ctx, userAlice, []hub.SessionRef{ref})[ref]; !p.Inherited || p.Dir != deviceDir {
			t.Fatalf("child place = %+v", p)
		}
		if err := e.a.dev.Release(ctx, userAlice, backendID, "parent"); err != nil {
			t.Fatal(err)
		}
		if p := e.a.dev.Places(ctx, userAlice, []hub.SessionRef{ref})[ref]; p.Kind != hub.PlaceNone {
			t.Fatalf("child place after the parent's release = %+v", p)
		}

		// A move whose bind fails goes back to the computer it left.
		e.plugin.SetBinding("s1", id, deviceDir)
		e.plugin.RefuseBindsTo(id2, "state_write_failed")
		if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id2, deviceDir); err == nil {
			t.Fatal("a refused move worked")
		}
		if b := e.plugin.Bindings()["s1"]; b.DeviceID != id || b.Dir != deviceDir {
			t.Fatalf("binding after a refused move = %+v", b)
		}

		// When the way back fails too, the error says the chat is on no computer.
		e.plugin.RefuseBinds("s1", "state_write_failed")
		e.plugin.RefuseBindsTo(id2, "")
		e.plugin.SetBinding("s1", id, deviceDir)
		_, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s1", id2, deviceDir)
		if !errors.Is(err, hub.ErrCannotMove) || !strings.Contains(err.Error(), "on no computer now") {
			t.Fatalf("move that cannot go back = %v", err)
		}
	})
}

// bobsComputer checks that alice cannot move or unbind chats on a computer
// of bob's, nor move one to it, and does not see its folder.
func bobsComputer(t *testing.T, e *env, id string) {
	t.Helper()
	ctx := context.Background()
	_, code, err := e.st.StartPairing(ctx, hub.PairingRequest{Name: "bobs", Hostname: "bob.example.com"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := e.st.ApprovePairing(ctx, code, userBob, backendID, "bobs")
	if err != nil {
		t.Fatal(err)
	}
	e.plugin.SetBinding("s-bob", bobs.ID, "/home/bob")
	if err := e.a.dev.Release(ctx, userAlice, backendID, "s-bob"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("release from bob's computer = %v", err)
	}
	if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s-bob", id, deviceDir); !errors.Is(err, hub.ErrCannotMove) {
		t.Fatalf("move from bob's computer = %v", err)
	}
	if p, _ := e.a.dev.Place(ctx, userAlice, backendID, "s-bob"); p.Name != "another computer" || p.Mine || p.Dir != "" || p.DeviceID != "" {
		t.Fatalf("place on bob's computer = %+v", p)
	}
	e.plugin.SetParent("s-bob-branch", "s-bob")
	if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s-bob-branch", id, deviceDir); !errors.Is(err, hub.ErrCannotMove) {
		t.Fatalf("move a branch that follows bob's computer = %v", err)
	}
	if _, ok := e.plugin.Bindings()["s-bob-branch"]; ok {
		t.Fatal("the branch of bob's chat was bound")
	}
	if _, err := e.a.dev.MoveTo(ctx, userAlice, backendID, "s2", bobs.ID, homeDir); !errors.Is(err, hub.ErrUnknownDevice) {
		t.Fatalf("move to bob's computer = %v", err)
	}
}
