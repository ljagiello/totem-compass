//go:build tinygo && esp32s3

package main

// What the board's own parts tell the node, and what still comes from the
// console until a driver for it exists.
//
// The node takes one emulator.SensorSource, so this is where the two are
// joined: each field is either read from hardware or held at whatever the
// console last set, and the list below says which is which. As a driver
// lands, a field moves from the second group to the first and nothing else
// has to change.
//
// Real: the battery, from the AXP2101.
// Console: the position, the heading, the orientation and the clock.

import (
	"log/slog"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
)

// boardSensors reads what the board can read and falls back to a held
// reading for the rest.
type boardSensors struct {
	log *slog.Logger
	pmu *pmu

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

	// battByHand records that an operator set the battery with the batt
	// command. The power chip stops being read from then on: a command
	// that quietly did nothing would be worse than one that overrides the
	// hardware, and Controls says as much.
	battByHand bool
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
			"the battery is read from the chip but pos, heading, flat and clock are not")
		return &boardSensors{log: log, pmu: p, held: held}
	}
	log.Info("sensors", "battery", "AXP2101", "position", "console", "heading", "console")
	return &boardSensors{log: log, pmu: p, held: held, ctl: ctl}
}

// Read reports the board's sensors.
func (b *boardSensors) Read(now time.Time) emulator.Sensors {
	s := b.held.Read(now)
	if b.pmu != nil && !b.battByHand {
		s.Battery = b.pmu.battery()
	}
	return s
}

// The console's half. Each one goes straight through to the held reading,
// except the battery, which also stops the chip being read.

func (b *boardSensors) SetFix(f *emulator.Fix) {
	if b.ctl != nil {
		b.ctl.SetFix(f)
	}
}

func (b *boardSensors) SetAzimuth(deg int16) {
	if b.ctl != nil {
		b.ctl.SetAzimuth(deg)
	}
}

func (b *boardSensors) SetFlat(flat bool) {
	if b.ctl != nil {
		b.ctl.SetFlat(flat)
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
