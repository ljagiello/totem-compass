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
	// Checked like every other colour from outside: the console validates
	// its own argument, but a driver or an embedding caller does not, and
	// State writes this to flash.
	c = paletteColor(n.log, "caller", int8(c), n.leds.DefaultColor())
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
	// And whatever was waiting for the next radio window. A notice
	// queued a moment before the power went would otherwise go out on
	// the next power-up, telling a peer about something that happened on
	// the far side of a power cycle it never saw. Refusing to queue it
	// afterwards is only half of that; this is the state that survives.
	clear(n.outbox)
	// And the low-battery reminder owed from before it: the pack may be
	// charged while the device is down, and PowerOn sets the mode back
	// to normal directly rather than through applyPowerMode, so nothing
	// else would ever clear it. A device that came back on a full pack
	// flashed "battery low" as soon as its power-up ring ended.
	n.lowOwed = false
	// And the mesh replies owed from before it. sendOutbox repeats the
	// last locate reply for ten seconds, so one owed at the moment the
	// power went would go out on the next power-up — answering, with a
	// fresh position and the old UID, a request from the far side of a
	// power cycle the peers never saw.
	n.replyAt, n.replyUID, n.originUID = time.Time{}, 0, 0
	clearPending(n.inputs)
	n.leds.Dark(true, now)
	n.log.Info("powered down: the radio windows stop here")
}

// PowerOn brings a device back from PowerOff, as holding the button does.
func (n *Node) PowerOn(now time.Time) {
	if !n.power.off {
		return
	}
	n.power.off, n.power.mode = false, PowerNormal
	// Coming back up is a boot: it runs the post-boot touch wait and the
	// power-up ring, and on the device the wake restarts main.py and
	// builds modes again. The counters modes starts at zero start again
	// with it — the same rule that stops them being restored from flash.
	n.power.startRun(now)
	n.inputs = newInputs(now)
	n.leds.Dark(false, now)
	n.leds.Play(AnimBoot, now)
	// Guarded, because something may have armed a chain while the device
	// was down: two window chains means a status frame to every peer
	// twice a period, for the rest of the run.
	if !n.hasJob(jobWindow) {
		n.scheduleWindow(now)
	}
	if n.clockSet && !n.hasJob(jobMeshTick) {
		n.scheduleMeshTick(now)
	}
	n.log.Info("powered up")
}

// device runs the parts that are not the radio for this poll: the
// inputs, the power state and the LED frames. It is called from Poll,
// after the sensors have been read.
func (n *Node) device(now time.Time) {
	n.pollInputs(now)
	// Worked out again in each place that needs it, rather than once
	// into a variable. That hoist — which is what a reader of this file
	// would reach for, and what a review asked for twice — crashes the
	// board: with it, the boot-time settings read faults in
	// flashSector.ReadAt with EXCCAUSE 0 at an address inside the
	// flash-cached .text, so the cache is handing the CPU rubbish where
	// an instruction should be. Bisected to this one line, reproducible
	// on every boot, and nothing else in the change mattered.
	//
	// The driver has been near this edge all along — the note in
	// flash.go about 0xbad00bad is the same cache misbehaving — and
	// moving any code shifts what lands where. Until that is understood,
	// the shape that boots is the shape that stays. No host test can see
	// any of this.
	if w := n.wantedAnimation(); n.power.charging(n.sensors.Battery, now) &&
		restful(w) && n.leds.Animation() == w {
		n.power.takeCharger()
		// On the charger: the ring runs the powerup animation again, which
		// is what power_conn_new does once v_in has settled. It is a timed
		// animation, so it goes on only over a strip that is resting —
		// plugging in is the obvious thing to do during a download or an
		// alarm, and neither should lose the ring for two seconds because
		// of it.
		n.leds.Play(AnimBoot, now)
		n.log.Info("charger connected", "volts", n.sensors.Battery.Volts,
			"batt", n.sensors.Battery.Percent)
	}
	// The low-battery reminder, once the strip is free. Owed by
	// applyPowerMode rather than played there, so that coming up on a
	// low pack shows the power-up ring and then the reminder, in that
	// order rather than one over the other.
	//
	// Beside the charger's ring and before the sleep below, which is
	// the same place for the same two reasons: both are passing events
	// that may only take a resting strip, and the animation either
	// starts has to be in the frame deadline that sleep() measures
	// against, or the device books itself asleep through the frames it
	// is about to draw.
	//
	// It cannot be spent on a device that is off, because PowerOff
	// clears it: this used to be guarded by the powered-on block below
	// instead, which left the flag set across a power cycle and flashed
	// "battery low" at a pack that had been charged in the meantime.
	if w := n.wantedAnimation(); n.lowOwed && restful(w) && n.leds.Animation() == w {
		n.lowOwed = false
		n.leds.Play(AnimLowBattery, now)
	}
	// The clock has to be settled before the device may sleep through a
	// window, as "Block sleep for GNSS RTC Sync" does.
	n.power.HoldSleep(!n.clockSet || n.pairing || n.ota.Running())
	n.power.sleep(now, n.Next())
	if !n.power.Off() {
		// A device that is off has no compass and no frames to draw.
		n.updateDial()
		n.leds.Tick(now)
		// What the device's own state calls for, put back whenever the
		// strip is resting or showing something that no longer applies.
		// The strip keeps no memory of what was playing underneath a
		// flash: the node knows whether the alarm is still on, and this
		// is where it says so. After the tick, so an animation that has
		// just ended is replaced in the same pass.
		if want := n.wantedAnimation(); openEnded(n.leds.Animation()) && n.leds.Animation() != want {
			n.leds.Play(want, now)
			n.leds.Tick(now)
		}
	}
}

// applyPowerMode moves the power mode for the current reading and acts on
// it: the cutoff has to actually power the device down, and a battery
// that has just gone low has to say so on the ring.
//
// Everywhere the reading changes goes through here. Calling update and
// dropping what it reports leaves the mode moved and nothing done about
// it — and because it only reports a change once, the next poll sees
// nothing to do either. A node restored on a flat pack reported power
// mode "off" while its radio windows kept running.
func (n *Node) applyPowerMode(now time.Time) {
	mode, changed := n.power.update(n.sensors.Battery)
	if !changed {
		return
	}
	n.log.Info("power mode", "mode", mode, "batt", n.sensors.Battery.Percent,
		"volts", n.sensors.Battery.Volts)
	// The reminder belongs to the mode, so it goes wherever the mode
	// goes. One owed while the ring was busy and not cleared here
	// flashed "battery low" at someone whose pack was full and on the
	// charger, minutes later: the mode had gone back to normal and
	// nothing had told the reminder.
	n.lowOwed = false
	switch mode {
	case PowerOff:
		n.log.Warn("voltages too low, powering down", "volts", n.sensors.Battery.Volts)
		n.PowerOff(now)
	case PowerLow:
		// Owed rather than played here: this is a reminder, and a boot
		// animation, an alarm or a download is the picture someone is
		// actually looking at. A device built on a low pack used to
		// stamp the reminder over its own power-up ring, which the
		// firmware plays to the end. It goes on as soon as the strip is
		// resting.
		//
		// Owed with no deadline, which is where it parts company with
		// the charger's ring above. That one says "the charger just went
		// in", which stops being true, so it expires; this one says the
		// pack is low, which stays true for exactly as long as the mode
		// does — and the line above is what ends it.
		n.lowOwed = true
	}
}

// restful reports whether an animation is one the device shows because
// nothing is happening, rather than because something is. Only over one
// of these may a passing event — the charger going in — take the ring:
// an alarm, a pairing window or an update in flight is the picture
// someone is actually looking at.
func restful(a Animation) bool { return a == AnimIdle || a == AnimGNSSSearch }

// wantedAnimation is what the device's state calls for when nothing
// timed is playing: an update in flight, then the alarm, then a pairing
// window, then the search for a fix, then rest.
func (n *Node) wantedAnimation() Animation {
	switch {
	case n.power.Off():
		return AnimIdle
	case n.ota.State() == OTAChecking:
		return AnimWiFi
	case n.ota.Running():
		// An update drives the ring itself while it blocks the loop, but
		// nothing enforces that a transport blocks: a poll that landed
		// anywhere between the download and the reboot would otherwise
		// wipe the progress ring.
		return AnimOTA
	case n.cfg.SOS && !n.sosMuted:
		return AnimSOS
	case n.pairing:
		return AnimPairing
	case n.fix() == nil:
		return AnimGNSSSearch
	}
	return AnimIdle
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
