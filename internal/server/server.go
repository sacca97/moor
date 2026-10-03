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
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sacca/moor/internal/protocol"
	"github.com/sacca/moor/internal/session"
)

const (
	scrollbackSize = 8 << 20
	// writeTimeout bounds how long a single write to a client may block
	// before the client is dropped. Each client has its own writer goroutine,
	// so a stuck one never holds up the shell or the other clients.
	writeTimeout     = 10 * time.Second
	handshakeTimeout = 5 * time.Second
	replayChunk      = 64 << 10
	// drainTimeout is how long to wait for remaining PTY output after the
	// shell exits (background jobs may keep the PTY open indefinitely).
	drainTimeout = 250 * time.Millisecond
	readyFD      = 3
)

// client is one attached terminal. Every client sees the session's output,
// but only the writer, the most recent client that did not ask to be
// read-only, may type into it and set its size.
type client struct {
	conn     net.Conn
	readOnly bool            // asked to only watch; never becomes the writer
	writer   atomic.Bool     // announced role; changed under server.mu
	size     protocol.Resize // last known terminal size; guarded by server.mu

	// Frames for the client are queued and sent by its own writer goroutine
	// (writeLoop), so a slow client never blocks anyone else. While replaying,
	// the attach goroutine is still sending the scrollback and the writer has
	// not started: frames just accumulate. Payloads are shared between
	// clients and must never be modified. All guarded by server.mu, except
	// the channels.
	replaying  bool
	queue      []frame
	queueBytes int
	final      bool     // the last queued frame ends the session
	inq        [][]byte // input waiting to be written to the PTY
	inBytes    int
	inWake     chan struct{} // signals the input goroutine
	wake       chan struct{} // signals the writer that the queue changed
	quit       chan struct{} // closed when the client is dropped
	writerDone chan struct{} // closed when the writer goroutine has returned
}

type frame struct {
	typ     byte
	payload []byte
}

// maxInput bounds the keyboard input queued for the PTY per client; beyond it
// input is discarded, which only happens when the shell has stopped reading.
const maxInput = 8 << 20

// highWater is how much output may be queued for the writer before the shell's
// output is held back until it catches up, as a slow terminal does to any
// program, replaying or not. Watchers get no such courtesy: they are dropped
// at maxQueue.
const highWater = maxQueue / 4

// maxQueue bounds what is queued for a client that is slow to take its
// output; beyond it the client is dropped.
const maxQueue = scrollbackSize

type server struct {
	id   int
	dir  string
	ln   net.Listener
	ptmx *os.File
	cmd  *exec.Cmd

	// mu guards clients, lastWriter, scroll and the clients' queues. Nothing
	// is written to a connection while holding it.
	mu         sync.Mutex
	clients    []*client // in attach order; the newest non-read-only one writes
	lastWriter *client
	scroll     *Ring
	nclients   atomic.Int32
	// ptyMu serializes writes to the PTY master, so input from two clients
	// (around a change of writer) or the startup command never interleaves.
	ptyMu   sync.Mutex
	drained *sync.Cond // on mu; broadcast when a queue shrinks or a client leaves

	stop chan struct{} // receives when "moor kill" asks the session to end

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

	// Catch these rather than ignore them: an ignored signal stays ignored
	// in the shell and every program it starts, which would make Ctrl-C and
	// hangups do nothing inside the session. Handled signals are reset to
	// their defaults on exec. The channel is never read; sends do not block.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGPIPE)
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
		stop:        make(chan struct{}, 1),
		firstOutput: make(chan struct{}),
		readerDone:  make(chan struct{}),
	}
	s.drained = sync.NewCond(&s.mu)
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
	case <-s.stop:
		s.hangup(waited)
	}

	code := -1
	if st := s.cmd.ProcessState; st != nil {
		code = st.ExitCode()
	}
	s.ln.Close()
	s.notifyExit(code)
	session.Remove(s.id, os.Getpid())
	s.ptmx.Close()
	return 0
}

// notifyExit tells every client that the session ended, and closes them.
func (s *server) notifyExit(code int) {
	exit := protocol.EncodeExit(code)
	var replaying, live []*client
	s.mu.Lock()
	clients := s.clients
	s.clients = nil
	s.drained.Broadcast()
	for _, c := range clients {
		if c.replaying {
			replaying = append(replaying, c)
			continue
		}
		// Let the writer deliver what is queued, then the exit.
		c.queue = append(c.queue, frame{protocol.MsgExit, exit})
		c.final = true
		c.signal()
		live = append(live, c)
	}
	s.mu.Unlock()

	// A whole frame is written atomically, so this is safe even while a
	// client's attach goroutine is still sending its replay; the client
	// treats an exit between replay chunks as the end of the session.
	for _, c := range replaying {
		c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		protocol.WriteFrame(c.conn, protocol.MsgExit, exit)
		c.conn.Close()
	}
	// Give the writers a moment; a client that is not draining is cut off.
	grace := time.After(time.Second)
	for _, c := range live {
		select {
		case <-c.writerDone:
		case <-grace:
		}
		c.conn.Close()
	}
}

// hangup terminates the shell as a terminal hangup would: closing the PTY
// master sends SIGHUP to its foreground job, and the shell's process group
// gets SIGHUP explicitly. SIGKILL follows if the shell refuses to go.
func (s *server) hangup(waited <-chan struct{}) {
	pid := s.cmd.Process.Pid
	// The foreground job has its own process group. Closing the master
	// hangs it up too, but a shell that does not pass SIGHUP on (bash
	// started as sh) would otherwise leave it running, so signal it
	// explicitly.
	fg := foregroundPgrp(s.ptmx)
	syscall.Kill(-pid, syscall.SIGHUP)
	syscall.Kill(pid, syscall.SIGHUP)
	if fg > 0 && fg != pid {
		syscall.Kill(-fg, syscall.SIGHUP)
	}
	s.ptmx.Close()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		if fg > 0 && fg != pid {
			syscall.Kill(-fg, syscall.SIGKILL)
		}
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
			// Backpressure from the client in control, so a flood of output
			// does not overrun a terminal that is merely slow. It ends when
			// the client catches up, is dropped (a stuck write times out), or
			// loses control.
			for w := s.writerLocked(); w != nil && w.queueBytes > highWater; w = s.writerLocked() {
				s.drained.Wait()
			}
			s.scroll.Write(buf[:n])
			var failed []*client
			if len(s.clients) > 0 {
				// One copy, shared by every client's queue.
				data := bytes.Clone(buf[:n])
				for _, c := range s.clients {
					if err := s.sendLocked(c, protocol.MsgOutput, data); err != nil {
						failed = append(failed, c)
					}
				}
			}
			for _, c := range failed {
				s.dropLocked(c)
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
	s.writePTY(nil, []byte(command+"\r"))
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
		conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		protocol.WriteFrame(conn, protocol.MsgPong, []byte{byte(min(s.nclients.Load(), 255))})
		conn.Close()
	case protocol.MsgRename:
		s.rename(conn, string(payload))
	case protocol.MsgKill:
		select {
		case s.stop <- struct{}{}:
		default:
		}
		conn.Close()
	case protocol.MsgHello:
		s.attach(conn, payload)
	default:
		conn.Close()
	}
}

// rename changes the session's name in session.json and answers conn. The
// running shell keeps the name it was started with in its prompt.
func (s *server) rename(conn net.Conn, name string) {
	defer conn.Close()
	err := session.ValidateName(name)
	if err == nil {
		s.metaMu.Lock()
		s.meta.Name = name
		err = session.WriteMeta(s.dir, s.meta)
		s.metaMu.Unlock()
	}
	var reply []byte
	if err != nil {
		reply = []byte(err.Error())
	}
	conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	protocol.WriteFrame(conn, protocol.MsgRename, reply)
}

func (s *server) attach(conn net.Conn, hello []byte) {
	defer conn.Close()
	h, err := protocol.DecodeHello(hello)
	if err != nil {
		return
	}
	c := &client{
		conn: conn, readOnly: h.ReadOnly, size: h.Size,
		wake: make(chan struct{}, 1), inWake: make(chan struct{}, 1), quit: make(chan struct{}), writerDone: make(chan struct{}),
	}
	role := protocol.RoleWriter
	if c.readOnly {
		role = protocol.RoleReadOnly
	}
	c.writer.Store(!c.readOnly)

	// Register the client first, then stream the scrollback without holding
	// the lock: a slow terminal must not stall the session or its writer.
	// Live output for the client is queued until the replay is through.
	s.mu.Lock()
	replay := s.replayLocked()
	c.replaying = true
	s.clients = append(s.clients, c)
	s.nclients.Store(int32(len(s.clients)))
	s.rolesLocked()
	s.mu.Unlock()
	go s.inputLoop(c)

	conn.SetWriteDeadline(time.Now().Add(writeTimeout + time.Duration(len(replay)>>20)*time.Second))
	err = protocol.WriteFrame(conn, protocol.MsgHello,
		protocol.HelloReply{Role: role, ReplayLen: uint32(len(replay))}.Encode())
	for len(replay) > 0 && err == nil {
		n := min(len(replay), replayChunk)
		err = protocol.WriteFrame(conn, protocol.MsgOutput, replay[:n])
		replay = replay[n:]
	}
	if err == nil {
		err = s.finishReplay(c)
	}
	if err != nil {
		s.mu.Lock()
		s.dropLocked(c)
		s.mu.Unlock()
		return
	}

loop:
	for {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			break
		}
		switch typ {
		case protocol.MsgInput:
			s.queueInput(c, payload)
		case protocol.MsgResize:
			if r, err := protocol.DecodeResize(payload); err == nil {
				s.mu.Lock()
				c.size = r
				if c.writer.Load() {
					setSize(s.ptmx, r, false)
				}
				s.mu.Unlock()
			}
		case protocol.MsgPing:
			s.mu.Lock()
			if err := s.sendLocked(c, protocol.MsgPong, []byte{byte(min(len(s.clients), 255))}); err != nil {
				s.dropLocked(c)
			}
			s.mu.Unlock()
		case protocol.MsgDetach:
			break loop
		}
	}
	s.mu.Lock()
	s.dropLocked(c)
	s.mu.Unlock()
}

// queueInput hands keyboard input to c's input goroutine, so a program that
// has stopped reading its terminal never blocks the connection (and with it
// the detach key).
func (s *server) queueInput(c *client, p []byte) {
	if !c.writer.Load() {
		return
	}
	s.mu.Lock()
	if c.inBytes+len(p) <= maxInput {
		c.inq = append(c.inq, p)
		c.inBytes += len(p)
		select {
		case c.inWake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

// inputLoop writes c's queued input to the PTY until c is dropped.
func (s *server) inputLoop(c *client) {
	for {
		quit := false
		select {
		case <-c.inWake:
		case <-c.quit:
			quit = true // deliver what was typed before the detach
		}
		s.mu.Lock()
		queued := c.inq
		c.inq, c.inBytes = nil, 0
		s.mu.Unlock()
		for _, p := range queued {
			s.writePTY(c, p)
		}
		if quit {
			return
		}
	}
}

// writePTY writes p to the PTY in small pieces. For a client it stops as soon
// as the client no longer controls the session; a nil client always writes.
func (s *server) writePTY(c *client, p []byte) {
	for len(p) > 0 {
		n := min(len(p), 4096)
		s.ptyMu.Lock()
		ok := c == nil || c.writer.Load()
		if ok {
			s.ptmx.Write(p[:n])
		}
		s.ptyMu.Unlock()
		if !ok {
			return
		}
		p = p[n:]
	}
}

// signal wakes c's writer.
func (c *client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// sendLocked queues a frame for c. A non-nil error means c should be
// dropped. The payload is shared and must not be modified afterwards.
func (s *server) sendLocked(c *client, typ byte, payload []byte) error {
	if c.queueBytes+len(payload) > maxQueue {
		return errors.New("client too slow to take its output")
	}
	c.queue = append(c.queue, frame{typ, payload})
	c.queueBytes += len(payload)
	if !c.replaying {
		c.signal()
	}
	return nil
}

// finishReplay hands c over to its writer goroutine, which sends what was
// queued during the replay and everything after it.
func (s *server) finishReplay(c *client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.clients, c) {
		return errors.New("client was dropped")
	}
	c.replaying = false
	go s.writeLoop(c)
	c.signal()
	return nil
}

// writeLoop delivers c's queued frames until c is dropped or the session
// ends. It is the only goroutine writing to c once the replay is done.
func (s *server) writeLoop(c *client) {
	defer close(c.writerDone)
	for {
		select {
		case <-c.wake:
		case <-c.quit:
			return
		}
		s.mu.Lock()
		queued, final := c.queue, c.final
		c.queue, c.queueBytes = nil, 0
		s.drained.Broadcast()
		s.mu.Unlock()

		for _, f := range queued {
			c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := protocol.WriteFrame(c.conn, f.typ, f.payload); err != nil {
				s.mu.Lock()
				s.dropLocked(c)
				s.mu.Unlock()
				return
			}
		}
		if final {
			return
		}
	}
}

// writerLocked returns the client that currently controls the session: the
// newest one that is not read-only, or nil.
func (s *server) writerLocked() *client {
	for i := len(s.clients) - 1; i >= 0; i-- {
		if !s.clients[i].readOnly {
			return s.clients[i]
		}
	}
	return nil
}

// rolesLocked tells every client whose role changed about it, and gives the
// PTY the new writer's size, forcing a SIGWINCH so full-screen programs
// redraw for it.
func (s *server) rolesLocked() {
	w := s.writerLocked()
	for _, c := range s.clients {
		if is := c == w; is != c.writer.Load() {
			c.writer.Store(is)
			role := protocol.RoleReadOnly
			if is {
				role = protocol.RoleWriter
			}
			if err := s.sendLocked(c, protocol.MsgRole, []byte{role}); err != nil {
				// Its attach goroutine sees the failed read and drops it.
				c.conn.Close()
			}
		}
	}
	if w != nil && w != s.lastWriter {
		setSize(s.ptmx, w.size, true)
	}
	s.lastWriter = w
}

// dropLocked closes and forgets a client, if it is still attached, and hands
// control to the next writer.
func (s *server) dropLocked(c *client) {
	i := slices.Index(s.clients, c)
	if i < 0 {
		return
	}
	s.clients = slices.Delete(s.clients, i, i+1)
	s.nclients.Store(int32(len(s.clients)))
	close(c.quit)
	c.conn.Close()
	s.drained.Broadcast()
	s.rolesLocked()
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
