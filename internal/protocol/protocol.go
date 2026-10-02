// Package protocol implements the framing used between a moor client and a
// session server over a Unix domain socket.
//
// Every message is a frame:
//
//	TYPE    1 byte
//	LENGTH  4 bytes, big endian
//	PAYLOAD LENGTH bytes
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	MsgHello byte = iota + 1
	MsgInput
	MsgOutput
	MsgResize
	MsgDetach
	MsgExit
	MsgPing
	MsgPong
	MsgRole
	MsgKill // ask the server to terminate the session; no reply
)

// Roles of an attached client, carried in the first byte of the server's
// MsgHello reply and in MsgRole. Only the writer's input and terminal size
// reach the session; read-only clients just watch its output.
const (
	RoleWriter   byte = 0
	RoleReadOnly byte = 1
)

// MaxPayload bounds the size of a single frame so a corrupt or hostile peer
// cannot make us allocate arbitrary amounts of memory.
const MaxPayload = 16 << 20

const headerSize = 5

var ErrFrameTooLarge = errors.New("protocol: frame too large")

// WriteFrame writes one frame with a single Write call, so concurrent writers
// that serialize on a mutex never interleave partial frames.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrFrameTooLarge
	}
	buf := make([]byte, headerSize+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:headerSize], uint32(len(payload)))
	copy(buf[headerSize:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// Hello is the payload of the client's MsgHello: its terminal size, and
// whether it only wants to watch.
type Hello struct {
	Rows, Cols uint16
	ReadOnly   bool
}

func (h Hello) Encode() []byte {
	b := Resize{Rows: h.Rows, Cols: h.Cols}.Encode()
	if h.ReadOnly {
		return append(b, 1)
	}
	return append(b, 0)
}

func DecodeHello(p []byte) (Hello, error) {
	if len(p) != 5 {
		return Hello{}, fmt.Errorf("protocol: bad hello length %d", len(p))
	}
	r, _ := DecodeResize(p[:4])
	return Hello{Rows: r.Rows, Cols: r.Cols, ReadOnly: p[4] != 0}, nil
}

// Resize is the payload of MsgResize.
type Resize struct {
	Rows uint16
	Cols uint16
}

func (r Resize) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], r.Rows)
	binary.BigEndian.PutUint16(b[2:4], r.Cols)
	return b
}

func DecodeResize(p []byte) (Resize, error) {
	if len(p) != 4 {
		return Resize{}, fmt.Errorf("protocol: bad resize payload length %d", len(p))
	}
	return Resize{
		Rows: binary.BigEndian.Uint16(p[0:2]),
		Cols: binary.BigEndian.Uint16(p[2:4]),
	}, nil
}

// HelloReply is the server's answer to a client's MsgHello: the client's
// initial role, then exactly ReplayLen bytes of buffered output (in MsgOutput
// frames) before live output is streamed.
type HelloReply struct {
	Role      byte
	ReplayLen uint32
}

func (h HelloReply) Encode() []byte {
	b := make([]byte, 5)
	b[0] = h.Role
	binary.BigEndian.PutUint32(b[1:], h.ReplayLen)
	return b
}

func DecodeHelloReply(p []byte) (HelloReply, error) {
	if len(p) != 5 {
		return HelloReply{}, fmt.Errorf("protocol: bad hello reply length %d", len(p))
	}
	return HelloReply{Role: p[0], ReplayLen: binary.BigEndian.Uint32(p[1:])}, nil
}

// EncodeExit encodes the shell's exit code for MsgExit.
func EncodeExit(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	return b
}

func DecodeExit(p []byte) int {
	if len(p) != 4 {
		return -1
	}
	return int(int32(binary.BigEndian.Uint32(p)))
}
