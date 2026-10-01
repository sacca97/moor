package server

// Ring is a bounded byte buffer that keeps the most recent output. Memory is
// allocated as output arrives, up to the limit.
type Ring struct {
	buf     []byte
	max     int
	pos     int // next write position once wrapped
	wrapped bool
}

func NewRing(max int) *Ring {
	return &Ring{max: max}
}

func (r *Ring) Write(p []byte) {
	if len(p) >= r.max {
		r.buf = append(r.buf[:0], p[len(p)-r.max:]...)
		r.pos = 0
		r.wrapped = true
		return
	}
	if !r.wrapped {
		if len(r.buf)+len(p) <= r.max {
			r.buf = append(r.buf, p...)
			return
		}
		n := r.max - len(r.buf)
		r.buf = append(r.buf, p[:n]...)
		p = p[n:]
		r.wrapped = true
		r.pos = 0
	}
	for len(p) > 0 {
		n := copy(r.buf[r.pos:], p)
		p = p[n:]
		r.pos = (r.pos + n) % r.max
	}
}

// Bytes returns a copy of the buffered output, oldest first.
func (r *Ring) Bytes() []byte {
	out := make([]byte, 0, len(r.buf))
	if !r.wrapped {
		return append(out, r.buf...)
	}
	out = append(out, r.buf[r.pos:]...)
	return append(out, r.buf[:r.pos]...)
}

// Wrapped reports whether old output has been discarded.
func (r *Ring) Wrapped() bool { return r.wrapped }
