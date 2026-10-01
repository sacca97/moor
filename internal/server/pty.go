package server

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
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

// setSize applies a client's terminal size to the PTY, which makes the kernel
// deliver SIGWINCH to the foreground process group. When force is set and the
// size is unchanged, SIGWINCH is sent anyway so full-screen programs redraw
// for a newly attached client.
func setSize(ptmx *os.File, rows, cols uint16, force bool) {
	if rows == 0 || cols == 0 {
		return
	}
	cur, err := pty.GetsizeFull(ptmx)
	if err == nil && cur.Rows == rows && cur.Cols == cols {
		if force {
			if pgrp, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP); err == nil && pgrp > 0 {
				syscall.Kill(-pgrp, syscall.SIGWINCH)
			}
		}
		return
	}
	pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}
