//go:build tinygo && esp32s3

package main

// What the board's own parts tell the node, and what still comes from the
// console until a driver for it exists.
//
// The node takes one emulator.SensorSource, so this is where they are
// joined: each field is either read from hardware or held at whatever the
// console last set, and this list says which is which. As a driver lands, a
// field moves from the second group to the first and nothing else changes.
//
// Real: the battery, from the AXP2101. The position, the speed, the course
// and the clock, from the GNSS receiver on UART1. The orientation, from the
// IMU.
//
// Console: the heading. A magnetometer is fitted — a QMC6310 at 0x3c, and an
// earlier version of this comment said there was none, which was wrong — but
// the field around it reads several times the Earth's, so until it has been
// calibrated by a turn through every orientation a bearing from it would be
// confidently wrong. mag_esp32s3.go has the detail.
//
// An operator can take any of it back by hand. Controls says a command must
// not quietly do nothing, so pos, heading, batt, flat and clock all still
// work, and the three that hardware also fills say in the log that they have
// stopped being read from it.

import (
	"log/slog"
	"math"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/gnss"
)

// gnssBlock is how much of the receiver's FIFO is taken at a time. The
// FIFO is 128 bytes and a second of sentences from a receiver with a fix is
// several hundred, so this is drained in a loop until it is empty rather
// than once per poll.
const gnssBlock = 128

// How often each part is actually asked, because the node polls every 5 ms
// and none of them has anything new to say that often.
//
// Two of them are expensive: the battery costs an I2C bus rebuild — 200 of
// those a second, for a value that moves over minutes — and the IMU an SPI
// transaction, against a part producing samples at 31 Hz. Neither is worth
// taking time from the radio, whose windows this same loop has to hit.
//
// The receiver is not rate limited, deliberately. Draining it is a read of
// the FIFO's depth and costs nothing like the other two, while not draining
// it does cost something: at 9600 baud a 128-byte FIFO fills in 133 ms, and
// a sentence lost to an overrun is a second with no position.
const (
	battEvery = 2 * time.Second
	// Half the shortest orientation dwell, so a pose still gets two
	// readings to make its case within the 100 ms it has to hold for.
	imuEvery = 50 * time.Millisecond
	// The compass, at the rate a person can turn: a heading is for pointing
	// at a friend, and ten a second is far more than a hand can follow.
	magEvery = 100 * time.Millisecond
)

// noiseFloorM is the least the odometer will count as a step, whatever the
// receiver claims for its accuracy. Five metres is inside any fix's
// uncertainty, so nothing below it is movement.
const noiseFloorM = 5

// boardSensors reads what the board can read and falls back to a held
// reading for the rest.
type boardSensors struct {
	log  *slog.Logger
	pmu  *pmu
	port *gnssPort
	imu  *imu
	mag  *mag

	// heading is the last bearing the compass gave, in degrees.
	heading int16

	// nmea turns the receiver's bytes into fixes, and block is the buffer
	// they are read into. Kept here rather than made per call so a
	// sentence split across two polls is not lost.
	nmea  gnss.Reader
	block [gnssBlock]byte

	// fix is the last solution the receiver vouched for, already in the
	// node's own shape. Nil until it has one, which indoors is forever.
	fix *emulator.Fix
	// odometerM is how far the receiver has been seen to move, summed over
	// the fixes it gave. The firmware keeps one, so peers expect it.
	//
	// counted is the position the current leg is measured from: the last
	// one far enough from its predecessor to be movement rather than a
	// receiver wandering where it stands.
	odometerM int32
	counted   *emulator.Fix

	// held is the reading the console drives, and the source of every
	// field no driver fills in yet. It is emulator's own static source
	// rather than a struct of our own because the awkward parts of being
	// one are already solved in there: it hands out a copy of the fix so
	// nobody can write through into the readings, and it advances the
	// clock from when it was set instead of pinning it to that instant.
	held emulator.SensorSource
	// ctl is held again, for the console to drive. Kept as its own field
	// so the type assertion happens once, at construction, where it can
	// still be reported.
	ctl emulator.Controls

	// battery is the last reading from the power chip and battAt when it was
	// taken; imuAt is the same for the IMU, whose reading the tracker keeps
	// rather than this.
	battery emulator.Battery
	battAt  time.Time
	imuAt   time.Time

	// orientation applies the firmware's own two-state machine to what the
	// IMU reads, so a board reports upright and flat on the same
	// thresholds and hold times a Totem does.
	orientation emulator.OrientationTracker

	// What an operator has taken over by hand. The hardware stops being
	// read for that field from then on: a command that quietly did nothing
	// would be worse than one that overrides a sensor, and Controls says
	// as much.
	//
	// Only the position can be handed back, with `pos off`, because that is
	// the only one of the three the console has a spelling for. `flat` takes
	// on or off and `batt` takes a percentage, and neither has a word that
	// means "read the part again" — so for those two the way back is a
	// reboot. Giving them one means a three-way argument, a field on
	// Command, and the parse-and-print round trip that is fuzzed, which is
	// more than a diagnostic console needs to be worth; it is written down
	// here rather than left to be discovered.
	battByHand    bool
	posByHand     bool
	orientByHand  bool
	headingByHand bool

	// saidNoIMU is whether the IMU's failure has already been reported,
	// cleared by the next reading that works.
	saidNoIMU bool
	saidNoMag bool

	// iron is the hard-iron offset a calibration turn measured, and
	// calibrated says whether one has been done. Until it has, no heading
	// is taken from the magnetometer: the field on this board reads several
	// times the Earth's, so a bearing from it would be confidently wrong.
	//
	// It is not saved. A reboot needs another turn, which is the honest
	// arrangement while the store's shape is fixed at a name, a colour and
	// the bonds — and a calibration is only good for where the board is
	// mounted anyway.
	iron       emulator.HardIron
	calibrated bool
	magAt      time.Time
	// haveHeading says a compass reading has actually succeeded. Without it
	// the zero value of heading — due north — went into the status frame
	// between calibration finishing and the first read working, which is a
	// bearing nothing ever measured.
	haveHeading bool

	// cal is a calibration turn in progress, collected over successive
	// polls, and calUntil is when it ends. Nil when none is running.
	cal      *emulator.Sweep
	calUntil time.Time
}

// newSensorSource wires up the board's sensors. It returns nil when there
// is nothing to read, which leaves the node on the reading its own
// configuration describes — the behaviour before any of this existed.
//
// Nil on failure is deliberate, and it is the safe direction. A source that
// came back reporting zeroes for a battery it could not read would be a
// flat cell as far as the power mode is concerned, and the board would
// switch itself off a second after starting.
func newSensorSource(log *slog.Logger, fallback emulator.Sensors) emulator.SensorSource {
	p, err := openPMU(log)
	if err != nil {
		log.Warn("no power chip: the battery reading stays as configured", "err", err)
		return nil
	}
	held := emulator.NewStatic(fallback)
	ctl, ok := held.(emulator.Controls)
	if !ok {
		// Not reachable with the emulator package as it stands, and
		// handled anyway. It must not return a *boardSensors: that type
		// has all five setters, so it satisfies Controls whatever ctl
		// holds, and the node would call them, get nil back and report
		// every pos and heading as having worked while they did nothing —
		// which is the one outcome Controls exists to rule out. Handing
		// back the static source alone means the node finds no Controls
		// and says so.
		log.Warn("the static sensor source no longer takes console settings; " +
			"the battery and position are read from hardware and the console cannot set anything")
		return held
	}
	b := &boardSensors{log: log, pmu: p, port: openGNSS(log), held: held, ctl: ctl}
	if m, err := openIMU(log); err != nil {
		// Not fatal. A board with no IMU reports the orientation the
		// console sets, which is what every board did until now.
		log.Warn("no imu: the orientation stays as the console sets it", "err", err)
	} else {
		b.imu = m
	}
	if g, err := openMag(log); err != nil {
		log.Warn("no magnetometer: the heading stays as the console sets it", "err", err)
	} else {
		b.mag = g
	}
	b.report()
	return b
}

// report says what the receiver is saying, once, at startup. Whether there
// is a receiver on the port at all is worth knowing before waiting for a
// fix that is never coming, and a receiver indoors says plenty without
// having one: the sentences below are what this board sends from a desk.
func (b *boardSensors) report() {
	if b.imu != nil {
		// Gravity, as the part sees it. One axis carrying about a whole g
		// and the others near nothing is a board sitting still on a desk;
		// three small numbers would be a part that is answering but not
		// measuring.
		if pitch, roll, err := b.imu.pitchRoll(); err != nil {
			b.log.Warn("imu could not be read", "err", err)
		} else {
			// The pose and the axes it came from, from one reading rather
			// than two: asking twice and discarding the second error
			// printed pitch 0 and roll 0 beside real axes whenever the
			// second read failed its gravity check, which reads as a board
			// lying perfectly flat — the one claim that check refuses.
			x, y, z := b.imu.last()
			b.log.Info("imu reading", "x_g", x, "y_g", y, "z_g", z,
				"pitch_deg", pitch, "roll_deg", roll)
		}
	}
	if b.mag != nil {
		// The field, as the part sees it. Anywhere on the planet its
		// length is between about 0.25 and 0.65 gauss, so a vector near
		// that is the Earth and one near zero is a part that is answering
		// without measuring.
		if x, y, z, err := b.mag.field(); err != nil {
			b.log.Warn("magnetometer could not be read", "err", err)
		} else {
			field := math.Sqrt(x*x + y*y + z*z)
			b.log.Info("mag reading", "x_g", x, "y_g", y, "z_g", z, "field_g", field)
			// Said plainly, because a heading is what a Totem is for and
			// this board is not giving one yet. The part is here and it
			// measures; what it measures at this bench is several times the
			// Earth's field, so a bearing taken from it would be confidently
			// wrong. A magnetometer needs its hard-iron offsets calibrated
			// by being turned through every orientation — which is what the
			// firmware's own 2D and 3D calibration runs do — and that is a
			// person picking the board up, not something this can do while
			// it sits still.
			if field < magFieldMinG || field > magFieldMaxG {
				b.log.Warn("the magnetic field here is not the Earth's, so no heading is taken from it",
					"field_g", field, "earth_g", "0.25 to 0.65",
					"needs", "a calibration turn through every orientation")
			}
		}
	}
	sample := b.port.sample()
	if len(sample) == 0 {
		b.log.Warn("the gnss receiver said nothing in a second; " +
			"the position stays as the console sets it")
		return
	}
	// Feed the sample in rather than throwing it away: it may hold a fix,
	// and a sentence half of which arrived in the sample would otherwise
	// be a bad one.
	b.consume(sample)
	b.log.Info("gnss receiver talking", "bytes", len(sample),
		"sentences", b.nmea.Sentences(), "rejected", b.nmea.Bad(), "fix", b.fix != nil)
}

// Read reports the board's sensors.
func (b *boardSensors) Read(now time.Time) emulator.Sensors {
	s := b.held.Read(now)
	if !b.battByHand {
		if b.battAt.IsZero() || now.Sub(b.battAt) >= battEvery {
			// Only a reading that says something replaces the last one. A
			// failed or implausible read returns the zero Battery, and
			// caching that would publish 0 V and 0% to every peer and
			// close the OTA gate — which is what a flat cell looks like,
			// and the opposite of what this driver means by "could not
			// read it". The previous reading is the honest answer until
			// there is a better one.
			if got := b.pmu.battery(); got != (emulator.Battery{}) {
				b.battery = got
			}
			b.battAt = now
		}
		// And only a reading that says something is reported. Guarding the
		// cache alone was half a fix: until the first read succeeds the
		// cache is itself the zero Battery, and assigning it here threw
		// away the configured fallback and published 0 V at 0% — which is
		// a flat cell to the power mode and to the OTA gate both.
		if b.battery != (emulator.Battery{}) {
			s.Battery = b.battery
		}
	}
	b.drain()
	if b.imu != nil && !b.orientByHand {
		if b.imuAt.IsZero() || now.Sub(b.imuAt) >= imuEvery {
			b.imuAt = now
			if pitch, roll, err := b.imu.pitchRoll(); err != nil {
				// Once, not twenty times a second: an IMU that has
				// stopped answering would otherwise fill the only
				// diagnostic channel the board has.
				warnOnce(b.log, &b.saidNoIMU, "imu could not be read", err)
			} else {
				b.saidNoIMU = false
				b.orientation.Update(pitch, roll, now)
			}
		}
		// The committed state, whether or not it was just asked: the
		// tracker holds a pose for seconds at a time, so the answer
		// between readings is the same one.
		s.Orientation = b.orientation.Orientation()
	}
	if b.cal != nil {
		b.collectCal(now)
	}
	if b.mag != nil && b.calibrated && b.cal == nil && !b.headingByHand {
		if b.magAt.IsZero() || now.Sub(b.magAt) >= magEvery {
			b.magAt = now
			if az, err := b.azimuth(); err != nil {
				warnOnce(b.log, &b.saidNoMag, "magnetometer could not be read", err)
			} else {
				b.saidNoMag = false
				b.heading, b.haveHeading = az, true
			}
		}
		// Only once a reading has succeeded. The zero value of heading is
		// due north, and reporting it between a calibration finishing and
		// the first read working would put a bearing nothing ever measured
		// into the status frame and into every peer's arrow.
		if b.haveHeading {
			s.Azimuth = b.heading
		}
	}
	if b.fix != nil && !b.posByHand {
		// The copy made when the fix arrived, not a fresh one now. The node
		// must not hold a pointer that the next fix writes through, which
		// is why there is a copy at all, but consume already makes one and
		// fixes arrive once a second where this runs two hundred times —
		// and main.go keeps one timer for the life of the loop precisely to
		// stay out of a heap this size.
		s.Fix = b.fix
	}
	return s
}

// drain empties the receiver's FIFO into the parser.
func (b *boardSensors) drain() {
	for {
		n := b.port.read(b.block[:])
		if n == 0 {
			return
		}
		b.consume(b.block[:n])
		if n < len(b.block) {
			return
		}
	}
}

// consume feeds bytes to the parser and keeps the fix that comes out.
func (b *boardSensors) consume(p []byte) {
	got, ok := b.nmea.FeedAll(p)
	if !ok {
		return
	}
	fix := &emulator.Fix{
		Lat: got.Lat, Lon: got.Lon,
		AccuracyM: got.AccuracyM, AltitudeM: got.AltitudeM,
		SpeedKPH: got.SpeedKPH, SatCount: got.SatCount,
		SolutionID:      emulator.SolutionFor(got.AccuracyM),
		HeadingOfMotion: got.CourseDeg,
		Time:            got.Time,
	}
	// The odometer carries over, plus however far this fix is from the last
	// position that counted.
	// How far the receiver must have moved for it to count as movement.
	//
	// A fixed five metres is not enough, and three minutes on a bench
	// proved it: the odometer reached 890 m without the board leaving the
	// desk. A receiver reporting nine metres of accuracy wanders by about
	// that, so a five-metre leg is inside its own uncertainty — and every
	// time the wander crossed the floor it was counted and the anchor moved
	// with it. Movement smaller than the receiver's stated accuracy is not
	// movement it can see, so the floor is whichever is larger.
	// Movement is counted when the receiver says it is moving, and only
	// then measured. Speed comes from RMC's own knots field, so it is the
	// receiver's answer to the question rather than this program's guess
	// from displacement — a fix wandering on a bench reports nothing, and
	// distance alone could not tell that from a slow walk. The floor on top
	// of it is the position noise: whichever is larger of five meters and
	// whatever accuracy the receiver claims, because movement smaller than
	// its own uncertainty is not movement it can see.
	//
	// Both halves earned their place on the bench: with neither, the
	// odometer reached 890 m in three minutes without the board leaving the
	// desk, and with the floor alone it still reached 286 m.
	//
	// Neither is a cure, and it is worth being plain about why. Indoors with
	// four satellites and fourteen metres of claimed accuracy the receiver
	// reported 127 km/h — its own speed field, pinned at the top of the
	// byte — and a position that wandered hundreds of metres. The odometer
	// followed it, because the odometer's job is to report what the receiver
	// says. Filtering harder would mean deciding that a receiver claiming
	// motorway speed is wrong, and sometimes it is in a car. A bad fix
	// produces a bad odometer on a real Totem too; what this code owes is
	// not to invent movement on top of it.
	floor := float64(max(noiseFloorM, int(fix.AccuracyM)))
	moving := fix.SpeedKPH > 0
	if b.counted != nil {
		// From the last position that was counted, not the last fix. A
		// receiver sends one fix a second, so walking covers about 1.4 m
		// between them — under the floor every time, and with the anchor
		// moving each second the distance was thrown away rather than
		// accumulated. A person could walk a kilometre and the odometer
		// would still read nothing; only travel above about 18 km/h ever
		// registered at all.
		moved := emulator.DistanceM(b.counted.Lat, b.counted.Lon, fix.Lat, fix.Lon)
		if moving && moved >= floor {
			b.odometerM += int32(moved)
			anchor := *fix
			b.counted = &anchor
		}
	} else {
		anchor := *fix
		b.counted = &anchor
	}
	fix.OdometerM = b.odometerM
	first := b.fix == nil
	b.fix = fix
	if first {
		b.log.Info("gnss fix", "lat", fix.Lat, "lon", fix.Lon, "sats", fix.SatCount,
			"accuracy_m", fix.AccuracyM, "solution", fix.SolutionID, "utc", fix.Time)
	}
}

// azimuth is the tilt-compensated bearing, with the hard iron taken off.
func (b *boardSensors) azimuth() (int16, error) {
	x, y, z, err := b.mag.field()
	if err != nil {
		return 0, err
	}
	pitch, roll, err := b.imuTilt()
	if err != nil {
		return 0, err
	}
	return bearing(x-b.iron.X, y-b.iron.Y, z-b.iron.Z, pitch, roll), nil
}

// bearing is emulator.Heading rounded to a whole degree that is still a
// bearing.
//
// Heading answers in [0, 360), so anything from 359.5 up rounds to 360 — and
// the node refuses 360 as not a bearing and keeps the previous one, so
// pointing the board just west of north dropped the reading entirely. The
// NMEA parser guards the identical case for the course over ground; this is
// the same guard.
func bearing(x, y, z, pitchDeg, rollDeg float64) int16 {
	deg := int16(emulator.Heading(x, y, z, pitchDeg, rollDeg) + 0.5)
	if deg >= 360 {
		deg = 0
	}
	return deg
}

// imuTilt is the pose the compass is corrected for. Without an IMU the board
// is taken to be flat, which is what an uncompensated compass assumes and is
// worth being explicit about rather than silently true.
func (b *boardSensors) imuTilt() (pitch, roll float64, err error) {
	if b.imu == nil {
		return 0, 0, nil
	}
	// The pose the orientation branch read at most 50 ms ago, if there is
	// one. The part converts at 31 Hz and this is asked every 100 ms, so a
	// fresh burst would usually return the same conversion at twice the SPI
	// traffic, in a loop that has 5 ms radio windows to hit.
	if pitch, roll, ok := b.imu.lastPose(); ok {
		return pitch, roll, nil
	}
	return b.imu.pitchRoll()
}

// calibrateMag starts a calibration turn. It returns at once: the readings
// are collected by Read over the next twenty seconds, because a loop here
// would hold the main loop and with it the radio — see mag_esp32s3.go.
func (b *boardSensors) calibrateMag(now func() time.Time) {
	if b.mag == nil {
		b.log.Warn("no magnetometer to calibrate on this board")
		return
	}
	b.cal, b.calUntil = &emulator.Sweep{}, now().Add(magCalFor)
	b.log.Info("magnetometer calibration: turn the board through every orientation, slowly",
		"seconds", int(magCalFor.Seconds()))
}

// collectCal adds a reading to a calibration in progress and finishes it when
// its time is up.
func (b *boardSensors) collectCal(now time.Time) {
	if b.magAt.IsZero() || now.Sub(b.magAt) >= magEvery {
		b.magAt = now
		if x, y, z, err := b.mag.field(); err == nil {
			b.cal.Add(x, y, z)
		}
	}
	if now.Before(b.calUntil) {
		return
	}
	sweep := b.cal
	b.cal = nil
	dx, dy, dz := sweep.Spans()
	offset, ok := sweep.Offset()
	if !ok {
		b.log.Warn("calibration refused: the board was not turned enough",
			"readings", sweep.Readings(), "span_x_g", dx, "span_y_g", dy, "span_z_g", dz,
			"need_each_g", emulator.SweepMinSpanG)
		return
	}
	b.iron, b.calibrated, b.haveHeading = offset, true, false
	b.log.Info("magnetometer calibrated", "readings", sweep.Readings(),
		"offset_x_g", offset.X, "offset_y_g", offset.Y, "offset_z_g", offset.Z,
		"span_x_g", dx, "span_y_g", dy, "span_z_g", dz)
	b.log.Info("the compass is now read from the magnetometer")
}

// magReport prints the field, and says why no heading is taken when none is.
func (b *boardSensors) magReport() {
	if b.mag == nil {
		b.log.Warn("no magnetometer on this board")
		return
	}
	x, y, z, err := b.mag.field()
	if err != nil {
		b.log.Warn("magnetometer could not be read", "err", err)
		return
	}
	field := math.Sqrt(x*x + y*y + z*z)
	if !b.calibrated {
		b.log.Warn("no heading is taken from the magnetometer until it is calibrated",
			"field_g", field, "earth_g", "0.25 to 0.65", "run", "mag calibrate")
		return
	}
	// The bearing from the reading already in hand, not from another one.
	// field() clears the part's data-ready flag, and at 50 Hz a second read
	// microseconds later finds nothing new — so asking twice reported "no
	// measurement ready yet" instead of the heading, every time.
	pitch, roll, err := b.imuTilt()
	if err != nil {
		b.log.Warn("the tilt could not be read, so the bearing would not be compensated", "err", err)
		return
	}
	b.log.Info("compass", "heading_deg", bearing(x-b.iron.X, y-b.iron.Y, z-b.iron.Z, pitch, roll),
		"field_g", field, "iron_x_g", b.iron.X, "iron_y_g", b.iron.Y, "iron_z_g", b.iron.Z)
}

// The console's half. Each one goes straight through to the held reading,
// and the two that hardware also fills stop it being read.

func (b *boardSensors) SetFix(f *emulator.Fix) {
	if b.ctl == nil {
		return
	}
	b.ctl.SetFix(f)
	// pos off is how the receiver is given back, not another way to take
	// it away. Latching on nil left an operator who had set a position by
	// hand with no command that undid it — and made it worse, because the
	// held fix was cleared too, so the board reported no position at all
	// while a receiver with a good one sat unread until a reboot.
	if f == nil {
		if b.posByHand {
			b.posByHand = false
			b.log.Info("position cleared: the gnss receiver is read again")
		}
		return
	}
	if !b.posByHand {
		b.posByHand = true
		b.log.Warn("position set by hand: the gnss receiver is no longer read")
	}
}

func (b *boardSensors) SetAzimuth(deg int16) {
	if b.ctl == nil {
		return
	}
	b.ctl.SetAzimuth(deg)
	// Latched like the battery and the orientation, and for the same reason:
	// once the compass is calibrated Read overwrites the azimuth on every
	// poll, so without this the heading command reported success and the
	// next status frame five milliseconds later carried the magnetometer's
	// bearing again. A command that quietly does nothing is the one thing
	// Controls exists to rule out.
	if !b.headingByHand {
		b.headingByHand = true
		b.log.Warn("heading set by hand: the magnetometer is no longer read", "deg", deg)
	}
}

func (b *boardSensors) SetFlat(flat bool) {
	if b.ctl == nil {
		return
	}
	b.ctl.SetFlat(flat)
	if !b.orientByHand {
		b.orientByHand = true
		b.log.Warn("orientation set by hand: the imu is no longer read", "flat", flat)
	}
}

func (b *boardSensors) SetClock(wall, now time.Time) {
	if b.ctl != nil {
		b.ctl.SetClock(wall, now)
	}
}

func (b *boardSensors) SetBattery(percent int8, charging bool, now time.Time) {
	if b.ctl == nil {
		return
	}
	b.ctl.SetBattery(percent, charging, now)
	if !b.battByHand {
		b.battByHand = true
		b.log.Warn("battery set by hand: the power chip is no longer read", "percent", percent)
	}
}
