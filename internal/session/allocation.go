package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// CreateOptions describes a new session.
type CreateOptions struct {
	// Name is an explicit session name. Empty means derive one.
	Name string
	// DefaultName is the name to use, made unique, when Name is empty;
	// without it the name is derived from Command.
	DefaultName string
	// Command is injected into the shell after it starts. Empty means none.
	Command string
	// Rows and Cols are the initial PTY size.
	Rows, Cols uint16
}

const startTimeout = 10 * time.Second

// LowestFree returns the smallest non-negative integer not in used, which
// must be sorted ascending (as scanLocked returns it).
func LowestFree(used []int) int {
	id := 0
	for _, u := range used {
		if u > id {
			break
		}
		if u == id {
			id++
		}
	}
	return id
}

// Create allocates an ID, writes the session metadata and starts the session
// server in the background. The whole operation runs under the runtime lock,
// so concurrent invocations never pick the same ID or name.
func Create(opts CreateOptions) (Meta, error) {
	unlock, err := Lock()
	if err != nil {
		return Meta{}, err
	}
	defer unlock()

	live, used, err := scanLocked()
	if err != nil {
		return Meta{}, err
	}
	names := make(map[string]bool, len(live))
	for _, s := range live {
		names[s.Name] = true
	}

	name := opts.Name
	if name != "" {
		if err := ValidateName(name); err != nil {
			return Meta{}, err
		}
		if names[name] {
			return Meta{}, fmt.Errorf("session name %q is already in use", name)
		}
	} else {
		base := opts.DefaultName
		if base == "" {
			base = AutoName(opts.Command)
		}
		name = UniqueName(base, names)
	}

	id := LowestFree(used)
	dir := Dir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Meta{}, err
	}
	cwd, _ := os.Getwd()
	m := Meta{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now().Truncate(time.Second),
		CWD:       cwd,
		Command:   opts.Command,
	}
	if err := WriteMeta(dir, m); err != nil {
		os.RemoveAll(dir)
		return Meta{}, err
	}
	if err := spawnServer(id, opts.Rows, opts.Cols); err != nil {
		os.RemoveAll(dir)
		return Meta{}, err
	}
	if updated, err := ReadMeta(dir); err == nil {
		m = updated
	}
	return m, nil
}

// spawnServer starts "moor __serve" detached from the current terminal and
// waits until it reports readiness on an inherited pipe.
func spawnServer(id int, rows, cols uint16) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()

	cmd := exec.Command(exe, "__serve", strconv.Itoa(id),
		strconv.Itoa(int(rows)), strconv.Itoa(int(cols)))
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.ExtraFiles = []*os.File{w} // fd 3 in the server
	// A new session: no controlling terminal, so closing the launching
	// terminal or logging out never delivers SIGHUP to the server.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return err
	}
	w.Close()

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data, err}
	}()

	var res result
	select {
	case res = <-ch:
	case <-time.After(startTimeout):
		cmd.Process.Kill()
		cmd.Wait()
		return errors.New("timed out waiting for session server to start")
	}
	if res.err == nil && bytes.Equal(res.data, []byte("ok")) {
		// The server outlives us; don't keep it as a child to reap.
		return cmd.Process.Release()
	}
	cmd.Wait()
	msg := string(bytes.TrimSpace(res.data))
	if msg == "" {
		msg = "server exited during startup"
	}
	return fmt.Errorf("failed to start session: %s", msg)
}
