package broker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
	"github.com/USA-RedDragon/hermes-device/internal/browser/nativemsg"
)

// fakeExtension plays Firefox and the extension on the other end of the
// native host's stdio.
func fakeExtension(t *testing.T, fromHost io.Reader, toHost io.Writer) {
	t.Helper()
	hello := mustJSON(t, broker.Message{Type: broker.TypeHello, Version: "0.1.0", Browser: "Firefox 157.0"})
	if err := nativemsg.Write(toHost, hello, nativemsg.MaxFromBrowser); err != nil {
		t.Error(err)
		return
	}
	for {
		data, err := nativemsg.Read(fromHost, nativemsg.MaxToBrowser)
		if err != nil {
			return
		}
		var m broker.Message
		if err := json.Unmarshal(data, &m); err != nil {
			t.Error(err)
			return
		}
		if m.Type == broker.TypeConfig {
			if len(m.AllowHosts) != 1 || m.AllowHosts[0] != "127.0.0.1" {
				t.Errorf("config = %+v", m)
			}
			continue
		}
		if m.Type != broker.TypeRequest {
			continue
		}
		resp := broker.Message{Type: broker.TypeResponse, ID: m.ID, OK: true}
		switch m.Op {
		case "echo":
			resp.Result = m.Args
		default:
			resp.OK = false
			resp.Code = broker.CodeRefused
			resp.Error = "unknown op " + m.Op
		}
		out := mustJSON(t, resp)
		if err := nativemsg.Write(toHost, out, nativemsg.MaxFromBrowser); err != nil {
			return
		}
	}
}

func start(t *testing.T) (*broker.Broker, string) {
	t.Helper()
	var b *broker.Broker
	// t.TempDir paths can pass the 108 byte limit of a socket path.
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "state", broker.SocketName)
	b, err = broker.Listen(broker.Options{Path: sock, ExtensionID: host.ExtensionID, AllowHosts: []string{"127.0.0.1"},
		HubURL: "https://hub.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return b, sock
}

func connectHost(t *testing.T, sock, extensionID string) <-chan error {
	t.Helper()
	ffIn, hostOut := io.Pipe()
	hostIn, ffOut := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- host.Run(hostIn, hostOut, sock, extensionID)
		_ = hostOut.Close()
	}()
	go fakeExtension(t, ffIn, ffOut)
	t.Cleanup(func() { _ = ffOut.Close() })
	return errc
}

func TestCallThroughHost(t *testing.T) {
	t.Parallel()
	b, sock := start(t)
	if _, err := b.Call(t.Context(), "echo", nil); !errors.Is(err, broker.ErrNotConnected) {
		t.Fatalf("err before connect = %v", err)
	}
	connectHost(t, sock, host.ExtensionID)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hello, err := b.WaitConnected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Browser != "Firefox 157.0" || hello.ExtensionID != host.ExtensionID || hello.HostPID == 0 {
		t.Fatalf("hello = %+v", hello)
	}
	res, err := b.Call(ctx, "echo", map[string]any{"url": "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"url":"https://example.com/"}` {
		t.Fatalf("result = %s", res)
	}
	_, err = b.Call(ctx, "nope", nil)
	var extErr *broker.Error
	if !errors.As(err, &extErr) || !extErr.Refused() || !strings.Contains(err.Error(), "unknown op nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostRefusesOtherExtension(t *testing.T) {
	t.Parallel()
	_, sock := start(t)
	err := host.Run(strings.NewReader(""), io.Discard, sock, "evil@example.com")
	if !errors.Is(err, host.ErrWrongExtension) {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerRefusesOtherExtensionHello(t *testing.T) {
	t.Parallel()
	b, sock := start(t)
	// A local process that skips the host's own check gets dropped by the
	// broker.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		hh := mustJSON(t, broker.Message{Type: broker.TypeHostHello, ExtensionID: "evil@example.com"})
		_ = nativemsg.Write(pw, hh, nativemsg.MaxToBrowser)
	}()
	conn := dialRaw(t, sock)
	go func() { _, _ = io.Copy(conn, pr) }()
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("read err = %v, want EOF from a closed connection", err)
	}
	if _, ok := b.Connected(); ok {
		t.Fatal("broker accepted another extension")
	}
}

func TestCallFailsWhenExtensionGoes(t *testing.T) {
	t.Parallel()
	b, sock := start(t)
	conn := dialRaw(t, sock)
	for _, m := range []broker.Message{
		{Type: broker.TypeHostHello, ExtensionID: host.ExtensionID, HostPID: 1},
		{Type: broker.TypeHello, Version: "0.1.0"},
	} {
		data := mustJSON(t, m)
		if err := nativemsg.Write(conn, data, nativemsg.MaxToBrowser); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := b.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = nativemsg.Read(conn, nativemsg.MaxToBrowser)
		_ = conn.Close()
	}()
	if _, err := b.Call(ctx, "echo", nil); !errors.Is(err, broker.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// The extension's credential reaches Token, a malformed one is ignored,
// and the device and the hub's answer reach the extension.
func TestAuthAndDevice(t *testing.T) {
	t.Parallel()
	b, sock := start(t)
	b.SetDevice("d_1", "desktop")
	conn := dialRaw(t, sock)
	send := func(m broker.Message) {
		if err := nativemsg.Write(conn, mustJSON(t, m), nativemsg.MaxToBrowser); err != nil {
			t.Fatal(err)
		}
	}
	read := func() broker.Message {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		data, err := nativemsg.Read(conn, nativemsg.MaxToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		var m broker.Message
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(broker.Message{Type: broker.TypeHostHello, ExtensionID: host.ExtensionID, HostPID: 1})
	send(broker.Message{Type: broker.TypeHello, Version: "0.5.0"})
	if m := read(); m.Type != broker.TypeConfig || m.DeviceID != "d_1" || m.DeviceName != "desktop" || m.HubURL != "https://hub.example.com" {
		t.Fatalf("config = %+v", m)
	}
	if m := read(); m.Type != broker.TypeAuthStatus || m.State != broker.AuthOffline {
		t.Fatalf("first auth_status = %+v", m)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := b.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	waitToken := func(want string) {
		for b.Token() != want {
			select {
			case <-b.Changed():
			case <-ctx.Done():
				t.Fatalf("token = %q, want %q", b.Token(), want)
			}
		}
	}
	ch := b.Changed()
	send(broker.Message{Type: broker.TypeAuth, Token: "not-a-hub-credential"}) //nolint:gosec // a test value
	send(broker.Message{Type: broker.TypeAuth, Token: "hbt_abc"})
	<-ch
	waitToken("hbt_abc")
	b.SetAuth(broker.AuthStatus{State: broker.AuthSignedIn, User: "alice"})
	if m := read(); m.Type != broker.TypeAuthStatus || m.State != broker.AuthSignedIn || m.User != "alice" {
		t.Fatalf("auth_status = %+v", m)
	}
	b.SetDevice("d_2", "laptop")
	if m := read(); m.Type != broker.TypeConfig || m.DeviceID != "d_2" {
		t.Fatalf("config after SetDevice = %+v", m)
	}
	send(broker.Message{Type: broker.TypeAuth})
	waitToken("")
	_ = conn.Close()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Error(err)
	}
	return data
}
