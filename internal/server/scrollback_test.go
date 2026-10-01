package server

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRingUnderCapacity(t *testing.T) {
	r := NewRing(16)
	r.Write([]byte("hello "))
	r.Write([]byte("world"))
	if got := string(r.Bytes()); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if r.Wrapped() {
		t.Fatal("should not be wrapped")
	}
}

func TestRingWraps(t *testing.T) {
	r := NewRing(8)
	r.Write([]byte("abcdef"))
	r.Write([]byte("ghij"))
	if got := string(r.Bytes()); got != "cdefghij" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte("klm"))
	if got := string(r.Bytes()); got != "fghijklm" {
		t.Fatalf("got %q", got)
	}
	if !r.Wrapped() {
		t.Fatal("should be wrapped")
	}
}

func TestRingOversizedWrite(t *testing.T) {
	r := NewRing(4)
	r.Write([]byte("ab"))
	r.Write([]byte("0123456789"))
	if got := string(r.Bytes()); got != "6789" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte("x"))
	if got := string(r.Bytes()); got != "789x" {
		t.Fatalf("got %q", got)
	}
}

func TestRingMatchesReference(t *testing.T) {
	const max = 1000
	r := NewRing(max)
	var all []byte
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		chunk := make([]byte, rng.Intn(300))
		rng.Read(chunk)
		r.Write(chunk)
		all = append(all, chunk...)
		want := all
		if len(want) > max {
			want = want[len(want)-max:]
		}
		if !bytes.Equal(r.Bytes(), want) {
			t.Fatalf("mismatch after write %d", i)
		}
	}
}
