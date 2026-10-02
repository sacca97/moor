package server

import (
	"net"
	"os"
)

// sameUser rejects connections from other users, in addition to the socket
// and directory permissions.
func sameUser(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var uid uint32
	var perr error
	if err := raw.Control(func(fd uintptr) { uid, perr = peerUID(int(fd)) }); err != nil {
		return false
	}
	return perr == nil && int(uid) == os.Getuid()
}
