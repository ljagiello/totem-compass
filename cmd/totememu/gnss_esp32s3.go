//go:build tinygo && esp32s3

package main

// The board's GNSS receiver, on UART1.
//
// TinyGo's UART support for this chip does not reach it. Its Configure sets
// the clock divider and nothing else: no peripheral clock, no pins, no
// receive path — and its UART type only reads through a ring buffer that an
// interrupt nobody installs would have to fill. So the bring-up is here,
// and it is three things: switch the peripheral's clock on, route the pins
// through the GPIO matrix, and read the receive FIFO by polling it.
//
// The pins are LILYGO's for this board: the receiver's data arrives on
// GPIO9 and the pin that talks back to it is GPIO8, with GPIO7 holding the
// module awake and GPIO6 carrying its pulse per second. The receiver has no
// power until the PMU's ALDO4 is on, which is why this runs after that.
//
// Which receiver is on the other end is not assumed. The board is sold with
// a u-blox MAX-M10S or a Quectel L76K, so the driver asks at boot (ask,
// gnss.Identify) and logs the answer. The bench board's says MAX-M10S.
//
// Signal numbers are from ESP-IDF's
// components/soc/esp32s3/include/soc/gpio_sig_map.h. They are not the
// ESP32's: U1RXD is signal 15 on this chip. The file is the same one
// TinyGo's I2C driver takes its 89 to 92 from, which is how these were
// checked.

import (
	"log/slog"
	"machine"
	"runtime/volatile"
	"time"
	"unsafe"

	"device/esp"

	"github.com/ljagiello/totem-compass/gnss"
)

const (
	// The receiver's pins.
	pinGNSSRX   = machine.GPIO9 // the board listens here
	pinGNSSTX   = machine.GPIO8 // ... and answers here
	pinGNSSWake = machine.GPIO7 // held high: an L76K sleeps without it; a MAX-M10S ignores it
	pinGNSSPPS  = machine.GPIO6 // one pulse a second, unused so far

	// U1RXD_IN_IDX and U1TXD_OUT_IDX.
	sigU1RXD = 15
	sigU1TXD = 15

	// gnssBaud is what both receivers this board ships with start at,
	// L76K and u-blox M10 alike.
	gnssBaud = 9600
)

// openGNSS brings UART1 up on the receiver's pins. It does not wait for a
// fix, or for anything at all: whether the receiver is talking is a
// question for whoever reads it.
func openGNSS(log *slog.Logger) *gnssPort {
	// Awake, and then powered: the wake pin is what an L76K watches, and a
	// u-blox M10 ignores it.
	pinGNSSWake.Configure(machine.PinConfig{Mode: machine.PinOutput})
	pinGNSSWake.High()

	// The peripheral's clock. Nothing in the UART answers while this is
	// off, and TinyGo's Configure does not touch it.
	esp.SYSTEM.SetPERIP_CLK_EN0_UART1_CLK_EN(1)
	esp.SYSTEM.SetPERIP_RST_EN0_UART1_RST(0)

	setGNSSBaud(gnssBaud)

	// The pins, through the GPIO matrix. Input and output are set up from
	// opposite ends: the input register is indexed by signal and names the
	// pin, and the output register is indexed by pin and names the signal.
	pinGNSSRX.Configure(machine.PinConfig{Mode: machine.PinInput})
	inSel(sigU1RXD).Set(esp.GPIO_FUNC_IN_SEL_CFG_SEL |
		uint32(pinGNSSRX)<<esp.GPIO_FUNC_IN_SEL_CFG_IN_SEL_Pos)
	pinGNSSTX.Configure(machine.PinConfig{Mode: machine.PinOutput})
	outSel(pinGNSSTX).Set(sigU1TXD)

	log.Info("gnss uart up", "rx", int(pinGNSSRX), "tx", int(pinGNSSTX),
		"wake", int(pinGNSSWake), "baud", gnssBaud)
	return &gnssPort{}
}

// gnssPort reads the receiver's serial port. Named for the port rather than
// for what is on it because the gnss package, which turns what comes off it
// into fixes, has the better claim to the plain name.
//
// It keeps no logger: every diagnostic about the receiver belongs to
// boardSensors, which is what decides whether a fix is worth reporting.
type gnssPort struct{}

// read empties the receive FIFO into p and says how many bytes it took.
// Polled rather than interrupt driven, for the same reason the console is:
// the receive interrupt stops being delivered once the WiFi blob runs.
func (g *gnssPort) read(p []byte) int {
	n := 0
	for n < len(p) {
		if esp.UART1.GetSTATUS_RXFIFO_CNT() == 0 {
			return n
		}
		p[n] = byte(esp.UART1.FIFO.Get() & 0xff)
		n++
	}
	return n
}

// setGNSSBaud clocks UART1 and sets its baud rate.
//
// machine.UART1.Configure cannot be used for this, and at 9600 it is worse
// than useless. All it does is CLKDIV = 40 MHz / baud, and the integer part
// of that register is twelve bits wide: 40000000/9600 is 4166, which does
// not fit in 4095, so the value lands wrapped and the port runs at some
// unrelated speed. That is what this looked like on the board — 154 bytes
// of 0x00 arriving in a second, a line that reads as a break rather than
// as data. Nothing else in Configure is right either: UART1 comes out of
// reset with its clock source unselected and its transmit and receive
// clocks switched off.
//
// So the divider is worked out the way ESP-IDF's uart_ll_set_baudrate does
// it, by first dividing the source clock enough that what is left fits:
// for 9600 from a 40 MHz crystal that gives a pre-divider of 2, an integer
// divider of 2083 and a fractional sixteenth of 5, which comes back out as
// 9600.1 baud.
func setGNSSBaud(baud uint32) {
	const (
		xtalHz  = 40_000_000 // the crystal, as TinyGo's own constant has it
		sclkSel = 3          // 3 selects the crystal
		maxDiv  = 1<<12 - 1  // the integer divider is twelve bits
	)
	// Enough pre-division that the integer divider fits, rounded up.
	pre := (xtalHz + uint32(maxDiv)*baud - 1) / (uint32(maxDiv) * baud)
	if pre == 0 {
		pre = 1
	}
	esp.UART1.SetCLK_CONF_SCLK_SEL(sclkSel)
	esp.UART1.SetCLK_CONF_SCLK_DIV_NUM(pre - 1)
	esp.UART1.SetCLK_CONF_SCLK_DIV_A(0)
	esp.UART1.SetCLK_CONF_SCLK_DIV_B(0)
	esp.UART1.SetCLK_CONF_SCLK_EN(1)
	esp.UART1.SetCLK_CONF_TX_SCLK_EN(1)
	esp.UART1.SetCLK_CONF_RX_SCLK_EN(1)

	// The divider in sixteenths, split into the register's integer and
	// fractional halves.
	div16 := (xtalHz << 4) / (baud * pre)
	esp.UART1.CLKDIV.Set(div16>>4&esp.UART_CLKDIV_CLKDIV_Msk |
		div16&0xf<<esp.UART_CLKDIV_FRAG_Pos)

	// 8N1, and an empty receive FIFO: whatever arrived while the port was
	// running at the wrong speed is not data.
	esp.UART1.SetCONF0_BIT_NUM(3) // 3 is eight data bits
	esp.UART1.SetCONF0_PARITY_EN(0)
	esp.UART1.SetCONF0_STOP_BIT_NUM(1) // 1 is one stop bit
	esp.UART1.SetCONF0_RXFIFO_RST(1)
	esp.UART1.SetCONF0_RXFIFO_RST(0)
}

// sample waits up to about a second for the receiver to say something and
// returns what it said, so a log line can show whether there is a receiver
// there at all and whether it is talking a language we know.
func (g *gnssPort) sample() []byte {
	var buf []byte
	block := make([]byte, 64)
	for range 100 {
		if n := g.read(block); n > 0 {
			buf = append(buf, block[:n]...)
			if len(buf) >= 128 {
				return buf
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return buf
}

// write puts p on the wire to the receiver, waiting whenever the transmit
// FIFO is nearly full. Used only for the few bytes of a query.
func (g *gnssPort) write(p []byte) {
	for _, c := range p {
		for esp.UART1.GetSTATUS_TXFIFO_CNT() >= 120 {
			time.Sleep(time.Millisecond)
		}
		esp.UART1.FIFO.Set(uint32(c))
	}
}

// identify asks the receiver what it is: UBX-MON-VER first, which a u-blox
// answers with its module and firmware, then PCAS06 for a receiver that
// did not. Each question gets up to a second, and the wait stops as soon
// as the answer names the part. The regular output keeps flowing while it
// waits and comes back mixed with the answer; gnss.Identify looks past it.
func (g *gnssPort) identify() gnss.Ident {
	var id gnss.Ident
	for _, q := range [][]byte{gnss.UBXMonVerPoll, gnss.PCASVersionQuery} {
		g.write(q)
		var buf []byte
		block := make([]byte, 64)
		for end := time.Now().Add(time.Second); time.Now().Before(end); {
			if n := g.read(block); n > 0 {
				buf = append(buf, block[:n]...)
				if id = gnss.Identify(buf); id.Model != "" || id.Vendor == "quectel" {
					return id
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		if id.Known() {
			return id
		}
	}
	return id
}

// inSel is the FUNCy_IN_SEL_CFG register for a signal, which names the pin
// that signal is read from.
func inSel(signal uint32) *volatile.Register32 {
	return (*volatile.Register32)(unsafe.Add(unsafe.Pointer(&esp.GPIO.FUNC0_IN_SEL_CFG), uintptr(signal)*4))
}

// outSel is the FUNCx_OUT_SEL_CFG register for a pin, which names the
// signal that pin drives.
func outSel(p machine.Pin) *volatile.Register32 {
	return (*volatile.Register32)(unsafe.Add(unsafe.Pointer(&esp.GPIO.FUNC0_OUT_SEL_CFG), uintptr(p)*4))
}
