package client

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// TermSize returns the size of the terminal on fd, or 24x80 if it has none.
func TermSize(fd int) (rows, cols uint16) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return ws.Row, ws.Col
}

// watchResize calls send with the new terminal size on every SIGWINCH until
// stop is closed.
func watchResize(fd int, send func(rows, cols uint16), stop <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				send(TermSize(fd))
			case <-stop:
				return
			}
		}
	}()
}
