package emulator

// What a Totem does that is not radio: the ring and the crystal, the
// power state, SOS muting, and the compass dial that points at a peer.
// The node ties them together — an event on the mesh becomes an
// animation, a flat battery becomes a power mode — so a driver only has
// to poll and render.

import (
	"time"
)

// LEDs is the ring and the crystal, for a driver that can render them.
func (n *Node) LEDs() *LEDs { return n.leds }

// Power is the power state: the mode a peer would see, the sleep the
// device would have taken, and the watchdog blockers held.
func (n *Node) Power() *Power { return n.power }

// ToggleBrightness is the power button's single tap.
func (n *Node) ToggleBrightness(now time.Time) {
	b := n.leds.ToggleBrightness()
	n.leds.Tick(now)
	n.log.Info("brightness", "level", b)
}

// SetColor changes the crystal's default colour, as the app does.
func (n *Node) SetColor(c Color, now time.Time) {
	if n.inGroup {
		// "Can't change color - in bond group".
		n.log.Warn("cannot change colour while in a bond group", "color", c)
		return
	}
	n.cfg.ColorID = int8(c)
	n.leds.SetDefaultColor(c)
	n.leds.Tick(now)
	n.log.Info("default crystal colour", "color", c)
}

// MuteSOS is the SOS button's single tap: the alarm keeps going out, the
// device stops blinking about it ("Muting SOS" / "Un-muting SOS").
func (n *Node) MuteSOS(now time.Time) {
	n.sosMuted = !n.sosMuted
	if n.sosMuted {
		n.leds.Stop(AnimSOS, now)
		n.log.Info("muting SOS")
		return
	}
	n.log.Info("un-muting SOS")
	if n.cfg.SOS {
		n.leds.Play(AnimSOS, now)
	}
}

// PowerOff is the power button's hold (device_off) and the low-voltage
// cutoff. The node stops sending, which is what a peer sees.
func (n *Node) PowerOff(now time.Time) {
	if n.power.off {
		return
	}
	// Stop pairing first: clearing the job list would take the timer that
	// ends it with it, and the node would come back still pairing — which
	// holds back every status frame it would otherwise send.
	n.stopPairing(now)
	n.power.off, n.power.mode = true, PowerOff
	n.jobs = nil
	n.leds.Dark(true, now)
	n.log.Info("powered down: the radio windows stop here")
}

// PowerOn brings a device back from PowerOff, as holding the button does.
func (n *Node) PowerOn(now time.Time) {
	if !n.power.off {
		return
	}
	n.power.off, n.power.mode = false, PowerNormal
	n.inputs = newInputs(now)
	n.leds.Dark(false, now)
	n.leds.Play(AnimBoot, now)
	n.scheduleWindow(now)
	if n.clockSet {
		n.scheduleMeshTick(now)
	}
	n.log.Info("powered up")
}

// device runs the parts that are not the radio for this poll: the
// inputs, the power state and the LED frames. It is called from Poll,
// after the sensors have been read.
func (n *Node) device(now time.Time) {
	n.pollInputs(now)
	if mode, changed := n.power.update(n.sensors.Battery, now); changed {
		n.log.Info("power mode", "mode", mode, "batt", n.sensors.Battery.Percent,
			"volts", n.sensors.Battery.Volts)
		switch mode {
		case PowerOff:
			n.log.Warn("voltages too low, powering down", "volts", n.sensors.Battery.Volts)
			n.PowerOff(now)
		case PowerLow:
			n.leds.Play(AnimLowBattery, now)
		}
	}
	// A device with no fix sweeps the ring while its receiver looks for
	// one (anim_gnss_search), and stops when it has one. Only from idle:
	// a pairing or an alarm is worth more than a search.
	switch {
	case n.leds.Animation() == AnimIdle && n.fix() == nil && !n.power.Off():
		n.leds.Play(AnimGNSSSearch, now)
	case n.leds.Animation() == AnimGNSSSearch && n.fix() != nil:
		n.leds.Stop(AnimGNSSSearch, now)
	}
	// The clock has to be settled before the device may sleep through a
	// window, as "Block sleep for GNSS RTC Sync" does.
	n.power.HoldSleep(!n.clockSet || n.pairing || n.ota.State() == OTADownloading)
	n.power.sleep(now, n.Next())
	if !n.power.Off() {
		// A device that is off has no compass and no frames to draw.
		n.updateDial()
		n.leds.Tick(now)
	}
}

// updateDial points the compass at the first bonded peer whose position
// is known, in the colour that peer was given when it bonded.
func (n *Node) updateDial() {
	f := n.fix()
	if f == nil {
		n.leds.SetDial(-1, ColorWhite)
		return
	}
	for _, mac := range n.order {
		p := n.peers[mac]
		if !p.hasCoords {
			continue
		}
		deg := int16(norm360(bearingBetween(float64(f.Lat), float64(f.Lon), float64(p.lat), float64(p.lon))))
		n.leds.SetDial(deg, p.color)
		return
	}
	n.leds.SetDial(-1, ColorWhite)
}

// Describe is the whole device in one line, for a console.
func (n *Node) Describe() string {
	return n.leds.Describe() + "; " + n.power.Describe()
}
