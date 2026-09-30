package emulator

import "math"

// finitePosition says whether a reading is a pair of real numbers. The NMEA
// parser refuses these at the door, so this is the backstop for the day
// another source does not.
func finitePosition(lat, lon float32) bool {
	f, g := float64(lat), float64(lon)
	return !math.IsNaN(f) && !math.IsNaN(g) && !math.IsInf(f, 0) && !math.IsInf(g, 0)
}

// What a reported position is low-passed with. A receiver standing still
// does not report one place: three minutes on the bench, with five
// satellites and six meters of claimed accuracy, wandered a box about nine
// meters on a side and never settled. Every fix of it went out on the mesh,
// so a peer pointing at the board saw its direction swing, and the odometer
// counted each excursion as travel — 25 m of it in those three minutes.
//
// The weight given to a new fix rises with how far it is from the position
// being reported, measured against the snap distance: the step at which a
// fix is believed whole. Noise, which is small against that, barely moves
// what is reported; travel, which is not, is followed at once. One rule
// covers both because what separates them is the size of the step, not
// anything the receiver says about it — its speed field read 0, 1, 2 and
// 9 km/h during that same stationary run, so it cannot be asked.
//
// This does not make a position accurate, and it is not meant to. The
// receiver's bias is whatever it is; what this removes is the part that
// changes while nothing moves.
const (
	positionAlphaMin = 0.05
	// snapFloorM applies when the receiver claims no accuracy (-1) or an
	// implausibly good one.
	snapFloorM           = 20
	snapAccuracyMultiple = 3
)

// PositionSmoother low-passes a receiver's position against its own wander.
// The zero value is ready, and takes the first position it is given.
type PositionSmoother struct {
	lat, lon float32
	have     bool
}

// Steady returns the position to report for a receiver reading lat, lon
// with accuracyM meters of claimed accuracy (-1 when it claims none).
//
// The weight is never zero, so a receiver whose bias drifts slowly is still
// followed; without that the board would go on reporting where it used to
// be. It is never partial for a large step either: averaging into a jump
// reports a place between two readings that is neither of them, and a jump
// too large to be travel is PlausibleStep's to refuse, not this.
func (p *PositionSmoother) Steady(lat, lon float32, accuracyM int8) (float32, float32) {
	// A reading that is not a number is not a place. Report the last one
	// that was, and if there has not been one stay uninitialised so the
	// next real fix is taken whole. Letting it through poisons the
	// position from then on: every later step measures against NaN, and
	// the board reports NaN to every peer for the rest of the run.
	if !finitePosition(lat, lon) {
		return p.lat, p.lon
	}
	if !p.have {
		p.lat, p.lon, p.have = lat, lon, true
		return p.lat, p.lon
	}
	snap := float64(max(snapFloorM, snapAccuracyMultiple*int(accuracyM)))
	alpha := DistanceM(p.lat, p.lon, lat, lon) / snap
	switch {
	// Written against the step rather than for it, so a distance that is
	// not a number takes this branch instead of scaling by one.
	case !(alpha < 1):
		p.lat, p.lon = lat, lon
		return p.lat, p.lon
	case alpha < positionAlphaMin:
		alpha = positionAlphaMin
	}
	p.lat += float32(alpha) * (lat - p.lat)
	p.lon += float32(alpha) * (lon - p.lon)
	return p.lat, p.lon
}
