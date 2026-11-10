package broker

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkPeer refuses a peer that runs as another user. The socket's mode
// already limits it to this user; this also covers a socket in a directory
// someone loosened.
func checkPeer(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d is not this user (%d)", cred.Uid, os.Getuid())
	}
	return nil
}
