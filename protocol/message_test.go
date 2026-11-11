package protocol_test

import (
	"errors"
	"testing"

	"github.com/USA-RedDragon/hermes-device/protocol"
)

func TestMessageType(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`{"type":"browser","id":"x"}`:       "browser",
		`{"id":"x"}`:                        "",
		"\n{\"type\":\"ping\",\"seq\":1}\n": "ping",
		`{"type":"exec","cmd":{"a":[1]}}`:   "exec",
	} {
		got, _, err := protocol.MessageType([]byte(in))
		if err != nil || got != want {
			t.Errorf("MessageType(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		`{"type":"browser","type":null}`,
		`{"type":"browser_auth_result","type":null,"ok":true}`,
		`{"type":"exec","type":"browser"}`,
		`{"type":"exec","TYPE":"browser"}`,
		`{"Type":"browser"}`,
		`{"type":null}`,
		`{"type":1}`,
		`{"type":["browser"]}`,
		`["browser"]`,
		`"browser"`,
		`{"type":"ping"} {"type":"browser"}`,
		`{"type":"ping"`,
		`{"id":"a","id":"b"}`,
	} {
		if _, _, err := protocol.MessageType([]byte(in)); !errors.Is(err, protocol.ErrAmbiguousMessage) {
			t.Errorf("MessageType(%s) err = %v, want ErrAmbiguousMessage", in, err)
		}
	}
}
