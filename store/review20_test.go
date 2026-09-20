package store

// Regressions for the twentieth review round.

import (
	"encoding/binary"
	"fmt"
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

// TestTheCounterIsNotParkedAtTheErasedValue: all ones is the pattern of
// a sector nobody has written, not a number anybody chose. A counter
// parked there stays there — Save will not go past it — so every save
// afterwards writes the number the sector already holds, which is the
// one thing counting is for.
func TestTheCounterIsNotParkedAtTheErasedValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint32
		body []byte
	}{
		{"a whole record claiming it", 0xffffffff, []byte("settings someone chose")},
		{"a record with nothing in it", 0xffffffff, nil},
		{"one step below it", 0xfffffffe, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := newMemSector(4096)
			writeRaw(t, sec, 0, tc.seq, tc.body)
			j, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			if got := j.Seq(); got == 0xffffffff {
				t.Fatal("the counter was parked at the value erased flash reads as")
			}
			was := j.Seq()
			for i := range 3 {
				if err := j.Save([]byte{byte(i), 'x'}); err != nil {
					t.Fatal(err)
				}
				got := j.Seq()
				if got <= was {
					t.Fatalf("save %d took number %d, and the one before it was %d", i, got, was)
				}
				was = got
			}
		})
	}
}

// TestSaveNeverWritesTheErasedValue: scan refuses to read all ones as a
// number, so a record written with it is a record whose number is
// thrown away. A sector of them comes back with the counter at 0, and
// the saves after that hand out numbers the sector already holds.
func TestSaveNeverWritesTheErasedValue(t *testing.T) {
	sec := newMemSector(4096)
	// A whole, proven record one short of the top, which is the only way
	// the counter gets there at all.
	writeRaw(t, sec, 0, 0xfffffffe, []byte("the settings before the top"))
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Seq(); got != 0xfffffffe {
		t.Fatalf("the counter opened at %d", got)
	}
	for i := range 3 {
		if err := j.Save([]byte{byte(i), 'x'}); err != nil {
			t.Fatal(err)
		}
		if got := j.Seq(); got == 0xffffffff {
			t.Fatalf("save %d wrote the value erased flash reads as", i)
		}
	}
	// And what is on the flash still reads back as saved.
	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Load(); err != nil || len(got) != 2 {
		t.Errorf("after the saves: %q, %v", got, err)
	}
}

// TestABogusLengthHidesNothing: a torn record's header failed its
// checksum, so its length field is no more trustworthy than its
// sequence number. Sixteen bytes of noise claiming a length of 2000
// stepped the scan two kilobytes down the sector, past every good
// record in between — Load never saw them and the next save appended
// beyond them.
func TestABogusLengthHidesNothing(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("an older setting")); err != nil {
		t.Fatal(err)
	}

	// Noise in the shape of a header: the magic, a length the sector can
	// hold, and a checksum that does not match what follows.
	off := j.Used()
	copy(sec.b[off:], magic[:])
	binary.LittleEndian.PutUint32(sec.b[off+4:], 7)
	binary.LittleEndian.PutUint16(sec.b[off+8:], 2000)
	binary.LittleEndian.PutUint32(sec.b[off+12:], 0xdeadbeef)

	// And the newest settings, written right after it as a save would.
	want := []byte("what the device chose last")
	writeRaw(t, sec, off+headerLen+align, 2, want)

	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if again.Torn() != 1 {
		t.Errorf("the noise was counted as %d torn writes", again.Torn())
	}
	got, err := again.Load()
	if err != nil {
		t.Fatalf("loading after the noise: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("loaded %q, and the newest record says %q", got, want)
	}
	// And the write head is exactly after that record, not somewhere
	// within a slack of it: every term here is known, since writeRaw
	// built the record two lines up.
	end := off + headerLen + align + headerLen + len(want)
	if got := again.Used(); got != end+pad(len(want)) {
		t.Errorf("the write head is at %d, and the last record ends at %d", got, end+pad(len(want)))
	}
}

// TestATornWriteCostsNoErase: a reset caught mid-save is this journal's
// ordinary failure, and the save after it has to append past the
// wreckage as any other save would. Sending the write head to the end of
// the sector instead made every such save erase the whole thing — which
// is the one operation this package is built to avoid, and which a
// second reset in that window turns into the loss of every setting.
func TestATornWriteCostsNoErase(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("the settings before the reset")); err != nil {
		t.Fatal(err)
	}
	used := j.Used()

	// The save the reset cut short.
	sec.failWriteAfter = 20
	_ = j.Save([]byte("a save that never finished"))
	sec.failWriteAfter = -1

	again, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if again.Torn() == 0 {
		t.Error("the torn write was not noticed")
	}
	if got := again.Used(); got <= used || got > used+128 {
		t.Errorf("the write head is at %d, and the last good record ended at %d", got, used)
	}
	if got := again.Free(); got < 3000 {
		t.Errorf("a torn write left %d bytes free in a 4096-byte sector", got)
	}

	erases := sec.erases
	if err := again.Save([]byte("the settings after it")); err != nil {
		t.Fatal(err)
	}
	if sec.erases != erases {
		t.Errorf("the save after a torn write erased the sector %d times", sec.erases-erases)
	}
	// And the save lands: what comes back is what was written last,
	// past the wreckage rather than instead of it. What was there before
	// the reset is gone from Load by design — only the newest record is
	// kept — but it was not erased to get here, which is what the erase
	// count above says.
	third, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := third.Load(); err != nil || string(got) != "the settings after it" {
		t.Errorf("after the save: %q, %v", got, err)
	}
}

// TestAFailedWriteCostsNoErase: a driver that gives up partway through a
// write is not a reset — the journal is still running and knows what it
// attempted. Leaving the head where it was had the next save find bytes
// that are not erased and clear the whole sector, which is the cost this
// design exists to avoid.
func TestAFailedWriteCostsNoErase(t *testing.T) {
	// How much of the record reached the flash. Zero is the one the
	// board's own driver produces most: a range check, an alignment
	// check and a lock that will not open all refuse before writing a
	// byte, and stepping over a record that does not exist leaves erased
	// bytes in the middle of the journal — which the scan stops at, so
	// every save after it is invisible at the next boot.
	for _, landed := range []int{0, 2, 12, 20} {
		t.Run(fmt.Sprintf("%d bytes landed", landed), func(t *testing.T) {
			sec := newMemSector(4096)
			j, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Save([]byte("the settings before the failure")); err != nil {
				t.Fatal(err)
			}

			sec.failWriteAfter = landed
			if err := j.Save([]byte("a write the driver gave up on")); err == nil {
				t.Fatal("a write that failed reported success")
			}
			sec.failWriteAfter = -1

			erases := sec.erases
			want := []byte("the settings after it")
			if err := j.Save(want); err != nil {
				t.Fatal(err)
			}
			// A save after a whole header landed appends past it. One
			// after a stump of bytes that is not a header has to clear
			// them, which costs the sector and loses nothing. What must
			// not happen either way is a save that reports success and
			// cannot be read back.
			if landed >= headerLen && sec.erases != erases {
				t.Errorf("the save after a failed write erased the sector %d times", sec.erases-erases)
			}
			again, err := Open(sec)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := again.Load(); err != nil || string(got) != string(want) {
				t.Errorf("after the save: %q, %v", got, err)
			}
		})
	}
}
