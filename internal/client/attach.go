// Package client attaches the local terminal to a session server.
package client

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/sacca/moor/internal/protocol"
)

// Result says how an attachment ended.
type Result int

const (
	Detached Result = iota // the user pressed Ctrl-\ twice
	Exited                 // the shell exited; ExitCode is set
	Lost                   // the connection broke or the client was signaled
)

// Outcome is returned by Attach.
type Outcome struct {
	Result   Result
	ExitCode int
}

// While read-only, the terminal title says so: the screen belongs to the
// session, so nothing can be drawn on it. The title is pushed on the
// terminal's title stack and popped when the client can write again.
const (
	titlePush     = "\x1b[22;2t"
	titlePop      = "\x1b[23;2t"
	readOnlyTitle = "\x1b]2;moor: read-only\x07"
)

// resetModes undoes terminal modes a program in the session may have enabled,
// so the local terminal is usable after detaching: cursor visible, mouse
// reporting, focus events, bracketed paste, modifyOtherKeys and the kitty
// keyboard protocol off, normal cursor keys and keypad, default cursor shape
// and attributes. All of these are harmless when the mode was not set.
const resetModes = "\x1b[<99u\x1b[>4m\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l\x1b[?2004l\x1b[?1l\x1b>\x1b[0 q\x1b[?25h\x1b[0m"

// clearScreen homes the cursor and erases the screen and the scrollback.
const clearScreen = "\x1b[H\x1b[2J\x1b[3J"

// Attach connects the terminal on stdin/stdout to the session server
// listening on sock and runs until detach, shell exit or disconnect. The
// local terminal state is always restored before it returns.
//
// Several terminals may be attached at once. The newest one that did not ask
// for readOnly controls the session; the others only watch, and regain
// control when the newer ones detach.
func Attach(sock string, readOnly bool) (Outcome, error) {
	in, out := os.Stdin, os.Stdout
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return Outcome{}, errors.New("stdin is not a terminal")
	}

	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return Outcome{}, fmt.Errorf("connecting to session: %w", err)
	}
	defer conn.Close()

	rows, cols := TermSize(fd)
	if err := protocol.WriteFrame(conn, protocol.MsgHello,
		protocol.Hello{Rows: rows, Cols: cols, ReadOnly: readOnly}.Encode()); err != nil {
		return Outcome{}, err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		return Outcome{}, fmt.Errorf("session did not answer: %w", err)
	}
	conn.SetReadDeadline(time.Time{})
	if typ == protocol.MsgExit {
		return Outcome{Result: Exited, ExitCode: protocol.DecodeExit(payload)}, nil
	}
	if typ != protocol.MsgHello {
		return Outcome{}, fmt.Errorf("unexpected message %d from session", typ)
	}
	hello, err := protocol.DecodeHelloReply(payload)
	if err != nil {
		return Outcome{}, err
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return Outcome{}, err
	}
	modes := &modeTracker{}
	a := &attachment{conn: conn, out: out, modes: modes, done: make(chan Outcome, 4)}
	restore := sync.OnceFunc(func() {
		a.setRole(protocol.RoleWriter) // pops the read-only title
		// Once the terminal is restored, nothing else may be written to it.
		a.outMu.Lock()
		defer a.outMu.Unlock()
		a.closed = true
		reset := resetModes
		if modes.altScreen {
			// Pop the alternate screen's keyboard flags, leave it, then
			// pop the main screen's.
			reset = "\x1b[<99u\x1b[?1049l" + reset
		}
		out.WriteString(reset)
		term.Restore(fd, oldState)
	})
	defer restore()

	// Start from a blank screen and scrollback, so the terminal shows only
	// the session: its buffered output is replayed into the empty
	// scrollback. Then make sure terminal answers to any queries in the
	// replay are not mistaken for typing.
	out.WriteString(clearScreen)
	a.setRole(hello.Role)
	for remaining := int(hello.ReplayLen); remaining > 0; {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return Outcome{Result: Lost}, nil
		}
		switch typ {
		case protocol.MsgOutput:
			a.write(payload)
			remaining -= len(payload)
		case protocol.MsgRole:
			if len(payload) == 1 {
				a.setRole(payload[0])
			}
		case protocol.MsgExit:
			return Outcome{Result: Exited, ExitCode: protocol.DecodeExit(payload)}, nil
		}
	}
	if hello.ReplayLen > 0 {
		a.drain.start()
		out.Write(drainQuery)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	stop := make(chan struct{})
	defer close(stop)
	watchResize(fd, func(rows, cols uint16) {
		a.send(protocol.MsgResize, protocol.Resize{Rows: rows, Cols: cols}.Encode())
	}, stop)
	// The terminal may have been resized between the hello and raw mode.
	if r, c := TermSize(fd); r != rows || c != cols {
		a.send(protocol.MsgResize, protocol.Resize{Rows: r, Cols: c}.Encode())
	}

	go a.readServer()
	go a.readInput(in)

	select {
	case o := <-a.done:
		if o.Result == Detached {
			a.send(protocol.MsgDetach, nil)
		}
		return o, nil
	case <-sigs:
		return Outcome{Result: Lost}, nil
	}
}

type attachment struct {
	conn  net.Conn
	out   *os.File
	modes *modeTracker
	done  chan Outcome

	sendMu sync.Mutex

	// outMu serializes writes to the terminal, and closed stops them once
	// the terminal has been restored. It also guards modes.
	outMu  sync.Mutex
	closed bool

	// writer is whether this client controls the session. While it does
	// not, input is discarded except for the detach key. titled is whether
	// the read-only title is showing; roleMu guards it.
	writer atomic.Bool
	roleMu sync.Mutex
	titled bool

	// mu guards filter, drain and timer.
	mu     sync.Mutex
	filter detachFilter
	drain  replyDrain
	timer  *time.Timer
}

func (a *attachment) send(typ byte, payload []byte) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteFrame(a.conn, typ, payload); err != nil {
		a.finish(Outcome{Result: Lost})
	}
}

func (a *attachment) setRole(role byte) {
	a.roleMu.Lock()
	defer a.roleMu.Unlock()
	writer := role == protocol.RoleWriter
	a.writer.Store(writer)
	switch {
	case !writer && !a.titled:
		a.titled = true
		a.emit([]byte(titlePush + readOnlyTitle))
	case writer && a.titled:
		a.titled = false
		a.emit([]byte(titlePop))
	}
}

// forward sends keyboard input to the session if this client may write.
func (a *attachment) forward(p []byte) {
	if len(p) > 0 && a.writer.Load() {
		a.send(protocol.MsgInput, p)
	}
}

func (a *attachment) finish(o Outcome) {
	select {
	case a.done <- o:
	default:
	}
}

// write sends session output to the terminal, tracking its mode changes.
func (a *attachment) write(p []byte) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if !a.closed {
		a.modes.observe(p)
		a.out.Write(p)
	}
}

// emit sends our own escape sequences to the terminal.
func (a *attachment) emit(p []byte) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if !a.closed {
		a.out.Write(p)
	}
}

func (a *attachment) readServer() {
	for {
		typ, payload, err := protocol.ReadFrame(a.conn)
		if err != nil {
			a.finish(Outcome{Result: Lost})
			return
		}
		switch typ {
		case protocol.MsgOutput:
			a.write(payload)
		case protocol.MsgRole:
			if len(payload) == 1 {
				a.setRole(payload[0])
			}
		case protocol.MsgExit:
			a.finish(Outcome{Result: Exited, ExitCode: protocol.DecodeExit(payload)})
			return
		}
	}
}

func (a *attachment) readInput(in *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 && a.handleInput(buf[:n]) {
			a.finish(Outcome{Result: Detached})
			return
		}
		if err != nil {
			a.finish(Outcome{Result: Lost})
			return
		}
	}
}

// handleInput filters keyboard input and forwards it. It returns true on
// Ctrl-\ Ctrl-\.
func (a *attachment) handleInput(p []byte) bool {
	a.mu.Lock()
	p = a.drain.filter(p)
	epoch := a.filter.epoch
	fwd, detach := a.filter.feed(p)
	if !detach && a.filter.pending() && a.filter.epoch != epoch {
		a.armTimer(a.filter.epoch)
	}
	a.mu.Unlock()
	a.forward(fwd)
	return detach
}

// armTimer releases a held-back Ctrl-\ after detachTimeout, unless the
// filter has moved on to a newer pending state by then. Caller holds a.mu.
func (a *attachment) armTimer(epoch int) {
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(detachTimeout, func() {
		a.mu.Lock()
		var fwd []byte
		if a.filter.epoch == epoch && a.filter.pending() {
			fwd = a.filter.flush()
		}
		a.mu.Unlock()
		a.forward(fwd)
	})
}

// modeTracker follows whether the session's output has switched the terminal
// to the alternate screen, so detaching can switch it back. Leaving the
// alternate screen when it is not active would move the cursor, so this is
// the one mode that is tracked rather than reset unconditionally.
type modeTracker struct {
	altScreen bool
	tail      []byte // end of the previous chunk, for sequences split across writes
}

var (
	altOn  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altOff = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

func (m *modeTracker) observe(p []byte) {
	// Fast path: no ESC in the chunk or the kept tail means no sequence can
	// start or finish here. This covers most output, including big replays.
	if bytes.IndexByte(p, esc) < 0 && bytes.IndexByte(m.tail, esc) < 0 {
		return
	}
	data := append(m.tail, p...)
	last, on := -1, false
	for _, seq := range altOn {
		if i := bytes.LastIndex(data, seq); i > last {
			last, on = i, true
		}
	}
	for _, seq := range altOff {
		if i := bytes.LastIndex(data, seq); i > last {
			last, on = i, false
		}
	}
	if last >= 0 {
		m.altScreen = on
	}
	keep := min(len(data), 7)
	m.tail = append(m.tail[:0:0], data[len(data)-keep:]...)
}
