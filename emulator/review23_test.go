package emulator

// Regressions for the twenty-third review round.

import (
	"math"
	"testing"
	"time"
)

// TestTheMeshScheduleIsATimeThisDeviceReaches: peerDistance feeds
// meshDelivery and comes out as meshNext, the instant this peer is next
// asked about. distance() no longer hands it a NaN, but the arithmetic
// after it is still unguarded, and a number that overflows or that lands
// centuries away is a peer the mesh never asks about again.
//
// The far end is the firmware's own answer and is kept: a peer on the
// other side of the world really is put off for days by
// cal_mesh_delivery, and this package's job is to behave like the
// device. What is pinned is that the answer is a real, positive,
// climbing span for every distance a device can be from another one.
func TestTheMeshScheduleIsATimeThisDeviceReaches(t *testing.T) {
	const halfWayRound = 20_015_000 // meters, pole to pole the long way
	var last time.Duration
	for d := 0.0; d <= halfWayRound; d += 5000 {
		got := meshDelivery(d)
		switch {
		case got <= 0:
			t.Fatalf("meshDelivery(%.0f m) = %v", d, got)
		case got < 6*time.Second:
			t.Fatalf("meshDelivery(%.0f m) = %v, under the floor", d, got)
		case got < last:
			t.Fatalf("meshDelivery(%.0f m) = %v, less than the %v before it", d, got, last)
		case got > 7*24*time.Hour:
			t.Fatalf("meshDelivery(%.0f m) = %v, which is not a span a device waits out", d, got)
		}
		last = got
	}

	// And a distance that could not be measured does not become one.
	if got := meshDelivery(-1); got < 6*time.Second || got > time.Minute {
		t.Errorf("meshDelivery(-1) = %v, want the floor", got)
	}
	if got := meshDelivery(math.NaN()); got <= 0 {
		t.Errorf("meshDelivery(NaN) = %v", got)
	}
}

// TestABoardBuiltOnAFlatPackComesUpOff: New takes a reading, and the
// mode has to follow it there as much as anywhere else. Left to the
// first poll, a node built on a pack under the cutoff reports power mode
// normal until something else looks — and on the board that was masked
// only by main happening to call restore two lines later.
func TestABoardBuiltOnAFlatPackComesUpOff(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.1, 40 })
	if !h.n.Power().Off() {
		t.Error("a node built on a pack under the cutoff came up running")
	}

	// The window is armed before the mode is applied, so powering down
	// empties the job list rather than leaving a job behind it: a device
	// that is off must not transmit.
	if h.n.hasJob(jobWindow) {
		t.Error("a device that came up switched off still has a radio window armed")
	}

	// And one on a good pack is unaffected.
	h = newHarness(t, nil)
	if h.n.Power().Off() {
		t.Error("a node on a full pack came up switched off")
	}
	if !h.n.hasJob(jobWindow) {
		t.Error("a node on a full pack came up with no radio window")
	}
}
