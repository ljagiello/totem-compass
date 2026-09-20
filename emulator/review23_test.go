package emulator

// Regressions for the twenty-third review round.

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
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
		case got > 155*time.Hour:
			// The firmware's own answer for the far side of the world is
			// about six and a half days. Anything beyond it is the
			// arithmetic having gone wrong rather than a distance.
			t.Fatalf("meshDelivery(%.0f m) = %v, longer than the width of the world", d, got)
		}
		last = got
	}

	// And a distance that could not be measured goes to the floor, not
	// into the arithmetic. The same band as the loop above, because
	// "positive" is not an assertion here: int(NaN) is the most negative
	// int64 on amd64, and the multiplications that follow overflow into
	// a large positive number — a peer next asked about in two hundred
	// years, which reads as positive and is a peer never asked about
	// again.
	for _, d := range []float64{-1, -20_000_000, math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := meshDelivery(d)
		if got < 6*time.Second || got > time.Minute {
			t.Errorf("meshDelivery(%v) = %v, want the floor", d, got)
		}
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

	// The reading is taken before the window is armed and acted on where
	// it is taken, so a device that came up under the cutoff is already
	// down by then and New does not arm one at all: a device that is off
	// must not transmit.
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

// TestEveryReadingIsActedOn: the mode is not an opinion about the
// battery, it is the battery — so it follows every reading, from the one
// place that takes them. Five of the ten callers of read() paired it
// with applyPowerMode and five did not, so `batt 2` on the console
// reported power mode normal until something else polled, and a
// simulation swapped in on a flat pack went on running.
func TestEveryReadingIsActedOn(t *testing.T) {
	t.Run("a console battery", func(t *testing.T) {
		h := newHarness(t, nil)
		h.advance(bootAnim + time.Second)
		if err := h.n.SetBattery(2, false, h.now); err != nil {
			t.Fatal(err)
		}
		if got := h.n.Power().Mode(); got == PowerNormal {
			t.Error("a pack at 2 percent still reads as mode normal")
		}
	})

	t.Run("a simulation swapped in", func(t *testing.T) {
		h := newHarness(t, nil)
		h.advance(bootAnim + time.Second)
		h.n.SetSensors(NewStatic(Sensors{Battery: Battery{Volts: 3.1, Percent: 40}}), h.now)
		if !h.n.Power().Off() {
			t.Error("a source reporting a pack under the cutoff left the device running")
		}
	})

	t.Run("a clock", func(t *testing.T) {
		pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
		h := newHarness(t, func(c *Config) { c.Sensors = pack })
		h.advance(bootAnim + time.Second)
		pack.b = Battery{Volts: 3.1, Percent: 40}
		if err := h.n.SetClock(t0, h.now); err != nil {
			t.Fatal(err)
		}
		if !h.n.Power().Off() {
			t.Error("the reading taken with the clock was not acted on")
		}
	})
}

// TestTheLowBatteryReminderWaitsForTheRing: it is a reminder, and a boot
// animation, an alarm or a download is the picture someone is actually
// looking at. Owed rather than drawn over them, and not lost either.
func TestTheLowBatteryReminderWaitsForTheRing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.4, 8 })
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Fatalf("a device on a low pack came up showing %s, not its power-up ring", got)
	}
	h.advance(bootAnim + time.Second)
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("once the strip was free the ring showed %s", got)
	}

	// And a reminder owed while something else holds the ring is not
	// spent on it: the alarm keeps the strip, and the reminder follows.
	h = newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(8, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Errorf("the low-battery reminder took the ring from the alarm: %s", got)
	}
	h.n.SetSOS(false)
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("after the alarm ended the ring showed %s, and the reminder was owed", got)
	}
}

// TestTheRecordHoldsEveryNameAFrameCan: the two name limits are set in
// different packages — store.maxName bounds what SanitizeName leaves,
// mesh.MaxPeerName bounds what a status frame carries — and a name that
// passes the first and fails the second reaches mustMarshal, which
// panics in the middle of a transmit rather than returning an error.
// The sibling pair, maxBonds against store.MaxPeers, is pinned the same
// way; this one had only prose saying they agree.
func TestTheRecordHoldsEveryNameAFrameCan(t *testing.T) {
	long := strings.Repeat("n", 200)
	kept := store.SanitizeName(long)
	if len(kept) > mesh.MaxPeerName {
		t.Errorf("a name the settings keep is %d bytes and a peer frame holds %d",
			len(kept), mesh.MaxPeerName)
	}
	// And the frame really takes what survives, rather than the two
	// limits merely agreeing on paper.
	h := newHarness(t, func(c *Config) { c.Name = long })
	if _, err := h.n.status(h.now, mesh.PeerStatus, false).MarshalBinary(); err != nil {
		t.Errorf("the status frame refused this device's own name: %v", err)
	}
}
