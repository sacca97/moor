package session

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sacca/moor/internal/protocol"
)

const probeTimeout = time.Second

var (
	// ErrStale means nothing is listening on the session's socket.
	ErrStale = errors.New("session server is not running")
	// ErrUnresponsive means the socket accepted a connection but the server
	// did not answer in time. Such sessions are hidden but not removed.
	ErrUnresponsive = errors.New("session server is not responding")
)

// Root returns the moor runtime directory:
// $XDG_RUNTIME_DIR/moor, or /tmp/moor-$UID as a fallback.
func Root() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
		// Only trust it if it is ours and not writable by others, as the
		// XDG specification requires.
		if fi, err := os.Stat(xdg); err == nil && fi.IsDir() && fi.Mode().Perm()&0o022 == 0 {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
				return filepath.Join(xdg, "moor")
			}
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("moor-%d", os.Getuid()))
}

// EnsureRoot creates the runtime directory with mode 0700 and refuses to use
// one that is a symlink or owned by someone else.
func EnsureRoot() (string, error) {
	root := Root()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is owned by another user", root)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			return "", err
		}
	}
	return root, nil
}

// Lock takes the runtime-directory lock that serializes session creation,
// stale cleanup and session removal. Call the returned function to release.
func Lock() (func(), error) {
	root, err := EnsureRoot()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// Probe asks the session server whether it is alive and how many clients are
// attached.
func Probe(id int) (clients int, err error) {
	conn, err := net.DialTimeout("unix", SocketPath(id), probeTimeout)
	if err != nil {
		return 0, ErrStale
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(probeTimeout))
	if err := protocol.WriteFrame(conn, protocol.MsgPing, nil); err != nil {
		return 0, ErrUnresponsive
	}
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, ErrUnresponsive
		}
		// The server closed the connection without answering: it is
		// shutting down.
		return 0, ErrStale
	}
	if typ != protocol.MsgPong || len(payload) != 1 {
		return 0, ErrUnresponsive
	}
	return int(payload[0]), nil
}

// Kill asks the server of session id to terminate its shell and exit. It
// goes through the session's own socket rather than a signal to a pid, so it
// can only ever reach that session's server.
func Kill(id int) error {
	conn, err := net.DialTimeout("unix", SocketPath(id), probeTimeout)
	if err != nil {
		return ErrStale
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(probeTimeout))
	return protocol.WriteFrame(conn, protocol.MsgKill, nil)
}

// scanLocked lists live sessions and removes the directories of dead ones.
// It also returns every session ID still present on disk, live or not, so
// allocation never collides with an existing directory. The caller must hold
// the lock.
func scanLocked() (live []Meta, used []int, err error) {
	root, err := EnsureRoot()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var ids []int
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil || id < 0 || strconv.Itoa(id) != e.Name() || !e.IsDir() {
			continue
		}
		ids = append(ids, id)
	}

	// Probe every session at once, so servers that do not answer cost one
	// timeout in total rather than one each.
	type probe struct {
		clients int
		err     error
	}
	probes := make([]probe, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probes[i].clients, probes[i].err = Probe(id)
		}()
	}
	wg.Wait()

	for i, id := range ids {
		clients, perr := probes[i].clients, probes[i].err
		switch {
		case perr == nil:
			m, err := ReadMeta(Dir(id))
			if err != nil {
				// Live server but unreadable metadata: show what we know.
				m = Meta{ID: id, Name: "?"}
			}
			m.ID = id
			m.Clients = clients
			live = append(live, m)
			used = append(used, id)
		case errors.Is(perr, ErrStale):
			os.RemoveAll(Dir(id))
		default:
			used = append(used, id)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	sort.Ints(used)
	return live, used, nil
}

// List returns all live sessions sorted by ID, cleaning up stale ones.
func List() ([]Meta, error) {
	unlock, err := Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	live, _, err := scanLocked()
	return live, err
}

// Resolve finds a live session by exact numeric ID, then by exact name.
func Resolve(target string) (Meta, error) {
	sessions, err := List()
	if err != nil {
		return Meta{}, err
	}
	if id, err := strconv.Atoi(target); err == nil {
		for _, s := range sessions {
			if s.ID == id {
				return s, nil
			}
		}
	}
	for _, s := range sessions {
		if s.Name == target {
			return s, nil
		}
	}
	return Meta{}, fmt.Errorf("no session %q", target)
}

// Remove deletes the runtime directory of session id, but only if it still
// belongs to the server with the given pid. This prevents an exiting server
// from deleting a directory that was already cleaned up and reused.
func Remove(id, pid int) error {
	unlock, err := Lock()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := ReadMeta(Dir(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Creation writes metadata before releasing the lock, so a
		// directory without readable metadata is garbage.
		return os.RemoveAll(Dir(id))
	}
	if m.PID != pid {
		return nil
	}
	return os.RemoveAll(Dir(id))
}
