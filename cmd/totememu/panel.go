//go:build tinygo && esp32

package main

// The board's own front panel: one LED and one button.
//
// A Totem has 60 ring pixels, 7 crystal pixels and three inputs. A stock
// ESP32 board has an LED on GPIO2 and the BOOT button on GPIO0, so this
// maps what it does have: the LED follows the crystal, and the button
// stands in for the SOS button, which is the one with the most to say
// (hold starts the alarm, a tap mutes it, three taps start an update).
//
// A board with a real APA106 ring would drive emulator.LEDs().Ring()
// instead; the model is the same either way.

import (
	"log/slog"
	"machine"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
)

const (
	// panelLED is the LED most ESP32 dev boards have, and panelButton is
	// the BOOT button. BOOT reads low while it is pressed.
	panelLED    = machine.GPIO2
	panelButton = machine.GPIO0
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
	panelLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	panelLED.Low()
	panelButton.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	log.Info("front panel", "led", "GPIO2 follows the crystal", "button", "GPIO0 is the SOS button")
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
	if lit != p.lit {
		p.lit = lit
		if lit {
			panelLED.High()
		} else {
			panelLED.Low()
		}
	}
}
