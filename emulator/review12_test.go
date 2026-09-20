package emulator

// Regressions for the twelfth review round.

import (
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestAnExpiredLocateIsNotImmortal: relay() reads an expiry of zero as
// "none set" and skips the age test, so a frame stamped zero is relayed
// for ever — the opposite of what stamping it was for. A board whose
// clock has not started produces exactly that.
func TestAnExpiredLocateIsNotImmortal(t *testing.T) {
	if got := locateExpiry(time.Time{}); got == 0 {
		t.Error("a pre-epoch clock stamped the frame with no expiry at all")
	}
	if got := locateExpiry(time.Time{}); got < 0 {
		t.Errorf("a pre-epoch clock stamped the frame %d", got)
	}

	// And such a frame is refused by the relay, not carried.
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.take()
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4242, ReplyRequested: false,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(time.Time{}),
	})
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Locate); ok && m.Origin == totem {
			t.Errorf("relayed a frame that expired in 1970: %+v", m)
		}
	}
}

// TestNewTakesItsOwnCopies: Config() and Sensors() hand out copies, but
// the way in was the same hole — a caller that kept the slice or the
// pointer it passed to New could widen the owned scope, or write a
// position, afterwards.
func TestNewTakesItsOwnCopies(t *testing.T) {
	owned := []mesh.MAC{totem}
	pos := &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}
	h := newHarness(t, func(c *Config) { c.Owned, c.Position = owned, pos })

	owned[0] = stranger
	if got := h.n.Config().Owned; got[0] == stranger {
		t.Error("a caller widened the owned scope after New")
	}
	if err := h.n.AddBond(stranger, "nope", h.now); err == nil {
		t.Error("the node bonded with a Totem outside its owned scope")
	}

	pos.Lat = float32(math.NaN())
	if f := h.n.fix(); f == nil || math.IsNaN(float64(f.Lat)) {
		t.Error("a caller wrote a position through the Config it passed to New")
	}
}

// TestAStaleSavedMaximumDoesNotUnstretchTheCurve: `store open` on a
// running device reads a record written before this run measured a
// higher voltage. Taking it as it stands would throw away what the
// device learned an hour ago.
func TestAStaleSavedMaximumDoesNotUnstretchTheCurve(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.collect(h.n.Poll(h.now))
	learned := h.n.Power().LearnedMaxVolts()
	if learned < 4.24 {
		t.Fatalf("this run learned %v, want 4.25", learned)
	}
	h.n.Restore(store.State{LearnedMaxVolts: 4.15}, h.now)
	if got := h.n.Power().LearnedMaxVolts(); got != learned {
		t.Errorf("a stale saved value moved the learned maximum to %v, want %v", got, learned)
	}
}

// TestRestoreTakesAFreshReading: the learned maximum changes what a
// voltage means, so a restore that installs one and leaves the last
// reading alone leaves `status`, the next status frame and the OTA gate
// all looking at a percentage worked out without it.
func TestRestoreTakesAFreshReading(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.15, 0 })
	h.collect(h.n.Poll(h.now))
	if got := h.n.Sensors().Battery.Percent; got != 100 {
		t.Fatalf("4.15 V with no learned maximum reads %d%%, want 100%%", got)
	}
	h.n.Restore(store.State{LearnedMaxVolts: 4.3}, h.now)
	if got := h.n.Sensors().Battery.Percent; got == 100 {
		t.Error("the battery still reads 100% after a restore that stretched the curve")
	}
}

// TestAFactoryResetForgetsThePack: the learned maximum describes a
// battery. A reset that left it behind would keep stretching the curve
// for a pack the device is being told it never met.
func TestAFactoryResetForgetsThePack(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.collect(h.n.Poll(h.now))
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Fatal("nothing was learned to forget")
	}
	h.n.ForgetPeers(h.now)
	if got := h.n.Power().LearnedMaxVolts(); got != 0 {
		t.Errorf("a factory reset left the learned maximum at %v", got)
	}
}
