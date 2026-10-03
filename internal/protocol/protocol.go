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
	"net"
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
	MsgKill   // ask the server to terminate the session; no reply
	MsgRename // payload: new name; the reply is empty on success, else an error message
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
const MaxPayload = 1 << 20

const headerSize = 5

var ErrFrameTooLarge = errors.New("protocol: frame too large")

// WriteFrame writes one frame without copying the payload. On a net.Conn the
// header and payload go out in one writev, which holds the connection's write
// lock throughout, so frames from concurrent writers never interleave.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrFrameTooLarge
	}
	hdr := make([]byte, headerSize)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	_, err := (&net.Buffers{hdr, payload}).WriteTo(w)
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
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// Wire compatibility. Payloads only ever grow by appending fields, and
// decoders accept the shorter forms that older moor versions send as well as
// longer ones from newer versions (extra bytes are ignored), so a running
// session server and a freshly installed client can still talk. The first
// release sent terminal sizes without pixels (4 bytes) and a hello of
// size + read-only flag (5 bytes).

// Hello is the payload of the client's MsgHello: its terminal size, and
// whether it only wants to watch.
type Hello struct {
	Size     Resize
	ReadOnly bool
}

func (h Hello) Encode() []byte {
	b := h.Size.Encode()
	return append(b, boolByte(h.ReadOnly))
}

// EncodeLegacy is the hello older servers understand: no pixel size.
func (h Hello) EncodeLegacy() []byte {
	return append(h.Size.EncodeLegacy(), boolByte(h.ReadOnly))
}

func DecodeHello(p []byte) (Hello, error) {
	switch {
	case len(p) == legacyResizeSize: // before the read-only flag existed
		r, _ := DecodeResize(p)
		return Hello{Size: r}, nil
	case len(p) == legacyResizeSize+1:
		r, _ := DecodeResize(p[:legacyResizeSize])
		return Hello{Size: r, ReadOnly: p[legacyResizeSize] != 0}, nil
	case len(p) >= resizeSize+1:
		r, _ := DecodeResize(p[:resizeSize])
		return Hello{Size: r, ReadOnly: p[resizeSize] != 0}, nil
	}
	return Hello{}, fmt.Errorf("protocol: bad hello length %d", len(p))
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// Resize is a terminal size, the payload of MsgResize. The pixel size is
// passed along because programs that draw images read it from the PTY.
type Resize struct {
	Rows, Cols     uint16
	XPixel, YPixel uint16
}

const (
	legacyResizeSize = 4 // rows and columns only
	resizeSize       = 8
)

func (r Resize) Encode() []byte {
	b := make([]byte, resizeSize)
	binary.BigEndian.PutUint16(b[0:], r.Rows)
	binary.BigEndian.PutUint16(b[2:], r.Cols)
	binary.BigEndian.PutUint16(b[4:], r.XPixel)
	binary.BigEndian.PutUint16(b[6:], r.YPixel)
	return b
}

// EncodeLegacy is the size older servers understand: rows and columns only.
func (r Resize) EncodeLegacy() []byte { return r.Encode()[:legacyResizeSize] }

func DecodeResize(p []byte) (Resize, error) {
	if len(p) != legacyResizeSize && len(p) < resizeSize {
		return Resize{}, fmt.Errorf("protocol: bad resize payload length %d", len(p))
	}
	r := Resize{Rows: binary.BigEndian.Uint16(p[0:]), Cols: binary.BigEndian.Uint16(p[2:])}
	if len(p) >= resizeSize {
		r.XPixel = binary.BigEndian.Uint16(p[4:])
		r.YPixel = binary.BigEndian.Uint16(p[6:])
	}
	return r, nil
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
