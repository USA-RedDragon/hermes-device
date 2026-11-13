package device_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

func TestPutFile(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		e.plugin.SetBinding("s-pc", id, deviceDir)
		p, ok, err := e.a.dev.PutFile(ctx, userAlice, backendID, "s-pc", "notes.txt", []byte("hi"))
		want := deviceDir + "/.hermes/attachments/notes.txt"
		if err != nil || !ok || p != want || string(e.plugin.Uploads()[want]) != "hi" {
			t.Fatalf("PutFile = %q, %v, %v", p, ok, err)
		}
		if _, ok, err := e.a.dev.PutFile(ctx, userAlice, backendID, "s-new", "a.txt", []byte("x")); ok || err != nil {
			t.Fatalf("a session on the pod = %v, %v", ok, err)
		}
		e.plugin.SetSandbox(sandboxID, sandboxFS)
		e.plugin.Connect(sandboxID, sandboxName, protocol.Hello{Roots: []string{sandboxFS}})
		if p, ok, err := e.a.dev.PutFile(ctx, userAlice, backendID, "s-new", "a.txt", []byte("x")); !ok || err != nil ||
			p != sandboxFS+"/s-new/.hermes/attachments/a.txt" {
			t.Fatalf("sandbox PutFile = %q, %v, %v", p, ok, err)
		}
		if _, ok, _ := e.a.dev.PutFile(ctx, userAlice, "elsewhere", "x", "a.txt", []byte("x")); ok {
			t.Fatal("a backend without devices took a file")
		}
		e.plugin.Disconnect(id)
		_, _, err = e.a.dev.PutFile(ctx, userAlice, backendID, "s-pc", "notes.txt", []byte("hi"))
		if !errors.Is(err, hub.ErrAttachment) || !strings.Contains(hub.AttachmentReason(err), "offline") {
			t.Fatalf("offline PutFile = %v", err)
		}
		if err := e.a.dev.Release(ctx, userAlice, backendID, "s-pc"); err != nil {
			t.Fatal(err)
		}
		_, _, err = e.a.dev.PutFile(ctx, userAlice, backendID, "s-pc", "notes.txt", []byte("hi"))
		if !errors.Is(err, hub.ErrAttachment) || !strings.Contains(hub.AttachmentReason(err), "runs on no computer") {
			t.Fatalf("released PutFile = %v", err)
		}
		e.access.denied.Store(true)
		if _, _, err := e.a.dev.PutFile(ctx, userAlice, backendID, "s-new", "a.txt", []byte("x")); err == nil {
			t.Fatal("PutFile on a backend the user lost")
		}
	})
}

func readMedia(t *testing.T, f *hub.MediaFile) string {
	t.Helper()
	defer f.Body.Close()
	b, err := io.ReadAll(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeviceMedia(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		ctx := context.Background()
		id := e.computer(deviceName)
		chart := deviceDir + "/out/chart.png"
		e.plugin.SetDeviceFile(id, chart, []byte("png bytes"))
		e.plugin.SetDeviceFile(id, "/etc/hosts.txt", []byte("refused"))
		e.plugin.SetBinding("s-pc", id, deviceDir)
		f, ok, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-pc", chart)
		if err != nil || !ok || f.Name != "chart.png" || readMedia(t, f) != "png bytes" {
			t.Fatalf("DeviceMedia = %+v, %v, %v", f, ok, err)
		}
		e.plugin.BusyReads(2)
		if f, ok, err = e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-pc", chart); err != nil || !ok || readMedia(t, f) != "png bytes" {
			t.Fatalf("after busy answers = %v, %v", ok, err)
		}
		for _, p := range []string{deviceDir + "/missing.png", "/etc/hosts.txt"} {
			if _, ok, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-pc", p); !ok || !errors.Is(err, hub.ErrNoMedia) {
				t.Fatalf("%s = %v, %v", p, ok, err)
			}
		}
		if _, ok, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-new", chart); ok || err != nil {
			t.Fatalf("a session on the pod = %v, %v", ok, err)
		}
		if _, ok, _ := e.a.dev.DeviceMedia(ctx, userAlice, "elsewhere", "x", chart); ok {
			t.Fatal("a backend without devices read a file")
		}
		e.plugin.SetSandbox(sandboxID, sandboxFS)
		e.plugin.Connect(sandboxID, sandboxName, protocol.Hello{Roots: []string{sandboxFS}})
		e.plugin.SetDeviceFile(sandboxID, sandboxFS+"/s-new/plot.png", []byte("plot"))
		if f, ok, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-new", sandboxFS+"/s-new/plot.png"); err != nil || !ok || readMedia(t, f) != "plot" {
			t.Fatalf("sandbox DeviceMedia = %v, %v", ok, err)
		}
		e.plugin.Disconnect(id)
		if _, ok, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-pc", chart); !ok || !errors.Is(err, hub.ErrMediaOffline) {
			t.Fatalf("offline = %v, %v", ok, err)
		}
		e.access.denied.Store(true)
		if _, _, err := e.a.dev.DeviceMedia(ctx, userAlice, backendID, "s-new", sandboxFS+"/s-new/plot.png"); err == nil {
			t.Fatal("DeviceMedia on a backend the user lost")
		}
	})
}
