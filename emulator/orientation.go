package emulator

import (
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// OrientationTracker decides whether a device is upright or lying flat from
// its pitch and roll, the way the firmware's imu_fusion_auto._update does.
//
// It is here rather than in a board's driver because the rule is the
// firmware's, not a board's, and because it is the kind of rule worth
// testing on a host: two thresholds, two dwell times and two hold times,
// with a state that only ever moves between the two of them.
//
// The numbers are from the firmware, decoded at
// imu_fusion_auto.dis:3606-3810 and :3018-3021 and written up in
// docs/subsystems/navigation. What the tracker does not do is pretend to
// know which physical pose trips them on a given board: a Totem has its IMU
// mounted its own way, so a development board reaching "vertical" means its
// own axes crossed the threshold, not that anyone is carrying it.
type OrientationTracker struct {
	// committed is the state reported, and committedAt is when it became
	// that. The zero value is the firmware's own starting point: unknown,
	// which behaves like horizontal for how long it has to be held.
	committed   mesh.Orientation
	committedAt time.Time

	// candidate is the pose currently being argued for, and candidateAt is
	// when it started making the case.
	candidate   mesh.Orientation
	candidateAt time.Time
}

// The thresholds a pose has to cross to become a candidate. A pitch at or
// below -35 degrees with a small positive roll is upright; anything less
// steep, with roll within 35 degrees either way, is flat. A reading that is
// neither leaves the candidate alone — there is no third state and the
// firmware has no else branch here either.
const (
	orientationPitchDeg = -35
	orientationRollDeg  = 35
)

// How long a pose must persist to be committed, and how long the state it
// replaces must already have lasted. Both depend on which state, so unknown,
// vertical and horizontal each get their own, and both exist to stop a
// device that is being put down or picked up from flapping between the two.
//
// The asymmetry is the firmware's: claiming upright takes 100 ms but
// abandoning it takes 2.8 s, because a Totem held in a hand wobbles and
// every change of state ends up in a status frame to every peer.
//
// Switches rather than maps. A map is looked up on every reading, is a
// package-level variable anything could write, and — the part that matters —
// answers zero for a key it does not have. This field carries whatever a
// frame gave it, and node.read only normalises it after this has run, so an
// orientation outside the three would have committed instantly and abandoned
// the old state instantly: exactly the flapping the numbers exist to stop.
func orientationDwell(o mesh.Orientation) time.Duration {
	if o == mesh.OrientationHorizontal {
		return 300 * time.Millisecond
	}
	return 100 * time.Millisecond
}

func orientationHold(o mesh.Orientation) time.Duration {
	if o == mesh.OrientationVertical {
		return 2800 * time.Millisecond
	}
	return 800 * time.Millisecond
}

// Update gives the tracker a reading and returns the state to report, which
// is the committed one whether or not this reading changed it.
func (t *OrientationTracker) Update(pitchDeg, rollDeg float64, now time.Time) mesh.Orientation {
	if want := candidateFor(pitchDeg, rollDeg); want != mesh.OrientationUnknown && want != t.candidate {
		t.candidate, t.candidateAt = want, now
	}
	if t.candidate == mesh.OrientationUnknown || t.candidate == t.committed {
		return t.committed
	}
	// The new pose has to have persisted, and the old one has to have had
	// its turn. A first reading arriving before either clock has run is
	// not enough, which is why committedAt starts at the zero time and is
	// compared rather than assumed.
	if now.Sub(t.candidateAt) < orientationDwell(t.candidate) {
		return t.committed
	}
	if !t.committedAt.IsZero() && now.Sub(t.committedAt) < orientationHold(t.committed) {
		return t.committed
	}
	t.committed, t.committedAt = t.candidate, now
	return t.committed
}

// Orientation is the state being reported without giving a new reading.
func (t *OrientationTracker) Orientation() mesh.Orientation { return t.committed }

// candidateFor is the pose a reading argues for, or unknown when it argues
// for neither.
func candidateFor(pitchDeg, rollDeg float64) mesh.Orientation {
	switch {
	case pitchDeg <= orientationPitchDeg && rollDeg > 0 && rollDeg < orientationRollDeg:
		return mesh.OrientationVertical
	case pitchDeg > orientationPitchDeg && rollDeg > -orientationRollDeg && rollDeg < orientationRollDeg:
		return mesh.OrientationHorizontal
	}
	return mesh.OrientationUnknown
}
