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

// TestASequenceNumberOnlyClimbs: the number is what a reader sees, so
// the next save has to write one the sector does not already hold. A
// record carrying nothing proves least of all — the checksum of an empty
// body is 0 whatever the header says — so sixteen bytes of noise in that
// shape could otherwise hand the journal any number at all, including
// one that sends the counter backwards or wraps it to zero.
func TestASequenceNumberOnlyClimbs(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint32
	}{
		{"as far back as it goes", 0},
		{"one, in a sector that has seen five", 1},
		{"all ones, which wraps the next save to zero", 0xffffffff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := newMemSector(4096)
			j, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 5 {
				if err := j.Save([]byte{byte(i), 'x'}); err != nil {
					t.Fatal(err)
				}
			}
			was := j.Seq()
			writeRaw(t, sec, j.Used(), tc.seq, nil)

			again, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			if got := again.Seq(); got < was {
				t.Errorf("a record with nothing in it took the counter from %d back to %d", was, got)
			}
			if err := again.Save([]byte("what came next")); err != nil {
				t.Fatal(err)
			}
			if got := again.Seq(); got <= was {
				t.Errorf("the save after it took number %d, and the sector already holds up to %d", got, was)
			}
		})
	}
}

// TestATornWriteKeepsItsNumber: a reset caught mid-save is this
// journal's ordinary failure, and the number in that header is the one
// the save meant to use — so dropping it had the next save write it
// again, twice in one sector.
func TestATornWriteKeepsItsNumber(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("the settings before the reset")); err != nil {
		t.Fatal(err)
	}
	was := j.Seq()

	// The save the reset cut short: a whole header, and a body that does
	// not check out.
	off := j.Used()
	writeRaw(t, sec, off, was+1, []byte("half of what it meant to write"))
	sec.b[off+headerLen+2] ^= 0xff // where the reset landed

	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Torn(); got != 1 {
		t.Errorf("the torn write was counted %d times", got)
	}
	if err := again.Save([]byte("the settings after it")); err != nil {
		t.Fatal(err)
	}
	if got := again.Seq(); got <= was+1 {
		t.Errorf("the save after a torn write took number %d, which the torn record already carries", got)
	}
}

// TestNoiseCannotDriveTheCounter: the checksum covers the body and not
// the header, so a torn write and a record carrying nothing prove
// nothing about their own number. One word of rubbish claiming four
// billion would otherwise pin the counter near the top of the range for
// good, and every save after it would wrap to zero.
func TestNoiseCannotDriveTheCounter(t *testing.T) {
	for _, tc := range []struct {
		name string
		torn bool
	}{
		{"a record with nothing in it", false},
		{"a torn write", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := newMemSector(4096)
			j, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Save([]byte("the settings someone chose")); err != nil {
				t.Fatal(err)
			}
			was := j.Seq()

			off := j.Used()
			if tc.torn {
				writeRaw(t, sec, off, 0xfffffe00, []byte("a save a reset cut short"))
				sec.b[off+headerLen+3] ^= 0xff
			} else {
				writeRaw(t, sec, off, 0xfffffe00, nil)
			}

			again, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			// One step at most: enough that the next save writes a number
			// the sector does not hold, not enough to be driven.
			if got := again.Seq(); got != was+1 {
				t.Errorf("a header claiming %d took the counter from %d to %d",
					uint32(0xfffffe00), was, got)
			}
			// And the saves after it climb rather than wrap.
			for range 3 {
				if err := again.Save([]byte("and what came after")); err != nil {
					t.Fatal(err)
				}
			}
			if got := again.Seq(); got <= was {
				t.Errorf("after three more saves the counter reads %d, below the %d it started at", got, was)
			}
		})
	}
}

// TestALongLivedBoardKeepsCounting: a record whose checksum holds over a
// body with something in it was written by this journal — nothing else
// lands that pattern by accident — so its number is taken as it stands,
// and a board that has been saving for a year is not reset to 1 by a
// reboot.
func TestALongLivedBoardKeepsCounting(t *testing.T) {
	sec := newMemSector(4096)
	// What a sector looks like after many erases: the numbers carry on
	// past the few records the sector currently holds.
	writeRaw(t, sec, 0, 4000, []byte("an older setting"))
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Seq(); got != 4000 {
		t.Errorf("a board that had saved 4000 times came back at %d", got)
	}
	if err := j.Save([]byte("what it chose next")); err != nil {
		t.Fatal(err)
	}
	if got := j.Seq(); got != 4001 {
		t.Errorf("the next save took number %d", got)
	}
}
