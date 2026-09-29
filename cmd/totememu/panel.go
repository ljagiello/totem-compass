//go:build tinygo && (esp32 || esp32s3)

package main

// The board's own front panel: a button, and an LED where there is one.
//
// A Totem has 60 ring pixels, 7 crystal pixels and three inputs. This maps
// what a bare board does have: the LED, if any, follows the crystal, and the
// button stands in for the SOS button, which is the one with the most to say
// (hold starts the alarm, a tap mutes it, three taps start an update).
//
// Which pins those are is per chip, in the chip files, because it is a fact
// about a board and not about this program. The LED is optional for the same
// reason: the T-Beam S3 Supreme publishes a pin map with a button on GPIO0
// and no LED anywhere, and driving a pin that map does not mention — GPIO2,
// because most bare ESP32 boards have one there — is the guess that
// chip_esp32.go refuses to make about I2C pins, for the same reason.
//
// A board with a real APA106 ring would drive emulator.LEDs().Ring()
// instead; the model is the same either way.

import (
	"log/slog"
	"machine"
	"strconv"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
)

// panel drives the LED and reads the button.
type panel struct {
	log *slog.Logger
	// lit is what the LED is doing, so it is only written when it changes.
	lit bool
	// down is the button's last state, so only edges reach the node.
	down bool
}

func newPanel(log *slog.Logger) *panel {
	led := "none on this board"
	if panelHasLED {
		panelLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
		panelLED.Low()
		led = "GPIO" + strconv.Itoa(int(panelLED)) + " follows the crystal"
	}
	panelButton.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	log.Info("front panel", "led", led,
		"button", "GPIO"+strconv.Itoa(int(panelButton))+" is the SOS button")
	return &panel{log: log}
}

// poll reads the button and writes the LED. It runs every pass of the
// main loop, which is often enough for a 30 ms debounce and a 25 ms LED
// frame.
func (p *panel) poll(n *emulator.Node, now time.Time) {
	// BOOT is pulled up and grounded by the press.
	if down := !panelButton.Get(); down != p.down {
		p.down = down
		if down {
			n.Press(emulator.SOSButton, now)
		} else {
			n.Release(emulator.SOSButton, now)
		}
	}
	// The LED is one pixel, so it shows whether the crystal is lit at
	// all: that is enough to see a pairing breathe, an SOS blink and the
	// dark of a powered-down device.
	crystal := n.LEDs().Crystal()
	lit := len(crystal) > 0 && crystal[0] != emulator.Off
	if panelHasLED && lit != p.lit {
		p.lit = lit
		if lit {
			panelLED.High()
		} else {
			panelLED.Low()
		}
	}
}
