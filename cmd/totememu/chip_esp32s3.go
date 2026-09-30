//go:build tinygo && esp32s3

package main

// What the console and the random numbers need on the ESP32-S3. The esp32
// file beside this one answers the same two questions for the classic chip.

import (
	"machine"
	"time"
)

// consoleByte returns the next byte typed on the console.
//
// Buffered has to be called, and not just for the count. On this chip
// machine.Serial is the USB Serial/JTAG controller, whose TinyGo driver
// (machine_esp32xx_usb.go) moves bytes out of the EP1 receive FIFO in
// exactly one place: Buffered. Its interrupt handler only masks and clears
// the interrupt, so nothing else ever drains the FIFO, and ReadByte on its
// own returns whatever is already in the ring buffer — forever nothing.
//
// It also cannot be asked the way the UART driver can. That ReadByte
// answers (0, nil) on an empty buffer rather than an error, so the classic
// chip's "read it and check err" reports success for a byte nobody typed:
// it fed the line reader an endless stream of NULs while the host's input
// sat unread in the FIFO. Hence the count first, and the byte second.
func consoleByte() (byte, bool) {
	if machine.Serial.Buffered() == 0 {
		return 0, false
	}
	b, err := machine.Serial.ReadByte()
	return b, err == nil
}

// hwRandom returns a hardware random number for seeding.
//
// machine.GetRNG rather than a register of our own: TinyGo implements it for
// this chip, reading RNG_DATA through the generated register definitions and
// first making sure the two clocks the generator draws its noise from — the
// ADC clock and the fast RTC clock — are actually running. The classic
// ESP32's file reads the register directly only because TinyGo has no
// GetRNG for that chip.
//
// The address matters here: the classic chip's RNG_DATA_REG at 0x3FF75144
// is not mapped on the S3, so the register read that works next door would
// not have produced random numbers on this board.
func hwRandom() uint64 {
	hi, err1 := machine.GetRNG()
	lo, err2 := machine.GetRNG()
	if err1 != nil || err2 != nil {
		// GetRNG cannot fail on this chip today. If that changes, a clock
		// that has at least counted the jitter of getting this far beats
		// seeding two zeros.
		return uint64(time.Now().UnixNano())
	}
	return uint64(hi)<<32 | uint64(lo)
}
