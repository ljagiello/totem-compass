//go:build tinygo && esp32s3

package main

// The board's IMU, a QMI8658 on the SPI bus.
//
// Registers, bit positions and scales are from SensorLib's SensorQMI8658.hpp,
// which LILYGO's own examples for this board use, and the pins are from
// their pin map: the IMU has its own chip select on GPIO34 and shares the
// clock and data lines with the SD card, which has its own on GPIO47.
//
// Chip select is driven here as an ordinary output rather than handed to the
// SPI peripheral. Two devices share this bus, so which one is being spoken
// to has to be this program's decision and not a side effect of configuring
// a peripheral.
//
// Only the accelerometer is set up. What a Totem does with this part is
// decide whether it is upright or lying down, and gravity answers that on
// its own; the gyroscope earns its keep in the firmware's heading fusion,
// which needs a magnetometer this board does not have.

import (
	"errors"
	"fmt"
	"log/slog"
	"machine"
	"math"
	"time"
)

const (
	// The SPI bus, as the vendor's pin map has it.
	pinIMUSCK = machine.GPIO36
	pinIMUSDO = machine.GPIO35 // into the IMU
	pinIMUSDI = machine.GPIO37 // out of it
	pinIMUCS  = machine.GPIO34
	pinIMUINT = machine.GPIO33 // not used: the accelerometer is polled

	// 1 MHz, mode 0, most significant bit first — the settings SensorLib
	// talks to this part with.
	imuClockHz = 1_000_000

	// The registers.
	imuRegWhoAmI = 0x00 // reads 0x05
	imuRegCtrl1  = 0x02 // bit 6 auto-increments the address on a burst read
	imuRegCtrl2  = 0x03 // accelerometer range and output rate
	imuRegCtrl7  = 0x08 // bit 0 enables the accelerometer
	imuRegAccelX = 0x35 // six bytes: x, y, z, each a little-endian int16

	imuWhoAmI = 0x05

	// A read sets the top bit of the address; a write leaves it clear.
	imuRead = 0x80

	// Range 0 is +/-2 g, which is the most sensitive and all that is needed
	// to find which way down is, and rate 8 is 31.25 Hz. The firmware
	// commits an orientation change no faster than every 100 ms, so there
	// is nothing to gain from asking for more.
	imuRange2G   = 0
	imuRate31Hz  = 8
	imuGPerCount = 2.0 / 32768.0
)

// imu is the accelerometer.
type imu struct {
	bus *machine.SPI
	log *slog.Logger
}

// openIMU sets the accelerometer running and checks it is the part the pin
// map says it is.
func openIMU(log *slog.Logger) (*imu, error) {
	pinIMUCS.Configure(machine.PinConfig{Mode: machine.PinOutput})
	pinIMUCS.High() // deselected: the SD card may be listening

	bus := machine.SPI0
	if err := bus.Configure(machine.SPIConfig{
		Frequency: imuClockHz,
		SCK:       pinIMUSCK,
		SDO:       pinIMUSDO,
		SDI:       pinIMUSDI,
		// No CS: this program drives it, see the note at the top.
		CS:   machine.NoPin,
		Mode: 0,
	}); err != nil {
		return nil, fmt.Errorf("imu: configure spi: %w", err)
	}
	m := &imu{bus: bus, log: log}
	id, err := m.read(imuRegWhoAmI)
	if err != nil {
		return nil, fmt.Errorf("imu: read the identity register: %w", err)
	}
	if id != imuWhoAmI {
		return nil, fmt.Errorf("imu: the part on chip select %d answered %s, want a QMI8658's %s",
			int(pinIMUCS), hexByte(id), hexByte(imuWhoAmI))
	}
	if err := m.start(); err != nil {
		return nil, err
	}
	log.Info("imu", "part", "QMI8658", "cs", int(pinIMUCS), "range_g", 2, "rate_hz", 31)
	return m, nil
}

// start turns the accelerometer on.
func (m *imu) start() error {
	// Auto-increment, so the six data registers come back in one read
	// rather than six.
	if err := m.setBit(imuRegCtrl1, 6); err != nil {
		return fmt.Errorf("imu: enable address auto-increment: %w", err)
	}
	if err := m.write(imuRegCtrl2, imuRange2G<<4|imuRate31Hz); err != nil {
		return fmt.Errorf("imu: set the accelerometer range and rate: %w", err)
	}
	if err := m.setBit(imuRegCtrl7, 0); err != nil {
		return fmt.Errorf("imu: enable the accelerometer: %w", err)
	}
	// The first conversion at 31.25 Hz is 32 ms away; reading before it
	// lands gives zeroes, which look like freefall.
	time.Sleep(50 * time.Millisecond)
	return nil
}

// accel reads the three axes, in g.
func (m *imu) accel() (x, y, z float64, err error) {
	var raw [6]byte
	if err := m.readInto(imuRegAccelX, raw[:]); err != nil {
		return 0, 0, 0, err
	}
	g := func(lo, hi byte) float64 {
		return float64(int16(uint16(hi)<<8|uint16(lo))) * imuGPerCount
	}
	return g(raw[0], raw[1]), g(raw[2], raw[3]), g(raw[4], raw[5]), nil
}

// PitchRoll is the board's tilt in degrees, worked out from gravity.
//
// The convention is the usual one for an accelerometer at rest: roll turns
// about the x axis and pitch about y, both zero with the board lying flat
// and its z axis pointing up.
//
// These are the board's axes, not a Totem's. A Totem is a different shape
// with its IMU mounted its own way, so "upright" here means this board
// standing on edge rather than a Totem being carried. The thresholds the
// firmware commits an orientation on are matched, but which physical pose
// trips them is the board's own business.
func (m *imu) pitchRoll() (pitch, roll float64, err error) {
	x, y, z, err := m.accel()
	if err != nil {
		return 0, 0, err
	}
	// Gravity has to be there, and about the right size. A part sitting
	// still measures one g spread across its three axes however it is
	// turned, so a vector far from that length is not a pose — it is a
	// part that is answering without measuring.
	//
	// Checking the length rather than checking for three zeroes, which is
	// what this did first and is not enough: six bytes of 0x01 read as
	// (0.0001, 0.0001, 0.0001) g, which passes a zero test and comes out
	// as a pitch of -35.3 degrees, past the threshold the firmware commits
	// "upright" on. One bad burst could argue for a change of state.
	//
	// The band is wide because it has to survive being picked up. Half a g
	// to two g covers a board being carried and still refuses a reading
	// that carries no gravity at all.
	mag := math.Sqrt(x*x + y*y + z*z)
	if mag < minGravityG || mag > maxGravityG {
		return 0, 0, fmt.Errorf("%w: %.4f g across (%.4f, %.4f, %.4f)", errNoGravity, mag, x, y, z)
	}
	const deg = 180 / math.Pi
	roll = math.Atan2(y, z) * deg
	pitch = math.Atan2(-x, math.Sqrt(y*y+z*z)) * deg
	return pitch, roll, nil
}

// The band a resting or carried board's acceleration vector stays inside.
const (
	minGravityG = 0.5
	maxGravityG = 2.0
)

var errNoGravity = errors.New("imu: the acceleration vector is not gravity, so the part is not measuring")

// read reads one register.
func (m *imu) read(reg byte) (byte, error) {
	var b [1]byte
	if err := m.readInto(reg, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// readInto reads len(p) registers from reg onwards, which needs the
// auto-increment bit of CTRL1 for more than one.
func (m *imu) readInto(reg byte, p []byte) error {
	pinIMUCS.Low()
	defer pinIMUCS.High()
	if _, err := m.bus.Transfer(reg | imuRead); err != nil {
		return err
	}
	for i := range p {
		b, err := m.bus.Transfer(0)
		if err != nil {
			return err
		}
		p[i] = b
	}
	return nil
}

// write writes one register.
func (m *imu) write(reg, val byte) error {
	pinIMUCS.Low()
	defer pinIMUCS.High()
	if _, err := m.bus.Transfer(reg); err != nil {
		return err
	}
	_, err := m.bus.Transfer(val)
	return err
}

// setBit sets one bit of a register and leaves the others as they were.
func (m *imu) setBit(reg, bit byte) error {
	was, err := m.read(reg)
	if err != nil {
		return err
	}
	if was&(1<<bit) != 0 {
		return nil
	}
	return m.write(reg, was|1<<bit)
}
