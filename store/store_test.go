package store

import (
	"bytes"
	"errors"
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
