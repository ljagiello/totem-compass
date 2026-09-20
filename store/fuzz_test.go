package store

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzScan feeds the journal a sector full of arbitrary bytes, which is
// what a reboot finds after a torn write, a crash, a previous firmware or
// a wild pointer. Opening it must never panic, and whatever it decides
// about the old contents, the device must still be able to save and read
// its settings back.
func FuzzScan(f *testing.F) {
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 256))
	f.Add(bytes.Repeat([]byte{0}, 256))
	good, _ := State{Name: "emu_totem_abb0", Peers: []PeerState{{Name: "LCFs totem"}}}.MarshalBinary()
	f.Add(good)
	// Seed with real sectors — one record, and several — so the mutator
	// starts from bytes that already carry the magic and works on the
	// header fields, rather than having to guess four magic bytes.
	f.Add(sectorWith(good))
	f.Add(sectorWith(good, []byte("second"), []byte("third")))
	f.Fuzz(func(t *testing.T, raw []byte) {
		const size = 512
		sec := newMemSector(size)
		copy(sec.b, raw)

		j, err := Open(sec)
		if err != nil {
			t.Fatalf("a sector of noise failed to open: %v", err)
		}
		if _, err := j.Load(); err != nil && !errors.Is(err, ErrEmpty) {
			t.Fatalf("Load returned %v, want a payload or ErrEmpty", err)
		}
		if u, free := j.Used(), j.Free(); u < 0 || free < 0 || u+free != size {
			t.Fatalf("used %d + free %d != %d", u, free, size)
		}

		// Whatever was in there, a save has to stick: a device that
		// cannot write its bonds after a bad boot never recovers.
		want := []byte("settings after a bad boot")
		if err := j.Save(want); err != nil {
			t.Fatalf("saving over noise: %v", err)
		}
		got, err := mustReopen(t, sec).Load()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("after saving over noise: %q, %v", got, err)
		}
		// And a second save still reads back, whether it appended or had
		// to erase first.
		next := []byte("and the save after that")
		if err := j.Save(next); err != nil {
			t.Fatalf("second save: %v", err)
		}
		got, err = mustReopen(t, sec).Load()
		if err != nil || !bytes.Equal(got, next) {
			t.Fatalf("after the second save: %q, %v", got, err)
		}
	})
}

// FuzzStateDecode decodes arbitrary bytes as a state. The bytes come off
// flash, so anything can arrive: it must be rejected or decoded, never
// panic, and a state that decodes must encode back to what was read.
func FuzzStateDecode(f *testing.F) {
	seed, _ := State{
		Name: "emu_totem_abb0", ColorID: 6, Brightness: 255, SOSMuted: true, BootCount: 3,
		Peers: []PeerState{{MAC: [6]byte{0x8c, 0x94, 0xdf, 0x7b, 4, 0x78}, Name: "LCFs totem", Lat: 37.5, Lon: -122}},
	}.MarshalBinary()
	f.Add(seed)
	f.Add([]byte{1})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		var s State
		if err := s.UnmarshalBinary(b); err != nil {
			return
		}
		if len(s.Peers) > MaxPeers {
			t.Fatalf("decoded %d peers, over the %d limit", len(s.Peers), MaxPeers)
		}
		if len(s.Name) > maxName {
			t.Fatalf("decoded a name of %d bytes", len(s.Name))
		}
		again, err := s.MarshalBinary()
		if err != nil {
			t.Fatalf("a decoded state failed to encode: %v", err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("re-encoding changed the bytes:\n got %x\nwant %x", again, b)
		}
		var back State
		if err := back.UnmarshalBinary(again); err != nil {
			t.Fatalf("re-encoded state failed to decode: %v", err)
		}
	})
}

// sectorWith returns the bytes of a sector holding these records, so a
// fuzz seed starts from a journal rather than from noise.
func sectorWith(records ...[]byte) []byte {
	sec := newMemSector(512)
	j, err := Open(sec)
	if err != nil {
		panic(err)
	}
	for _, r := range records {
		if err := j.Save(r); err != nil {
			panic(err)
		}
	}
	return sec.b
}

func mustReopen(t *testing.T, sec Sector) *Journal {
	t.Helper()
	j, err := Open(sec)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return j
}
