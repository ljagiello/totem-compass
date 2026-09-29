package emulator

import (
	"math"
	"testing"
)

// TestSweepRefusesABoardThatDidNotTurn: the center of a reading that never
// moved is just where it was pointing. Accepting it would leave the heading
// exactly as wrong as before while reporting that it had been fixed, which
// is worse than refusing — the board would stop saying it has no compass.
func TestSweepRefusesABoardThatDidNotTurn(t *testing.T) {
	var still Sweep
	for range 200 {
		still.Add(1.92, -1.20, 0.001) // the T-Beam sitting on a bench
	}
	if _, ok := still.Offset(); ok {
		t.Error("a board that never moved produced an offset")
	}

	// Tilted, but not turned: two axes sweep and the third does not.
	var tilted Sweep
	for i := range 100 {
		a := float64(i) / 100 * 2 * math.Pi
		tilted.Add(1.92+0.5*math.Cos(a), -1.20+0.5*math.Sin(a), 0.001)
	}
	if _, ok := tilted.Offset(); ok {
		t.Error("a board turned in one plane only produced a three-axis offset")
	}

	if _, ok := (&Sweep{}).Offset(); ok {
		t.Error("a sweep with no readings at all produced an offset")
	}
}

// TestSweepFindsTheOffset: turned through every orientation, the readings
// sweep a sphere of the Earth's field displaced by the hard iron, and the
// center of the box they fill is that displacement.
func TestSweepFindsTheOffset(t *testing.T) {
	want := HardIron{X: 1.92, Y: -1.20, Z: 0.30}
	const field = 0.48 // gauss, a plausible Earth
	var s Sweep
	// A coarse but complete sweep of directions.
	for i := range 20 {
		for j := range 20 {
			theta := float64(i) / 19 * math.Pi
			phi := float64(j) / 19 * 2 * math.Pi
			s.Add(
				want.X+field*math.Sin(theta)*math.Cos(phi),
				want.Y+field*math.Sin(theta)*math.Sin(phi),
				want.Z+field*math.Cos(theta),
			)
		}
	}
	got, ok := s.Offset()
	if !ok {
		dx, dy, dz := s.Spans()
		t.Fatalf("a full sweep was refused; spans %v %v %v", dx, dy, dz)
	}
	for _, c := range []struct {
		axis      string
		got, want float64
	}{{"x", got.X, want.X}, {"y", got.Y, want.Y}, {"z", got.Z, want.Z}} {
		if math.Abs(c.got-c.want) > 0.02 {
			t.Errorf("%s offset %v, want %v", c.axis, c.got, c.want)
		}
	}
}

// TestHeadingFlat: laid flat, the bearing is the one the field points along.
func TestHeadingFlat(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mx, my float64
		want   float64
	}{
		{"north", 1, 0, 0},
		{"east", 0, -1, 90},
		{"south", -1, 0, 180},
		{"west", 0, 1, 270},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Heading(tt.mx, tt.my, 0, 0, 0); math.Abs(got-tt.want) > 0.001 {
				t.Errorf("Heading = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHeadingIsTiltCompensated: the whole point. A board tilted while
// pointing the same way must report the same bearing, which an uncompensated
// compass does not.
func TestHeadingIsTiltCompensated(t *testing.T) {
	// A field 60 degrees below horizontal, as it is at this latitude,
	// pointing magnetic north.
	const dip = 60 * math.Pi / 180
	north, down := math.Cos(dip), math.Sin(dip)

	flat := Heading(north, 0, down, 0, 0)
	for _, tilt := range []struct {
		name        string
		pitch, roll float64
	}{
		{"pitched forward", 30, 0},
		{"pitched back", -25, 0},
		{"rolled right", 0, 20},
		{"rolled left", 0, -20},
		{"both", 15, 15},
	} {
		t.Run(tilt.name, func(t *testing.T) {
			// The field in the tilted board's own axes.
			mx, my, mz := rotate(north, 0, down, tilt.pitch, tilt.roll)
			got := Heading(mx, my, mz, tilt.pitch, tilt.roll)
			if d := angleDiff(got, flat); d > 0.5 {
				t.Errorf("tilted bearing %v, flat %v, %v degrees apart", got, flat, d)
			}
		})
	}
}

// rotate expresses a world-frame field in the axes of a board held at a
// given pitch and roll. It is the exact inverse of what Heading undoes, and
// it has to be: Heading computes Rx(roll)·Ry(pitch)·m, so the forward
// transform is Ry(-pitch)·Rx(-roll), in that order. Writing the two
// rotations the other way round agrees for a pure pitch and for a pure roll
// and disagrees for both at once, which is how this was caught.
func rotate(x, y, z, pitchDeg, rollDeg float64) (bx, by, bz float64) {
	const rad = math.Pi / 180
	sp, cp := math.Sincos(pitchDeg * rad)
	sr, cr := math.Sincos(rollDeg * rad)
	// Rx(-roll) first,
	x1, y1, z1 := x, y*cr+z*sr, -y*sr+z*cr
	// then Ry(-pitch).
	return x1*cp - z1*sp, y1, x1*sp + z1*cp
}

func angleDiff(a, b float64) float64 {
	d := math.Abs(a - b)
	if d > 180 {
		d = 360 - d
	}
	return d
}
