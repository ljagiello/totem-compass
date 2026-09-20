package emulator

// Regressions for the thirty-seventh review round, which found these in
// code older than this session's work rather than in its fixes.

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestAnOdometerHasBothEnds: the field on the air is an int16 and the
// reading is an int32, so a negative odometer past -32768 wrapped into a
// large positive distance — a device reporting -100000 told its peers it
// had traveled 31 km. The uptime beside it was given both ends; this
// had only the top.
func TestAnOdometerHasBothEnds(t *testing.T) {
	for _, m := range []int32{0, 1, 32767, 100000, -1, -100000, math.MinInt32, math.MaxInt32} {
		h := newHarness(t, func(c *Config) {
			c.Sensors = NewStatic(Sensors{
				Fix:     &Fix{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3, OdometerM: m},
				Battery: Battery{Volts: 4.1, Percent: 90},
			})
		})
		got := h.n.status(h.now, mesh.PeerStatus, false).OdometerM
		if got < 0 {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
		if m >= 0 && m <= math.MaxInt16 && int32(got) != m {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
	}
}

// TestTheConsoleTakesTheLongestFrameItAccepts: the rx command bounds
// what it injects by mesh.MaxFrame, and the line limit was sized for a
// 108-byte peer frame — so the largest frames the validator accepts were
// refused first, as a line too long, by a limit whose own comment
// described something shorter.
func TestTheConsoleTakesTheLongestFrameItAccepts(t *testing.T) {
	// A line carrying the longest frame rx will take.
	hex := strings.Repeat("ab", mesh.MaxFrame)
	line := "rx 8c94df7b0478 all -25 " + hex
	if len(line) > MaxCommandLine {
		t.Fatalf("the longest injectable frame is %d characters and the console takes %d",
			len(line), MaxCommandLine)
	}
	// It is refused for what it is, not for how long it is.
	_, err := ParseCommand(line)
	if err != nil && strings.Contains(err.Error(), "too long") {
		t.Errorf("the longest injectable frame was refused as too long: %v", err)
	}
}

// TestAVoltageTheFrameCannotCarry: the field is a half float and the
// encoder is a port with no range check of its own, so a voltage past
// its limit runs off the exponent into the sign bit. The emulator
// guards its own readings; mesh is an exported package, and its encoder
// refused an over-long name while taking any voltage at all.
func TestAVoltageTheFrameCannotCarry(t *testing.T) {
	for _, v := range []float32{
		100000, 131072, 1e6, float32(math.NaN()), float32(math.Inf(1)), -5,
	} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err == nil {
			t.Errorf("a battery of %v V was encoded", v)
		}
	}
	// And every voltage a cell reads still encodes.
	for _, v := range []float32{0, 3.1, 4.1, 4.48, 4.6} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err != nil {
			t.Errorf("a battery of %v V was refused: %v", v, err)
		}
	}
}

// TestTheChargerDebounceRunsWhileTheRingIsBusy: charging() is the only
// writer of when the cable went in, and it used to be the first operand
// of an && chain — so the debounce kept running only because of the
// order the operands happened to be in. Plugging in during an alarm is
// the case that breaks when someone tidies that.
func TestTheChargerDebounceRunsWhileTheRingIsBusy(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)

	// The alarm owns the strip, and the cable goes in underneath it.
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(70, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Long enough that a debounce started now would have finished.
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Fatalf("the alarm lost the ring to something else: %s", got)
	}

	// The alarm ends, and the connection is announced — which needs the
	// debounce to have been running the whole time it was held back.
	h.n.SetSOS(false)
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Errorf("the ring shows %s: the charger was never announced", got)
	}
}
