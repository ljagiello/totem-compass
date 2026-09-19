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
}

// SensorSource reads the sensors. Both the simulator and a real board's
// drivers implement it.
type SensorSource interface {
	Read(now time.Time) Sensors
}

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
	// Volts and Percent start the battery; it drains over Life.
	Volts   float32
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
}

// NewSim starts a simulated Totem at now.
func NewSim(cfg SimConfig, now time.Time) *Sim {
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(uint64(now.UnixNano()), 0x70712e))
	}
	if cfg.Percent == 0 {
		cfg.Percent = 95
	}
	cfg.Volts = voltsFor(cfg.Percent)
	if cfg.Life == 0 {
		cfg.Life = 12 * time.Hour
	}
	return &Sim{
		cfg: cfg, rng: cfg.Rand, start: now, last: now,
		lat: float64(cfg.Lat), lon: float64(cfg.Lon), bearing: float64(cfg.Bearing), heading: -1,
		prevLat: float64(cfg.Lat), prevLon: float64(cfg.Lon),
	}
}

// Config returns the simulation's settings.
func (s *Sim) Config() SimConfig { return s.cfg }

// SetMotion changes how the simulated Totem moves.
func (s *Sim) SetMotion(m Motion, bearing int16) {
	s.cfg.Motion, s.cfg.Bearing = m, bearing
	s.bearing = float64(bearing)
}

// SetFlat reports the Totem lying down or upright.
func (s *Sim) SetFlat(flat bool) { s.cfg.Flat = flat }

// SetBattery sets the charge level and whether it is charging.
func (s *Sim) SetBattery(percent int8, charging bool) {
	s.cfg.Percent, s.cfg.Charging = percent, charging
	s.cfg.Volts = voltsFor(percent)
}

// SetClock gives the simulated receiver the wall time, as a GNSS lock
// does.
func (s *Sim) SetClock(wall, now time.Time) {
	s.cfg.Clock = wall.Add(-now.Sub(s.start))
}

// SetFix moves the simulated Totem, or takes its fix away.
func (s *Sim) SetFix(lat, lon float32, has bool) {
	s.lat, s.lon, s.cfg.NoFix = float64(lat), float64(lon), !has
	s.cfg.Lat, s.cfg.Lon = lat, lon
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
		Azimuth:     int16(math.Mod(s.bearing+360, 360)),
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
		s.heading = int16(math.Mod(bearingBetween(s.prevLat, s.prevLon, s.lat, s.lon)+360, 360))
		s.prevLat, s.prevLon, s.headingOdoM = s.lat, s.lon, s.odometerM
	}
}

// battery drains from the starting level over Life, or holds while charging.
func (s *Sim) battery(now time.Time) Battery {
	pct := float64(s.cfg.Percent)
	if !s.cfg.Charging && s.cfg.Life > 0 {
		pct -= float64(s.cfg.Percent) * now.Sub(s.start).Seconds() / s.cfg.Life.Seconds()
	}
	p := int8(min(max(pct, 0), 100))
	return Battery{Volts: voltsFor(p), Percent: p, Charging: s.cfg.Charging, Low: p <= 10}
}

// voltsFor maps a charge level to a cell voltage over the range a Totem
// reports, 3.3 V empty to 4.2 V full.
func voltsFor(percent int8) float32 {
	return 3.3 + 0.9*float32(min(max(percent, 0), 100))/100
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
func NewStatic(s Sensors) SensorSource { return &staticSensors{s} }

// staticSensors reports one fixed reading, as the pos and heading commands
// set it.
type staticSensors struct {
	s Sensors
}

func (f *staticSensors) Read(time.Time) Sensors { return f.s }
