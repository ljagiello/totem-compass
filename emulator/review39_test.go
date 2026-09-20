package emulator

// A regression for the thirty-ninth review round.

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestTheSimCrossesThePoleInsteadOfSittingOnIt: the fold reflected the
// latitude and put the longitude on the far side, but left the course
// alone — so the next step set off north again from just below the pole,
// crossed it again, and the track alternated between two points forever.
// A peer watching saw the longitude flip 180 degrees at the poll rate
// and the heading alternate, while the odometer kept climbing.
//
// TestTheSimStaysOnTheGlobe passes either way: it only asks that the
// values stay finite and in range, which two points at the pole are.
func TestTheSimCrossesThePoleInsteadOfSittingOnIt(t *testing.T) {
	s := NewSim(SimConfig{
		Lat: 89.9, Lon: 0, Motion: Drive, Bearing: 0,
	}, t0)

	// Long enough to get there and well down the other side: 50 km/h for
	// 100 minutes is about 83 km, and the pole is 11 km away.
	now := t0
	var lats, lons []float64
	for range 200 {
		now = now.Add(30 * time.Second)
		s.Read(now)
		lats = append(lats, s.lat)
		lons = append(lons, s.lon)
	}

	// It must have got to the other side: a latitude that came back down
	// well past the pole, rather than hovering just below it.
	var lowest = math.Inf(1)
	for _, l := range lats {
		lowest = math.Min(lowest, l)
	}
	if lowest > 89.5 {
		t.Errorf("driving north from 89.9 for 100 minutes never left the pole: lowest latitude %.5f", lowest)
	}

	// And it is still going the same way on the ground rather than
	// oscillating: no two consecutive steps may reverse direction more
	// than once, which is the crossing itself.
	reversals := 0
	for i := 2; i < len(lats); i++ {
		a, b := lats[i-1]-lats[i-2], lats[i]-lats[i-1]
		if a != 0 && b != 0 && (a > 0) != (b > 0) {
			reversals++
		}
	}
	if reversals > 1 {
		t.Errorf("the track changed direction %d times; crossing the pole is one", reversals)
	}

	// And the longitude settles on the far side rather than flipping back
	// and forth: one change, at the crossing.
	flips := 0
	for i := 1; i < len(lons); i++ {
		if math.Abs(lons[i]-lons[i-1]) > 90 {
			flips++
		}
	}
	if flips > 1 {
		t.Errorf("the longitude jumped to the far side %d times; crossing the pole is one", flips)
	}
}

// TestTheCrystalIsNamedWhileDimmed: Describe read the pixel, which has
// been through dim(), and looked it up in a palette of undimmed values —
// so one tap of the power button turned "crystal red" into
// "crystal #3f0000" for as long as the strip stayed dimmed.
func TestTheCrystalIsNamedWhileDimmed(t *testing.T) {
	l := newLEDs(t0, ColorRed)
	l.Tick(t0)
	if got := l.Describe(); !strings.Contains(got, "crystal red") {
		t.Fatalf("at full brightness: %q", got)
	}
	l.ToggleBrightness()
	l.Tick(t0.Add(time.Second))
	if got := l.Describe(); !strings.Contains(got, "crystal red") {
		t.Errorf("dimmed: %q, want the colour named", got)
	}
}

// TestAStaticSourceCopiesTheFixItWasGiven: every other ingress copies,
// and says why — a caller must not be able to write through into the
// readings. NewStatic is the exported constructor a board driver calls,
// and it kept the caller's pointer.
func TestAStaticSourceCopiesTheFixItWasGiven(t *testing.T) {
	fix := &Fix{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3}
	src := NewStatic(Sensors{Fix: fix, Battery: Battery{Volts: 4.1, Percent: 90}})

	// The caller keeps hold of it and writes something impossible.
	fix.Lat = float32(math.NaN())

	got := src.Read(t0)
	if got.Fix == nil {
		t.Fatal("the fix went away")
	}
	if math.IsNaN(float64(got.Fix.Lat)) {
		t.Error("a caller wrote through NewStatic into the readings")
	}
}
