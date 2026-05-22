package hosts

import (
	"bytes"
	"testing"
)

func TestByteRing(t *testing.T) {
	tests := []struct {
		name   string
		cap    int
		writes [][]byte
		want   []byte
	}{
		{"empty", 8, nil, nil},
		{"under capacity", 8, [][]byte{[]byte("hello")}, []byte("hello")},
		{"exact capacity", 4, [][]byte{[]byte("abcd")}, []byte("abcd")},
		{"single overflow", 4, [][]byte{[]byte("abcdef")}, []byte("cdef")},
		{"multi-write with wrap", 5, [][]byte{[]byte("abc"), []byte("def")}, []byte("bcdef")},
		{"oversized single chunk", 4, [][]byte{[]byte("longer-than-cap")}, []byte("-cap")},
		{"writes after wrap", 4, [][]byte{[]byte("abcd"), []byte("e"), []byte("f")}, []byte("cdef")},
		{"zero-length writes are no-ops", 4, [][]byte{[]byte("ab"), {}, []byte("cd")}, []byte("abcd")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newByteRing(tc.cap)
			for _, w := range tc.writes {
				r.write(w)
			}
			got := r.snapshot()
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			// Snapshot must be a copy — mutating it must not corrupt
			// further writes.
			if len(got) > 0 {
				got[0] = 0
				r.write([]byte("z"))
				next := r.snapshot()
				if next[len(next)-1] != 'z' {
					t.Fatalf("write after snapshot mutation broke ring: %q", next)
				}
			}
		})
	}
}

func TestByteRingNilSafe(t *testing.T) {
	var r *byteRing
	r.write([]byte("anything"))      // no panic
	if got := r.snapshot(); got != nil {
		t.Fatalf("nil ring snapshot want nil, got %q", got)
	}
}
