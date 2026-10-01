// Package server implements the per-session server process ("moor __serve")
// that owns the PTY and the shell running in it.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sacca/moor/internal/protocol"
	"github.com/sacca/moor/internal/session"
)

const (
	scrollbackSize = 8 << 20
	// writeTimeout bounds how long PTY output may block on a stuck client
	// before the client is dropped; the shell must never stall on it.
	writeTimeout     = 10 * time.Second
	handshakeTimeout = 5 * time.Second
	replayChunk      = 64 << 10
	// drainTimeout is how long to wait for remaining PTY output after the
	// shell exits (background jobs may keep the PTY open indefinitely).
	drainTimeout = 250 * time.Millisecond
	readyFD      = 3
)

type server struct {
	id   int
	dir  string
	ln   net.Listener
	ptmx *os.File
	cmd  *exec.Cmd

	// mu guards client, scroll, and every write to client.
	mu       sync.Mutex
	client   net.Conn
	scroll   *Ring
	attached atomic.Bool

	metaMu sync.Mutex
	meta   session.Meta

	lastOutput  atomic.Int64 // unix nanos of the most recent PTY output
	firstOutput chan struct{}
	readerDone  chan struct{}
}

// Serve runs the server for session id. It is the entry point of the hidden
// "__serve" command and returns the process exit code.
func Serve(id int, rows, cols uint16) int {
	// The readiness pipe must not leak into the shell, or the launching
	// client would wait for the shell to close it.
	syscall.CloseOnExec(readyFD)
	ready := os.NewFile(readyFD, "ready")

	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGPIPE)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)

	s, err := start(id, rows, cols)
	if err != nil {
		fmt.Fprint(ready, err.Error())
		ready.Close()
		return 1
	}
	ready.Write([]byte("ok"))
	ready.Close()
	return s.run(term)
}

func start(id int, rows, cols uint16) (*server, error) {
	s := &server{
		id:          id,
		dir:         session.Dir(id),
		scroll:      NewRing(scrollbackSize),
		firstOutput: make(chan struct{}),
		readerDone:  make(chan struct{}),
	}
	meta, err := session.ReadMeta(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}
	s.meta = meta

	sock := session.SocketPath(id)
	if len(sock) >= len(syscall.RawSockaddrUnix{}.Path) {
		return nil, fmt.Errorf("socket path %s is too long; set XDG_RUNTIME_DIR to a shorter directory", sock)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	s.ln = ln

	ptmx, cmd, err := startShell(id, s.meta.Name, s.dir, rows, cols)
	if err != nil {
		ln.Close()
		return nil, err
	}
	s.ptmx, s.cmd = ptmx, cmd

	s.metaMu.Lock()
	s.meta.PID = os.Getpid()
	s.meta.ShellPID = cmd.Process.Pid
	err = session.WriteMeta(s.dir, s.meta)
	s.metaMu.Unlock()
	if err != nil {
		cmd.Process.Kill()
		ln.Close()
		return nil, err
	}
	return s, nil
}

func (s *server) run(term <-chan os.Signal) int {
	go s.readLoop()
	go s.acceptLoop()
	if s.meta.Command != "" {
		go s.inject(s.meta.Command)
	}

	waited := make(chan struct{})
	go func() {
		s.cmd.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		// Give the reader a moment to pick up the shell's last output.
		select {
		case <-s.readerDone:
		case <-time.After(drainTimeout):
		}
	case <-term:
		s.hangup(waited)
	}

	code := -1
	if st := s.cmd.ProcessState; st != nil {
		code = st.ExitCode()
	}
	s.ln.Close()
	s.mu.Lock()
	if s.client != nil {
		s.client.SetWriteDeadline(time.Now().Add(time.Second))
		protocol.WriteFrame(s.client, protocol.MsgExit, protocol.EncodeExit(code))
		s.client.Close()
		s.client = nil
	}
	s.mu.Unlock()
	session.Remove(s.id, os.Getpid())
	s.ptmx.Close()
	return 0
}

// hangup terminates the shell as a terminal hangup would: closing the PTY
// master sends SIGHUP to its foreground job, and the shell's process group
// gets SIGHUP explicitly. SIGKILL follows if the shell refuses to go.
func (s *server) hangup(waited <-chan struct{}) {
	pid := s.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGHUP)
	syscall.Kill(pid, syscall.SIGHUP)
	s.ptmx.Close()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-waited
	}
}

// readLoop continuously consumes PTY output, whether or not anyone is
// attached, so the program in the PTY never blocks on a full buffer.
func (s *server) readLoop() {
	defer close(s.readerDone)
	var once sync.Once
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.lastOutput.Store(time.Now().UnixNano())
			once.Do(func() { close(s.firstOutput) })
			s.mu.Lock()
			s.scroll.Write(buf[:n])
			if s.client != nil {
				s.client.SetWriteDeadline(time.Now().Add(writeTimeout))
				if err := protocol.WriteFrame(s.client, protocol.MsgOutput, buf[:n]); err != nil {
					s.dropClientLocked()
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// inject types command into the shell once it has started and gone quiet
// (printed its prompt), so the command runs inside the interactive shell.
func (s *server) inject(command string) {
	select {
	case <-s.firstOutput:
	case <-time.After(3 * time.Second):
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, s.lastOutput.Load())) >= 150*time.Millisecond {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.ptmx.Write([]byte(command + "\r"))
}

func (s *server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go s.handle(conn)
	}
}

func (s *server) handle(conn net.Conn) {
	if !sameUser(conn) {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	switch typ {
	case protocol.MsgPing:
		var b byte
		if s.attached.Load() {
			b = 1
		}
		conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		protocol.WriteFrame(conn, protocol.MsgPong, []byte{b})
		conn.Close()
	case protocol.MsgHello:
		s.attach(conn, payload)
	default:
		conn.Close()
	}
}

func (s *server) attach(conn net.Conn, hello []byte) {
	defer conn.Close()
	size, err := protocol.DecodeResize(hello)
	if err != nil {
		return
	}

	s.mu.Lock()
	if s.client != nil {
		conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		protocol.WriteFrame(conn, protocol.MsgHello,
			protocol.HelloReply{Status: protocol.HelloBusy}.Encode())
		s.mu.Unlock()
		return
	}
	// Replay under the lock so no live output can slip in between the
	// buffered output and the live stream.
	replay := s.replayLocked()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout + time.Duration(len(replay)>>20)*time.Second))
	err = protocol.WriteFrame(conn, protocol.MsgHello,
		protocol.HelloReply{Status: protocol.HelloOK, ReplayLen: uint32(len(replay))}.Encode())
	for len(replay) > 0 && err == nil {
		n := min(len(replay), replayChunk)
		err = protocol.WriteFrame(conn, protocol.MsgOutput, replay[:n])
		replay = replay[n:]
	}
	if err != nil {
		s.mu.Unlock()
		return
	}
	s.client = conn
	s.attached.Store(true)
	s.mu.Unlock()
	s.syncMeta()

	setSize(s.ptmx, size.Rows, size.Cols, true)

loop:
	for {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			break
		}
		switch typ {
		case protocol.MsgInput:
			s.ptmx.Write(payload)
		case protocol.MsgResize:
			if r, err := protocol.DecodeResize(payload); err == nil {
				setSize(s.ptmx, r.Rows, r.Cols, false)
			}
		case protocol.MsgPing:
			s.mu.Lock()
			if s.client == conn {
				protocol.WriteFrame(conn, protocol.MsgPong, []byte{1})
			}
			s.mu.Unlock()
		case protocol.MsgDetach:
			break loop
		}
	}
	s.mu.Lock()
	if s.client == conn {
		s.client = nil
		s.attached.Store(false)
	}
	s.mu.Unlock()
	s.syncMeta()
}

// replayLocked returns the scrollback to send to a newly attached client.
// Once the ring has wrapped, its start is probably mid-line or mid escape
// sequence, so it is trimmed to the first line boundary.
func (s *server) replayLocked() []byte {
	b := s.scroll.Bytes()
	if s.scroll.Wrapped() {
		if i := bytes.IndexByte(b[:min(len(b), 4096)], '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b
}

func (s *server) dropClientLocked() {
	s.client.Close()
	s.client = nil
	s.attached.Store(false)
	go s.syncMeta()
}

// syncMeta records the current attach state in session.json.
func (s *server) syncMeta() {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	attached := s.attached.Load()
	if s.meta.Attached == attached {
		return
	}
	s.meta.Attached = attached
	session.WriteMeta(s.dir, s.meta)
}

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
	var cred *unix.Ucred
	raw.Control(func(fd uintptr) {
		cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	return err == nil && cred != nil && int(cred.Uid) == os.Getuid()
}
