package device_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
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
