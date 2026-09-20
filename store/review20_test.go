package store

// Regressions for the twentieth review round.

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// writeRaw puts a well-formed record straight into the sector, which is
// how a build that did not refuse an empty payload could leave one.
func writeRaw(t *testing.T, s *memSector, off int, seq uint32, body []byte) {
	t.Helper()
	rec := make([]byte, headerLen+len(body)+pad(len(body)))
	copy(rec, magic[:])
	binary.LittleEndian.PutUint32(rec[4:8], seq)
	binary.LittleEndian.PutUint16(rec[8:10], uint16(len(body)))
	binary.LittleEndian.PutUint32(rec[12:16], crc32.ChecksumIEEE(body))
	copy(rec[headerLen:], body)
	for i := headerLen + len(body); i < len(rec); i++ {
		rec[i] = 0xff
	}
	if err := s.WriteAt(rec, off); err != nil {
		t.Fatal(err)
	}
}

// TestARecordWithNothingInItHidesNothing: Save refuses an empty payload
// now, but a sector an earlier build wrote can already hold one — and
// scan took it as the newest record, so Load answered ErrEmpty and every
// good record written before it was gone.
func TestARecordWithNothingInItHidesNothing(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("the settings someone chose")
	if err := j.Save(want); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, sec, j.Used(), j.Seq()+1, nil)

	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Load()
	if err != nil {
		t.Fatalf("a record with nothing in it hid the one before it: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("loaded %q, want %q", got, want)
	}
	// Counted, and counted apart from a torn write: its magic, length
	// and checksum all hold, so nothing was cut short, and a console
	// reporting flash trouble should not describe the wrong trouble.
	if again.Blank() == 0 {
		t.Error("the empty record was passed over in silence")
	}
	if got := again.Torn(); got != 0 {
		t.Errorf("a whole record with nothing in it was reported as %d torn writes", got)
	}
	// Its sequence number is taken even though its payload is not, or
	// the next save writes a number the sector already holds.
	if got := again.Seq(); got < 2 {
		t.Errorf("the sequence number went back to %d", got)
	}
	// And the next save lands past it rather than on top of it.
	next := []byte("and the ones they chose after that")
	if err := again.Save(next); err != nil {
		t.Fatal(err)
	}
	third, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := third.Load(); err != nil || string(got) != string(next) {
		t.Errorf("after the save: %q, %v", got, err)
	}
}

// TestAnEmptyRecordAtTheStartOfTheSector: the same record with nothing
// before it. Load has nothing to fall back on, so this is the boundary
// where "no saved state" is the right answer — and the sector still has
// to take a save afterwards.
func TestAnEmptyRecordAtTheStartOfTheSector(t *testing.T) {
	sec := newMemSector(4096)
	writeRaw(t, sec, 0, 1, nil)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Load(); err == nil {
		t.Error("an empty record was loaded as if it were settings")
	}
	if err := j.Save([]byte("what the device chose next")); err != nil {
		t.Fatal(err)
	}
	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Load(); err != nil || string(got) != "what the device chose next" {
		t.Errorf("after the save: %q, %v", got, err)
	}
}
