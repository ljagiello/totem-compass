package emulator

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// Sensors is what a Totem's hardware tells the firmware. A Sim provides it
// on a board with no sensors; the GNSS receiver, magnetometer, IMU and
// power chip of a board that has them replace it field by field.
type Sensors struct {
	Fix         *Fix // nil without a GNSS fix
	Orientation mesh.Orientation
	// Azimuth is the compass heading in degrees, from the magnetometer.
	Azimuth int16
	Battery Battery
}

// Position is a GNSS fix, the name the console and the CLI use for it.
type Position = Fix

// Fix is one GNSS solution, as ubx_gnss fills gnss_data.
type Fix struct {
	Lat, Lon  float32
	AccuracyM int8  // -1 unknown
	AltitudeM int16 // -500 unknown
	SpeedKPH  int8
	SatCount  int8
	// SolutionID is gnss_data.solution_id: 0 none or poor, 1 within 3.5 m,
	// 2 within 15 m.
	SolutionID int8
	// HeadingOfMotion is -1 until the device has traveled 10 m.
	HeadingOfMotion int16
	OdometerM       int32
	// Time is the GNSS clock. When it is set the Totem advertises it to
	// peers (rtc_method 1); a device without it borrows a peer's clock.
	Time time.Time
}

// Battery is what the power chip reports.
type Battery struct {
	Volts    float32
	Percent  int8
	Charging bool
	// Low is modes.power_level 2, below about 3.45 V.
	Low bool
	// NoPowerChip says this board is wired without a battery monitor at
	// all, so its zeroes are the absence of a reading rather than a
	// reading of zero. A chip whose reading has failed, and a cell that
	// really is flat, report the same zeroes — and those are the cases
	// the OTA gate exists for, so they must not be read as this one.
	//
	// Stated the negative way round on purpose: the zero value is "there
	// is a power chip", which is the answer that keeps the gate on. A
	// driver that has never heard of this field fails closed.
	NoPowerChip bool
}

// SensorSource reads the sensors. Both the simulator and a real board's
// drivers implement it.
type SensorSource interface {
	Read(now time.Time) Sensors
}

// Controls is the half of a sensor source an operator drives by hand: the
// pos, heading, batt, flat and clock commands reach the hardware through
// it. A source that implements it takes those settings whatever else it is
// doing, so the node needs no per-source branch and a command cannot
// quietly do nothing. A real board's drivers may leave it unimplemented,
// and the node then says the reading comes from the hardware.
type Controls interface {
	// SetFix places the receiver on a solution; nil takes it away, as
	// stepping indoors does.
	SetFix(f *Fix)
	// SetAzimuth points the compass. A simulation steers onto it.
	SetAzimuth(deg int16)
	SetFlat(flat bool)
	SetBattery(percent int8, charging bool, now time.Time)
	// SetClock hands the receiver the wall time, as a GNSS lock does.
	SetClock(wall, now time.Time)
}

// norm360 folds an angle into [0, 360). math.Mod alone keeps the sign, and
// a walk that has wandered below zero would otherwise report a negative
// azimuth in its status frames.
func norm360(deg float64) float64 { return math.Mod(math.Mod(deg, 360)+360, 360) }

// Motion is how a simulated Totem moves.
type Motion uint8

// The motion patterns a Sim can follow.
const (
	Still Motion = iota
	Walk         // 5 km/h
	Drive        // 50 km/h
)

// String names the motion as the console takes it.
func (m Motion) String() string {
	switch m {
	case Walk:
		return "walk"
	case Drive:
		return "drive"
	}
	return "still"
}

// SimConfig describes a simulated Totem.
type SimConfig struct {
	Lat, Lon float32
	Motion   Motion
	// Bearing is the course in degrees; a walk wanders around it.
	Bearing int16
	// NoFix leaves the simulated receiver without a solution, as indoors.
	NoFix bool
	// Percent starts the battery; it drains to empty over Life. The
	// voltage follows the charge level, as voltsFor maps it. A negative
	// value asks for the default, because 0 is a real reading: a flat
	// device reporting a full battery to its peers is worse than a
	// simulation that has to say what it wants.
	Percent int8
	Life    time.Duration
	// Charging holds the battery level and marks it charging.
	Charging bool
	// Clock is the wall time at the simulation's start. Without it the
	// simulated receiver reports no time, as a Totem does before it locks
	// on, and the node borrows a peer's clock instead of advertising one.
	Clock time.Time
	// Flat reports the Totem lying down, which makes peers transmit faster.
	Flat bool
	Rand *rand.Rand
}

// wanderDegPerSec is how much a simulated walk turns, in degrees per
// second of walking.
const wanderDegPerSec = 20

// Sim is a Totem's hardware on a board that has none: it walks a track,
// drains a battery and points a compass, so the emulator behaves like a
// device someone is carrying.
type Sim struct {
	cfg   SimConfig
	rng   *rand.Rand
	start time.Time
	last  time.Time

	// The track is kept in float64: at 5 ms polls a float32 degree loses
	// the millimeters a walk covers, and the device never moves.
	lat, lon  float64
	bearing   float64
	odometerM float64
	// The heading of motion is measured over a 10 m leg, as
	// nav_helpers.get_heading_mot does: between two fixes a poll apart it
	// would be noise.
	prevLat, prevLon float64
	headingOdoM      float64
	heading          int16

	// The battery drains from battFrom, the level it was last set to, over
	// Life from battAt. Draining from the simulation's start instead would
	// make a battery set mid-run jump back to where it would have got to by
	// now on its own.
	battFrom int8
	battAt   time.Time
}

// NewSim starts a simulated Totem at now.
func NewSim(cfg SimConfig, now time.Time) *Sim {
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(uint64(now.UnixNano()), 0x70712e))
	}
	if cfg.Percent < 0 {
		cfg.Percent = 95
	}
	if cfg.Life == 0 {
		cfg.Life = 12 * time.Hour
	}
	return &Sim{
		cfg: cfg, rng: cfg.Rand, start: now, last: now,
		lat: float64(cfg.Lat), lon: float64(cfg.Lon), bearing: float64(cfg.Bearing), heading: -1,
		prevLat: float64(cfg.Lat), prevLon: float64(cfg.Lon),
		battFrom: cfg.Percent, battAt: now,
	}
}

// Config returns the simulation's settings.
func (s *Sim) Config() SimConfig { return s.cfg }

// SetMotion changes how the simulated Totem moves.
func (s *Sim) SetMotion(m Motion, bearing int16) {
	s.cfg.Motion, s.cfg.Bearing = m, bearing
	s.bearing = float64(bearing)
}

// SetAzimuth steers the simulated Totem onto a course: the compass and the
// track follow the same bearing, as they do on a device someone carries.
func (s *Sim) SetAzimuth(deg int16) { s.SetMotion(s.cfg.Motion, deg) }

// SetFlat reports the Totem lying down or upright.
func (s *Sim) SetFlat(flat bool) { s.cfg.Flat = flat }

// SetBattery sets the charge level and whether it is charging. The drain
// starts again from now, so the level asked for is the level reported.
func (s *Sim) SetBattery(percent int8, charging bool, now time.Time) {
	s.cfg.Percent, s.cfg.Charging = percent, charging
	s.battFrom, s.battAt = percent, now
}

// SetClock gives the simulated receiver the wall time, as a GNSS lock
// does.
func (s *Sim) SetClock(wall, now time.Time) {
	s.cfg.Clock = wall.Add(-now.Sub(s.start))
}

// SetFix moves the simulated Totem, or takes its fix away. The receiver
// keeps deriving the rest of the solution as it walks.
func (s *Sim) SetFix(f *Fix) {
	if f == nil {
		s.cfg.NoFix = true
		return
	}
	s.lat, s.lon, s.cfg.NoFix = float64(f.Lat), float64(f.Lon), false
	s.cfg.Lat, s.cfg.Lon = f.Lat, f.Lon
	s.prevLat, s.prevLon = s.lat, s.lon
}

// speedKPH is the motion's ground speed.
func (s *Sim) speedKPH() float64 {
	switch s.cfg.Motion {
	case Walk:
		return 5
	case Drive:
		return 50
	}
	return 0
}

// Read advances the simulation to now and reports the sensors.
func (s *Sim) Read(now time.Time) Sensors {
	dt := now.Sub(s.last)
	if dt < 0 {
		dt = 0
	}
	s.last = now
	speed := s.speedKPH()
	if speed > 0 {
		// A walk wanders, a drive holds its course. The wander is per
		// second, not per reading: the driver reads the sensors often.
		if s.cfg.Motion == Walk {
			s.bearing += (s.rng.Float64() - 0.5) * wanderDegPerSec * dt.Seconds()
		}
		s.move(speed*1000/3600*dt.Seconds(), s.bearing)
	}
	orientation := mesh.OrientationVertical
	if s.cfg.Flat {
		orientation = mesh.OrientationHorizontal
	}
	out := Sensors{
		Orientation: orientation,
		Azimuth:     int16(norm360(s.bearing)),
		Battery:     s.battery(now),
	}
	if s.cfg.NoFix {
		return out
	}
	acc := int8(3)
	if speed > 0 {
		acc = 5
	}
	out.Fix = &Fix{
		Lat: float32(s.lat), Lon: float32(s.lon), AccuracyM: acc, AltitudeM: 12,
		SpeedKPH: int8(min(speed, 127)), SatCount: 11, SolutionID: solutionFor(acc),
		HeadingOfMotion: s.heading, OdometerM: int32(s.odometerM),
	}
	if !s.cfg.Clock.IsZero() {
		out.Fix.Time = s.cfg.Clock.Add(now.Sub(s.start))
	}
	return out
}

// move walks the track distanceM meters along bearing and keeps the
// odometer and heading of motion the firmware reports.
func (s *Sim) move(distanceM, bearing float64) {
	if distanceM <= 0 {
		return
	}
	const meterPerDegree = 111320
	rad := bearing * math.Pi / 180
	s.lat += distanceM * math.Cos(rad) / meterPerDegree
	s.lon += distanceM * math.Sin(rad) / (meterPerDegree * math.Cos(s.lat*math.Pi/180))
	s.odometerM += distanceM
	// get_heading_mot: the course of the last 10 m traveled.
	if s.odometerM-s.headingOdoM >= 10 {
		s.heading = int16(norm360(bearingBetween(s.prevLat, s.prevLon, s.lat, s.lon)))
		s.prevLat, s.prevLon, s.headingOdoM = s.lat, s.lon, s.odometerM
	}
}

// battery drains from the level last set over Life, or holds while charging.
func (s *Sim) battery(now time.Time) Battery {
	pct := float64(s.battFrom)
	if !s.cfg.Charging && s.cfg.Life > 0 {
		pct -= float64(s.battFrom) * now.Sub(s.battAt).Seconds() / s.cfg.Life.Seconds()
	}
	p := int8(min(max(pct, 0), 100))
	return Battery{Volts: voltsFor(p), Percent: p, Charging: s.cfg.Charging, Low: p <= 10}
}

// voltsFor maps a charge level back to a cell voltage. It walks the same
// table get_batt_pct does, the other way, so a level set by hand and the
// voltage reported with it agree: a straight line from 3.3 V to 4.2 V
// would say 3.75 V at 50%, where the curve reads that as 41%.
func voltsFor(percent int8) float32 {
	p := min(max(percent, 0), 100)
	switch {
	case p <= battCurve[0].pct:
		return battCurve[0].volts
	case p >= battCurve[len(battCurve)-1].pct:
		return battCurve[len(battCurve)-1].volts
	}
	for i := 1; i < len(battCurve); i++ {
		hi := battCurve[i]
		if p > hi.pct {
			continue
		}
		lo := battCurve[i-1]
		span := float32(hi.pct - lo.pct)
		return lo.volts + (hi.volts-lo.volts)*float32(p-lo.pct)/span
	}
	return battCurve[len(battCurve)-1].volts
}

// solutionFor is gnss_data.solution_id from the position accuracy.
func solutionFor(accuracyM int8) int8 {
	switch {
	case accuracyM < 0:
		return 0
	case accuracyM <= 3:
		return 1
	case accuracyM <= 15:
		return 2
	}
	return 0
}

// bearingBetween is the initial course from one point to another, in
// degrees.
func bearingBetween(lat1, lon1, lat2, lon2 float64) float64 {
	φ1, φ2 := lat1*math.Pi/180, lat2*math.Pi/180
	dλ := (lon2 - lon1) * math.Pi / 180
	y := math.Sin(dλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(dλ)
	return math.Atan2(y, x) * 180 / math.Pi
}

// NewStatic returns a source that always reports s, as a board with no
// sensors does.
func NewStatic(s Sensors) SensorSource { return &staticSensors{s: s} }

// staticSensors reports one fixed reading, as the pos, heading, batt and
// flat commands set it.
type staticSensors struct {
	s Sensors
	// clock is the wall time at at, kept as a base rather than a stamped
	// reading: a fixed timestamp handed to the node every poll would pin
	// the emulated clock to the instant it was set.
	clock time.Time
	at    time.Time
}

// Read reports the fixed sensors, with the clock advanced to now.
func (f *staticSensors) Read(now time.Time) Sensors {
	out := f.s
	if out.Fix != nil {
		// Copy, so the caller holds no pointer into the source.
		fix := *out.Fix
		if !f.clock.IsZero() {
			fix.Time = f.clock.Add(now.Sub(f.at))
		}
		out.Fix = &fix
	}
	return out
}

// SetFix places the fixed reading, or takes its solution away.
func (f *staticSensors) SetFix(fix *Fix) {
	if fix == nil {
		f.s.Fix = nil
		return
	}
	// Copy, so a caller that keeps the fix cannot change the readings.
	held := *fix
	f.s.Fix = &held
}

// SetAzimuth points the fixed compass.
func (f *staticSensors) SetAzimuth(deg int16) { f.s.Azimuth = deg }

// SetFlat reports the Totem lying down or upright.
func (f *staticSensors) SetFlat(flat bool) {
	f.s.Orientation = mesh.OrientationVertical
	if flat {
		f.s.Orientation = mesh.OrientationHorizontal
	}
}

// SetBattery sets the charge level and whether it is charging. A board
// with no power chip holds it there.
func (f *staticSensors) SetBattery(percent int8, charging bool, _ time.Time) {
	// The four fields a reading actually changes, assigned rather than a
	// whole new struct: everything else about the battery — what kind of
	// board this is — is then carried by construction. Rebuilding it
	// dropped NoPowerChip and silently re-armed the OTA gate on a board
	// with no battery to gate, and the next field added would have gone
	// the same way.
	f.s.Battery.Volts = voltsFor(percent)
	f.s.Battery.Percent = percent
	f.s.Battery.Charging = charging
	f.s.Battery.Low = percent <= 10
}

// SetClock hands the fixed receiver the wall time, which then runs on.
func (f *staticSensors) SetClock(wall, now time.Time) { f.clock, f.at = wall, now }
