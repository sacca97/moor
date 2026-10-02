package client

import (
	"testing"
	"time"
)

type step struct {
	in     string
	out    string
	detach bool
}

const bs = "\x1c" // Ctrl-\

// Detaching with real terminals and timing is covered end to end in
// cmd/moor; these cases pin down the key-sequence state machine.
func TestDetachFilter(t *testing.T) {
	paste := "\x1b[200~a" + bs + bs + "b\x1b[201~"
	cases := map[string][]step{
		"plain input":        {{in: "hello\r", out: "hello\r"}},
		"double press":       {{in: bs + bs, detach: true}},
		"split double press": {{in: "ab", out: "ab"}, {in: bs}, {in: bs, detach: true}},
		"input before it":    {{in: "ls\r" + bs + bs + "junk", out: "ls\r", detach: true}},
		"escape is instant": {
			{in: "\x1b", out: "\x1b"}, {in: "\x1b\x1b", out: "\x1b\x1b"}, {in: "\x1b[A", out: "\x1b[A"},
			{in: "\x1bOB", out: "\x1bOB"}, {in: "\x1bx", out: "\x1bx"},
			{in: "\x1b[27u\x1b[27u", out: "\x1b[27u\x1b[27u"}, // kitty Esc
		},
		"single press then key": {{in: bs}, {in: "x", out: bs + "x"}, {in: bs}, {in: "\x1b[A", out: bs + "\x1b[A"}},
		"split kitty sequence":  {{in: "\x1b[92;5u"}, {in: "\x1b[92"}, {in: ";5u", detach: true}},
		"kitty":                 {{in: "\x1b[92;5u"}, {in: "\x1b[92;5u", detach: true}},
		"kitty with events":     {{in: "\x1b[92;5:1u"}, {in: "\x1b[92;5:3u"}, {in: "\x1b[92;5:1u", detach: true}},
		"kitty with num lock":   {{in: "\x1b[92;133u"}, {in: "\x1b[92;133u", detach: true}},
		"modifyOtherKeys":       {{in: "\x1b[27;5;92~"}, {in: bs, detach: true}},
		"ctrl-shift is not it":  {{in: "\x1b[92;6u\x1b[92;6u", out: "\x1b[92;6u\x1b[92;6u"}, {in: "\x1b[92u\\", out: "\x1b[92u\\"}},
		"lone release":          {{in: "\x1b[92;5:3u", out: "\x1b[92;5:3u"}},
		"bracketed paste":       {{in: paste, out: paste}, {in: bs + bs, detach: true}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			f := &detachFilter{}
			for i, s := range steps {
				out, detach := f.feed([]byte(s.in))
				if string(out) != s.out || detach != s.detach {
					t.Fatalf("step %d: feed(%q) = (%q, %v), want (%q, %v)", i, s.in, out, detach, s.out, s.detach)
				}
			}
		})
	}
}

// A lone Ctrl-\ is held until the caller flushes it, and each new press starts
// a new epoch so a stale timer cannot flush a newer one.
func TestDetachHoldAndFlush(t *testing.T) {
	f := &detachFilter{}
	f.feed([]byte("\x1b[A"))
	if f.pending() {
		t.Fatal("nothing should be held back")
	}
	f.feed([]byte(bs))
	e := f.epoch
	if !f.pending() || string(f.flush()) != bs || f.pending() {
		t.Fatal("a lone Ctrl-\\ should be held, then released by flush")
	}
	f.feed([]byte(bs))
	f.feed([]byte("a"))
	f.feed([]byte(bs))
	if f.epoch != e+2 || !f.pending() {
		t.Fatal("a new press must start a new epoch")
	}
}

func TestReplyDrain(t *testing.T) {
	var d replyDrain
	if got := d.filter([]byte("x")); string(got) != "x" {
		t.Fatal("inactive drain must pass input through")
	}
	d.start()
	// Replies are dropped, even when split across reads, up to our own answer.
	for _, in := range []string{"\x1b[?62;22c\x1b[12;", "1R\x1b[0"} {
		if got := d.filter([]byte(in)); got != nil {
			t.Fatalf("got %q", got)
		}
	}
	if got := d.filter([]byte("nls")); string(got) != "ls" {
		t.Fatalf("got %q", got)
	}
	if got := d.filter([]byte("more")); string(got) != "more" {
		t.Fatalf("got %q", got)
	}

	d.start()
	d.deadline = time.Now().Add(-time.Second)
	if got := d.filter([]byte("y")); string(got) != "y" {
		t.Fatal("expired drain must pass input through")
	}
}
