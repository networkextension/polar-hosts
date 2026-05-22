package hosts

// byteRing is a minimal fixed-capacity byte ring buffer used as the
// per-shell-run backscroll. The supervisor appends every stdout chunk;
// a reattaching bridge calls snapshot() to grab a copy of the current
// contents (oldest-to-newest) for replay before subscribing to live
// bytes.
//
// Not a "scrollback" in the xterm sense — we don't parse ANSI escapes
// or maintain a row buffer. We just keep the raw byte stream xterm
// would have rendered, so replay gives the operator the most recent
// screen state including all escape codes (color, cursor moves, etc.).
//
// Locking is internal; safe for concurrent supervisor writes + bridge
// snapshot reads.

import "sync"

type byteRing struct {
	mu    sync.Mutex
	buf   []byte // capacity-sized; reused
	start int    // logical start of oldest byte
	size  int    // number of valid bytes (≤ len(buf))
}

func newByteRing(cap int) *byteRing {
	if cap <= 0 {
		cap = 1
	}
	return &byteRing{buf: make([]byte, cap)}
}

// write appends bytes, evicting the oldest when full. Operates in
// amortized O(n) for the input size.
func (r *byteRing) write(p []byte) {
	if r == nil || len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cap := len(r.buf)
	// If the incoming chunk is bigger than capacity, only the tail
	// matters — drop everything else.
	if len(p) >= cap {
		copy(r.buf, p[len(p)-cap:])
		r.start = 0
		r.size = cap
		return
	}
	// Write at the slot after the last byte.
	writeAt := (r.start + r.size) % cap
	if writeAt+len(p) <= cap {
		copy(r.buf[writeAt:], p)
	} else {
		first := cap - writeAt
		copy(r.buf[writeAt:], p[:first])
		copy(r.buf[:len(p)-first], p[first:])
	}
	r.size += len(p)
	if r.size > cap {
		// Evicted oldest bytes — advance start by the overflow amount.
		overflow := r.size - cap
		r.start = (r.start + overflow) % cap
		r.size = cap
	}
}

// snapshot returns a fresh slice with the current contents (oldest to
// newest). Safe for the caller to retain.
func (r *byteRing) snapshot() []byte {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	out := make([]byte, r.size)
	cap := len(r.buf)
	if r.start+r.size <= cap {
		copy(out, r.buf[r.start:r.start+r.size])
	} else {
		first := cap - r.start
		copy(out, r.buf[r.start:])
		copy(out[first:], r.buf[:r.size-first])
	}
	return out
}
