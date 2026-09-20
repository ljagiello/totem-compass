package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
	"unicode/utf8"
)

// memSector is a flash sector in memory: bits only clear on a write, and
// only an erase sets them back, exactly as flash behaves. A test that
// forgot to erase therefore fails here the way it would on the device.
type memSector struct {
	b []byte
	// failWriteAfter stops writes after this many bytes, which is a reset
	// caught mid-save.
	failWriteAfter int
	writes         int
	erases         int
}

func newMemSector(size int) *memSector {
	s := &memSector{b: make([]byte, size), failWriteAfter: -1}
	for i := range s.b {
		s.b[i] = 0xff
	}
	return s
}

func (s *memSector) Size() int { return len(s.b) }

func (s *memSector) ReadAt(p []byte, off int) error {
	if off < 0 || off+len(p) > len(s.b) {
		return errors.New("read out of range")
	}
	copy(p, s.b[off:])
	return nil
}

func (s *memSector) WriteAt(p []byte, off int) error {
	if off < 0 || off+len(p) > len(s.b) {
		return errors.New("write out of range")
	}
	s.writes++
	for i, v := range p {
		if s.failWriteAfter >= 0 && i >= s.failWriteAfter {
			return errors.New("power lost mid-write")
		}
		s.b[off+i] &= v // flash clears bits; it never sets them
	}
	return nil
}

func (s *memSector) Erase() error {
	s.erases++
	for i := range s.b {
		s.b[i] = 0xff
	}
	return nil
}

func TestSaveAndLoad(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Load(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("a fresh sector loaded %v, want ErrEmpty", err)
	}
	for i, want := range []string{"first", "second", "third"} {
		if err := j.Save([]byte(want)); err != nil {
			t.Fatal(err)
		}
		// A reopened journal reads what the last save wrote, which is what
		// a reboot does.
		j2, err := Open(sec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := j2.Load()
		if err != nil || string(got) != want {
			t.Fatalf("save %d: loaded %q, %v; want %q", i, got, err, want)
		}
		if j2.Seq() != uint32(i+1) {
			t.Errorf("seq = %d, want %d", j2.Seq(), i+1)
		}
	}
	if sec.erases != 0 {
		t.Errorf("erased %d times for three saves that fit", sec.erases)
	}
}

// TestSectorFillsAndWraps: the journal erases only when a save no longer
// fits, which is what keeps the radio running on the device.
func TestSectorFillsAndWraps(t *testing.T) {
	sec := newMemSector(512)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	rec := bytes.Repeat([]byte("x"), 100)
	for i := 0; i < 20; i++ {
		if err := j.Save(rec); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if sec.erases == 0 {
		t.Error("never erased, so a full sector was written past")
	}
	if sec.erases > 6 {
		t.Errorf("erased %d times in 20 saves of 116 bytes into 512", sec.erases)
	}
	j2, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := j2.Load()
	if err != nil || !bytes.Equal(got, rec) {
		t.Fatalf("after wrapping: %q, %v", got, err)
	}
}

// TestTornWriteKeepsThePrevious: a reset during a save must not cost the
// settings that were already there. The half-written record fails its
// checksum and the one before it still loads.
func TestTornWriteKeepsThePrevious(t *testing.T) {
	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("good state")); err != nil {
		t.Fatal(err)
	}
	sec.failWriteAfter = 20 // the reset lands inside the next record
	if err := j.Save([]byte("this save never finishes")); err == nil {
		t.Fatal("the cut-short write reported success")
	}
	sec.failWriteAfter = -1

	j2, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := j2.Load()
	if err != nil || string(got) != "good state" {
		t.Fatalf("after a torn write: %q, %v; want the previous state", got, err)
	}
	if j2.Torn() == 0 {
		t.Error("the torn record was not counted")
	}
	// And the journal still takes new saves.
	if err := j2.Save([]byte("after the reset")); err != nil {
		t.Fatal(err)
	}
	j3, _ := Open(sec)
	if got, _ := j3.Load(); string(got) != "after the reset" {
		t.Fatalf("loaded %q after saving again", got)
	}
}

// TestNoiseOpensEmpty: a sector that holds anything else — a previous
// firmware, a wild write — must still let the device boot.
func TestNoiseOpensEmpty(t *testing.T) {
	sec := newMemSector(4096)
	for i := range sec.b {
		sec.b[i] = byte(i * 7)
	}
	j, err := Open(sec)
	if err != nil {
		t.Fatalf("noise failed to open: %v", err)
	}
	if _, err := j.Load(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("noise loaded %v, want ErrEmpty", err)
	}
	if err := j.Save([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if got, _ := (mustOpen(t, sec)).Load(); string(got) != "fresh" {
		t.Fatalf("loaded %q after writing over noise", got)
	}
}

func TestRecordTooLarge(t *testing.T) {
	j := mustOpen(t, newMemSector(4096))
	if err := j.Save(make([]byte, MaxRecord+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a record over the limit returned %v", err)
	}
}

func TestTinySectorFails(t *testing.T) {
	if _, err := Open(newMemSector(8)); err == nil {
		t.Fatal("a sector too small for a record opened")
	}
}

func mustOpen(t *testing.T, sec Sector) *Journal {
	t.Helper()
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestStateRoundTrip(t *testing.T) {
	want := State{
		Name: "emu_totem_abb0", ColorID: 6, Brightness: 200, SOSMuted: true,
		BootCount: 42, SleepMs: 1234567, LearnedMaxVolts: 4.11,
		Peers: []PeerState{
			{MAC: [6]byte{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78}, Name: "LCFs totem", ColorID: 2,
				Lat: 37.5867, Lon: -122.0073, LastSeenUnix: 1758300000},
			{MAC: [6]byte{1, 2, 3, 4, 5, 6}},
		},
	}
	b, err := want.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var got State
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.ColorID != want.ColorID || got.Brightness != want.Brightness ||
		!got.SOSMuted || got.BootCount != want.BootCount || got.SleepMs != want.SleepMs ||
		got.LearnedMaxVolts != want.LearnedMaxVolts || len(got.Peers) != 2 {
		t.Fatalf("round trip gave %+v", got)
	}
	if got.Peers[0] != want.Peers[0] || got.Peers[1] != want.Peers[1] {
		t.Errorf("peers = %+v", got.Peers)
	}
}

func TestStateRejects(t *testing.T) {
	good, err := State{Name: "x", Peers: []PeerState{{Name: "y"}}}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"empty":            {},
		"other version":    {99, 0, 0, 0, 0},
		"truncated":        good[:len(good)-3],
		"trailing bytes":   append(append([]byte(nil), good...), 0),
		"too many peers":   {1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, MaxPeers + 1},
		"name over frame":  append([]byte{1, maxName + 1}, bytes.Repeat([]byte("a"), maxName+1)...),
		"name not utf-8":   {1, 2, 0xff, 0xfe},
		"muted not a bool": {1, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, b := range tests {
		var s State
		if err := s.UnmarshalBinary(b); err == nil {
			t.Errorf("%s decoded to %+v", name, s)
		} else if !errors.Is(err, ErrBadState) {
			t.Errorf("%s: %v, want ErrBadState", name, err)
		}
	}
}

func TestStateMarshalRejects(t *testing.T) {
	if _, err := (State{Peers: make([]PeerState, MaxPeers+1)}).MarshalBinary(); err == nil {
		t.Error("encoded more peers than the record holds")
	}
	if _, err := (State{Name: string(bytes.Repeat([]byte("a"), maxName+1))}).MarshalBinary(); err == nil {
		t.Error("encoded a name longer than a frame holds")
	}
	if _, err := (State{Name: "\xff\xfe"}).MarshalBinary(); err == nil {
		t.Error("encoded a name that is not UTF-8")
	}
}

// TestOversizedSaveKeepsWhatIsThere: a record too big for the sector can
// never be written, and finding that out after the erase would cost the
// settings that were already saved.
func TestOversizedSaveKeepsWhatIsThere(t *testing.T) {
	sec := newMemSector(512)
	j := mustOpen(t, sec)
	if err := j.Save([]byte("the bonds")); err != nil {
		t.Fatal(err)
	}
	erases := sec.erases

	if err := j.Save(make([]byte, 600)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a record bigger than the sector returned %v", err)
	}
	if sec.erases != erases {
		t.Error("it erased the sector on its way to failing")
	}
	got, err := mustReopen(t, sec).Load()
	if err != nil || string(got) != "the bonds" {
		t.Fatalf("after the refused save: %q, %v", got, err)
	}
	// And the journal still works.
	if err := j.Save([]byte("later")); err != nil {
		t.Fatal(err)
	}
	if got, _ := mustReopen(t, sec).Load(); string(got) != "later" {
		t.Fatalf("loaded %q", got)
	}
}

// TestSanitizeName: a name comes off the air, where nothing checks it, and
// ends up in a record this package refuses to encode. Rather than let one
// bad frame stop every save for good, a caller runs it through here.
func TestSanitizeName(t *testing.T) {
	long := string(bytes.Repeat([]byte("é"), 40)) // 80 bytes
	for in, want := range map[string]string{
		"LCFs totem": "LCFs totem",
		"":           "",
		"ok\xff\xfe": "ok",
		"\xff":       "",
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeName(long); len(got) > maxName || !utf8.ValidString(got) {
		t.Errorf("a long name came back as %d bytes: %q", len(got), got)
	}
	// Whatever it returns has to be encodable, which is the whole point.
	for _, in := range []string{"ok\xff", long, "\x00\x01", "héllo"} {
		s := State{Name: SanitizeName(in), Peers: []PeerState{{Name: SanitizeName(in)}}}
		if _, err := s.MarshalBinary(); err != nil {
			t.Errorf("a sanitized %q still failed to encode: %v", in, err)
		}
	}
}

// TestAnEmptyRecordIsRefused: Load reads "nothing saved" off a payload
// with nothing in it, so a save with nothing in it reported success and
// left every reader — and the next scan — believing the sector had never
// held anything. The settings that were there were gone and nobody was
// told.
func TestAnEmptyRecordIsRefused(t *testing.T) {
	sec := newMemSector(512)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("the settings that were there")); err != nil {
		t.Fatal(err)
	}
	if err := j.Save(nil); !errors.Is(err, ErrEmptyRecord) {
		t.Errorf("saving nothing returned %v, want %v", err, ErrEmptyRecord)
	}
	got, err := j.Load()
	if err != nil {
		t.Fatalf("after the refused save: %v", err)
	}
	if string(got) != "the settings that were there" {
		t.Errorf("the record became %q", got)
	}
	// And a fresh scan of the same sector says the same.
	got, err = mustReopen(t, sec).Load()
	if err != nil || string(got) != "the settings that were there" {
		t.Errorf("reopened: %q, %v", got, err)
	}
}

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

// TestATornRecordOfErasedBytesIsNotARecord: the record checksum covers
// the body and not the header, and crc32 of four 0xff bytes is
// 0xffffffff — which is what the erased checksum field already reads. So
// a save of a 4-byte payload cut short after its header, with body and
// checksum still erased, checked out as a valid record: Load handed back
// four 0xff bytes as the saved state, the good record before it stayed
// hidden, and Torn() reported nothing wrong.
func TestATornRecordOfErasedBytesIsNotARecord(t *testing.T) {
	// The arithmetic the whole thing turns on, stated so a change to it
	// cannot pass quietly.
	if got := crc32.ChecksumIEEE([]byte{0xff, 0xff, 0xff, 0xff}); got != 0xffffffff {
		t.Fatalf("crc32 of four erased bytes is %#x, not the erased value", got)
	}

	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("the settings that were really saved")
	if err := j.Save(want); err != nil {
		t.Fatal(err)
	}

	// Now tear a second save after its header: magic, sequence and a
	// length of 4 written, body and checksum still erased.
	at := j.next
	var h [headerLen]byte
	copy(h[0:4], magic[:])
	binary.LittleEndian.PutUint32(h[4:8], j.seq+1)
	binary.LittleEndian.PutUint16(h[8:10], 4)
	binary.LittleEndian.PutUint32(h[12:16], 0xffffffff)
	if err := sec.WriteAt(h[:], at); err != nil {
		t.Fatal(err)
	}

	back, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.Load()
	if err != nil {
		t.Fatalf("the journal would not load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("loaded %q, want the record before the tear, %q", got, want)
	}
	if back.Torn() == 0 {
		t.Error("the torn write was not counted")
	}
}

// TestTheOnePayloadARecordCannotCarry: four erased bytes cannot be told
// from the sector they would be written to, so Save refuses rather than
// write something Load will not give back.
func TestTheOnePayloadARecordCannotCarry(t *testing.T) {
	j, err := Open(newMemSector(4096))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte{0xff, 0xff, 0xff, 0xff}); !errors.Is(err, ErrErasedRecord) {
		t.Errorf("saving four erased bytes: %v, want ErrErasedRecord", err)
	}
	// One byte either side is fine, and so is the same run with anything
	// else in it: the collision is only at this one length.
	for _, p := range [][]byte{
		{0xff, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xfe},
	} {
		if err := j.Save(p); err != nil {
			t.Errorf("saving % x: %v", p, err)
		}
	}
}
