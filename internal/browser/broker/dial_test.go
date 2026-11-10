package broker_test

import (
	"net"
	"testing"
)

func dialRaw(t *testing.T, sock string) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
