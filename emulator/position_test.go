package emulator

import (
	"math"
	"testing"
)

// The bench: five satellites, six meters of claimed accuracy, nothing
// moving. These are the positions the receiver actually reported over three
// minutes, and the box they cover is about nine meters on a side.
var benchWander = [][2]float32{
	{37.586899, -122.007278},
	{37.586891, -122.007301},
	{37.586872, -122.007332},
	{37.586834, -122.007347},
	{37.586887, -122.007370},
	{37.586842, -122.007378},
	{37.586815, -122.007339},
	{37.586823, -122.007339},
	{37.586830, -122.007362},
	{37.586826, -122.007362},
	{37.586815, -122.007362},
	{37.586800, -122.007339},
}

// path returns the total ground the positions cover in order. This is the
// figure that matters: it is what the odometer sums, and its step-to-step
// part is what swings a peer's ring. The straight-line spread is not, and
// measuring it is what hid the shape of this data at first — these readings
// drift about eleven meters one way rather than jitter about a point, and
// no low pass removes a drift.
func path(ps [][2]float32) float64 {
	var total float64
	for i, a := range ps[:len(ps)-1] {
		b := ps[i+1]
		total += DistanceM(a[0], a[1], b[0], b[1])
	}
	return total
}

// TestPositionSmootherHoldsStill: the readings above are what made a
// Totem's ring swing while the board sat on a desk. The reported track has
// to be far shorter than the one the receiver drew, so that what reaches a
// peer is the drift alone and not every step of the noise on top of it.
func TestPositionSmootherHoldsStill(t *testing.T) {
	var p PositionSmoother
	got := make([][2]float32, 0, len(benchWander))
	for _, w := range benchWander {
		lat, lon := p.Steady(w[0], w[1], 6)
		got = append(got, [2]float32{lat, lon})
	}
	raw, out := path(benchWander), path(got)
	if out > raw/2 {
		t.Errorf("reported track ran %.1f m against the receiver's %.1f m, want under %.1f m",
			out, raw, raw/2)
	}
	// Following the receiver is the job, so the reported position may not
	// sit off on its own: the drift is real and has to arrive.
	last, end := benchWander[len(benchWander)-1], got[len(got)-1]
	if d := DistanceM(last[0], last[1], end[0], end[1]); d > raw/2 {
		t.Errorf("reported position ended %.1f m from the last reading, want under %.1f m", d, raw/2)
	}
}

// TestPositionSmootherFollowsTravel: smoothing may not cost the board its
// position when it is genuinely moving. A step past the snap distance is
// taken whole, and a walk is followed rather than averaged into the past.
func TestPositionSmootherFollowsTravel(t *testing.T) {
	const lat, lon = 37.5868, -122.0073
	t.Run("a long step is taken whole", func(t *testing.T) {
		var p PositionSmoother
		p.Steady(lat, lon, 6)
		// 0.001 deg of latitude is about 111 m, well past the snap.
		gotLat, gotLon := p.Steady(lat+0.001, lon, 6)
		if d := DistanceM(lat+0.001, lon, gotLat, gotLon); d > 0.5 {
			t.Errorf("a 111 m step was reported %.1f m short of where the receiver put it", d)
		}
	})

	t.Run("a walk is not left behind", func(t *testing.T) {
		var p PositionSmoother
		p.Steady(lat, lon, 6)
		// About 1.4 m a second for a minute, which is a walk, and each
		// step alone is far under the snap distance.
		const step = 1.4 / 111320.0
		var last [2]float32
		cur := float64(lat)
		for range 60 {
			cur += step
			a, b := p.Steady(float32(cur), lon, 6)
			last = [2]float32{a, b}
		}
		// The reported position lags, because that is what a low pass
		// does; what it may not do is stop following.
		behind := DistanceM(float32(cur), lon, last[0], last[1])
		if behind > 20 {
			t.Errorf("after a minute's walk the reported position was %.1f m behind, want under 20 m", behind)
		}
		if moved := DistanceM(lat, lon, last[0], last[1]); moved < 60 {
			t.Errorf("a minute's walk of about 84 m was reported as %.1f m of movement", moved)
		}
	})
}

// TestPositionSmootherFirstFix: the first reading has nothing to average
// against and must be reported as it is, or the board starts life claiming
// to be at the zero value.
func TestPositionSmootherFirstFix(t *testing.T) {
	var p PositionSmoother
	lat, lon := p.Steady(51.4779, -0.0015, -1)
	if lat != 51.4779 || lon != -0.0015 {
		t.Errorf("the first fix was reported as %v,%v", lat, lon)
	}
}

// TestPositionSmootherUnknownAccuracy: a receiver claiming no accuracy
// (-1) still gets smoothed, against the floor rather than a negative snap
// distance that would make every step look long enough to believe.
func TestPositionSmootherUnknownAccuracy(t *testing.T) {
	const lat, lon = 37.5868, -122.0073
	var p PositionSmoother
	p.Steady(lat, lon, -1)
	// A couple of meters, which is wander and must not be taken whole.
	gotLat, gotLon := p.Steady(lat+0.00002, lon, -1)
	if d := DistanceM(lat+0.00002, lon, gotLat, gotLon); d < 1 {
		t.Errorf("with no claimed accuracy a 2 m step was taken whole (%.2f m from the reading)", d)
	}
}

// TestPositionSmootherRefusesNaN: a reading that is not a number must not
// reach the reported position, or every step after it measures against NaN
// and the board reports NaN to its peers for the rest of the run. The NMEA
// parser rejects these at the door, so this is the backstop for the day
// another source does not.
func TestPositionSmootherRefusesNaN(t *testing.T) {
	const lat, lon = 37.5868, -122.0073
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		var p PositionSmoother
		p.Steady(lat, lon, 6)
		gotLat, gotLon := p.Steady(bad, lon, 6)
		if gotLat != lat || gotLon != lon {
			t.Errorf("a %v reading moved the reported position to %v,%v", bad, gotLat, gotLon)
		}
		if gotLat, gotLon = p.Steady(lat+0.00002, lon, 6); math.IsNaN(float64(gotLat)) || math.IsNaN(float64(gotLon)) {
			t.Errorf("a good fix after a %v one was reported as %v,%v", bad, gotLat, gotLon)
		}
	}
	// One arriving first leaves nothing to report, and must not be kept as
	// the position either: the next real fix is the one to take whole.
	var p PositionSmoother
	p.Steady(float32(math.NaN()), lon, 6)
	if gotLat, gotLon := p.Steady(lat, lon, 6); gotLat != lat || gotLon != lon {
		t.Errorf("the first real fix after a NaN one was reported as %v,%v", gotLat, gotLon)
	}
}
