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
)

// detachTimeout is how long a Ctrl-\ is held back waiting for a second one.
// If it expires, the Ctrl-\ is forwarded to the session.
const detachTimeout = 400 * time.Millisecond

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// detachFilter watches keyboard input for Ctrl-\ Ctrl-\, the detach key.
//
// A Ctrl-\ press is held back rather than forwarded; a second one means
// detach, any other key releases the held Ctrl-\ followed by that key, and a
// timeout releases it alone. All other input, Esc included, passes through
// without delay.
//
// Besides the raw 0x1c byte, Ctrl-\ is recognized in the encodings terminals
// use when a program enables extended keyboard reporting: the kitty keyboard
// protocol (CSI 92;5u, as Codex enables) and xterm's modifyOtherKeys
// (CSI 27;5;92~). To find those, input is split into tokens: a complete
// escape sequence, an ESC plus one byte, or a single byte. Detection is
// suspended inside bracketed pastes.
//
// detachFilter does no timing itself; the caller calls flush after
// detachTimeout whenever a feed starts a new pending state (see epoch).
type detachFilter struct {
	seq     []byte // incomplete escape sequence
	held    []byte // a held Ctrl-\ press (plus any key releases after it)
	inPaste bool
	// epoch increments each time a Ctrl-\ starts being held, so a timer
	// armed for an earlier press can be recognized as stale.
	epoch int
}

// feed processes input and returns the bytes to forward. detach is true when
// Ctrl-\ Ctrl-\ was seen; any input after it is discarded.
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
		tok = bytes.Clone(tok)
		f.seq = f.seq[:0]
		if out, detach = f.emit(out, tok); detach {
			return out, true
		}
	}
	// An incomplete sequence at the end of a read is almost always a lone
	// Esc key. Forward it now rather than delaying Esc; only while a Ctrl-\
	// is held is it kept, in case it begins the second press.
	if len(f.seq) > 0 && len(f.held) == 0 {
		out = append(out, f.seq...)
		f.seq = f.seq[:0]
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
	switch keyEvent(tok) {
	case detachPress:
		if len(f.held) > 0 {
			f.reset()
			return out, true
		}
		f.epoch++
		f.held = append(f.held, tok...)
		return out, false
	case detachRelease:
		if len(f.held) > 0 {
			f.held = append(f.held, tok...)
			return out, false
		}
	}
	out = append(out, f.held...)
	f.held = f.held[:0]
	out = append(out, tok...)
	if bytes.Equal(tok, pasteStart) {
		f.inPaste = true
	}
	return out, false
}

func (f *detachFilter) pending() bool { return len(f.seq) > 0 || len(f.held) > 0 }

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
}

const (
	otherKey = iota
	detachPress
	detachRelease
)

// keyEvent classifies a token as a Ctrl-\ press, a Ctrl-\ release (kitty
// protocol only), or anything else.
func keyEvent(tok []byte) int {
	if len(tok) == 1 && tok[0] == detachByte {
		return detachPress
	}
	if string(tok) == "\x1b[27;5;92~" { // xterm modifyOtherKeys
		return detachPress
	}
	// kitty: CSI key[:alternates] ; modifiers[:event] [; text] u
	if len(tok) < 4 || tok[0] != esc || tok[1] != '[' || tok[len(tok)-1] != 'u' {
		return otherKey
	}
	fields := strings.Split(string(tok[2:len(tok)-1]), ";")
	if len(fields) < 2 {
		return otherKey
	}
	key, _, _ := strings.Cut(fields[0], ":")
	mods, event, _ := strings.Cut(fields[1], ":")
	m, err := strconv.Atoi(mods)
	// Modifiers are encoded as 1 + bits; ignore Caps Lock (64) and Num Lock
	// (128), require exactly Ctrl (4).
	if key != "92" || err != nil || (m-1)&^(64|128) != 4 {
		return otherKey
	}
	switch event {
	case "", "1", "2":
		return detachPress
	case "3":
		return detachRelease
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

const drainTimeout = time.Second

func (d *replyDrain) start() {
	d.active = true
	d.deadline = time.Now().Add(drainTimeout)
	d.buf = nil
}

// filter returns the part of p that should be processed as user input.
func (d *replyDrain) filter(p []byte) []byte {
	if !d.active {
		return p
	}
	if time.Now().After(d.deadline) {
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
