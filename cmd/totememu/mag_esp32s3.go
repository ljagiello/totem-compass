//go:build tinygo && esp32s3

package main

// The board's magnetometer, a QMC6310 on the sensor bus.
//
// It took two goes to establish that this part is even here, and the way it
// was missed is worth keeping. Read over a bus that had not yet been made
// reliable, 0x1c and 0x7c returned nothing and 0x3c returned 0xff, which
// reads as "no magnetometer, and the thing at 0x3c is the display". All
// three conclusions were wrong. With the bus rebuilt before every
// transaction and absence established by a write rather than a read, 0x3c
// acknowledges and answers 0x80 — the QMC6310's own identity — and its
// register map is a magnetometer's: 0x80 at the identity register, 0x18 at
// the status register, zeros between. A display has no addressable register
// map at all.
//
// Registers, bit layouts and the scale are from SensorLib's
// SensorQSTMagnetic.hpp, which LILYGO's own examples for this board use.

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// magAddr is the N variant's address. The U variant sits at 0x1c and
	// does not answer on this board.
	magAddr = 0x3c

	magRegChipID = 0x00 // reads 0x80
	magRegData   = 0x01 // six bytes: x, y, z, each a little-endian int16
	magRegStatus = 0x09 // bit 0: a new measurement is ready
	magRegCmd1   = 0x0a // mode in bits 1:0, output rate in bits 3:2
	magRegCmd2   = 0x0b // range in bits 3:2; 0x80 is a soft reset

	magChipID = 0x80

	// Continuous measurement at 50 Hz, on the most sensitive range. Two
	// gauss full scale is 15000 counts per gauss, and the Earth's field is
	// between about 0.25 and 0.65 G, so a reading lands in the low
	// thousands of counts with room to spare.
	magModeContinuous = 0x03
	magRate50Hz       = 0x01 << 2
	magRange2G        = 0x03 << 2
	magGaussPerCount  = 1.0 / 15000.0

	// magFieldMinG and magFieldMaxG bracket what the Earth's field can be
	// anywhere on the surface, with room either side for the iron in a
	// board and a bench. A reading outside it is not a measurement of the
	// planet, which is the only thing a compass here is measuring.
	magFieldMinG = 0.15
	magFieldMaxG = 1.2
)

var errMagNotReady = errors.New("mag: no measurement ready yet")

// mag is the magnetometer.
type mag struct {
	bus i2cBus
	log *slog.Logger
}

// openMag resets the part, sets it measuring, and checks it is the part the
// pin map says it is.
func openMag(log *slog.Logger) (*mag, error) {
	bus, err := boardBus(sensorBusName)
	if err != nil {
		return nil, fmt.Errorf("mag: %w", err)
	}
	m := &mag{bus: bus, log: log}
	// Presence by a write, identity by a read: a read alone cannot tell an
	// absent part from a quiet bus on this chip, which is how this one came
	// to be written off in the first place.
	if !bus.present(magAddr, magRegChipID) {
		return nil, fmt.Errorf("mag: nothing acknowledged %s", hexByte(magAddr))
	}
	id, err := m.read(magRegChipID)
	if err != nil {
		return nil, fmt.Errorf("mag: read the identity register: %w", err)
	}
	if id != magChipID {
		return nil, fmt.Errorf("mag: %s answered %s, want a QMC6310's %s",
			hexByte(magAddr), hexByte(id), hexByte(magChipID))
	}
	if err := m.start(); err != nil {
		return nil, err
	}
	log.Info("mag", "part", "QMC6310", "addr", hexByte(magAddr), "range_g", 2, "rate_hz", 50)
	return m, nil
}

// start resets the part and puts it into continuous measurement.
func (m *mag) start() error {
	// The reset is the vendor's, and all three steps of it matter: 0x80, a
	// pause, then zero. Writing the range in place of that final zero is
	// how this was written first, and the range did not stick — the
	// register read back as 0x00, so the part stayed on its default scale
	// and every reading came out to the wrong size. The zero is part of
	// the reset, not a placeholder for the next value.
	if err := m.write(magRegCmd2, 0x80); err != nil {
		return fmt.Errorf("mag: reset: %w", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := m.write(magRegCmd2, 0x00); err != nil {
		return fmt.Errorf("mag: finish the reset: %w", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := m.write(magRegCmd2, magRange2G); err != nil {
		return fmt.Errorf("mag: set the range: %w", err)
	}
	// Read it back. A range that did not take is a scale factor that is
	// wrong by a factor of six, and nothing downstream could tell.
	if got, err := m.read(magRegCmd2); err != nil {
		return fmt.Errorf("mag: read the range back: %w", err)
	} else if got&0x0c != magRange2G {
		return fmt.Errorf("mag: the range register holds %s, want the %s bits",
			hexByte(got), hexByte(magRange2G))
	}
	if err := m.write(magRegCmd1, magRate50Hz|magModeContinuous); err != nil {
		return fmt.Errorf("mag: set the mode and rate: %w", err)
	}
	// One measurement period at 50 Hz, so the first read has something to
	// return rather than the zeros a part that has not converted yet holds.
	time.Sleep(25 * time.Millisecond)
	return nil
}

// field reads the magnetic field in gauss, along the part's own three axes.
func (m *mag) field() (x, y, z float64, err error) {
	status, err := m.read(magRegStatus)
	if err != nil {
		return 0, 0, 0, err
	}
	if status&1 == 0 {
		// Nothing new since the last read. Not an error worth reporting up
		// as a failure — at 50 Hz against a caller that reads far slower
		// it should not happen, and if it does the last heading stands.
		return 0, 0, 0, errMagNotReady
	}
	var raw [6]byte
	if err := m.readInto(magRegData, raw[:]); err != nil {
		return 0, 0, 0, err
	}
	g := func(lo, hi byte) float64 {
		return float64(int16(uint16(hi)<<8|uint16(lo))) * magGaussPerCount
	}
	return g(raw[0], raw[1]), g(raw[2], raw[3]), g(raw[4], raw[5]), nil
}

// read reads one register.
func (m *mag) read(reg uint8) (byte, error) { return m.bus.readReg(magAddr, reg) }

// readInto reads a run of registers in one transaction, which for the data
// registers it has to be.
//
// Six separate reads is how this was written first, and at 50 Hz in
// continuous mode the part moves on between them: the six bytes then come
// from up to six different measurements, so the vector they make is not a
// vector the part ever measured. It showed as a field of 0.15 gauss with
// nothing on the z axis, where the Earth's is 0.25 to 0.65 and points
// steeply down at this latitude. The part increments its own address across
// a burst, which is what makes one transaction possible.
func (m *mag) readInto(reg uint8, p []byte) error {
	if err := m.bus.configure(); err != nil {
		return err
	}
	return m.bus.bus.Tx(magAddr, []byte{reg}, p)
}

// write writes one register.
func (m *mag) write(reg, val uint8) error {
	if err := m.bus.configure(); err != nil {
		return err
	}
	return m.bus.bus.Tx(magAddr, []byte{reg, val}, nil)
}
