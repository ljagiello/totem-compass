package emulator

// The battery curve against the firmware's own arithmetic.

import (
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

	var checked, overHundred int
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
		// The firmware can exceed 100 when it has learned no maximum;
		// battPctFor caps there, because the frame's field is a signed
		// byte and the device would have faulted packing it.
		if want > 100 {
			overHundred++
			want = 100
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
	if overHundred == 0 {
		t.Error("no point in the golden file exercises the over-100 path")
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
