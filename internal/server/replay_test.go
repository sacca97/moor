package server

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/sacca/moor/internal/protocol"
	"github.com/sacca/moor/internal/session"
)

// A client that takes a big scrollback replay slowly, but keeps taking it,
// must not be dropped: the write deadline covers each chunk, not the whole
// replay.
func TestSlowButSteadyReplayIsNotDropped(t *testing.T) {
	xdg, err := os.MkdirTemp("", "moor")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(xdg) })
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	t.Setenv("SHELL", "/bin/sh")
	defer func(d time.Duration) { writeTimeout = d }(writeTimeout)
	writeTimeout = 300 * time.Millisecond

	if _, err := session.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(session.Dir(0), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteMeta(session.Dir(0), session.Meta{ID: 0, Name: "t"}); err != nil {
		t.Fatal(err)
	}
	s, err := start(0, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- s.run(make(chan os.Signal)) }()
	defer func() {
		s.stop <- struct{}{}
		<-done
	}()

	const size = 3 << 20
	s.mu.Lock()
	s.scroll.Write(make([]byte, size))
	s.mu.Unlock()

	conn, err := net.Dial("unix", session.SocketPath(0))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteFrame(conn, protocol.MsgHello, protocol.Hello{Size: protocol.Resize{Rows: 24, Cols: 80}}.Encode()); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil || typ != protocol.MsgHello {
		t.Fatalf("hello: %d %v", typ, err)
	}
	reply, _ := protocol.DecodeHelloReply(payload)

	// Take the replay at a pace that makes the whole of it last well past
	// the time a single write may block.
	start := time.Now()
	for got := 0; got < int(reply.ReplayLen); {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("dropped after %v with %d of %d bytes: %v", time.Since(start), got, reply.ReplayLen, err)
		}
		if typ == protocol.MsgOutput {
			got += len(payload)
		}
		time.Sleep(80 * time.Millisecond)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("replay took only %v; the test does not exercise a slow client", d)
	}
}
