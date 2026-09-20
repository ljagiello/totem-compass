package emulator

// Regressions for the twenty-first review round.

import (
	"errors"
	"math"
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
	if st := h.n.OTA().State(); st == OTADone {
		t.Error("the update reported itself complete on a powered-down board")
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

// TestTheRestoreReadingHappensOnce: the reading and the power mode that
// follows it are the node coming into step with its own sensors, so they
// happen whether or not there was a record — and from one place, so a
// change to the pairing cannot be made to one copy and not the other.
func TestTheRestoreReadingHappensOnce(t *testing.T) {
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
			h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.1, 40 })
			h.n.Restore(tc.rec, h.now)
			if !h.n.Power().Off() {
				t.Error("a device restored on a flat pack kept running")
			}
		})
	}
}
