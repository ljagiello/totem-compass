//go:build tinygo && esp32

package main

// What the console and the random numbers need on the classic ESP32. The
// esp32s3 file beside this one answers the same two questions for that chip,
// where both reach different hardware.

import (
	"log/slog"
	"machine"
	"runtime/volatile"
	"unsafe"

	"device/esp"

	"github.com/ljagiello/totem-compass/emulator"
)

// The front panel's pins. A bare ESP32 module usually has an LED on GPIO2
// and the BOOT button on GPIO0, and BOOT reads low while it is pressed.
// This is a guess about a module rather than a board, which is as good as it
// gets when there is no pin map to read — and it is why the S3 file, which
// has one, says something different.
const (
	panelHasLED = true
	panelLED    = machine.GPIO2
	panelButton = machine.GPIO0
)

// i2cScan answers the console's i2c command. There is nothing to scan
// here: this build targets a bare ESP32 module, where the sensor pins are
// whatever the person wiring it chose, so there is no bus to configure and
// a guess at two pins would drive somebody's wiring. The S3 board file has
// a real scan because that board has a published pin map.
func i2cScan(log *slog.Logger) {
	log.Warn("no I2C buses are wired on this build; the sensor pins of a bare ESP32 module are not known")
}

// newSensorSource has nothing to offer on this chip, for the same reason:
// no known parts to read. Nil leaves the node on the reading its own
// configuration describes, which is what this board has always done.
func newSensorSource(_ *slog.Logger, _ emulator.Sensors) emulator.SensorSource {
	return nil
}

// consoleByte returns the next byte typed on the console. The UART receive
// interrupt stops firing once the WiFi blob runs, so the RX FIFO is polled
// as well, through its AHB address (UART0 base + 0x200C0000) as TinyGo's
// driver reads it to avoid an ESP32 silicon erratum.
func consoleByte() (byte, bool) {
	if b, err := machine.Serial.ReadByte(); err == nil {
		return b, true
	}
	if esp.UART0.GetSTATUS_RXFIFO_CNT() == 0 {
		return 0, false
	}
	return (*volatile.Register8)(unsafe.Add(unsafe.Pointer(esp.UART0), 0x200C0000)).Get(), true
}

// hwRandom reads RNG_DATA_REG, which gives true random numbers while the
// radio runs (ESP32 Technical Reference Manual, Random Number Generator
// chapter, register RNG_DATA_REG at 0x3FF75144).
//
// Read straight from the register because TinyGo's machine.GetRNG does not
// exist for this chip: its build tag covers esp32c3 and esp32s3, not esp32.
func hwRandom() uint64 {
	reg := (*volatile.Register32)(unsafe.Pointer(uintptr(0x3ff75144)))
	return uint64(reg.Get())<<32 | uint64(reg.Get())
}
