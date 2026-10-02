package server

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestRingMatchesReference compares the ring with a plain slice over random
// writes, including ones larger than the ring and ones that exactly wrap.
func TestRingMatchesReference(t *testing.T) {
	const max = 1000
	r := NewRing(max)
	var all []byte
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		chunk := make([]byte, rng.Intn(1500))
		rng.Read(chunk)
		r.Write(chunk)
		all = append(all, chunk...)
		want := all
		if len(want) > max {
			want = want[len(want)-max:]
		}
		if !bytes.Equal(r.Bytes(), want) || r.Wrapped() != (len(all) > max) {
			t.Fatalf("mismatch after write %d", i)
		}
	}
}
