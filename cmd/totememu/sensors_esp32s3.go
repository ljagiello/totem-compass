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
// Console: the heading, because this board has no magnetometer fitted —
// none of the three addresses the vendor lists for one answers, and the one
// that does answer at 0x3c reads back 0xff, which is the display.
//
// An operator can take any of it back by hand. Controls says a command must
// not quietly do nothing, so pos, heading, batt, flat and clock all still
// work, and the three that hardware also fills say in the log that they have
// stopped being read from it.

import (
	"log/slog"
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
)

// boardSensors reads what the board can read and falls back to a held
// reading for the rest.
type boardSensors struct {
	log  *slog.Logger
	pmu  *pmu
	port *gnssPort
	imu  *imu

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
	odometerM int32

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
	battByHand   bool
	posByHand    bool
	orientByHand bool

	// saidNoIMU is whether the IMU's failure has already been reported,
	// cleared by the next reading that works.
	saidNoIMU bool
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
		// checked anyway: without it pos and heading would stop working
		// and the only clue would be the node saying the readings come
		// from the hardware.
		log.Warn("the static sensor source no longer takes console settings; " +
			"the sensors are read but pos, heading, flat and clock are not")
		return &boardSensors{log: log, pmu: p, held: held}
	}
	b := &boardSensors{log: log, pmu: p, port: openGNSS(log), held: held, ctl: ctl}
	if m, err := openIMU(log); err != nil {
		// Not fatal. A board with no IMU reports the orientation the
		// console sets, which is what every board did until now.
		log.Warn("no imu: the orientation stays as the console sets it", "err", err)
	} else {
		b.imu = m
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
		if x, y, z, err := b.imu.accel(); err != nil {
			b.log.Warn("imu could not be read", "err", err)
		} else {
			pitch, roll, _ := b.imu.pitchRoll()
			b.log.Info("imu reading", "x_g", x, "y_g", y, "z_g", z,
				"pitch_deg", pitch, "roll_deg", roll)
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
	if b.pmu != nil && !b.battByHand {
		if b.battAt.IsZero() || now.Sub(b.battAt) >= battEvery {
			b.battery, b.battAt = b.pmu.battery(), now
		}
		s.Battery = b.battery
	}
	if b.port != nil {
		b.drain()
	}
	if b.imu != nil && !b.orientByHand {
		if b.imuAt.IsZero() || now.Sub(b.imuAt) >= imuEvery {
			b.imuAt = now
			if pitch, roll, err := b.imu.pitchRoll(); err != nil {
				// Once, not twenty times a second. An IMU that has
				// stopped answering would otherwise fill the only
				// diagnostic channel the board has, which is the reason
				// the node latches its own repeated warnings.
				if !b.saidNoIMU {
					b.saidNoIMU = true
					b.log.Warn("imu could not be read", "err", err)
				}
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
	if b.fix != nil && !b.posByHand {
		// A copy: the node must not hold a pointer the next fix writes
		// through, which is the same reason the static source copies.
		fix := *b.fix
		s.Fix = &fix
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
	// The odometer carries over, plus however far this fix is from the
	// last one. Movement under five meters is not counted: a receiver
	// standing still wanders by a few meters a second, and adding that up
	// would have the board claim kilometers from a desk.
	const noiseFloorM = 5
	if b.fix != nil {
		if moved := emulator.DistanceM(b.fix.Lat, b.fix.Lon, fix.Lat, fix.Lon); moved >= noiseFloorM {
			b.odometerM += int32(moved)
		}
	}
	fix.OdometerM = b.odometerM
	first := b.fix == nil
	b.fix = fix
	if first {
		b.log.Info("gnss fix", "lat", fix.Lat, "lon", fix.Lon, "sats", fix.SatCount,
			"accuracy_m", fix.AccuracyM, "solution", fix.SolutionID, "utc", fix.Time)
	}
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
	if b.ctl != nil {
		b.ctl.SetAzimuth(deg)
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
