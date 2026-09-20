package emulator

// Regressions for the twenty-first review round.

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// collapsingPack is a sensor source whose battery the test changes
// without the node being told: a driver reports what the hardware says
// at the moment it is read, and between two reads a pack can go flat.
type collapsingPack struct{ b Battery }

func (c *collapsingPack) Read(time.Time) Sensors { return Sensors{Battery: c.b} }

// TestAnUpdateStopsWhenTheReadingSwitchesTheDeviceOff: Update takes a
// fresh reading and acts on it, and acting on it can switch the device
// off — under a cutoff in volts, where the gate beside it reads a
// percentage. The check at the top of Update was passed by a device that
// was still on a poll ago, so without a second look the update ran on a
// board whose ring, jobs and outbox had just been cleared: it held the
// touch inputs for the whole exchange and ended saying "ota complete".
func TestAnUpdateStopsWhenTheReadingSwitchesTheDeviceOff(t *testing.T) {
	// A pack that still reports a percentage the gate is happy with, so
	// that what stops the update is the cutoff and not the gate.
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 80}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	h.advance(bootAnim + time.Second)
	if h.n.Power().Off() {
		t.Fatal("the device was down before the pack collapsed")
	}

	// The pack collapses, and nothing has read it yet.
	pack.b = Battery{Volts: 3.2, Percent: 80}
	if h.n.Power().Off() {
		t.Fatal("the node was told about the collapse before it read it")
	}

	err := h.n.Update(h.now)
	if !h.n.Power().Off() {
		t.Fatal("the reading Update took did not switch the device off")
	}
	if !errors.Is(err, ErrPoweredDown) {
		t.Errorf("update on a board the reading switched off: %v", err)
	}
	// The positive: it went through fail(), so the ring shows an update
	// that did not happen rather than one still in flight. A plain
	// return would leave the state at OTAChecking, and the ring holding
	// AnimWiFi on a board that is off.
	if st := h.n.OTA().State(); st != OTAFailed {
		t.Errorf("the update ended in state %v, want it failed", st)
	}
}

// TestANameThatIsNotTextAtAll: New decides whether anyone chose the name
// before SanitizeName has had its say, and a name that is not text comes
// out of that empty. Such a device ran nameless and, because it counted
// as named, refused the saved name at every boot afterwards.
func TestANameThatIsNotTextAtAll(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Name = "\xff\xfe\xff" })
	// Nameless is not a name: the default stands in.
	if got := h.n.Config().Name; got != DefaultName(self) {
		t.Errorf("a name that is not text left the device called %q", got)
	}
	// And nobody chose it, so the record's name comes back.
	h.n.Restore(&store.State{Name: "lcfs_spare"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_spare" {
		t.Errorf("the saved name did not come back: %q", got)
	}
}

// TestADistanceWhoseIntermediateGoesNegative: the clamp was one-sided.
// With a latitude outside ±90 the two haversine terms cancel and the
// sign is left to the last bit; Sqrt of a negative is NaN, which Asin
// carries straight out past a guard that only looked at the inputs.
func TestADistanceWhoseIntermediateGoesNegative(t *testing.T) {
	// Found by sweeping: at these the two terms cancel to
	// -2.7755575615628914e-17, and Sqrt of that is NaN.
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{113.5, 0, 66.5, 180},
		{117.5, 0, 62.5, 180},
		{118.5, 0, 61.5, 180},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) {
			t.Errorf("distance(%v,%v -> %v,%v) is not a number",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		}
	}
}

// countingPack answers like a driver and counts how often it is asked.
type countingPack struct {
	b     Battery
	reads int
}

func (c *countingPack) Read(time.Time) Sensors {
	c.reads++
	return Sensors{Battery: c.b}
}

// TestEveryRestorePathReadsTheSensorsOnce: the reading and the power
// mode that follows it are the node coming into step with its own
// sensors rather than anything a record said, so they happen on every
// path through Restore — including the one where there is no record,
// which is where they were missing.
//
// Once, too, which is the narrower half: a reading taken in both
// restoreRecord and the tail would poll a driver twice per boot and
// book two spans of time against a device that lived through one.
//
// What no test here can see is that the pairing appears once in the
// source; the refactor that put it there is for the reader, and this is
// for the behavior.
func TestEveryRestorePathReadsTheSensorsOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *store.State
	}{
		{"no record", nil},
		{"a record", &store.State{Name: "lcfs_spare"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A pack under the cutoff: the device has to come up and go
			// straight back down, rather than report mode normal until
			// something polls it.
			pack := &countingPack{b: Battery{Volts: 3.1, Percent: 40}}
			h := newHarness(t, func(c *Config) { c.Sensors = pack })
			pack.reads = 0

			h.n.Restore(tc.rec, h.now)
			if !h.n.Power().Off() {
				t.Error("a device restored on a flat pack kept running")
			}
			if pack.reads != 1 {
				t.Errorf("Restore read the sensors %d times, want once", pack.reads)
			}
		})
	}
}

// TestTheNameWarningSaysWhatTheDeviceRunsAs: this line is what someone
// reads when a name did not take, and it reported the name from before
// the default was filled in — `using=""` while the device came up as
// emu_totem_xxxx.
func TestTheNameWarningSaysWhatTheDeviceRunsAs(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Name = "\xff\xfe\xff"
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	got := h.n.Config().Name
	if !strings.Contains(logged.String(), "name is not text") {
		t.Fatalf("a name that is not text was taken without a word about it: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "using="+got) {
		t.Errorf("the device runs as %q and the line says %s", got, logged.String())
	}
	// And the line does not call the MAC-derived default a remnant of
	// the name someone chose.
	if strings.Contains(logged.String(), "using what is left") {
		t.Error("the default name was described as what is left of the one that was given")
	}
}
