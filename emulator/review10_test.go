package emulator

// Regressions for the tenth review round.

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestALocateExpiryNeverWraps: the expiry a locate carries is an int32 of
// Unix seconds, and the lifetime is added inside it. Near the ceiling the
// sum wrapped, and a negative expiry is one every receiver reads as long
// past — so the flood would have died at the first hop.
func TestALocateExpiryNeverWraps(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	// A clock inside the ceiling, but within the locate lifetime of it.
	near := maxClock.Add(-30 * time.Second)
	if err := h.n.SetClock(near, h.now); err != nil {
		t.Fatal(err)
	}
	if got := locateExpiry(near); got <= 0 {
		t.Errorf("a locate near the ceiling expires at %d, which is before the epoch", got)
	}
	h.take()
	h.advance(30 * time.Second)
	seen := 0
	for _, s := range h.take() {
		m, ok := s.msg.(mesh.Locate)
		if !ok {
			continue
		}
		seen++
		if m.Expiry <= 0 {
			t.Errorf("sent a locate expiring at %d", m.Expiry)
		}
	}
	if seen == 0 {
		t.Error("no locate went out, so the on-air half of this test proved nothing")
	}
}

// TestTheErrorRingIsPlayedAtTheRightTime: an update blocks the loop, so
// the time Update was entered on is not the time the node is on when it
// finishes. A download that ran for longer than the error ring's own four
// seconds left it already expired, and the next tick replaced it with
// idle before a frame was drawn.
func TestTheErrorRingIsPlayedAtTheRightTime(t *testing.T) {
	// Long enough to be measurable, short enough that the suite does not
	// wait for it: the property is where the animation was armed, not how
	// long the test takes.
	const slow = 200 * time.Millisecond
	h := newHarness(t, func(c *Config) {
		c.OTATransport = slowFailingTransport{delay: slow}
	})
	h.advance(bootAnim + time.Second)
	began := h.now
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("the update did not fail")
	}
	if got := h.n.LEDs().Animation(); got != AnimOTAFailed {
		t.Fatalf("the ring shows %s after a failure, want the error ring", got)
	}
	// The node's clock has not moved — Update blocked the loop — so the
	// ring has to have been armed for where the node will be when it next
	// polls, not for where it was when Update was entered.
	if l := h.n.LEDs(); !l.start.After(began) {
		t.Errorf("the error ring was armed at %v, the instant the update began", l.start)
	}
	if l := h.n.LEDs(); !l.until.After(began.Add(otaFailAnim)) {
		t.Errorf("the error ring expires at %v, within its own length of the start", l.until)
	}
}

// slowFailingTransport takes its time and then fails, as a stalled server
// or a reset connection does.
type slowFailingTransport struct{ delay time.Duration }

func (s slowFailingTransport) Post(string, []byte) ([]byte, error) {
	time.Sleep(s.delay)
	return nil, errSlowTransport
}
func (s slowFailingTransport) Get(string) ([]byte, error) { return nil, errSlowTransport }
func (s slowFailingTransport) Download(string, func(int64, int64)) (int64, string, error) {
	return 0, "", errSlowTransport
}

var errSlowTransport = errors.New("the server gave up")

// TestASavedPositionTimeIsChecked: the second a peer's position was seen
// is four bytes off the same flash sector as everything else, and store
// decodes it without a range check. One that is not a time a device could
// have seen gave a coordsAt centuries away — and a peer whose position is
// always fresh is one the mesh never asks about again.
func TestASavedPositionTimeIsChecked(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Restore(store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", Lat: 37.775, Lon: -122.42,
		LastSeenUnix: 1 << 62,
	}}}, h.now)

	p := h.n.peers[totem]
	if p == nil || !p.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := h.now.Sub(p.coordsAt); age < 0 {
		t.Errorf("the position is %v old, which is in the future", age)
	}
}

// TestBrightnessComesBackWhereItWas: the level is stored as one byte.
// Truncating meant 0.25 came back as 0.2470…, which counts as a change —
// so the strip redrew and the next save wrote a different byte again, a
// flash write for a level nobody touched.
func TestBrightnessComesBackWhereItWas(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.n.ToggleBrightness(h.now) // to the dim level
	was := h.n.LEDs().Brightness()

	st := h.n.State(1)
	fresh := newHarness(t, nil)
	fresh.n.Restore(st, fresh.now)

	// Within half a step of the byte it was stored as, which is the best
	// one byte can do. Truncating is a whole step out.
	if got := fresh.n.LEDs().Brightness(); math.Abs(got-was)*255 > 0.5 {
		t.Errorf("brightness came back as %v, want %v — %.2f of a step out",
			got, was, math.Abs(got-was)*255)
	}
	// And saving it again produces the same byte, so an idle device
	// writes nothing.
	if a, b := st.Brightness, fresh.n.State(1).Brightness; a != b {
		t.Errorf("the saved level drifted from %d to %d across one restore", a, b)
	}
}

// TestVoltsAloneGiveAPercentage: a real power chip reports a cell
// voltage, and get_batt_pct is what makes a percentage of it. A source
// that gives one and not the other left every reader — the status frame,
// the power mode, the OTA gate — looking at a battery of 0%.
func TestVoltsAloneGiveAPercentage(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 4.0}})
	})
	h.collect(h.n.Poll(h.now))
	if got := h.n.Sensors().Battery.Percent; got < 80 || got > 92 {
		t.Errorf("4.0 V read as %d%%, want about 86%% from the curve", got)
	}
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Errorf("a battery at 4.0 V put the device in power mode %s", got)
	}
}

// TestTheLearnedMaxVoltsSurvivesAReboot: device_power learns the highest
// voltage this pack reaches, and that is a fact about the battery rather
// than about the run. It was encoded in every saved record and never set
// by anything.
func TestTheLearnedMaxVoltsSurvivesAReboot(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.19, 100 })
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().LearnedMaxVolts(); got < 4.18 {
		t.Fatalf("the power model learned %v, want the 4.19 it was shown", got)
	}
	st := h.n.State(1)
	if st.LearnedMaxVolts < 4.18 {
		t.Errorf("the state saved %v", st.LearnedMaxVolts)
	}

	fresh := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.8, 50 })
	fresh.n.Restore(st, fresh.now)
	if got := fresh.n.Power().LearnedMaxVolts(); got < 4.18 {
		t.Errorf("after a reboot the power model has %v, want what it learned", got)
	}
}

// TestConfigCannotWidenTheOwnedScope: Config() looks like a read, and the
// owned list is the one invariant this package promises to keep. Handing
// out the same backing array let a caller add a Totem its owner never
// listed.
func TestConfigCannotWidenTheOwnedScope(t *testing.T) {
	h := newHarness(t, nil)
	cfg := h.n.Config()
	if len(cfg.Owned) == 0 {
		t.Fatal("no owned Totems to test with")
	}
	cfg.Owned[0] = stranger
	if slices.Contains(h.n.Config().Owned, stranger) {
		t.Error("a caller widened the owned scope through Config()")
	}
	if err := h.n.AddBond(stranger, "nope", h.now); err == nil {
		t.Error("the node bonded with a Totem outside its owned scope")
	}
}
