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

func runSteps(t *testing.T, f *detachFilter, steps []step) {
	t.Helper()
	for i, s := range steps {
		out, detach := f.feed([]byte(s.in))
		if string(out) != s.out || detach != s.detach {
			t.Fatalf("step %d: feed(%q) = (%q, %v), want (%q, %v)", i, s.in, out, detach, s.out, s.detach)
		}
	}
}

const ctrlBackslash = "\x1c"

func TestDetachPlain(t *testing.T) {
	runSteps(t, &detachFilter{}, []step{{in: "hello\r", out: "hello\r"}})
}

func TestDetachDoublePress(t *testing.T) {
	runSteps(t, &detachFilter{}, []step{{in: ctrlBackslash + ctrlBackslash, detach: true}})
	runSteps(t, &detachFilter{}, []step{{in: ctrlBackslash}, {in: ctrlBackslash, detach: true}})
	runSteps(t, &detachFilter{}, []step{{in: "ab", out: "ab"}, {in: ctrlBackslash}, {in: ctrlBackslash, detach: true}})
}

func TestDetachForwardsInputBeforeIt(t *testing.T) {
	runSteps(t, &detachFilter{}, []step{{in: "ls\r" + ctrlBackslash + ctrlBackslash + "junk", out: "ls\r", detach: true}})
}

func TestEscapePassesThroughImmediately(t *testing.T) {
	f := &detachFilter{}
	runSteps(t, f, []step{
		{in: "\x1b", out: "\x1b"},
		{in: "\x1b\x1b", out: "\x1b\x1b"},
		{in: "\x1b[A", out: "\x1b[A"},
		{in: "\x1bOB", out: "\x1bOB"},
		{in: "\x1bx", out: "\x1bx"},
		{in: "\x1b[27u\x1b[27u", out: "\x1b[27u\x1b[27u"}, // kitty Esc
	})
	if f.pending() {
		t.Fatal("nothing should be held back")
	}
}

func TestSingleCtrlBackslashIsReleased(t *testing.T) {
	f := &detachFilter{}
	runSteps(t, f, []step{{in: ctrlBackslash}})
	if !f.pending() {
		t.Fatal("Ctrl-\\ should be held")
	}
	if got := string(f.flush()); got != ctrlBackslash {
		t.Fatalf("flush = %q", got)
	}
	// Followed by another key, both are forwarded in order.
	runSteps(t, f, []step{{in: ctrlBackslash}, {in: "x", out: ctrlBackslash + "x"}})
	runSteps(t, f, []step{{in: ctrlBackslash}, {in: "\x1b[A", out: ctrlBackslash + "\x1b[A"}})
}

func TestDetachHeldKeepsSplitSequence(t *testing.T) {
	// While Ctrl-\\ is held, a sequence split across reads is completed
	// before deciding, so a split kitty second press still detaches.
	runSteps(t, &detachFilter{}, []step{{in: "\x1b[92;5u"}, {in: "\x1b[92"}, {in: ";5u", detach: true}})
}

func TestDetachKitty(t *testing.T) {
	runSteps(t, &detachFilter{}, []step{{in: "\x1b[92;5u"}, {in: "\x1b[92;5u", detach: true}})
	// With event types and release events reported.
	runSteps(t, &detachFilter{}, []step{
		{in: "\x1b[92;5:1u"}, {in: "\x1b[92;5:3u"}, {in: "\x1b[92;5:1u", detach: true},
	})
	// Num Lock on (modifiers 1+4+128).
	runSteps(t, &detachFilter{}, []step{{in: "\x1b[92;133u"}, {in: "\x1b[92;133u", detach: true}})
	// Mixed with the legacy byte and modifyOtherKeys.
	runSteps(t, &detachFilter{}, []step{{in: "\x1b[27;5;92~"}, {in: ctrlBackslash, detach: true}})
	// Ctrl-Shift-\\ and plain "\\" are other keys.
	runSteps(t, &detachFilter{}, []step{
		{in: "\x1b[92;6u\x1b[92;6u", out: "\x1b[92;6u\x1b[92;6u"},
		{in: "\x1b[92u\\", out: "\x1b[92u\\"},
	})
	// A release with nothing held is forwarded.
	runSteps(t, &detachFilter{}, []step{{in: "\x1b[92;5:3u", out: "\x1b[92;5:3u"}})
}

func TestDetachEpoch(t *testing.T) {
	f := &detachFilter{}
	f.feed([]byte(ctrlBackslash))
	e := f.epoch
	f.feed([]byte("a"))
	f.feed([]byte(ctrlBackslash))
	if f.epoch == e || !f.pending() {
		t.Fatal("a new press must start a new epoch")
	}
}

func TestDetachBracketedPaste(t *testing.T) {
	paste := "\x1b[200~a" + ctrlBackslash + ctrlBackslash + "b\x1b[201~"
	runSteps(t, &detachFilter{}, []step{
		{in: paste, out: paste},
		{in: ctrlBackslash + ctrlBackslash, detach: true},
	})
}

func TestReplyDrain(t *testing.T) {
	var d replyDrain
	if got := d.filter([]byte("x")); string(got) != "x" {
		t.Fatal("inactive drain must pass input through")
	}
	d.start()
	if got := d.filter([]byte("\x1b[?62;22c\x1b[12;")); got != nil {
		t.Fatalf("got %q", got)
	}
	if got := d.filter([]byte("1R\x1b[0")); got != nil {
		t.Fatalf("got %q", got)
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

func TestModeTracker(t *testing.T) {
	var m modeTracker
	m.observe([]byte("hi\x1b[?1049hvim"))
	if !m.altScreen {
		t.Fatal("want alt screen")
	}
	m.observe([]byte("\x1b[?104"))
	m.observe([]byte("9l$ "))
	if m.altScreen {
		t.Fatal("split leave sequence not recognized")
	}
	m.observe([]byte("\x1b[?1049h...\x1b[?1049l...\x1b[?1049h"))
	if !m.altScreen {
		t.Fatal("last sequence should win")
	}
}
