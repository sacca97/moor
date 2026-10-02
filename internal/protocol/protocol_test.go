package protocol

import (
	"bytes"
	"encoding/hex"
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

// The wire format is a contract with already-running session servers, so its
// bytes are pinned here. Payloads only grow by appending; decoders accept the
// shorter forms of older versions and ignore bytes from newer ones.
func TestWireFormatGolden(t *testing.T) {
	size := Resize{Rows: 24, Cols: 80, XPixel: 640, YPixel: 480}
	if got := hex.EncodeToString(size.Encode()); got != "00180050"+"028001e0" {
		t.Fatalf("Resize.Encode = %s", got)
	}
	if got := hex.EncodeToString(size.EncodeLegacy()); got != "00180050" {
		t.Fatalf("Resize.EncodeLegacy = %s", got)
	}
	h := Hello{Size: size, ReadOnly: true}
	if got := hex.EncodeToString(h.Encode()); got != "00180050028001e0"+"01" {
		t.Fatalf("Hello.Encode = %s", got)
	}
	if got := hex.EncodeToString(h.EncodeLegacy()); got != "00180050"+"01" {
		t.Fatalf("Hello.EncodeLegacy = %s", got)
	}
	if got := hex.EncodeToString(HelloReply{Role: RoleReadOnly, ReplayLen: 7}.Encode()); got != "01"+"00000007" {
		t.Fatalf("HelloReply.Encode = %s", got)
	}
	frame := new(bytes.Buffer)
	WriteFrame(frame, MsgResize, size.EncodeLegacy())
	if got := hex.EncodeToString(frame.Bytes()); got != "04"+"00000004"+"00180050" {
		t.Fatalf("frame = %s", got)
	}
}

func TestDecodersAcceptOlderAndNewerForms(t *testing.T) {
	dec := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	for in, want := range map[string]Hello{
		"00180050":                    {Size: Resize{Rows: 24, Cols: 80}},
		"0018005001":                  {Size: Resize{Rows: 24, Cols: 80}, ReadOnly: true},
		"00180050028001e001":          {Size: Resize{Rows: 24, Cols: 80, XPixel: 640, YPixel: 480}, ReadOnly: true},
		"00180050028001e000" + "ffff": {Size: Resize{Rows: 24, Cols: 80, XPixel: 640, YPixel: 480}}, // a newer field
	} {
		got, err := DecodeHello(dec(in))
		if err != nil || got != want {
			t.Errorf("DecodeHello(%s) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if r, err := DecodeResize(dec("00180050")); err != nil || r != (Resize{Rows: 24, Cols: 80}) {
		t.Errorf("legacy resize: %+v, %v", r, err)
	}
	if r, err := DecodeResize(dec("00180050028001e0aa")); err != nil || r.YPixel != 480 {
		t.Errorf("resize with a newer field: %+v, %v", r, err)
	}
	for _, bad := range []string{"", "0018", "001800500102", "00180050028001"} {
		if _, err := DecodeResize(dec(bad)); err == nil {
			t.Errorf("DecodeResize(%q) accepted", bad)
		}
		if _, err := DecodeHello(dec(bad)); err == nil {
			t.Errorf("DecodeHello(%q) accepted", bad)
		}
	}
}
