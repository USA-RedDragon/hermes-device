//go:build !linux

package broker

import "net"

// checkPeer relies on the socket's 0600 mode in a 0700 directory on systems
// without SO_PEERCRED.
func checkPeer(*net.UnixConn) error { return nil }
