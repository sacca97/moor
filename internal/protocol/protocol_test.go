package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Everything else in the protocol is exercised by the end-to-end tests; these
// are the error paths they cannot reach.
func TestReadFrameErrors(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, MsgOutput, []byte("hello"))
	truncated := buf.Bytes()[:buf.Len()-2]
	if _, _, err := ReadFrame(bytes.NewReader(truncated)); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated frame: want ErrUnexpectedEOF, got %v", err)
	}
	huge := []byte{MsgOutput, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(huge)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame: want ErrFrameTooLarge, got %v", err)
	}
}
