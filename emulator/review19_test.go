package emulator

// Regressions for the nineteenth review round.

import (
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// TestTheDistanceToTheOtherSideOfTheWorld: haversine's intermediate can
// come out a hair above 1 for two points near opposite sides of the
// globe, and Asin of that is NaN — which reaches the mesh delay, where
// int(NaN) is implementation-defined, and furthestPeer, where the
// builtin max carries it to every peer.
func TestTheDistanceToTheOtherSideOfTheWorld(t *testing.T) {
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{48.8575, -31.900307, -48.8575, 148.09969},
		{0, 0, 0, 180},
		{90, 0, -90, 0},
		{37.775, -122.42, -37.775, 57.58},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Errorf("distance(%v,%v -> %v,%v) = %v", tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
		if got < 0 || got > 21_000_000 {
			t.Errorf("distance(%v,%v -> %v,%v) = %.0f m, which is further than round the world",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
	}

	// And such a peer does not poison the node.
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 48.8575, Lon: -31.900307, AccuracyM: 3}
	})
	h.bond()
	f := statusFrame(t0)
	f.Lat, f.Lon = -48.8575, 148.09969
	h.rx(totem, self, -40, f)
	for _, p := range h.n.Peers() {
		if math.IsNaN(p.DistanceM) {
			t.Error("a peer on the other side of the world is NaN away")
		}
	}
	// And the fleet-wide figure the same distance feeds: furthestPeer
	// takes the builtin max over every peer, which returns NaN if any
	// argument is one, so a single unplaceable peer turned off the
	// far-peer status copy for all of them.
	if got := h.n.furthestPeer(); math.IsNaN(got) {
		t.Error("one peer on the other side of the world made every distance NaN")
	}
}

// TestABlankRecordIsNotSettings: `store open` on a sector that is empty
// or unreadable has no record to hand back. Taking that for a set of
// settings un-muted an alarm and turned the crystal red — choices
// someone had made on a device that had not saved them yet.
func TestABlankRecordIsNotSettings(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ColorID = int8(ColorBlue) })
	h.advance(bootDebounce)
	h.n.SetSOS(true)
	h.n.MuteSOS(h.now)
	if !h.n.SOSMuted() {
		t.Fatal("the alarm was not muted to begin with")
	}

	h.n.Restore(nil, h.now)
	if !h.n.SOSMuted() {
		t.Error("restoring nothing un-muted the alarm")
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorBlue {
		t.Errorf("restoring nothing turned the crystal %s", got)
	}
}

// TestABuiltInNameBeatsTheSavedOne: the board takes `-X main.name=` over
// the saved name on purpose, and restore runs straight after New — so
// assigning unconditionally put the old name back, saved it again, and a
// reflash with a new name never took effect.
func TestABuiltInNameBeatsTheSavedOne(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Name = "fieldunit7" })
	h.n.Restore(&store.State{Name: "emu_totem_abb0"}, h.now)
	if got := h.n.Config().Name; got != "fieldunit7" {
		t.Errorf("the saved name overwrote the built-in one: %q", got)
	}

	// A device still carrying the default has not been named, so the
	// record wins.
	h = newHarness(t, nil)
	h.n.Restore(&store.State{Name: "lcfs_spare"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_spare" {
		t.Errorf("the saved name did not come back: %q", got)
	}
}

// TestTheChargerRingIsNotReplayedAfterAPowerCycle: charging() marks the
// connection shown while the device is down precisely so the ring is not
// replayed when it comes back on a cable that never moved. Starting a
// run cleared that mark.
func TestTheChargerRingIsNotReplayedAfterAPowerCycle(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	for end := h.now.Add(time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
	}
	h.advance(bootAnim + time.Second)

	h.n.PowerOff(h.now)
	h.now = h.now.Add(time.Second)
	h.n.PowerOn(h.now)
	// The power-up ring plays; what must not happen is a second one from
	// the charger once it ends.
	h.advance(bootAnim + time.Second)
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatal("the charger replayed the ring after a power cycle on the same cable")
		}
	}
}

// TestABrightnessThatIsNotANumber: NaN survives min and max — every
// comparison with it is false — and would reach the uint8 conversion in
// every pixel, which Go leaves implementation-defined.
func TestABrightnessThatIsNotANumber(t *testing.T) {
	l := newLEDs(t0, ColorTeal)
	was := l.Brightness()
	l.SetBrightness(math.NaN())
	if got := l.Brightness(); math.IsNaN(got) {
		t.Error("the strip took a brightness that is not a number")
	} else if got != was {
		t.Errorf("brightness moved to %v", got)
	}
	// And the same for the download's share, which reaches the ring.
	l.Play(AnimOTA, t0)
	l.SetProgress(0.5)
	l.Tick(t0.Add(time.Second))
	half := litPixels(l)
	if half == 0 {
		t.Fatal("half a download lit no pixels at all")
	}
	// The share that is not a number leaves the ring where it was: it is
	// not a smaller download, it is no answer, and the last real one is
	// the truest thing on the strip.
	l.SetProgress(math.NaN())
	l.Tick(t0.Add(2 * time.Second))
	if got := litPixels(l); got != half {
		t.Errorf("a share that is not a number moved the ring from %d pixels to %d", half, got)
	}
}

func litPixels(l *LEDs) int {
	n := 0
	for _, px := range l.Ring() {
		if px != Off {
			n++
		}
	}
	return n
}
