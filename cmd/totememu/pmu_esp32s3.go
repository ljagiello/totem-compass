//go:build tinygo && esp32s3

package main

// The AXP2101 power chip, which on this board is the gate to everything
// else. It sits on the power bus with the real-time clock, on a rail that
// is always on, and it decides whether the sensor bus and the GNSS
// receiver have any power at all: the magnetometer read 0x00 at both of
// its addresses until ALDO1 and ALDO2 were switched on, and LILYGO ship
// ALDO4, which feeds the GNSS, switched off.
//
// Registers, bit positions and the voltage encoding are from XPowersLib,
// the library LILYGO's own examples for this board use — REG/
// AXP2101Constants.h for the addresses and XPowersAXP2101.tpp for what
// each one does. Which rail feeds which part is from Meshtastic's setup
// for this board (src/Power.cpp, the LILYGO_TBEAM_S3_CORE arm), which
// names them in comments; its wording for ALDO2 is that it "is a necessary
// condition for sensor communication".

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
)

// The AXP2101's registers.
const (
	pmuAddr = 0x34

	pmuRegStatus1   = 0x00 // bit 3: a cell is connected
	pmuRegStatus2   = 0x01 // bits 7:5: 0 standby, 1 charging, 2 discharging
	pmuRegChipID    = 0x03 // reads 0x4a
	pmuRegADCEnable = 0x30 // bit 0: measure the battery voltage
	pmuRegBatDetect = 0x68 // bit 0: detect whether a cell is there
	pmuRegVBatHigh  = 0x34 // 13-bit battery voltage in mV, high 5 bits
	pmuRegVBatLow   = 0x35 // ... and its low 8
	pmuRegLDOEnable = 0x90 // one bit per LDO: ALDO1 is 0, ALDO4 is 3
	pmuRegALDO1Volt = 0x92 // low 5 bits set the voltage, high 3 are not ours
	pmuRegBatPct    = 0xa4 // the chip's own fuel gauge, in percent

	pmuChipID = 0x4a

	// The charging state in the top three bits of status 2.
	pmuChargeStateCharging = 1
)

// The rails this program needs, in the order it turns them on. Each is a
// bit in pmuRegLDOEnable and a voltage register pmuRegALDO1Volt + n, which
// is what lets one loop do all of them.
//
// Only these three. ALDO3 feeds the LoRa radio and DCDC3 the M.2 socket,
// neither of which a Totem has any use for, and BLDO1 feeds the SD card;
// leaving them as they came keeps the board's draw down and keeps this
// program away from parts it does not drive. DCDC1 is the ESP32's own
// supply and must never be touched — switching it off stops the program
// that switched it off.
var pmuRails = []struct {
	bit  uint8
	mV   uint16
	what string
}{
	{0, 3300, "ALDO1: magnetometer, IMU and the display header"},
	{1, 3300, "ALDO2: the sensor bus itself, and the real-time clock"},
	{3, 3300, "ALDO4: the GNSS receiver, which ships switched off"},
}

// pmu is the power chip.
type pmu struct {
	bus i2cBus
	log *slog.Logger

	// What has already been complained about, so a part that has failed
	// once does not fill the log. Each is cleared when its own read works
	// again — see warnOnce.
	saidNoAnswer bool
	saidNoVolts  bool
	saidOddVolts bool
	saidNoPct    bool
	saidNoCharge bool
}

// openPMU finds the chip, powers the rails the rest of the board needs,
// and starts the measurements the battery reading depends on.
func openPMU(log *slog.Logger) (*pmu, error) {
	bus, err := boardBus(pmuBusName)
	if err != nil {
		return nil, fmt.Errorf("pmu: %w", err)
	}
	// Built before the first read, so that read goes through the retry and
	// the per-transaction Configure like every other one. Reading the
	// identity register with the bare bus was the one place in this file
	// that did not, and it is the register the file's own header records
	// coming back as 0xff nine times out of ten: one unlucky transaction
	// there and the whole session ran on a fake battery with no GNSS, no
	// IMU and ALDO4 left switched off, which is how LILYGO ship it.
	p := &pmu{bus: bus, log: log}
	// Identity first. A wrong chip here would be asked to switch rails by
	// bit number, and the numbers mean something else on every other part.
	id, err := p.read(pmuRegChipID)
	if err != nil {
		return nil, fmt.Errorf("pmu: read the chip id at %s: %w", hexByte(pmuAddr), err)
	}
	if id != pmuChipID {
		return nil, fmt.Errorf("pmu: %s answered with id %s, want an AXP2101's %s",
			hexByte(pmuAddr), hexByte(id), hexByte(pmuChipID))
	}
	if err := p.powerRails(); err != nil {
		return nil, err
	}
	if err := p.startMeasuring(); err != nil {
		return nil, err
	}
	p.report()
	return p, nil
}

// report prints what the chip says about itself once, at startup. The
// registers are raw on purpose: a battery reading that looks wrong is
// worth being able to check against the datasheet without reflashing, and
// on a board with no cell fitted the interesting part is which of these
// says so.
func (p *pmu) report() {
	status1, err1 := p.read(pmuRegStatus1)
	status2, err2 := p.read(pmuRegStatus2)
	pct, err3 := p.read(pmuRegBatPct)
	mV, err4 := p.millivolts()
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		p.log.Warn("power chip state could not be read", "err", err)
		return
	}
	p.log.Info("power chip",
		"status1", hexByte(status1), "status2", hexByte(status2),
		"cell_connected", status1&(1<<3) != 0,
		"charge_state", status2>>5, "vbat_mv", mV, "gauge_pct", pct)
}

// How long the sensor rails stay off before they are switched back on, and
// which rails get that treatment.
//
// The sensors have to be power cycled, not just switched on. Bringing them
// up from whatever state the last boot left them in is not enough: the
// magnetometer answered 0x00 at both of its addresses with both rails
// already on and 3300 mV measured, which is a part holding the bus down
// rather than a part that is absent. LILYGO's own setup for this board does
// the same thing for the same reason, and says so — "in order to avoid bus
// occupation, during initialization, the SD card and QMC sensor are powered
// off and restarted" — with this delay between.
//
// ALDO1 and ALDO2 only. BLDO1 is in their list as well, but it feeds the SD
// card, which is on the SPI bus this program does not use; a rail is not
// cycled here just because somebody else cycles it.
const (
	pmuSensorRailSettle = 250 * time.Millisecond
	pmuRailALDO1        = 0
	pmuRailALDO2        = 1
)

// powerRails switches on what the sensors and the receiver need.
// Every voltage is written before any rail is switched on, rather than
// setting and enabling each one in turn. A rail coming up disturbs this bus
// enough to lose the next transaction, and interleaving the two meant every
// voltage write but the first landed in the wake of an enable: the write of
// ALDO4's voltage failed some boots and not others, through ten retries
// over 50 ms. Writing them while nothing is switching leaves only the
// enables themselves exposed, and those are followed by a settle and have
// nothing to say back.
func (p *pmu) powerRails() error {
	if err := p.cycleSensorRails(); err != nil {
		return err
	}
	for _, r := range pmuRails {
		if err := p.setVoltage(r.bit, r.mV); err != nil {
			return fmt.Errorf("pmu: set %s to %d mV: %w", r.what, r.mV, err)
		}
	}
	for _, r := range pmuRails {
		if err := p.setBit(pmuRegLDOEnable, r.bit); err != nil {
			return fmt.Errorf("pmu: enable %s: %w", r.what, err)
		}
		time.Sleep(pmuRailSettle)
		p.log.Info("power rail on", "rail", r.what, "mv", r.mV)
	}
	return nil
}

// cycleSensorRails drops the sensor rails so the parts on them restart with
// the bus released. It is skipped when they are already off, which is how
// the board arrives from a cold start: there is nothing to reset, and the
// quarter second is worth not spending.
func (p *pmu) cycleSensorRails() error {
	on, err := p.readControl(pmuRegLDOEnable)
	if err != nil {
		return fmt.Errorf("pmu: read which rails are on: %w", err)
	}
	const sensorRails = 1<<pmuRailALDO1 | 1<<pmuRailALDO2
	if on&sensorRails == 0 {
		return nil
	}
	if err := p.write(pmuRegLDOEnable, on&^sensorRails); err != nil {
		return fmt.Errorf("pmu: switch the sensor rails off: %w", err)
	}
	p.log.Info("sensor rails off to release the bus", "settle_ms", pmuSensorRailSettle.Milliseconds())
	time.Sleep(pmuSensorRailSettle)
	return nil
}

// setVoltage writes one LDO's output voltage. The chip takes it in steps of
// 100 mV above 500 mV in the low five bits, and the top three bits of the
// register belong to something else, so they are read and put back.
func (p *pmu) setVoltage(rail uint8, mV uint16) error {
	const (
		minMV  = 500
		stepMV = 100
		maxMV  = 3500
	)
	if mV < minMV || mV > maxMV || mV%stepMV != 0 {
		return fmt.Errorf("%d mV is not a multiple of %d between %d and %d", mV, stepMV, minMV, maxMV)
	}
	reg := pmuRegALDO1Volt + rail
	was, err := p.readControl(reg)
	if err != nil {
		return err
	}
	return p.write(reg, was&0xe0|uint8((mV-minMV)/stepMV))
}

// startMeasuring turns on the battery voltage ADC and the cell detector.
// Without them the voltage register reads zero and nothing says whether
// that zero is a flat pack or no pack.
func (p *pmu) startMeasuring() error {
	if err := p.setBit(pmuRegADCEnable, 0); err != nil {
		return fmt.Errorf("pmu: enable the battery voltage ADC: %w", err)
	}
	if err := p.setBit(pmuRegBatDetect, 0); err != nil {
		return fmt.Errorf("pmu: enable battery detection: %w", err)
	}
	return nil
}

// battery reads the cell, as emulator.Battery wants it.
//
// With no pack fitted — which is how this board arrived — it says so with
// NoBattery rather than reporting nought volts and nought percent, because
// those are the readings of a cell that has gone flat and the OTA gate and
// the power mode both exist to act on that. The chip's own cell detector
// is what tells them apart.
func (p *pmu) battery() emulator.Battery {
	status1, err := p.read(pmuRegStatus1)
	if err != nil {
		// A chip that has stopped answering is not a board without one:
		// say nothing rather than claim there is no cell, and let the
		// gates stay on.
		warnOnce(p.log, &p.saidNoAnswer, "power chip did not answer", err)
		return emulator.Battery{}
	}
	p.saidNoAnswer = false
	if status1&(1<<3) == 0 {
		return emulator.Battery{NoBattery: true}
	}
	// One read failing means the rest will too, and each one costs the
	// whole retry budget — five tries five milliseconds apart. Five reads
	// of a chip that has gone quiet is a tenth of a second inside a loop
	// that has 5 ms radio windows to hit, so the first failure ends the
	// attempt rather than paying for all of them.
	var b emulator.Battery
	mV, err := p.millivolts()
	if err != nil {
		warnOnce(p.log, &p.saidNoVolts, "battery voltage could not be read", err)
		return b
	}
	p.saidNoVolts = false
	if !plausibleMillivolts(mV) {
		// Not a cell voltage, so not reported as one. An all-0xff read is
		// 0x1fff, which is 8191 mV: over twice what any single lithium
		// cell reaches, and well inside what the status frame can carry,
		// so it would go out to every peer and drive the power mode. This
		// happened before the bus was made reliable, and the band is what
		// makes it a refused reading rather than a believed one.
		warnOnce(p.log, &p.saidOddVolts, "battery voltage is not a cell voltage",
			fmt.Errorf("%d mV is outside %d-%d", mV, minCellMV, maxCellMV))
		return b
	}
	p.saidOddVolts = false
	b.Volts = float32(mV) / 1000
	if pct, err := p.read(pmuRegBatPct); err != nil {
		warnOnce(p.log, &p.saidNoPct, "battery percentage could not be read", err)
		return b
	} else if pct <= 100 {
		// Above 100 is the gauge saying it does not know yet. Left at
		// zero, the node derives a percentage from the voltage instead,
		// which is what the firmware itself does.
		p.saidNoPct = false
		b.Percent = int8(pct)
	}
	status2, err := p.read(pmuRegStatus2)
	if err != nil {
		warnOnce(p.log, &p.saidNoCharge, "charging state could not be read", err)
		return b
	}
	p.saidNoCharge = false
	b.Charging = status2>>5 == pmuChargeStateCharging
	return b
}

// The band a single lithium cell can actually be in. Below the lower figure
// a protection circuit has already disconnected it; above the upper one it
// is not a cell.
const (
	minCellMV = 2500
	maxCellMV = 4600
)

func plausibleMillivolts(mV uint16) bool { return mV >= minCellMV && mV <= maxCellMV }

// warnOnce says something the first time and then stays quiet until the
// thing it is about works again.
//
// A free function because more than one driver needs it: the battery is read
// every two seconds and the IMU twenty times a second, so an unlatched
// warning is a part failing once and then filling the only diagnostic
// channel the board has. The node does the same with warnedClock,
// saidBondLimit and saidRestoreLimit, for the same reason.
//
// The caller clears the flag on the reading that works, which is the half
// that makes a latch useful rather than permanent.
func warnOnce(log *slog.Logger, said *bool, msg string, err error) {
	if *said {
		return
	}
	*said = true
	log.Warn(msg, "err", err)
}

// millivolts reads the 13-bit battery voltage, which the chip reports in
// millivolts across two registers.
func (p *pmu) millivolts() (uint16, error) {
	high, err := p.read(pmuRegVBatHigh)
	if err != nil {
		return 0, err
	}
	low, err := p.read(pmuRegVBatLow)
	if err != nil {
		return 0, err
	}
	return uint16(high&0x1f)<<8 | uint16(low), nil
}

// setBit sets one bit of a register, leaving the rest as they were. Read,
// or, write: the chip has no bit-set command, and a blind write would turn
// off whatever else the register holds.
func (p *pmu) setBit(reg, bit uint8) error {
	was, err := p.readControl(reg)
	if err != nil {
		return err
	}
	if was&(1<<bit) != 0 {
		return nil
	}
	return p.write(reg, was|1<<bit)
}

// Every transaction builds the I2C peripheral again before it starts, and
// this is the whole reason the driver reads reliably.
//
// TinyGo's I2C driver for this chip stops working after a transaction or
// two and does not say so. Ten reads of the chip's identity register in a
// row returned 0x4a once and then 0xff nine times; ten reads of a status
// register whose real value is 0x20 alternated 0x20, 0xff, 0x20, 0xff in
// lockstep. Nothing in the driver resets the controller's state machine —
// there is no bus recovery in it at all — and no amount of waiting or
// retrying brings it back, because what has gone wrong is on this side of
// the wire. Configure rebuilds the clock, the pins and the peripheral, and
// with one in front of every transaction the same twenty reads all came
// back correct: 0x4a ten times and 0x20 ten times.
//
// It costs a few hundred microseconds. The battery is read once per poll,
// so that is a price worth paying to not invent a voltage — and inventing
// one is what the alternative did: 0xff bytes read as a cell at 8.19 V,
// which drove the power mode between eco and normal on successive polls.
//
// The retry on top is for the chip rather than the controller: switching a
// rail on makes the AXP2101 miss a transaction, which a second attempt a
// few milliseconds later gets through.
const (
	pmuTries = 5
	pmuRetry = 5 * time.Millisecond
)

// pmuRailSettle is how long the chip is left alone after a rail is
// switched on before it is asked for anything else. A rail coming up costs
// the transaction that follows it its acknowledgement.
const pmuRailSettle = 25 * time.Millisecond

// transact runs one I2C operation against the chip, rebuilding the bus
// first and retrying a chip that did not answer.
func (p *pmu) transact(what string, op func() error) error {
	var err error
	for try := range pmuTries {
		if try > 0 {
			time.Sleep(pmuRetry)
		}
		// The bus rebuilds itself inside each of its own operations now,
		// which is where that belongs: the scan needs it as much as this
		// driver does. What is left here is the retry, which is for the
		// chip rather than the controller — switching a rail on makes the
		// AXP2101 miss a transaction, and a second attempt gets through.
		if err = op(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s after %d tries: %w", what, pmuTries, err)
}

// readControl reads a register this driver is about to write back, and
// refuses a value that is not a reading.
//
// Every write here is a read-modify-write, and on this bus a read that
// nobody answered comes back as 0xff with no error — see readReg. Feeding
// that through a read-modify-write of the rail enables would compute
// 0xff &^ 0x03 and switch on the LoRa radio, the SD card and every other
// rail the table above deliberately leaves alone. The battery voltage is
// range-checked for exactly this reason; the register that controls power
// deserves it more.
//
// All ones is the only value refused. It is what a silent bus gives, and it
// is not a plausible setting for any of these registers: it would mean
// every rail on, or a voltage field of all ones in a register whose top
// three bits are not ours.
func (p *pmu) readControl(reg uint8) (byte, error) {
	v, err := p.read(reg)
	if err != nil {
		return 0, err
	}
	if v == 0xff {
		return 0, fmt.Errorf("register %s read as all ones, which is a silent bus rather than a setting",
			hexByte(reg))
	}
	return v, nil
}

// read reads one register.
func (p *pmu) read(reg uint8) (byte, error) {
	var val byte
	err := p.transact("read register "+hexByte(reg), func() error {
		var err error
		val, err = p.bus.readReg(pmuAddr, reg)
		return err
	})
	return val, err
}

// write writes one register.
func (p *pmu) write(reg, val uint8) error {
	return p.transact("write "+hexByte(val)+" to register "+hexByte(reg), func() error {
		return p.bus.bus.Tx(pmuAddr, []byte{reg, val}, nil)
	})
}
