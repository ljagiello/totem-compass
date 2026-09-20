package mesh

// A regression for what CI's FuzzParse caught in the round before this
// one, kept in mesh because it is a property of this package's codec.

import (
	"bytes"
	"testing"
)

// basePeer is a frame that encodes, so patching two bytes of it leaves
// everything else valid.
func basePeer() Peer {
	return Peer{Command: PeerStatus, Name: "x", TimeOfDayMs: -1, Unix: -1}
}

// battBytes finds where the battery lives on the air, by encoding two
// voltages whose halves differ in both bytes and looking for the run.
// Derived rather than written down, so a change to the frame layout
// moves the test with it instead of leaving it patching the wrong field.
func battBytes(t *testing.T) int {
	t.Helper()
	lo, hi := basePeer(), basePeer()
	lo.BattVolts, hi.BattVolts = 0, MaxBattVolts // 0x0000 and 0x7bff
	a, err := lo.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	b, err := hi.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	at, n := -1, 0
	for i := range a {
		if a[i] != b[i] {
			if at < 0 {
				at = i
			}
			n++
		}
	}
	if at < 0 || n != 2 || a[at+1] == b[at+1] {
		t.Fatalf("the battery is not two bytes at one offset: %d bytes differ from %d", n, at)
	}
	return at
}

// TestEveryBatteryOnTheAirReEncodes: the battery field is sixteen
// arbitrary bits that arrive from a radio, and the encoder is a port of
// MicroPython's with no range check, so some of those bits decode to
// NaN, to an infinity, or to a negative. A round of review added a range
// check to MarshalBinary that refused all three, which made frames this
// package can read into frames it cannot write back — CI's FuzzParse
// found it within a second, on a frame of 0x30 bytes whose battery bits
// decode to NaN.
//
// Every pattern, not a sample: there are only 65536 and the codec is the
// thing being pinned.
func TestEveryBatteryOnTheAirReEncodes(t *testing.T) {
	at := battBytes(t)
	frame, err := basePeer().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for bits := range 1 << 16 {
		frame[at], frame[at+1] = byte(bits>>8), byte(bits)
		in := bytes.Clone(frame)
		m, err := Parse(in)
		if err != nil {
			t.Fatalf("battery bytes %02x %02x: the frame stopped parsing: %v",
				frame[at], frame[at+1], err)
		}
		if _, err := m.MarshalBinary(); err != nil {
			t.Fatalf("battery bytes %02x %02x decoded to %v, which will not go back out: %v",
				frame[at], frame[at+1], m.(Peer).BattVolts, err)
		}
	}
}

// TestAFiniteVoltageThatIsNotAHalfIsRefused is the other half of it: the
// check was worth having, because a finite value past the half's largest
// does not saturate. The exponent runs off the end of its five bits, so
// these go out as something else entirely and every receiver believes it.
func TestAFiniteVoltageThatIsNotAHalfIsRefused(t *testing.T) {
	for _, v := range []float32{100000, 131072, 1e6, -100000, -1e6} {
		p := basePeer()
		p.BattVolts = v
		if _, err := p.MarshalBinary(); err == nil {
			t.Errorf("a battery of %v V encoded as %v V", v, decodeHalf(encodeHalf(v)))
		}
	}
}
