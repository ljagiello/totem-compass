package emulator

// Regressions for the twentieth review round.

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestARecordThatSaysNothingIsStillARecord: Restore used to work out
// whether the flash held anything by looking at the State's fields, and
// a device saved with no name, no bonds, the default colour and the
// brightness left alone reads as blank that way — which is exactly what
// a device someone has only ever switched on and muted writes. Its mute
// was then dropped at every boot.
func TestARecordThatSaysNothingIsStillARecord(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)

	// The record a device that has only been muted writes: everything
	// else is still the default.
	rec := store.State{SOSMuted: true}
	h.n.Restore(&rec, h.now)
	if !h.n.SOSMuted() {
		t.Error("a saved mute was read as an empty record and dropped")
	}
	// No record at all is TestABlankRecordIsNotSettings, in review19.
}

// TestABuiltInNameThatLooksLikeTheDefault: the guard was `n.cfg.Name ==
// DefaultName(MAC)`, which asks the answer rather than the question. A
// board deliberately flashed with the name it would have been given
// anyway had chosen that name, and the saved one overwrote it.
func TestABuiltInNameThatLooksLikeTheDefault(t *testing.T) {
	built := DefaultName(self)
	h := newHarness(t, func(c *Config) { c.Name = built })
	h.n.Restore(&store.State{Name: "lcfs_old_spare"}, h.now)
	if got := h.n.Config().Name; got != built {
		t.Errorf("the saved name overwrote the one the board was flashed with: %q", got)
	}
}

// TestALastHopThatCannotBePlaced: the minimum-distance rule compares a
// distance against RelayMinDistM, and every comparison with NaN is
// false. The frames whose last-hop position is unreadable are the least
// worth trusting, and they were the ones the rule was deciding about on
// a number that meant nothing. An unplaceable hop is no hop at all now,
// which is what the firmware's own Null Island says, and the rule still
// bites on a hop that can be placed.
func TestALastHopThatCannotBePlaced(t *testing.T) {
	// Where this node is, and a rule that suppresses a relay within 30 km.
	const lat, lon = 37.7749, -122.4194
	relayed := func(t *testing.T, hopLat, hopLon float32) bool {
		t.Helper()
		h := newHarness(t, func(c *Config) {
			c.Position = &Position{Lat: lat, Lon: lon, AccuracyM: 3}
		})
		h.bond()
		// onLocate leaves the mesh alone until the node has a clock.
		if err := h.n.SetClock(t0, h.now); err != nil {
			t.Fatal(err)
		}
		h.take()
		h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
			Origin: totem, UID: 4242, ReplyRequested: true,
			MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
			Expiry:        locateExpiry(h.n.wall(h.now)),
			RelayMinDistM: 30_000,
			LastHopLat:    hopLat, LastHopLon: hopLon,
		})
		return relayCount(h.take()) > 0
	}

	// The rule itself: a hop a few streets away, well inside 30 km.
	if relayed(t, lat+0.01, lon+0.01) {
		t.Error("relayed a frame whose last hop was next door, past a rule that says not to")
	}
	// And one on the far side of the state is relayed, or the rule is a
	// refusal rather than a rule.
	if !relayed(t, lat+5, lon+5) {
		t.Error("refused to relay a frame whose last hop was hundreds of kilometers away")
	}

	// A hop that cannot be placed is not measured against the rule at
	// all: nothing that is not a number reaches the comparison, and the
	// frame travels as it would from a Totem with no fix.
	for _, tc := range []struct {
		name     string
		lat, lon float32
	}{
		{"not a number", float32(math.NaN()), float32(math.NaN())},
		{"one half of one", lat, float32(math.NaN())},
		{"off the globe", 400, 900},
		{"null island", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !relayed(t, tc.lat, tc.lon) {
				t.Error("a last hop that cannot be placed was read as one that is too close")
			}
		})
	}
}

// TestADistanceThatCannotBeMeasured: distance() is a helper anything in
// this file can reach, and Go's min returns NaN if either argument is
// one — so the clamp on the haversine intermediate never removed one.
// -1 is what the rest of the file means by "no distance".
func TestADistanceThatCannotBeMeasured(t *testing.T) {
	nan := float32(math.NaN())
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{nan, 0, 0, 0},
		{0, nan, 0, 0},
		{0, 0, nan, 0},
		{0, 0, 0, nan},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) {
			t.Errorf("distance(%v,%v -> %v,%v) is not a number",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		} else if got >= 0 {
			t.Errorf("distance(%v,%v -> %v,%v) = %v, which reads as a real distance",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
	}
}

// TestACableThatNeverMovedIsNotAnnouncedAgain: the driver keeps polling
// while the device is down, which is how the power button works, and
// those polls are what mark a connection as already announced. A day on
// the same cable must not produce a second ring, and the debounce must
// not be restarted into one either.
func TestACableThatNeverMovedIsNotAnnouncedAgain(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(70, true, h.now); err != nil {
		t.Fatal(err)
	}
	// The connection announces itself once, while the device is on.
	h.advance(2 * time.Second)

	h.n.PowerOff(h.now)
	// A day down, polled throughout, exactly as the loop does.
	for end := h.now.Add(24 * time.Hour); h.now.Before(end); h.now = h.now.Add(time.Second) {
		h.collect(h.n.Poll(h.now))
	}
	h.n.PowerOn(h.now)
	h.advance(bootAnim + time.Second)
	for end := h.now.Add(chargeAnnounce + time.Second); h.now.Before(end); h.now = h.now.Add(10 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatal("a cable that never moved played the charger ring again")
		}
	}
}

// TestADownloadDoesNotOpenOnTheLastOnesRing: the progress ring holds the
// share of the download before it, so an update starting after one that
// reached 80% opened with four fifths of the ring lit for a file it had
// not asked for.
func TestADownloadDoesNotOpenOnTheLastOnesRing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.OTATransport = nil })
	h.advance(bootAnim + time.Second)
	h.n.LEDs().Play(AnimOTA, h.now)
	h.n.LEDs().SetProgress(0.8)
	h.n.LEDs().Tick(h.now)
	if litPixels(h.n.LEDs()) == 0 {
		t.Fatal("the ring did not show the first download at all")
	}

	// No transport, so this fails at the first step — after the reset.
	_ = h.n.Update(h.now)
	h.n.LEDs().Play(AnimOTA, h.now)
	h.n.LEDs().Tick(h.now)
	if got := litPixels(h.n.LEDs()); got != 0 {
		t.Errorf("a new download opened with %d pixels of the last one's lit", got)
	}
}

// TestAnUpdateOnAFlatPackPowersDown: Update takes a fresh reading before
// the battery gate, and read() leaves the node holding a reading nothing
// has acted on. Every other caller pairs the two — a pack under the
// cutoff has to power the device down, not merely lose it an update —
// and this one did not, so a device that asked for an update on a dying
// pack carried on as though the pack were fine until the next poll.
func TestAnUpdateOnAFlatPackPowersDown(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if h.n.Power().Off() {
		t.Fatal("the device was down before the test started")
	}

	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	if v := h.n.Sensors().Battery.Volts; v > cutoffVolts {
		t.Fatalf("a flat pack reads as %v V, above the cutoff of %v", v, cutoffVolts)
	}
	// Acted on where it was read, which is what read() does now: the
	// device is down before anything asks it for an update.
	if !h.n.Power().Off() {
		t.Error("a pack below the cutoff was read and then left running")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrPoweredDown) {
		t.Fatalf("update on a flat pack: %v", err)
	}

	// And the gate itself, on a pack that is low and still alive: under
	// the 30% an update needs, above the cutoff that stops everything.
	h = newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(10, false, h.now); err != nil {
		t.Fatal(err)
	}
	if h.n.Power().Off() {
		t.Fatal("a pack at 10 percent switched the device off")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("update on a pack at 10 percent: %v", err)
	}
}

// TestTheRecordHoldsEveryBondTheNodeCan: State's truncation cannot bind
// while a node holds fewer bonds than a record holds peers, and the only
// thing between a breach of that and a device that silently stops saving
// is MarshalBinary's error. This pins the relationship rather than the
// branch: it is what has to stay true, and what someone raising the bond
// limit would otherwise not be told.
func TestTheRecordHoldsEveryBondTheNodeCan(t *testing.T) {
	if maxBonds > store.MaxPeers {
		t.Errorf("a node holds %d bonds and a saved record holds %d peers: "+
			"every bond past the record's limit would stop the device saving at all",
			maxBonds, store.MaxPeers)
	}
}
