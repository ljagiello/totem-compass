package emulator

// Regressions for the seventh review round.

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestOurOwnFixIsCheckedToo: four ingress paths got the "could a device
// be there" guard and the fifth — this device's own receiver — did not,
// which is the one position it puts on the air. A NaN reached the
// compass, where converting a bearing to an int16 is implementation
// defined, so it pointed confidently north; it reached the distance to
// every peer; and it went out in every status frame.
func TestOurOwnFixIsCheckedToo(t *testing.T) {
	for _, bad := range []struct {
		name     string
		lat, lon float32
	}{
		{"nan", float32(math.NaN()), float32(math.NaN())},
		{"infinite", float32(math.Inf(1)), -122.42},
		{"past the pole", 900, -122.42},
	} {
		h := newHarness(t, func(c *Config) {
			c.Position = &Position{Lat: bad.lat, Lon: bad.lon, AccuracyM: 3}
		})
		h.bond()
		h.rx(totem, self, -40, statusFrame(t0))
		h.advance(bootAnim + time.Second)

		if h.n.fix() != nil {
			t.Errorf("%s: the node believes it is somewhere", bad.name)
		}
		if deg := h.n.LEDs().dial; deg >= 0 {
			t.Errorf("%s: the compass points at %d", bad.name, deg)
		}
		for _, p := range h.n.Peers() {
			if math.IsNaN(p.DistanceM) || math.IsInf(p.DistanceM, 0) {
				t.Errorf("%s: a peer is %v away", bad.name, p.DistanceM)
			}
		}
		// And nothing impossible goes out on the air.
		for _, s := range h.take() {
			// usablePosition, which is the rule fix() applies: Null Island
			// is the firmware's own way of saying it has no fix, so a
			// frame carrying it is a frame carrying no position. The
			// earlier spelling paired livePosition with a zero check that
			// could never change the answer.
			if m, ok := s.msg.(mesh.Peer); ok && !usablePosition(m.Lat, m.Lon) && (m.Lat != 0 || m.Lon != 0) {
				t.Errorf("%s: broadcast a position of %v, %v", bad.name, m.Lat, m.Lon)
			}
		}
	}

	// And the exported setter refuses one outright, rather than storing
	// something every reader then has to ignore.
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 999, Lon: -999}, h.now); err == nil {
		t.Error("SetPosition accepted a place no device could be")
	}
}

// TestAConfiguredColourIsChecked: the board reads the crystal colour off
// a flash sector and hands it straight to New, so a corrupt id arrives
// before Restore ever runs — and Restore "keeping the default" would keep
// the corrupt one. An unlit crystal for good, from one bad byte, written
// back to flash on the next save.
func TestAConfiguredColourIsChecked(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.ColorID = 120
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d from the configuration", int(got))
	}
	if got := h.n.Config().ColorID; Color(got).InPalette() == false {
		t.Errorf("the configuration kept colour %d", got)
	}
	if !strings.Contains(logged.String(), "thirteen") {
		t.Errorf("nothing was said about it: %s", logged.String())
	}
	// And it is not written back to flash.
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved")
	}
}

// TestASmartGroupCannotBlankTheCrystal: the colour a group assigns is one
// byte off the air from the host. An id outside the thirteen renders
// unlit, and State writes it to flash, so one garbled frame would leave
// the crystal dark past the next reboot.
func TestASmartGroupCannotBlankTheCrystal(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.collect(h.n.Pair(h.now))
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 20000,
		Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: self}},
	})
	if !h.n.inGroup {
		t.Fatal("the node did not join the group")
	}
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupFinalize, UID: 7,
		Members: []mesh.SmartGroupMember{{MAC: self, ColorID: 120}, {MAC: totem, ColorID: 4}},
	})
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the group set the crystal to colour %d", int(got))
	}
	if got := h.n.Config().ColorID; !Color(got).InPalette() {
		t.Errorf("the group set the configuration to colour %d", got)
	}
}

// TestABatteryReadingKeepsTheBoardsShape: NoPowerChip describes the
// board, not the reading. Rebuilding the battery struct without it
// silently re-armed the OTA gate on a board that has nothing to gate.
func TestABatteryReadingKeepsTheBoardsShape(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NoPowerChip, c.BattVolts, c.BattPct = true, 0, 0 })
	h.collect(h.n.Poll(h.now))
	if err := h.n.Update(h.now); errors.Is(err, ErrBatteryLow) {
		t.Fatal("a board with no power chip was gated before any reading")
	}
	// A reading arrives — the console batt command, or a driver.
	if err := h.n.SetBattery(20, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if !h.n.Sensors().Battery.NoPowerChip {
		t.Error("a battery reading erased the fact that the board has no power chip")
	}

	// And a source of its own does not get to decide either.
	h = newHarness(t, func(c *Config) {
		c.NoPowerChip = true
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 3.6, Percent: 20}})
	})
	h.collect(h.n.Poll(h.now))
	if !h.n.Sensors().Battery.NoPowerChip {
		t.Error("a sensor source of its own overrode the board's own shape")
	}
}

// TestAPoweredDownStripRemembersNothing: a dark strip shows nothing, so
// there is nothing to play on it and nothing to remember having played.
// Guarding only the reader left the fields saying one thing and Next
// another, and `leds` reported a powered-down board as showing an update
// failure for ever.
func TestAPoweredDownStripRemembersNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.PowerOff(h.now)
	was := h.n.LEDs().Describe()

	// The refusal that flashes the ring.
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("an update on a powered-down device was allowed")
	}
	if got := h.n.LEDs().Animation(); got != AnimIdle {
		t.Errorf("a dark strip is playing %s", got)
	}
	if got := h.n.LEDs().Describe(); got != was {
		t.Errorf("a dark strip changed from %q to %q", was, got)
	}
	if got := h.n.LEDs().Next(); !got.IsZero() {
		t.Errorf("a dark strip asked to be woken at %v", got)
	}
	// Every pixel is still off.
	for i, p := range h.n.LEDs().Ring() {
		if p != Off {
			t.Fatalf("ring pixel %d is lit on a powered-down device: %v", i, p)
		}
	}
}

// TestAPendingTapDoesNotSurvivePowerOff: a device that is off asks for
// nothing. A tap still inside its multi-tap window kept a deadline alive
// on a device whose only answer was to throw the gesture away.
func TestAPendingTapDoesNotSurvivePowerOff(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.n.Tap(Crystal, 1, h.now)
	h.n.PowerOff(h.now)
	if got := h.n.Next(); !got.IsZero() {
		t.Errorf("a powered-down device wants waking at %v", got)
	}
	// And the power button still works: the wait is cleared, not re-armed.
	h.collect(h.n.HoldFor(PowerButton, holdTime+100*time.Millisecond, h.now))
	if h.n.Power().Off() {
		t.Error("holding power did not bring the device back")
	}
}

// TestAShortHoldSaysSo: the console takes a hold of 1 ms, and nothing
// registers — which is right, but silence looks the same as the console
// having missed the line.
func TestAShortHoldSaysSo(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	h.collect(h.n.HoldFor(PowerButton, time.Millisecond, h.now))
	if !strings.Contains(logged.String(), "too short") {
		t.Errorf("a hold too short to register said nothing: %s", logged.String())
	}
}

// TestTheChargerRingGoesStale: the ring says the charger has just gone
// in. Owed indefinitely, it would play ten minutes into an alarm as if
// the cable had only then been connected.
func TestTheChargerRingGoesStale(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.SetSOS(true)
	h.collect(h.n.Poll(h.now))
	if err := h.n.SetBattery(50, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Long past the point where the announcement would still be true.
	for end := h.now.Add(chargeAnnounce + time.Minute); h.now.Before(end); h.now = h.now.Add(time.Second) {
		h.collect(h.n.Poll(h.now))
	}
	h.n.SetSOS(false)
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatalf("the charger ring played %s after the charger went in", chargeAnnounce+time.Minute)
		}
	}
}

// TestAPeerColourOfRedIsLeftAlone: the peer guard and the crystal guard
// read the same flash sector, so they follow the same rule. 0 is red and
// also the zero value, so a saved 0 leaves the colour the bond drew.
func TestAPeerColourOfRedIsLeftAlone(t *testing.T) {
	h := newHarness(t, nil)
	h.n.Restore(&store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", ColorID: 0,
	}}}, h.now)
	got := h.n.peers[totem].color
	if !got.InPalette() {
		t.Errorf("restored colour %d", int(got))
	}
	mac := h.n.Config().MAC
	if want := BondColor(0, uint32(mac[2])<<24|uint32(mac[3])<<16|uint32(mac[4])<<8|uint32(mac[5])); got != want {
		t.Errorf("a saved colour of 0 gave %s, want the colour the bond drew, %s", got, want)
	}
}
