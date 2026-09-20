package emulator

// Regressions for the eighth review round.

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestOnlyTheBoardSaysItHasNoPowerChip: the flag describes the board, so
// the configuration decides it both ways. Forcing it on but never off let
// a sensor source claim there was no battery monitor and turn the OTA
// gate off on a device with a flat cell — the opposite of the guarantee.
func TestOnlyTheBoardSaysItHasNoPowerChip(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		// No flag: this board has a power chip.
		c.Sensors = NewStatic(Sensors{Battery: Battery{
			Volts: 3.4, Percent: 2, Low: true, NoPowerChip: true,
		}})
	})
	h.collect(h.n.Poll(h.now))
	if h.n.Sensors().Battery.NoPowerChip {
		t.Error("a sensor source talked the node out of its power chip")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("an update on a flat cell was refused with %v, want %v", err, ErrBatteryLow)
	}
}

// TestNullIslandIsNoFix: 0,0 is how the firmware says it has no solution.
// Treated as a place, it stops the search animation, points the compass
// on a bearing measured from the Gulf of Guinea, and puts distances of
// thousands of kilometers into the mesh timing.
func TestNullIslandIsNoFix(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.rx(totem, self, -40, statusFrame(t0))
	// Past New and SetPosition, both of which refuse this themselves: a
	// receiver driver reports through the sensor source, so that is where
	// a fix of 0,0 actually arrives.
	h.n.SetSensors(NewStatic(Sensors{
		Fix:         &Fix{Lat: 0, Lon: 0, AccuracyM: 3},
		Orientation: mesh.OrientationVertical,
	}), h.now)
	h.advance(bootAnim + time.Second)

	if h.n.fix() != nil {
		t.Error("the node believes Null Island is a place")
	}
	if got := h.n.LEDs().Animation(); got != AnimGNSSSearch {
		t.Errorf("the ring shows %s, want the search for a fix", got)
	}
	if deg := h.n.LEDs().dial; deg >= 0 {
		t.Errorf("the compass points at %d", deg)
	}
	for _, p := range h.n.Peers() {
		if p.DistanceM > 1_000_000 {
			t.Errorf("a peer is %.0f m away, measured from Null Island", p.DistanceM)
		}
	}
}

// TestAClockNeedsAPlace: a receiver whose position the node refuses has
// not earned its clock either — that clock is advertised to every peer
// and re-slots every radio window.
func TestAClockNeedsAPlace(t *testing.T) {
	h := newHarness(t, nil)
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{
			Lat: float32(math.NaN()), Lon: float32(math.NaN()),
			AccuracyM: 3, Time: t0,
		},
		Orientation: mesh.OrientationVertical,
	}), h.now)
	h.advance(time.Second)
	if h.n.gnssClock {
		t.Error("took a GNSS clock from a receiver that cannot say where it is")
	}
}

// TestAConfiguredPositionIsChecked: New validates the name and the
// colour; a position no device could be at was stored anyway, and then
// silently ignored by every reader — a device searching for a fix for
// ever with nothing in the log to say why.
func TestAConfiguredPositionIsChecked(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 999, Lon: -999}
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	if h.n.fix() != nil {
		t.Error("the node kept a position no device could be at")
	}
	if !bytes.Contains(logged.Bytes(), []byte("configured position")) {
		t.Errorf("nothing was said about it: %s", logged.String())
	}
}

// TestAHoldOfExactlyTheLockoutCounts: the "too short to register" notice
// used <=, and release() uses <. At exactly the lockout the console was
// told nothing had happened and then the gesture fired — on the power
// button, the device dimmed.
func TestAHoldOfExactlyTheLockoutCounts(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	full := h.n.LEDs().Brightness()

	h.collect(h.n.HoldFor(PowerButton, edgeLockout, h.now))
	h.advance(multiTapWindow + time.Second)

	dimmed := h.n.LEDs().Brightness() != full
	said := bytes.Contains(logged.Bytes(), []byte("too short"))
	if said && dimmed {
		t.Error("the console was told nothing happened, and then the brightness changed")
	}
}

// TestSetColorIsCheckedLikeTheRest: New, Restore and the Smart Group all
// check a colour id; SetColor is reachable from a driver or an embedding
// caller and was the one that did not. State writes it to flash.
func TestSetColorIsCheckedLikeTheRest(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ColorID = int8(ColorTeal) })
	h.n.SetColor(Color(120), h.now)
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d", int(got))
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorTeal {
		t.Errorf("the crystal is %s, want the colour it had", got)
	}
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved to flash")
	}
}

// TestNoPowerChipIsNotAFlatBattery: a board with no battery monitor
// reports zeroes. Read as a reading, they drove it into PowerLow, which
// goes out in every status frame and flashes the low-battery ring at
// someone whose device cannot go flat.
func TestNoPowerChipIsNotAFlatBattery(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NoPowerChip, c.BattVolts, c.BattPct = true, 0, 0 })
	h.collect(h.n.Poll(h.now))
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Errorf("a board with no power chip is in power mode %s", got)
	}
	if h.n.Power().Off() {
		t.Error("a board with no power chip powered itself down")
	}
}

// TestADarkStripComesBackAtNow: Dark(false) armed the frame deadline from
// the instant the device went down, so the first tick measured the whole
// off-duration in frames and started the animation mid-sequence.
func TestADarkStripComesBackAtNow(t *testing.T) {
	l := newLEDs(t0, ColorTeal)
	l.Tick(t0)
	l.Dark(true, t0)

	// No Play afterwards: PowerOn happens to call one, and that is what
	// hid this. A caller of the exported Dark gets whatever this leaves.
	later := t0.Add(2 * time.Hour)
	l.Dark(false, later)
	if l.next.Before(later) {
		t.Errorf("a frame is due at %v, %v before the strip came back",
			l.next, later.Sub(l.next))
	}
	l.Tick(later)
	if l.frame != 0 {
		t.Errorf("the strip came back at frame %d, two hours of frames in", l.frame)
	}
}

// TestSimStartsFromAPositionWeWouldUse: the simulation read the raw fix
// rather than the one the node will actually use, so a garbled configured
// position started a walk from a NaN — and every reading after it was NaN
// too, while `sim` cheerfully reported a walk in progress.
func TestSimStartsFromAPositionWeWouldUse(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: float32(math.NaN()), Lon: float32(math.NaN()), AccuracyM: 3}
	})
	h.n.StartSim(Walk, 90, h.now)
	h.advance(10 * time.Second)
	if f := h.n.fix(); f != nil && !usablePosition(f.Lat, f.Lon) {
		t.Errorf("the simulation is walking from %v, %v", f.Lat, f.Lon)
	}
}
