//go:build tinygo && esp32s3

package main

// The T-Beam S3 Supreme's own wiring, and the I2C scan that checks it.
//
// Every pin here is from LILYGO's pin map for this board — the table in
// docs/en/t_beam_supreme/t_beam_supreme_hw.md of their LilyGo-LoRa-Series
// repository, which agrees with the utilities.h its examples compile
// against. Nothing here is inferred from the silkscreen or guessed from a
// photograph, and the `i2c` console command exists so a driver is never
// written against an address that has not answered on this board.

import (
	"fmt"
	"log/slog"

	"machine"
)

// The two I2C buses. The board splits them on purpose: the sensors sit on
// one and the power chip on the other, so a sensor that hangs the bus
// cannot take the charger's registers with it.
const (
	// Bus 0, the sensor bus: magnetometer and the OLED header.
	pinSDA0 = machine.GPIO17
	pinSCL0 = machine.GPIO18
	// Bus 1, the power bus: PMU and real-time clock.
	pinSDA1 = machine.GPIO42
	pinSCL1 = machine.GPIO41
)

// The parts the vendor says are fitted, each with a register to read and
// the value that says it really is that part. Addresses and identity
// registers are from the libraries their own examples use: XPowersLib's
// AXP2101Constants.h for the PMU, and SensorLib's SensorQSTMagnetic.hpp and
// SensorPCF8563.hpp for the magnetometer and clock.
//
// An identity read, not a sweep of every address. A sweep asks only whether
// something acknowledged, and on this bus that question cannot be answered
// — see i2cScan below — so it answered yes 112 times. Reading a register
// whose value is known answers a question worth asking, and it is the same
// question a driver needs answered before it trusts the bus.
type i2cPart struct {
	addr uint16
	reg  uint8
	// want is the value reg must hold; 0 means the part has no identity
	// register and the value is only reported.
	want uint8
	name string
}

var (
	// The sensor bus. Nothing here answers until the PMU powers it: on this
	// board ALDO1 feeds the magnetometer and the display, and ALDO2 is, in
	// the words of Meshtastic's own setup for it, "a necessary condition for
	// sensor communication".
	// All three magnetometers the vendor lists for this board, because
	// which one is fitted is not something to assume: the pin map says
	// "QMC6310U/QMC6310N/QC6309" and they sit at three different
	// addresses, the last of them at 0x7c — outside the range a scan of
	// 0x08 to 0x77 would even reach.
	i2c0Parts = []i2cPart{
		{0x1c, 0x00, 0x80, "QMC6310U magnetometer"},
		{0x3c, 0x00, 0x80, "QMC6310N magnetometer (shares 0x3c with the SH1106 display)"},
		{0x7c, 0x00, 0x90, "QMC6309 magnetometer"},
	}
	// The power bus, which runs off a rail that is always on.
	i2c1Parts = []i2cPart{
		{0x34, 0x03, 0x4a, "AXP2101 power chip"},
		{0x51, 0x02, 0x00, "PCF8563 real-time clock (seconds register, BCD)"},
	}
)

// i2cBus is one bus with the name and pins to report it by, and what is
// meant to be on it.
type i2cBus struct {
	name     string
	bus      *machine.I2C
	sda, scl machine.Pin
	parts    []i2cPart
}

// The front panel, as LILYGO's pin map has it: a button on GPIO0, which
// their header calls BUTTON_PIN and which reads low while it is pressed, and
// no LED. Their map lists every pin this board uses and GPIO2 is not among
// them, so nothing here drives it — a pin that is free on the silkscreen is
// still somebody else's to wire.
const (
	panelHasLED = false
	panelLED    = machine.NoPin
	panelButton = machine.GPIO0
)

// The buses by name, so a driver can ask for the one its part is on rather
// than carry a copy of the pins.
const (
	sensorBusName = "i2c0 (sensors)"
	pmuBusName    = "i2c1 (power)"
)

func boardBuses() []i2cBus {
	return []i2cBus{
		{sensorBusName, machine.I2C0, pinSDA0, pinSCL0, i2c0Parts},
		{pmuBusName, machine.I2C1, pinSDA1, pinSCL1, i2c1Parts},
	}
}

// boardBus finds one bus by name.
//
// Indexed, not ranged. TinyGo 0.42 on xtensa gets == wrong on a string field
// of a struct that came out of a range copy — the same miscompile that made
// every `color <name>` fail on the board, and the reason main.go compares the
// length of a saved name rather than the name. It would fail silently here
// and in the worst possible way: no bus found, so no power chip, so the rails
// LILYGO ship off stay off and the board runs on a hard-coded battery with no
// GNSS and no IMU, with nothing in the log but one line about a missing bus.
// No host test can see it, because this file only builds for the board.
func boardBus(name string) (i2cBus, error) {
	buses := boardBuses()
	for i := range buses {
		if buses[i].name == name {
			return buses[i], nil
		}
	}
	return i2cBus{}, fmt.Errorf("no %s bus in this board's wiring", name)
}

// configure brings the bus up at the slowest standard rate; a scan is not
// in a hurry and 100 kHz is the kindest to a board whose pull-ups are not
// ours to choose.
func (b i2cBus) configure() error {
	return b.bus.Configure(machine.I2CConfig{
		Frequency: 100 * machine.KHz,
		SDA:       b.sda,
		SCL:       b.scl,
	})
}

// readReg reads one register, in a single combined transaction: the
// register number out, the byte back.
//
// A failure here cannot be trusted to mean absence, and there is no way to
// make it. TinyGo's driver for this chip drops a NACK on any transaction
// that ends in a read — from machine_esp32xx_i2c.go:
//
//	case mask&NACK_INT_ST != 0 && !readLast:
//		return errI2CAckExpected
//
// readLast is set for the final byte of every read, so the arm never fires,
// and an address nobody answers comes back as 0xff with a nil error.
//
// Both ways round that were tried are worse. Sending the register number as
// a write of its own does report the NACK, but with a stop in between the
// AXP2101 then returns 0xff for a status register this form reads correctly
// — the pointer does not survive. Probing with a write and then reading the
// combined form corrupted every second read instead: ten reads of a
// register holding 0x20 came back 0x20, 0xff, 0x20, 0xff in lockstep.
//
// So absence is established from the value, never from the error. Every
// part in the tables above carries a register whose contents are known, and
// a reading that is not what that register holds is a part that is not
// there. The one number that has no known value — the battery voltage — is
// held to a band a cell can actually be in, because 0xff read as a cell
// once already, at 8.19 V.
func (b i2cBus) readReg(addr uint16, reg uint8) (byte, error) {
	if err := b.configure(); err != nil {
		return 0, err
	}
	var got [1]byte
	if err := b.bus.Tx(addr, []byte{reg}, got[:]); err != nil {
		return 0, err
	}
	return got[0], nil
}

// writeReg writes one register, rebuilding the bus first like every other
// operation here.
//
// It exists because the PMU's own write did not. Every read went through
// readReg and got the rebuild; the one write went straight to Tx, on a
// controller that the header above explains no amount of retrying recovers.
// A wedge during the rail bring-up therefore failed all five attempts, and
// openPMU returning an error means no battery, no GNSS with ALDO4 left off
// as LILYGO ship it, no IMU and no compass.
func (b i2cBus) writeReg(addr uint16, reg, val uint8) error {
	if err := b.configure(); err != nil {
		return err
	}
	return b.bus.Tx(addr, []byte{reg, val}, nil)
}

// present says whether anything acknowledges this address.
//
// A write, because a write is the only transaction whose NACK this driver
// reports — see readReg. One byte is as short as it gets, and the byte is a
// register number: every part on this board reads a single byte as "point at
// this register", so the pointer moves and nothing is written. A device that
// is not there hears nothing at all.
//
// This is worth having beside the identity read rather than instead of it.
// The read says what a part is; this says whether one is there, and the two
// disagreeing is itself informative — 0x7c answers a read with 0x80 and
// acknowledges nothing, which is a bus returning the last thing it saw
// rather than a magnetometer.
func (b i2cBus) present(addr uint16, reg uint8) bool {
	if err := b.configure(); err != nil {
		return false
	}
	return b.bus.Tx(addr, []byte{reg}, nil) == nil
}

// i2cScan says, for each part the board is meant to carry, whether it
// answers with the identity it should.
//
// It does not sweep the address range. A sweep can only ask whether an
// address acknowledged, and on this chip that question has no dependable
// answer: see readReg, where TinyGo's driver drops the NACK. Sweeping this
// board reported 112 devices on each bus — every address, including both
// magnetometer variants at once and an IMU that is wired to SPI. So the
// scan asks what a driver actually needs to know instead: is the part
// there, and is it the part the pin map says it is.
func i2cScan(log *slog.Logger) {
	buses := boardBuses()
	for i := range buses {
		b := buses[i]
		// Every operation below configures the bus again, so this one is
		// not for them: it is the only check that the bus can be brought up
		// at all, which is worth reporting separately from a part that does
		// not answer on it.
		if err := b.configure(); err != nil {
			log.Warn("i2c bus could not be configured", "bus", b.name,
				"sda", int(b.sda), "scl", int(b.scl), "err", err)
			continue
		}
		for j := range b.parts {
			p := &b.parts[j]
			if !b.present(p.addr, p.reg) {
				log.Info("i2c part absent", "bus", b.name, "addr", hexByte(byte(p.addr)),
					"part", p.name, "detail", "nothing acknowledged the address")
				continue
			}
			got, err := b.readReg(p.addr, p.reg)
			switch {
			case err != nil:
				log.Info("i2c part absent", "bus", b.name, "addr", hexByte(byte(p.addr)),
					"part", p.name, "err", err)
			case p.want == 0:
				log.Info("i2c part answered", "bus", b.name, "addr", hexByte(byte(p.addr)),
					"part", p.name, "reg", hexByte(p.reg), "value", hexByte(got))
			case got == p.want:
				log.Info("i2c part found", "bus", b.name, "addr", hexByte(byte(p.addr)),
					"part", p.name, "id", hexByte(got), "registers", b.dump(p.addr, 0x0c))
			default:
				log.Warn("i2c part did not identify itself", "bus", b.name,
					"addr", hexByte(byte(p.addr)), "part", p.name, "reg", hexByte(p.reg),
					"want", hexByte(p.want), "got", hexByte(got))
			}
		}
	}
}

// dump reads the first n registers, so a part that answers can be told from
// one that answers the same byte to everything. A display's status register
// reads 0x80 as readily as a magnetometer's identity does, and only the
// shape of the map around it says which is which.
func (b i2cBus) dump(addr uint16, n uint8) []string {
	// One burst, not one transaction per register: these parts increment
	// their own address across a read, and a register at a time meant a full
	// peripheral rebuild twelve times over for one log line.
	raw := make([]byte, n)
	if err := b.configure(); err != nil {
		return []string{"bus: " + err.Error()}
	}
	if err := b.bus.Tx(addr, []byte{0}, raw); err != nil {
		return []string{"err: " + err.Error()}
	}
	out := make([]string, 0, n)
	for _, v := range raw {
		out = append(out, hexByte(v))
	}
	return out
}

// hexByte prints a byte the way a datasheet does, so a log line can be read
// against one.
//
// %#04x, not %#02x: the width counts the 0x, so the shorter verb printed
// hexByte(0x05) as "0x5" and hexByte(0x00) as "0x0" — ragged against a
// register table and awkward to grep for.
func hexByte(b byte) string { return fmt.Sprintf("%#04x", b) }
