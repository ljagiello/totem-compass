package emulator

// The battery curve against the firmware's own arithmetic.

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestBattPctMatchesTheFirmware walks testdata/battcurve.txt, which holds
// (max_volts, cur_volts, percent) triples computed by running
// peripherals.get_batt_pct's algorithm over the breakpoint table decoded
// out of the v5.0.3 bytecode — the 232-byte constant in peripherals.dis,
// 58 little-endian pairs of millivolts and raw level.
//
// Every voltage from 3.00 to 4.70 at a hundredth, against nine learned
// maxima, including the two that matter: none learned yet, and one above
// the table's top, which is where the old six-point curve disagreed.
func TestBattPctMatchesTheFirmware(t *testing.T) {
	b, err := os.ReadFile("testdata/battcurve.txt")
	if err != nil {
		t.Fatal(err)
	}

	var checked int
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("testdata line %q", line)
		}
		top, err := strconv.ParseFloat(fields[0], 32)
		if err != nil {
			t.Fatal(err)
		}
		v, err := strconv.ParseFloat(fields[1], 32)
		if err != nil {
			t.Fatal(err)
		}
		want, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatal(err)
		}
		if want < 0 || want > 100 {
			t.Fatalf("the golden file says %d%% at %.2f V (max %.2f), which is not a "+
				"percentage — get_batt_pct clamps at both ends", want, v, top)
		}
		if got := battPctFor(float32(v), float32(top)); int(got) != want {
			t.Errorf("battPctFor(%.2f, top %.2f) = %d, the firmware says %d",
				v, top, got, want)
		}
		checked++
	}
	if checked < 1000 {
		t.Fatalf("only %d points checked; the golden file looks truncated", checked)
	}
}

// TestAPackAboveTheTableReadsFull is the case that changed, stated on its
// own because it is the one a Totem is sitting next to this one
// demonstrating: it reports 4.48 V and 100%, and every voltage from the
// table's top up to its learned maximum has to read full.
func TestAPackAboveTheTableReadsFull(t *testing.T) {
	const top = 4.48
	for _, v := range []float32{4.12, 4.15, 4.20, 4.30, 4.40, 4.48} {
		if got := battPctFor(v, top); got != 100 {
			t.Errorf("a pack last seen at %.2f V reads %d%% at %.2f V, not full", top, got, v)
		}
	}
	// And the curve still falls away below the table's top.
	if got := battPctFor(4.00, top); got != 86 {
		t.Errorf("4.00 V reads %d%%, want 86%%", got)
	}
}

// TestAPackThatNeverReachesTheTopIsScaledUp: the rescale runs the
// opposite way round from the stretch it replaced. A learned maximum
// below the table's top lifts the reading, so that pack's own ceiling
// still reads full.
func TestAPackThatNeverReachesTheTopIsScaledUp(t *testing.T) {
	const top = 4.00
	if got := battPctFor(top, top); got != 100 {
		t.Errorf("a pack that tops out at %.2f V reads %d%% there, not full", top, got)
	}
	// Mid-pack reads higher than it would without the rescale.
	plain, scaled := battPctFor(3.80, 0), battPctFor(3.80, top)
	if scaled <= plain {
		t.Errorf("3.80 V reads %d%% unscaled and %d%% against a %.2f V pack: the rescale did not lift it",
			plain, scaled, top)
	}
}

// TestVoltsForRoundTrips: the inverse feeds the simulated sensors, so a
// level set by hand has to come back as itself through the curve.
func TestVoltsForRoundTrips(t *testing.T) {
	for p := int8(0); p <= 100; p++ {
		v := voltsFor(p)
		if !plausibleVolts(v) {
			t.Errorf("voltsFor(%d) = %v V, which this package calls implausible", p, v)
			continue
		}
		// Unlearned, so the table is read as written, which is what
		// voltsFor inverts.
		back := battPctFor(v, 0)
		if diff := int(back) - int(p); diff < -1 || diff > 1 {
			t.Errorf("voltsFor(%d) = %.4f V, which reads back as %d%%", p, v, back)
		}
	}
}

// TestTheRescaleBandIsMonotonic re-checks what the curve rewrite
// invalidated. The sweep in TestBatteryCurve uses learned maxima of 4.2
// and above, and the rescale fires only below the table's top — so every
// top it tests takes the no-rescale path, and the band the rewrite
// introduced had no monotonicity check at all.
func TestTheRescaleBandIsMonotonic(t *testing.T) {
	for top := float32(3.20); top <= 4.12; top += 0.01 {
		last := int8(-1)
		for i := range 341 {
			v := 3.0 + float32(i)*0.005
			got := battPctFor(v, top)
			if got < 0 || got > 100 {
				t.Fatalf("top %.2f at %.3f V gives %d%%", top, v, got)
			}
			if got < last {
				t.Fatalf("top %.2f dips at %.3f V: %d after %d", top, v, got, last)
			}
			last = got
		}
		if last != 100 {
			t.Errorf("top %.2f tops out at %d%%", top, last)
		}
	}
}

// TestBattPctForSurvivesAReadingThatIsNotOne: the conversion to
// millivolts is a float-to-int, which Go leaves implementation-defined
// when the value will not fit — the same hazard the learned maximum is
// guarded against a few lines up. read() fences NaN out before this is
// reached, so this is the guard holding rather than the caller.
func TestBattPctForSurvivesAReadingThatIsNotOne(t *testing.T) {
	for _, v := range []float32{
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)),
		-1, 1e30, -1e30,
	} {
		for _, top := range []float32{0, 3.9, 4.48} {
			got := battPctFor(v, top)
			if got < 0 || got > 100 {
				t.Errorf("battPctFor(%v, %v) = %d, which is not a percentage", v, top, got)
			}
		}
	}
}
