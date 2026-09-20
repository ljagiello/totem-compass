package settings

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

var (
	self  = mesh.MAC{0x20, 0x9b, 0xa9, 0x70, 0xab, 0xb0}
	totem = mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78}
	t0    = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
)

// memSector is a flash sector in memory: bits only clear on a write, and
// only an erase sets them back, exactly as flash behaves.
type memSector struct {
	b      []byte
	writes int
	erases int
	// failWrite makes every write fail, which is a driver that has gone.
	failWrite bool
}

// sectorSize is the smallest region the ESP32 can erase, which is what
// the board gives the journal.
const sectorSize = 4096

func newMemSector() *memSector {
	s := &memSector{b: make([]byte, sectorSize)}
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
	if s.failWrite {
		return errors.New("no flash driver")
	}
	if off < 0 || off+len(p) > len(s.b) {
		return errors.New("write out of range")
	}
	s.writes++
	for i, v := range p {
		s.b[off+i] &= v
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

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(b))
	return len(b), nil
}

func testLog(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// node is a device with the given name, bonded to the owned Totem when
// asked, which is what there is to save.
func node(t *testing.T, name string, bond bool) *emulator.Node {
	t.Helper()
	n := emulator.New(emulator.Config{
		MAC: self, Owned: []mesh.MAC{totem}, Name: name,
		BattVolts: 4.1, BattPct: 90, Logger: testLog(t),
	}, t0)
	if bond {
		if err := n.AddBond([6]byte(totem), "Lukasz", t0); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestASaveComesBack is the whole point of the sector: what a device was
// running with is what it comes up with.
func TestASaveComesBack(t *testing.T) {
	sec := newMemSector()
	s := Open(testLog(t), sec)
	n := node(t, "lcfs_spare", true)
	n.SetColor(emulator.ColorBlue, t0)
	s.Save(n)

	// A reboot: a new store over the same sector, into a fresh node.
	again := Open(testLog(t), sec)
	fresh := node(t, "", false)
	again.Restore(fresh, t0)
	if got := fresh.BondCount(); got != 1 {
		t.Errorf("bonds after a reboot = %d", got)
	}
	if got := fresh.Config().Name; got != "lcfs_spare" {
		t.Errorf("name after a reboot = %q", got)
	}
	if got := fresh.LEDs().DefaultColor(); got != emulator.ColorBlue {
		t.Errorf("colour after a reboot = %s", got)
	}
	if got := again.State().BootCount; got != 1 {
		t.Errorf("boot count = %d, want the second boot", got)
	}
}

// TestReopenDoesNotWriteOverWhatItJustRead: `store open` is the path a
// board takes when its flash driver is being brought up and the sector
// was not read at boot. It copied the freshly-read journal into the live
// store but not the flag saying a record had been found, so Restore was
// handed no record where there was one — and the save that follows wrote
// the running defaults over every bond and setting on the device.
func TestReopenDoesNotWriteOverWhatItJustRead(t *testing.T) {
	sec := newMemSector()
	// A device that has been used: bonded, named, and blue.
	was := Open(testLog(t), sec)
	n := node(t, "lcfs_spare", true)
	n.SetColor(emulator.ColorBlue, t0)
	was.Save(n)

	// It comes up without reading the sector, as the bring-up const does.
	s := Open(testLog(t), nil)
	fresh := node(t, "", false)
	s.Restore(fresh, t0) // nothing to restore: nothing was read

	// Then someone types `store open`.
	s.Reopen(fresh, t0, sec)
	if got := fresh.BondCount(); got != 1 {
		t.Errorf("bonds after store open = %d, want the saved one", got)
	}
	if got := fresh.Config().Name; got != "lcfs_spare" {
		t.Errorf("name after store open = %q", got)
	}
	if got := fresh.LEDs().DefaultColor(); got != emulator.ColorBlue {
		t.Errorf("colour after store open = %s", got)
	}

	// And the sector still holds it, rather than the defaults the node
	// was running with a moment ago.
	after := Open(testLog(t), sec)
	if got := after.State().Name; got != "lcfs_spare" {
		t.Errorf("the sector says %q: store open wrote over what it read", got)
	}
	if got := len(after.State().Peers); got != 1 {
		t.Errorf("the sector holds %d peers after store open", got)
	}
}

// TestForgetLeavesNothingToComeBackTo: a factory reset has to wipe the
// node as well as the flash, or the save that follows writes the live
// bonds straight back.
func TestForgetLeavesNothingToComeBackTo(t *testing.T) {
	sec := newMemSector()
	s := Open(testLog(t), sec)
	n := node(t, "lcfs_spare", true)
	s.Save(n)

	if err := s.Forget(n, t0); err != nil {
		t.Fatal(err)
	}
	if got := n.BondCount(); got != 0 {
		t.Errorf("%d bonds survived the reset", got)
	}
	// A save right afterwards must not put them back.
	s.Save(n)
	after := Open(testLog(t), sec)
	if got := len(after.State().Peers); got != 0 {
		t.Errorf("%d bonds came back after a reset", got)
	}
	// The record a reset writes says nothing about the run that ended.
	if after.State().SleepMs != 0 || after.State().LearnedMaxVolts != 0 {
		t.Errorf("the reset record still carries the last run: %+v", after.State())
	}
}

// TestASaveThatChangesNothingCostsNoWrite: an erase stalls the radio, so
// a save nobody asked for is not free.
func TestASaveThatChangesNothingCostsNoWrite(t *testing.T) {
	sec := newMemSector()
	s := Open(testLog(t), sec)
	n := node(t, "lcfs_spare", true)
	s.Save(n)
	was := sec.writes
	for range 20 {
		s.Save(n)
	}
	if sec.writes != was {
		t.Errorf("%d writes for settings nobody changed", sec.writes-was)
	}
	// And a real change still lands.
	n.SetColor(emulator.ColorHotPink, t0)
	s.Save(n)
	if sec.writes == was {
		t.Error("a colour someone chose was not written")
	}
}

// TestABoardWithNoFlashStillRuns: the device has to boot whatever the
// sector says, and a save that cannot land is reported rather than lost
// silently or fatal.
func TestABoardWithNoFlashStillRuns(t *testing.T) {
	s := Open(testLog(t), nil)
	n := node(t, "lcfs_spare", true)
	s.Restore(n, t0)
	s.Save(n)
	s.Report()
	if err := s.Forget(n, t0); err == nil {
		t.Error("a reset on a board with no sector reported success")
	}

	// And one whose driver refuses every write.
	sec := newMemSector()
	sec.failWrite = true
	s = Open(testLog(t), sec)
	s.Save(n)
	if got := len(s.State().Peers); got != 0 {
		t.Errorf("a save that failed was recorded as having %d peers", got)
	}
}

// TestNoiseInTheSectorIsNotSettings: the sector holds whatever a torn
// write, a bad block or a previous firmware left.
func TestNoiseInTheSectorIsNotSettings(t *testing.T) {
	sec := newMemSector()
	for i := range sec.b {
		sec.b[i] = byte(i*31 + 7)
	}
	s := Open(testLog(t), sec)
	n := node(t, "lcfs_spare", true)
	s.Restore(n, t0)
	if got := n.Config().Name; got != "lcfs_spare" {
		t.Errorf("noise in the sector renamed the device to %q", got)
	}
	if got := n.BondCount(); got != 1 {
		t.Errorf("noise in the sector left %d bonds", got)
	}
	// A save over the noise has to land, or the device can never save
	// again.
	s.Save(n)
	after := Open(testLog(t), sec)
	if got := after.State().Name; got != "lcfs_spare" {
		t.Errorf("the save over the noise did not stick: %q", got)
	}
}

// TestARecordFromAnotherVersionIsIgnored: the format can change, and a
// board flashed with a new build reads the old one's bytes.
func TestARecordFromAnotherVersionIsIgnored(t *testing.T) {
	sec := newMemSector()
	j, err := store.Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte("not a State of any version")); err != nil {
		t.Fatal(err)
	}
	s := Open(testLog(t), sec)
	if got := s.State().Name; got != "" {
		t.Errorf("a record this build cannot read was used anyway: %q", got)
	}
	n := node(t, "lcfs_spare", true)
	s.Restore(n, t0)
	if got := n.Config().Name; got != "lcfs_spare" {
		t.Errorf("an unreadable record renamed the device to %q", got)
	}
}
