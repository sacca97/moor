package server

import "golang.org/x/sys/unix"

// peerUID returns the user ID of the process at the other end of a Unix socket.
func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
