//go:build cgo

package ring

import (
	"bytes"
	"testing"
)

func frame(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestRing(t *testing.T) {
	Reset()
	src, dst := [6]byte{1, 2, 3, 4, 5, 6}, [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if _, _, _, _, ok := Pop(); ok {
		t.Fatal("an empty ring returned a frame")
	}
	Push(src, dst, -20, frame(108))
	gotSrc, gotDst, rssi, data, ok := Pop()
	if !ok || gotSrc != src || gotDst != dst || rssi != -20 || !bytes.Equal(data, frame(108)) {
		t.Fatalf("pop = %x %x %d % x, %v", gotSrc, gotDst, rssi, data, ok)
	}
	// Frames a Totem cannot send are refused rather than truncated.
	Push(src, dst, 0, nil)
	Push(src, dst, 0, frame(MaxFrame+1))
	if _, _, _, _, ok := Pop(); ok {
		t.Error("an empty or oversized frame was kept")
	}
	if Lost() != 0 {
		t.Errorf("refused frames counted as dropped: %d", Lost())
	}
	// A full ring drops the newest rather than overwriting the oldest.
	for i := range Slots + 3 {
		Push(src, dst, int8(i), frame(i+1))
	}
	if Lost() != 3 {
		t.Errorf("dropped %d, want 3", Lost())
	}
	for i := range Slots {
		_, _, _, data, ok := Pop()
		if !ok || !bytes.Equal(data, frame(i+1)) {
			t.Fatalf("slot %d = % x, %v", i, data, ok)
		}
	}
	if _, _, _, _, ok := Pop(); ok {
		t.Error("the drained ring still returns frames")
	}
}

// FuzzRing offers arbitrary frames to the receive ring and drains it: every
// frame that fits comes back byte for byte in order, one too long for an
// ESP-NOW payload is refused, and a full ring drops instead of overwriting.
func FuzzRing(f *testing.F) {
	f.Add([]byte{1, 4, 0xaa, 0xbb, 0xcc, 0xdd}, uint16(0))
	f.Add([]byte{0, 0}, uint16(1))
	f.Add(bytes.Repeat([]byte{2, 255}, 40), uint16(7))
	f.Fuzz(func(t *testing.T, script []byte, seed uint16) {
		Reset()
		var want [][]byte // what is still in the ring, oldest first
		dropped := 0
		dst := [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		for i := 0; i+1 < len(script); {
			op, n := script[i], int(script[i+1])
			i += 2
			n = min(n, len(script)-i)
			data := script[i : i+n]
			i += n
			src := [6]byte{byte(seed), byte(seed >> 8), 1, 2, 3, byte(n)}
			switch op % 3 {
			case 0, 1:
				Push(src, dst, int8(n), data)
				switch {
				case len(data) == 0 || len(data) > MaxFrame:
					// Refused: not an ESP-NOW payload.
				case len(want) >= Slots:
					dropped++
				default:
					want = append(want, bytes.Clone(data))
				}
			case 2:
				gotSrc, gotDst, rssi, data, ok := Pop()
				if ok != (len(want) > 0) {
					t.Fatalf("pop = %v with %d frames in the ring", ok, len(want))
				}
				if !ok {
					continue
				}
				if !bytes.Equal(data, want[0]) {
					t.Fatalf("popped % x, want % x", data, want[0])
				}
				if rssi != int8(len(want[0])) || gotDst != dst || gotSrc[5] != byte(len(want[0])) {
					t.Fatalf("popped src %x dst %x rssi %d for a %d-byte frame", gotSrc, gotDst, rssi, len(want[0]))
				}
				want = want[1:]
			}
			if Lost() != dropped {
				t.Fatalf("dropped %d, want %d", Lost(), dropped)
			}
		}
		for _, w := range want {
			_, _, _, data, ok := Pop()
			if !ok || !bytes.Equal(data, w) {
				t.Fatalf("draining: %v % x, want % x", ok, data, w)
			}
		}
		if _, _, _, _, ok := Pop(); ok {
			t.Fatal("the drained ring still returns frames")
		}
	})
}
