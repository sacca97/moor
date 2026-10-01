package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := []struct {
		typ     byte
		payload []byte
	}{
		{MsgInput, []byte("ls -la\r")},
		{MsgOutput, bytes.Repeat([]byte{0x1b, '[', 'A'}, 1000)},
		{MsgDetach, nil},
		{MsgResize, Resize{Rows: 40, Cols: 120}.Encode()},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f.typ, f.payload); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range frames {
		typ, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if typ != f.typ || !bytes.Equal(payload, f.payload) {
			t.Fatalf("got (%d, %q), want (%d, %q)", typ, payload, f.typ, f.payload)
		}
	}
	if _, _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("want io.EOF at end, got %v", err)
	}
}

func TestReadFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, MsgOutput, []byte("hello"))
	data := buf.Bytes()[:buf.Len()-2]
	if _, _, err := ReadFrame(bytes.NewReader(data)); err != io.ErrUnexpectedEOF {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	hdr := []byte{MsgOutput, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestResizeCodec(t *testing.T) {
	r := Resize{Rows: 0xabcd, Cols: 7}
	got, err := DecodeResize(r.Encode())
	if err != nil || got != r {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := DecodeResize([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for short payload")
	}
}

func TestHelloAndExitCodec(t *testing.T) {
	h := HelloReply{Status: HelloBusy, ReplayLen: 8 << 20}
	got, err := DecodeHelloReply(h.Encode())
	if err != nil || got != h {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, code := range []int{0, 1, 127, -1} {
		if got := DecodeExit(EncodeExit(code)); got != code {
			t.Fatalf("exit code %d round-tripped to %d", code, got)
		}
	}
}
