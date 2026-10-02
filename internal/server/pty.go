package server

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/sacca/moor/internal/protocol"
)

// ShellPath returns the user's shell: $SHELL, or /bin/sh.
func ShellPath() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return sh
		}
	}
	return "/bin/sh"
}

// startShell starts the user's shell on a new PTY. The shell becomes a
// session leader with the PTY as its controlling terminal, and is interactive
// because its stdin is a terminal. It inherits our environment and working
// directory, plus MOOR_SESSION and MOOR_SESSION_NAME.
func startShell(id int, name, dir string, rows, cols uint16) (*os.File, *exec.Cmd, error) {
	shell := ShellPath()
	cmd := shellCommand(shell, name, dir)
	// Later entries win, so the wrapper's variables override inherited ones.
	cmd.Env = append(append(os.Environ(), cmd.Env...),
		fmt.Sprintf("MOOR_SESSION=%d", id), "MOOR_SESSION_NAME="+name)
	if os.Getenv("TERM") == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, nil, fmt.Errorf("starting %s: %w", shell, err)
	}
	return ptmx, cmd, nil
}

// ptyControl runs f with the PTY master's descriptor. It goes through
// SyscallConn rather than Fd, because Fd switches the file to blocking mode,
// after which closing it can no longer interrupt a pending Read: the master
// would stay open and the hangup that closing it causes would never happen.
func ptyControl(ptmx *os.File, f func(fd int)) {
	if rc, err := ptmx.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { f(int(fd)) })
	}
}

// foregroundPgrp returns the PTY's foreground process group, or 0.
func foregroundPgrp(ptmx *os.File) (pgrp int) {
	ptyControl(ptmx, func(fd int) {
		if p, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err == nil && p > 0 {
			pgrp = p
		}
	})
	return pgrp
}

// setSize applies a client's terminal size to the PTY, which makes the kernel
// deliver SIGWINCH to the foreground process group. When force is set and the
// size is unchanged, SIGWINCH is sent anyway so full-screen programs redraw
// for a newly attached client.
func setSize(ptmx *os.File, size protocol.Resize, force bool) {
	if size.Rows == 0 || size.Cols == 0 {
		return
	}
	want := unix.Winsize{Row: size.Rows, Col: size.Cols, Xpixel: size.XPixel, Ypixel: size.YPixel}
	ptyControl(ptmx, func(fd int) {
		if cur, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil && *cur == want {
			if force {
				if pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err == nil && pgrp > 0 {
					syscall.Kill(-pgrp, syscall.SIGWINCH)
				}
			}
			return
		}
		unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &want)
	})
}
