package device_test

import (
	"context"
	"errors"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/protocol"
)

const (
	appDir    = "app"
	otherHome = "/home/carol"
	keyCount  = "count"
	keyTools  = "tools"
	toolPatch = "patch"
)

func TestChatFolderAndFile(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		e.plugin.SetBinding("s-pc", id, deviceDir)
		e.plugin.SetDeviceFile(id, deviceDir+"/main.go", []byte("package main\n"))
		e.plugin.SetDeviceFile(id, deviceDir+"/"+appDir+"/x.txt", []byte("x"))

		f, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-pc", "")
		if err != nil || f.Path != deviceDir || f.Dir != deviceDir || len(f.Entries) != 2 || f.Entries[0].Name != appDir ||
			f.Entries[0].Kind != keyDir || f.Entries[1].Size != 13 || len(f.Roots) != 1 || f.Roots[0] != homeDir {
			t.Fatalf("ChatFolder = %+v, %v", f, err)
		}
		if _, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-pc", "/etc"); !errors.Is(err, hub.ErrFileRefused) {
			t.Fatalf("folder outside the roots = %v", err)
		}
		m, err := e.a.dev.ChatFile(ctx, userAlice, backendID, "s-pc", deviceDir+"/main.go")
		if err != nil || m.Name != "main.go" || readMedia(t, m) != "package main\n" {
			t.Fatalf("ChatFile = %+v, %v", m, err)
		}
		if _, err := e.a.dev.ChatFile(ctx, userAlice, backendID, "s-pc", deviceDir+"/nope"); !errors.Is(err, hub.ErrFileNotFound) {
			t.Fatalf("missing file = %v", err)
		}

		// The sandbox shows its folder.
		e.plugin.SetSandbox(sandboxID, sandboxFS)
		e.plugin.Connect(sandboxID, sandboxName, protocol.Hello{Roots: []string{sandboxFS}})
		e.plugin.SetDeviceFile(sandboxID, sandboxFS+"/s-new/plot.png", []byte("plot"))
		if f, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-new", ""); err != nil || len(f.Entries) != 1 || f.Entries[0].Name != "plot.png" {
			t.Fatalf("sandbox folder = %+v, %v", f, err)
		}

		e.plugin.Disconnect(id)
		if _, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-pc", ""); !errors.Is(err, hub.ErrMediaOffline) {
			t.Fatalf("offline = %v", err)
		}
		if _, err := e.a.dev.ChatFolder(ctx, userAlice, "elsewhere", "x", ""); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("other backend = %v", err)
		}
		e.access.denied.Store(true)
		if _, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-new", ""); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("lost backend = %v", err)
		}
	})
}

func TestChatDiffAndChanges(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		e.plugin.SetBinding("s-pc", id, deviceDir)
		e.plugin.SetDiff("s-pc", map[string]any{keyDir: deviceDir, "git": true, "files": []any{map[string]any{keyPath: "main.go", "status": "modified"}},
			"untracked": []any{"new.txt"}, "diff": "-a\n+b\n", "hidden": 1})
		d, err := e.a.dev.ChatDiff(ctx, userAlice, backendID, "s-pc")
		if err != nil || !d.Git || len(d.Files) != 1 || d.Files[0].Status != "modified" || d.Untracked[0] != "new.txt" || d.Hidden != 1 || d.Diff != "-a\n+b\n" {
			t.Fatalf("ChatDiff = %+v, %v", d, err)
		}
		e.plugin.SetDiff("s-pc", map[string]any{keyDir: deviceDir, "git": false, "refused": true})
		if d, err := e.a.dev.ChatDiff(ctx, userAlice, backendID, "s-pc"); err != nil || d.Git || !d.Refused {
			t.Fatalf("refused ChatDiff = %+v, %v", d, err)
		}
		e.plugin.SetChanges("s-pc", map[string]any{"session_id": "s-pc", "files": []any{
			map[string]any{keyPath: "notes.md", keyTools: []any{"write_file"}, keyCount: 2, "last_at": 1.7e9, "row_id": 7},
			map[string]any{keyPath: "~/x.txt", keyTools: []any{toolPatch}, keyCount: 1},
			map[string]any{keyPath: "/tmp/y", keyTools: []any{toolPatch}, keyCount: 1},
			map[string]any{keyPath: "~bob/z", keyTools: []any{toolPatch}, keyCount: 1}}})
		c, err := e.a.dev.ChatChanges(ctx, userAlice, backendID, "s-pc")
		if err != nil || len(c.Files) != 4 || c.Files[0].Abs != deviceDir+"/notes.md" || c.Files[0].RowID != 7 || c.Files[0].At.IsZero() ||
			c.Files[1].Abs != homeDir+"/x.txt" || c.Files[2].Abs != "/tmp/y" || c.Files[3].Abs != "" {
			t.Fatalf("ChatChanges = %+v, %v", c, err)
		}
	})
}

func TestChatFilesOnlyForOwnFolders(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		e.computer(deviceName)
		// A chat on the server, or on another person's computer, has no
		// folder to show.
		if _, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-new", ""); !errors.Is(err, hub.ErrNoFolder) {
			t.Fatalf("chat on the server = %v", err)
		}
		e.plugin.Connect("d-carol", "laptop", protocol.Hello{Home: otherHome, Roots: []string{otherHome}})
		e.plugin.SetBinding("s-carol", "d-carol", otherHome)
		e.plugin.SetDeviceFile("d-carol", otherHome+"/secret.txt", []byte("carol's"))
		for _, fn := range []func() error{
			func() error { _, err := e.a.dev.ChatFolder(ctx, userAlice, backendID, "s-carol", ""); return err },
			func() error {
				_, err := e.a.dev.ChatFile(ctx, userAlice, backendID, "s-carol", otherHome+"/secret.txt")
				return err
			},
			func() error { _, err := e.a.dev.ChatDiff(ctx, userAlice, backendID, "s-carol"); return err },
			func() error { _, err := e.a.dev.ChatChanges(ctx, userAlice, backendID, "s-carol"); return err },
		} {
			if err := fn(); !errors.Is(err, hub.ErrNoFolder) {
				t.Fatalf("another person's computer = %v", err)
			}
		}
	})
}
