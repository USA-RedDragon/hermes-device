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

// The config carries the daemon's hub.url, the legacy auth_status follows
// it, and an old extension's auth message changes nothing.
func TestConfigHasHubURL(t *testing.T) {
	t.Parallel()
	b, sock := start(t)
	conn := dialRaw(t, sock)
	send := func(m any) {
		if err := nativemsg.Write(conn, mustJSON(t, m), nativemsg.MaxToBrowser); err != nil {
			t.Fatal(err)
		}
	}
	send(broker.Message{Type: broker.TypeHostHello, ExtensionID: host.ExtensionID, HostPID: 1})
	send(broker.Message{Type: broker.TypeHello, Version: "0.5.0"})
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := nativemsg.Read(conn, nativemsg.MaxToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	var m broker.Message
	if err := json.Unmarshal(data, &m); err != nil || m.Type != broker.TypeConfig || m.HubURL != "https://hub.example.com" ||
		len(m.AllowHosts) != 1 {
		t.Fatalf("config = %s, %v", data, err)
	}
	if data, err = nativemsg.Read(conn, nativemsg.MaxToBrowser); err != nil || string(data) != `{"type":"auth_status","state":"signed_in"}` {
		t.Fatalf("legacy auth_status = %s, %v", data, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := b.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	b.SetDeviceID("d_8f2c")
	if data, err = nativemsg.Read(conn, nativemsg.MaxToBrowser); err != nil ||
		json.Unmarshal(data, &m) != nil || m.Type != broker.TypeConfig || m.DeviceID != "d_8f2c" || m.HubURL == "" {
		t.Fatalf("config after SetDeviceID = %s, %v", data, err)
	}
	ch := b.Changed()
	send(map[string]string{"type": "auth", "token": "hbt_abc"})
	select {
	case <-ch:
		t.Fatal("an auth message changed the broker's state")
	case <-time.After(200 * time.Millisecond):
	}
	if _, ok := b.Connected(); !ok {
		t.Fatal("extension dropped after an auth message")
	}
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
