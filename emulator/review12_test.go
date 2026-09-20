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

	// And such a frame is refused by the relay, not carried. It has to
	// ask for a reply: onLocate returns before relay() for one that does
	// not, so a frame without the flag proves nothing about the relay.
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.take()
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4242, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(time.Time{}),
	})
	if got := relayCount(h.take()); got != 0 {
		t.Errorf("relayed %d copies of a frame that expired in 1970", got)
	}

	// And one stamped exactly zero, which is what relay() used to read as
	// "no expiry set" — a frame off the air can carry it however
	// carefully this node stamps its own.
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4244, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: 0,
	})
	if got := relayCount(h.take()); got != 0 {
		t.Errorf("relayed %d copies of a frame carrying no expiry at all", got)
	}

	// The same frame with a live expiry is relayed, which is what shows
	// the two above were refused for their expiry and not ignored for
	// some other reason.
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4243, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(t0),
	})
	if got := relayCount(h.take()); got == 0 {
		t.Error("a frame with a live expiry was not relayed either, so the test proves nothing")
	}
}

// relayCount is how many of these frames carry another Totem's origin,
// which is what a relay is.
func relayCount(sent []sent) int {
	n := 0
	for _, s := range sent {
		if m, ok := s.msg.(mesh.Locate); ok && m.Origin == totem {
			n++
		}
	}
	return n
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

	// A poll, so the node takes a fresh reading: without one the fix it
	// answers with is a copy staticSensors made during New, and the
	// caller's write could not have reached it either way.
	pos.Lat = float32(math.NaN())
	h.collect(h.n.Poll(h.now))
	if f := h.n.fix(); f == nil || math.IsNaN(float64(f.Lat)) {
		t.Error("a caller wrote a position through the Config it passed to New")
	}
	if got := h.n.Config().Position; got == nil || math.IsNaN(float64(got.Lat)) {
		t.Error("the node's own configured position was written from outside")
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
// for a pack the device is being told it never met — and it has to take
// a fresh reading with it, or the percentage worked out with the
// forgotten stretch stands until the next poll and then jumps.
func TestAFactoryResetForgetsThePack(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.collect(h.n.Poll(h.now))
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Fatal("nothing was learned to forget")
	}
	// Dropping bonds is not a reset: the name promises the bonds, and a
	// caller that wants those should not lose the battery calibration.
	h.n.ForgetPeers(h.now)
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Error("ForgetPeers threw away the battery calibration")
	}

	// The pack is swapped for one that peaks lower, and the device is
	// told to forget what it knew. What a reset throws away is the
	// history: it keeps whatever the present reading says, because that
	// is a measurement and not a memory.
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Sensors() // no-op read, so the harness state is settled
	if err := h.n.SetPosition(nil, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.SetSensors(NewStatic(Sensors{Battery: Battery{Volts: 4.15, Percent: 100}}), h.now)
	h.n.FactoryReset(h.now)
	// The reset drops the memory and the reading that follows measures
	// the pack that is actually there — so what is left is the new pack,
	// never the old one.
	got := h.n.Power().LearnedMaxVolts()
	if got == 0 {
		t.Error("nothing was learned from the pack that is there")
	}
	if got > 4.16 {
		t.Errorf("a reset kept %v, which is the pack it was told to forget", got)
	}
}
