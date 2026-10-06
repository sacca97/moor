package client

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

const (
	esc        = 0x1b
	detachByte = 0x1c // Ctrl-\
	prefixByte = 0x02 // Ctrl-b
)

// detachTimeout is how long a Ctrl-\ is held back waiting for a second one.
// Ctrl-b, the prefix key, is different: as in tmux it waits for the next key
// for as long as it takes, which a touch keyboard needs.
// If it expires, the Ctrl-\ is forwarded to the session.
const detachTimeout = 400 * time.Millisecond

// escTimeout is how long an incomplete escape sequence that may be part of a
// paste marker is held back.
const escTimeout = 30 * time.Millisecond

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// detachFilter watches keyboard input for the detach keys: Ctrl-\ Ctrl-\, or
// Ctrl-b d as in tmux.
//
// A Ctrl-\ or Ctrl-b press is held back rather than forwarded. Ctrl-\ then
// Ctrl-\, or Ctrl-b then d, means detach. After Ctrl-b, as after tmux's
// prefix, the filter waits for the next key with no timeout; Ctrl-b Ctrl-b
// sends a single literal Ctrl-b, and any other key is forwarded after the
// Ctrl-b. A lone Ctrl-\ is released after a timeout, or by the next key.
// All other input passes through without delay, except that an incomplete
// escape sequence at the end of a read is held for escTimeout when it could be
// the start of a bracketed-paste marker, so a marker split across reads is
// still recognized; a lone Esc is released when the timeout expires.
//
// Besides the raw bytes, the keys are recognized in the encodings terminals
// use when a program enables extended keyboard reporting: the kitty keyboard
// protocol (CSI 92;5u for Ctrl-\, as Codex enables) and xterm's
// modifyOtherKeys (CSI 27;5;92~). To find those, input is split into tokens: a complete
// escape sequence, an ESC plus one byte, or a single byte. Detection is
// suspended inside bracketed pastes.
//
// detachFilter does no timing itself; the caller calls flush after
// detachTimeout whenever a feed starts a new pending state that is timed (see
// epoch and timed).
type detachFilter struct {
	seq     []byte // incomplete escape sequence
	held    []byte // a held Ctrl-\ or Ctrl-b press (plus any key releases after it)
	prefix  bool   // held is Ctrl-b, waiting for d, rather than Ctrl-\
	inPaste bool
	// epoch increments each time a press starts being held, so a timer
	// armed for an earlier press can be recognized as stale.
	epoch int
}

// feed processes input and returns the bytes to forward. detach is true when
// a detach sequence was seen; any input after it is discarded.
func (f *detachFilter) feed(p []byte) (out []byte, detach bool) {
	for _, b := range p {
		if len(f.seq) == 0 {
			if b == esc {
				f.seq = append(f.seq, b)
				continue
			}
			if out, detach = f.emit(out, []byte{b}); detach {
				return out, true
			}
			continue
		}

		var tok []byte
		switch {
		case len(f.seq) == 1:
			switch b {
			case esc:
				// ESC ESC: the first is a key of its own.
				if out, detach = f.emit(out, []byte{esc}); detach {
					return out, true
				}
				continue
			case '[', 'O':
				f.seq = append(f.seq, b)
				continue
			default:
				tok = []byte{esc, b}
			}
		case f.seq[1] == 'O':
			tok = append(f.seq, b)
		default: // CSI
			f.seq = append(f.seq, b)
			if (b < 0x40 || b > 0x7e) && len(f.seq) < 64 {
				continue
			}
			tok = f.seq
		}
		// tok may alias f.seq's backing array, which is reused for the next
		// sequence, so copy it before resetting.
		tok = bytes.Clone(tok)
		f.seq = f.seq[:0]
		if out, detach = f.emit(out, tok); detach {
			return out, true
		}
	}
	// An incomplete sequence at the end of a read is kept if a press is
	// held (it may begin the second key) or it may be a split paste marker;
	// otherwise it is forwarded now.
	if len(f.seq) > 0 && len(f.held) == 0 && !partialMarker(f.seq) {
		out = append(out, f.seq...)
		f.seq = f.seq[:0]
	}
	if len(f.seq) > 0 {
		f.epoch++ // a half-key is waiting and needs a timeout
	}
	return out, false
}

// emit handles one complete token.
func (f *detachFilter) emit(out, tok []byte) ([]byte, bool) {
	if f.inPaste {
		if bytes.Equal(tok, pasteEnd) {
			f.inPaste = false
		}
		return append(out, tok...), false
	}
	ev := keyEvent(tok)
	if len(f.held) > 0 {
		switch {
		case ev == detachPress && !f.prefix, ev == dKey && f.prefix:
			f.reset()
			return out, true
		case ev == prefixPress && f.prefix:
			// Ctrl-b Ctrl-b sends one literal Ctrl-b, as in tmux.
			f.reset()
			return append(out, tok...), false
		case ev == keyRelease:
			f.held = append(f.held, tok...)
			return out, false
		}
		// Not the second half of a detach: release what was held, then
		// treat tok afresh (it may itself start a new press).
		out = append(out, f.held...)
		f.reset()
	}
	if ev == detachPress || ev == prefixPress {
		f.epoch++
		f.prefix = ev == prefixPress
		f.held = append(f.held, tok...)
		return out, false
	}
	out = append(out, tok...)
	if bytes.Equal(tok, pasteStart) {
		f.inPaste = true
	}
	return out, false
}

// partialMarker reports whether seq is a proper prefix of a paste marker.
func partialMarker(seq []byte) bool {
	return bytes.HasPrefix(pasteStart, seq) || bytes.HasPrefix(pasteEnd, seq)
}

// timeout is how long what is held back may wait before the caller flushes
// it: escTimeout for a bare partial sequence, detachTimeout once a press is
// held.
func (f *detachFilter) timeout() time.Duration {
	if len(f.held) == 0 {
		return escTimeout
	}
	return detachTimeout
}

func (f *detachFilter) pending() bool { return len(f.seq) > 0 || len(f.held) > 0 }

// timed reports whether what is held back must be released after
// detachTimeout. A Ctrl-b waiting for its key is not: it waits indefinitely,
// unless a partial escape sequence is also pending.
//
// Inside a bracketed paste nothing is timed: releasing a partial marker early
// would make the filter miss the end of the paste, and a lone Esc is not
// ambiguous there.
func (f *detachFilter) timed() bool {
	return f.pending() && !f.inPaste && (!f.prefix || len(f.seq) > 0)
}

// flush releases everything held back. The caller invokes it when
// detachTimeout expires.
func (f *detachFilter) flush() []byte {
	out := append(append([]byte(nil), f.held...), f.seq...)
	f.reset()
	return out
}

func (f *detachFilter) reset() {
	f.seq = f.seq[:0]
	f.held = f.held[:0]
	f.prefix = false
}

const (
	otherKey    = iota
	detachPress // Ctrl-\
	prefixPress // Ctrl-b
	dKey        // d, with no modifiers
	keyRelease  // release of a Ctrl-\ or Ctrl-b (kitty protocol only)
)

// keyEvent classifies a token as one of the keys the filter cares about, in
// any of the encodings it recognizes, or anything else.
func keyEvent(tok []byte) int {
	if len(tok) == 1 {
		switch tok[0] {
		case detachByte:
			return detachPress
		case prefixByte:
			return prefixPress
		case 'd':
			return dKey
		}
		return otherKey
	}
	switch string(tok) { // xterm modifyOtherKeys
	case "\x1b[27;5;92~":
		return detachPress
	case "\x1b[27;5;98~":
		return prefixPress
	}
	// kitty: CSI key[:alternates] [; modifiers[:event] [; text]] u
	if len(tok) < 4 || tok[0] != esc || tok[1] != '[' || tok[len(tok)-1] != 'u' {
		return otherKey
	}
	fields := strings.Split(string(tok[2:len(tok)-1]), ";")
	key, _, _ := strings.Cut(fields[0], ":")
	mods, event := "1", ""
	if len(fields) > 1 {
		mods, event, _ = strings.Cut(fields[1], ":")
	}
	m, err := strconv.Atoi(mods)
	if err != nil {
		return otherKey
	}
	// Modifiers are encoded as 1 + bits; ignore Caps Lock (64) and Num Lock
	// (128).
	ctrl := (m-1)&^(64|128) == 4
	plain := (m-1)&^(64|128) == 0
	var ev int
	switch {
	case key == "92" && ctrl:
		ev = detachPress
	case key == "98" && ctrl:
		ev = prefixPress
	case key == "100" && plain:
		ev = dKey
	default:
		return otherKey
	}
	switch event {
	case "", "1", "2":
		return ev
	case "3":
		if ev != dKey {
			return keyRelease
		}
	}
	return otherKey
}

// replyDrain discards terminal replies to queries contained in replayed
// output. Replayed output may hold queries (cursor position, device
// attributes, colors, ...) that the terminal answers on stdin; forwarding
// those answers would inject garbage into the session. After the replay the
// client sends its own status query (drainQuery); everything on stdin up to
// its answer (drainReply) is discarded, since terminals answer in order.
type replyDrain struct {
	active   bool
	deadline time.Time
	buf      []byte
}

var (
	drainQuery = []byte("\x1b[5n")
	drainReply = []byte("\x1b[0n")
)

// drainTimeout is how long, after the status query is sent, to wait for its
// answer. A terminal chewing through a huge replay answers late.
const drainTimeout = 5 * time.Second

// start begins discarding input. It runs before the replay is written, so
// replies to its queries are read as they arrive instead of piling up in the
// tty input buffer, which drops what does not fit, possibly our own answer.
// There is no deadline until arm.
func (d *replyDrain) start() {
	d.active = true
	d.deadline = time.Time{}
	d.buf = nil
}

// arm starts the clock once the status query has been sent.
func (d *replyDrain) arm() {
	d.deadline = time.Now().Add(drainTimeout)
}

// filter returns the part of p that should be processed as user input.
func (d *replyDrain) filter(p []byte) []byte {
	if !d.active {
		return p
	}
	if !d.deadline.IsZero() && time.Now().After(d.deadline) {
		// The terminal never answered; stop discarding.
		d.active = false
		d.buf = nil
		return p
	}
	d.buf = append(d.buf, p...)
	if i := bytes.Index(d.buf, drainReply); i >= 0 {
		rest := d.buf[i+len(drainReply):]
		d.active = false
		d.buf = nil
		return rest
	}
	// Keep only a tail long enough to find a reply split across reads.
	if len(d.buf) > 64 {
		d.buf = append(d.buf[:0], d.buf[len(d.buf)-len(drainReply):]...)
	}
	return nil
}
