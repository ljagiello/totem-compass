package mesh

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// FuzzParse feeds Parse whatever a radio on channel 6 might hear. A decoded
// frame must re-encode, and decoding that encoding must give the same
// frame back: the encoders are what the emulator puts on the air.
func FuzzParse(f *testing.F) {
	for _, g := range golden {
		f.Add(unhex(f, g.hex))
	}
	f.Add([]byte{0xa7, 0x74, 0, 0})
	f.Add([]byte{0xa7, 0x74, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x7f})
	f.Fuzz(func(t *testing.T, b []byte) {
		orig := bytes.Clone(b)
		m, err := Parse(b)
		if !bytes.Equal(b, orig) {
			t.Fatal("Parse modified its input")
		}
		if (m == nil) == (err == nil) {
			t.Fatalf("Parse(%x) = %v, %v: want exactly one of message or error", b, m, err)
		}
		if err != nil {
			return
		}
		out, err := m.MarshalBinary()
		if err != nil {
			// Only what a Totem cannot send may fail to re-encode: a frame
			// too large for ESP-NOW, or a peer name that overruns the
			// 108-byte buffer (a longer frame can still carry one).
			if p, ok := m.(Peer); len(b) <= MaxFrame && (!ok || len(p.Name) <= MaxPeerName) {
				t.Fatalf("re-encode %#v: %v", m, err)
			}
			return
		}
		switch m := m.(type) {
		case Peer:
			if len(out) != PeerFrameLen {
				t.Fatalf("peer frame re-encoded to %d bytes", len(out))
			}
			if !bytes.Equal(out[peerNameOffset:peerNameOffset+len(m.Name)], b[peerNameOffset:peerNameOffset+len(m.Name)]) {
				t.Fatalf("name changed: %q", m.Name)
			}
		case Locate:
			// Every field but the two flags is copied verbatim.
			want := bytes.Clone(b[:LocateFrameLen])
			want[19], want[22] = byte(flag(m.SOS)), byte(flag(m.ReplyRequested))
			if !equalModNaN(out, want, 10, 14, 35, 39) {
				t.Fatalf("locate re-encoded to\n%x\nwant\n%x", out, want)
			}
		case SmartGroup:
			if len(out) != smartGroupHeaderLen+smartGroupMemberLen*len(m.Members) {
				t.Fatalf("%d members re-encoded to %d bytes", len(m.Members), len(out))
			}
			var floats []int
			for i := range m.Members {
				off := smartGroupHeaderLen + smartGroupMemberLen*i
				floats = append(floats, off+6, off+10)
			}
			want := bytes.Clone(b[:len(out)])
			copy(want[:smartGroupHeaderLen], out)
			if !equalModNaN(out, want, floats...) {
				t.Fatalf("member records changed:\n%x\nwant\n%x", out, want)
			}
		case DemiGod, Unknown:
			if !bytes.Equal(out, b) {
				t.Fatalf("opaque frame re-encoded to %x", out)
			}
		}
		m2, err := Parse(out)
		if err != nil {
			t.Fatalf("Parse(re-encoded %x): %v", out, err)
		}
		out2, err := m2.MarshalBinary()
		if _, ok := m.(Peer); ok && err == nil && isSubnormalHalf(out[43:]) {
			// MicroPython's encoder does not round-trip subnormals
			// (TestHalfMatchesMicroPython); no battery reads that low.
			out2[43], out2[44] = out[43], out[44]
		}
		if err != nil || !bytes.Equal(out2, out) {
			t.Fatalf("second round trip = %x, %v; want %x", out2, err, out)
		}
	})
}

// equalModNaN compares two frames, treating the float32s at the given
// offsets as equal when both are NaN: reflect, which encoding/binary uses,
// quiets a signaling NaN.
func equalModNaN(a, b []byte, floats ...int) bool {
	if len(a) != len(b) {
		return false
	}
	b = bytes.Clone(b)
	for _, off := range floats {
		if isNaN32(a[off:]) && isNaN32(b[off:]) {
			copy(b[off:off+4], a[off:off+4])
		}
	}
	return bytes.Equal(a, b)
}

func isSubnormalHalf(b []byte) bool {
	h := binary.LittleEndian.Uint16(b)
	return h>>10&0x1f == 0 && h&0x3ff != 0
}

func isNaN32(b []byte) bool {
	return math.IsNaN(float64(math.Float32frombits(binary.LittleEndian.Uint32(b))))
}

// FuzzHalf checks the half-float encoder against the exact value: for a
// finite float32 in the normal half range the error is at most half a unit
// in the last place, as round-half-up promises. When rounding carries out
// of the mantissa, MicroPython ORs the carry into the exponent instead of
// adding it (7.99994 encodes as 4), and so must encodeHalf.
func FuzzHalf(f *testing.F) {
	for _, v := range []float32{0, 1, 3.75, 4.2, -2, 65504, 6.1035156e-05, 1 + 1.0/2048} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v float32) {
		a := math.Abs(float64(v))
		if math.IsNaN(a) || a < 1.0/16384 || a > 65504 {
			return
		}
		bits := math.Float32bits(v)
		if bits>>13&0x3ff == 0x3ff && bits&(1<<12) != 0 {
			e := uint16(bits>>23&0xff) - (127 - 15)
			want := uint16(bits>>16)&0x8000 | (e|1)<<10
			if got := encodeHalf(v); got != want {
				t.Fatalf("encodeHalf(%g) = %#04x, want the MicroPython carry %#04x", v, got, want)
			}
			return
		}
		got := float64(decodeHalf(encodeHalf(v)))
		exp := math.Floor(math.Log2(a))
		if half := math.Ldexp(1, int(exp)-11); math.Abs(got-float64(v)) > half {
			t.Fatalf("decodeHalf(encodeHalf(%g)) = %g, error over %g", v, got, half)
		}
	})
}
