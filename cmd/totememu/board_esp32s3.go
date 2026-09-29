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
func boardBus(name string) (i2cBus, error) {
	for _, b := range boardBuses() {
		if b.name == name {
			return b, nil
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

// readReg reads one register, writing the register number and reading the
// byte back in a single transaction.
//
// The error is not enough on its own to say a device is there. TinyGo's
// driver for this chip ignores a NACK on a transaction that ends in a read
// — from machine_esp32xx_i2c.go:
//
//	case mask&NACK_INT_ST != 0 && !readLast:
//		return errI2CAckExpected
//
// readLast is set for the final byte of any read, so the arm never fires
// and an address nobody answers reports success with whatever the bus was
// left holding. The value has to be judged, which is why every part here
// carries a register whose contents are known.
// It is two transactions rather than one for that reason. The register
// number goes out as a write of its own, which does report a NACK, and the
// byte is read after it. Done as a single combined transaction the whole
// thing counts as ending in a read, and then a device that is not there —
// or a bus that has stopped working — comes back as 0xff with no error at
// all. That is not a theoretical worry: it was read as a battery at 8.19 V,
// which is 0x1ffe, which is what thirteen bits of 0xff look like.
func (b i2cBus) readReg(addr uint16, reg uint8) (byte, error) {
	var got [1]byte
	if err := b.bus.Tx(addr, []byte{reg}, got[:]); err != nil {
		return 0, err
	}
	return got[0], nil
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
	for _, b := range boardBuses() {
		if err := b.configure(); err != nil {
			log.Warn("i2c bus could not be configured", "bus", b.name,
				"sda", int(b.sda), "scl", int(b.scl), "err", err)
			continue
		}
		for _, p := range b.parts {
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
					"part", p.name, "id", hexByte(got))
			default:
				log.Warn("i2c part did not identify itself", "bus", b.name,
					"addr", hexByte(byte(p.addr)), "part", p.name, "reg", hexByte(p.reg),
					"want", hexByte(p.want), "got", hexByte(got))
			}
		}
	}
}

// hexByte prints a byte the way a datasheet does, so a log line can be read
// against one.
func hexByte(b byte) string { return fmt.Sprintf("%#02x", b) }
