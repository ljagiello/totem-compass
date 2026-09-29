package emulator

import "math"

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

// Add gives the sweep one reading, in gauss.
func (s *Sweep) Add(x, y, z float64) {
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
	if s.n < 2 || dx < SweepMinSpanG || dy < SweepMinSpanG || dz < SweepMinSpanG {
		return HardIron{}, false
	}
	return HardIron{
		X: (s.maxX + s.minX) / 2,
		Y: (s.maxY + s.minY) / 2,
		Z: (s.maxZ + s.minZ) / 2,
	}, true
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
