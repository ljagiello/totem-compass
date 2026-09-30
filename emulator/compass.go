package emulator

import (
	"math"
	"time"
)

// HardIron is the constant offset a magnetometer reads on top of the Earth's
// field, from the iron and magnets fixed around it.
//
// It is measured, not guessed: turn the sensor through every orientation and
// the readings sweep out a sphere of the Earth's field, displaced from the
// origin by exactly this. The center of the box they fill is the offset, and
// subtracting it puts the sphere back where it belongs. This is what the
// firmware's own 2D and 3D calibration runs are doing, and it is why a
// compass that has not been turned cannot be trusted: on the T-Beam the
// uncorrected field reads 2.27 gauss where the Earth's is at most 0.65.
type HardIron struct{ X, Y, Z float64 }

// Sweep collects the extremes of a calibration turn.
//
// The zero value is ready to use and has seen nothing. Feed it every reading
// while the board is turned; Offset says what it found, and whether the turn
// was enough of one to believe.
type Sweep struct {
	minX, maxX float64
	minY, maxY float64
	minZ, maxZ float64
	n          int
}

// SweepMinSpanG is how far each axis must move for a turn to count.
//
// A board that sat still has a span of nothing, and the center of nothing is
// just wherever it was pointing — an "offset" that would leave the heading
// exactly as wrong as before while claiming to have fixed it. The Earth's
// field is at least 0.25 gauss, so turning through every orientation sweeps
// each axis across at least twice that; a third of it is a generous floor
// that still refuses a board that was only tilted.
const SweepMinSpanG = 0.15

// Add gives the sweep one reading, in gauss. A reading that is not three
// numbers is ignored rather than allowed into the extremes, where one of them
// would spread to every axis through Min and Max.
func (s *Sweep) Add(x, y, z float64) {
	if !isNumber(x) || !isNumber(y) || !isNumber(z) {
		return
	}
	if s.n == 0 {
		s.minX, s.maxX = x, x
		s.minY, s.maxY = y, y
		s.minZ, s.maxZ = z, z
		s.n = 1
		return
	}
	s.minX, s.maxX = math.Min(s.minX, x), math.Max(s.maxX, x)
	s.minY, s.maxY = math.Min(s.minY, y), math.Max(s.maxY, y)
	s.minZ, s.maxZ = math.Min(s.minZ, z), math.Max(s.maxZ, z)
	s.n++
}

// Readings is how many have been added.
func (s *Sweep) Readings() int { return s.n }

// Spans are how far each axis moved, which is what says whether the board
// was actually turned.
func (s *Sweep) Spans() (x, y, z float64) {
	if s.n == 0 {
		return 0, 0, 0
	}
	return s.maxX - s.minX, s.maxY - s.minY, s.maxZ - s.minZ
}

// Offset is the hard iron the sweep found, and whether the turn was enough
// of one for it to mean anything.
func (s *Sweep) Offset() (HardIron, bool) {
	dx, dy, dz := s.Spans()
	// Spans are required to be large, which is a test NaN passes by failing
	// every comparison: one NaN reading propagates through Min and Max into
	// the extremes, and `dx < 0.15` is false for it, so the offset came back
	// NaN with ok true. From there every bearing is NaN, and converting that
	// to an int16 is not even defined. Required to be a number first.
	if !isNumber(dx) || !isNumber(dy) || !isNumber(dz) {
		return HardIron{}, false
	}
	if s.n < 2 || dx < SweepMinSpanG || dy < SweepMinSpanG || dz < SweepMinSpanG {
		return HardIron{}, false
	}
	return HardIron{
		X: (s.maxX + s.minX) / 2,
		Y: (s.maxY + s.minY) / 2,
		Z: (s.maxZ + s.minZ) / 2,
	}, true
}

// isNumber says whether a value is one: not a NaN, and not an infinity that
// arithmetic would carry into everything downstream.
func isNumber(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// MaxTravelKPH is the fastest a fix is allowed to imply it moved. Above an
// airliner's cruise, and far below anything a broken receiver produces.
const MaxTravelKPH = 1000

// PlausibleStep says whether moving from one fix to the next in the given
// time is travel rather than a receiver inventing a position.
//
// A receiver indoors can report a solution it has flagged valid and be a
// hundred kilometers wrong: measured on this bench, the board reported a
// position 120 km from a Totem sitting beside it, with no satellites, no
// accuracy figure and a speed pinned at the top of its byte, while the
// Totem had a 2 m fix. Nothing downstream can tell that from a real place —
// it goes into the status frame, out to every peer, and into their arrows.
//
// The rule is only about what is physically possible, so it refuses almost
// nothing real: a Totem in a car, a train or a plane all stay well inside
// it. A gap with no previous fix, or one long enough that anything could
// have happened in it, is not judged at all.
func PlausibleStep(prevLat, prevLon, lat, lon float32, elapsed time.Duration) bool {
	if elapsed <= 0 || elapsed > time.Minute {
		return true
	}
	km := DistanceM(prevLat, prevLon, lat, lon) / 1000
	if km < 0 {
		return true // no distance to be had, which DistanceM says with -1
	}
	return km/elapsed.Hours() <= MaxTravelKPH
}

// Heading is the compass bearing in degrees, from a magnetometer reading and
// the tilt the board is held at.
//
// Tilt compensated, because a compass that is not is only right when it is
// flat. Rotating the field back into the horizontal plane by the pitch and
// roll the IMU reports is the standard closed form, and it is what lets a
// Totem being carried at any angle still point at a friend.
//
// The reading must already have its hard iron subtracted; without that the
// result is a bearing towards whatever is magnetic nearby. Angles in degrees,
// bearing in [0, 360).
func Heading(mx, my, mz, pitchDeg, rollDeg float64) float64 {
	const rad = math.Pi / 180
	sp, cp := math.Sincos(pitchDeg * rad)
	sr, cr := math.Sincos(rollDeg * rad)
	// The field as it would read with the board laid flat.
	xh := mx*cp + mz*sp
	yh := mx*sr*sp + my*cr - mz*sr*cp
	return norm360(math.Atan2(-yh, xh) / rad)
}
