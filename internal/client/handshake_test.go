package client

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sacca/moor/internal/protocol"
)

// fakeServer listens on a Unix socket and calls handle for every connection
// with the hello it received. It records the hello lengths in order.
type fakeServer struct {
	sock  string
	mu    sync.Mutex
	hello []int
}

func newFakeServer(t *testing.T, handle func(s *fakeServer, conn net.Conn, hello []byte)) *fakeServer {
	t.Helper()
	s := &fakeServer{sock: filepath.Join(t.TempDir(), "s")}
	ln, err := net.Listen("unix", s.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			typ, payload, err := protocol.ReadFrame(conn)
			if err != nil || typ != protocol.MsgHello {
				conn.Close()
				continue
			}
			s.mu.Lock()
			s.hello = append(s.hello, len(payload))
			s.mu.Unlock()
			handle(s, conn, payload)
		}
	}()
	return s
}

func (s *fakeServer) lengths() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.hello...)
}

func accept(conn net.Conn) {
	protocol.WriteFrame(conn, protocol.MsgHello, protocol.HelloReply{Role: protocol.RoleWriter}.Encode())
}

var testHello = protocol.Hello{Size: protocol.Resize{Rows: 24, Cols: 80, XPixel: 1, YPixel: 2}}

func TestHandshakeCurrentServer(t *testing.T) {
	s := newFakeServer(t, func(_ *fakeServer, conn net.Conn, _ []byte) { accept(conn) })
	conn, typ, _, legacy, err := handshake(s.sock, testHello)
	if err != nil || legacy || typ != protocol.MsgHello {
		t.Fatalf("handshake: typ=%d legacy=%v err=%v", typ, legacy, err)
	}
	conn.Close()
	if got := s.lengths(); len(got) != 1 || got[0] != 9 {
		t.Fatalf("hellos sent: %v, want one of 9 bytes", got)
	}
}

// A server from the first release hangs up on the longer hello; the client
// then sends the old one and asks for old-style resizes from there on.
func TestHandshakeFallsBackToLegacyServer(t *testing.T) {
	s := newFakeServer(t, func(_ *fakeServer, conn net.Conn, hello []byte) {
		if len(hello) != 5 {
			conn.Close()
			return
		}
		accept(conn)
	})
	conn, typ, _, legacy, err := handshake(s.sock, testHello)
	if err != nil || !legacy || typ != protocol.MsgHello {
		t.Fatalf("handshake: typ=%d legacy=%v err=%v", typ, legacy, err)
	}
	conn.Close()
	if got := s.lengths(); len(got) != 2 || got[0] != 9 || got[1] != 5 {
		t.Fatalf("hellos sent: %v, want 9 then 5", got)
	}
}

func TestHandshakeIncompatibleServer(t *testing.T) {
	s := newFakeServer(t, func(_ *fakeServer, conn net.Conn, _ []byte) { conn.Close() })
	_, _, _, _, err := handshake(s.sock, testHello)
	if err == nil || !strings.Contains(err.Error(), "does not understand") {
		t.Fatalf("error %v", err)
	}
	if got := s.lengths(); len(got) != 2 {
		t.Fatalf("hellos sent: %v, want exactly two attempts", got)
	}
}

// If the server went away (and its socket with it), hanging up is not a sign
// of an old version, and the retry must not reach whatever took the path.
func TestHandshakeDoesNotRetryAfterTheServerWentAway(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer, conn net.Conn, _ []byte) {
		os.Remove(s.sock)
		conn.Close()
	})
	_, _, _, _, err := handshake(s.sock, testHello)
	if err == nil || !strings.Contains(err.Error(), "ended while attaching") {
		t.Fatalf("error %v", err)
	}
	if got := s.lengths(); len(got) != 1 {
		t.Fatalf("hellos sent: %v, want a single attempt", got)
	}
}
