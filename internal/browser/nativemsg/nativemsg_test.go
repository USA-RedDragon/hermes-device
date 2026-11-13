package nativemsg_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/browser/nativemsg"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	for _, m := range []string{`{"a":1}`, `{}`, `"x"`} {
		if err := nativemsg.Write(&buf, []byte(m), nativemsg.MaxToBrowser); err != nil {
			t.Fatal(err)
		}
	}
	if got := buf.Bytes()[:4]; !bytes.Equal(got, []byte{7, 0, 0, 0}) {
		t.Fatalf("header = %v, want little-endian 7", got)
	}
	for _, want := range []string{`{"a":1}`, `{}`, `"x"`} {
		got, err := nativemsg.Read(&buf, nativemsg.MaxFromBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if _, err := nativemsg.Read(&buf, nativemsg.MaxFromBrowser); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
}

func TestLimits(t *testing.T) {
	t.Parallel()
	if err := nativemsg.Write(io.Discard, make([]byte, 11), 10); !errors.Is(err, nativemsg.ErrTooLarge) {
		t.Fatalf("write err = %v", err)
	}
	r := bytes.NewReader([]byte{0xff, 0xff, 0xff, 0x7f})
	if _, err := nativemsg.Read(r, nativemsg.MaxFromBrowser); !errors.Is(err, nativemsg.ErrTooLarge) {
		t.Fatalf("read err = %v", err)
	}
	r = bytes.NewReader([]byte{5, 0, 0, 0, '{'})
	if _, err := nativemsg.Read(r, 10); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read err = %v", err)
	}
}
