package emulator

// The Touch Crystal and the two physical buttons.
//
// A Totem has three inputs: the capacitive crystal on top
// (touch_button_v2.py, over a native c_cap_touch driver), and the power
// and SOS buttons (button.AsyncButton). All three report the same small
// set of gestures — taps, counted within a window, and holds of three
// lengths — and the firmware wires each gesture to a callback in compass.
//
// The recogniser here takes edges, not touches: press at one time,
// release at another. That way the same code serves a real pin on a
// board, a capacitive reading, and the console's touch and button
// commands.

import (
	"fmt"
	"time"
)

// Input names one of the three.
type Input uint8

// The inputs, in the order the firmware constructs them.
const (
	// Crystal is the capacitive Touch Crystal: the one a person uses to
	// pair, and the only one on the top face.
	Crystal Input = iota
	// PowerButton is sw_power, AsyncButton(4, hold_ms=800).
	PowerButton
	// SOSButton is sw_sos, AsyncButton(0, hold_ms=800, long_hold_ms=10000).
	SOSButton
)

// String names the input as the console takes it.
func (i Input) String() string {
	switch i {
	case PowerButton:
		return "power"
	case SOSButton:
		return "sos"
	}
	return "crystal"
}

// Gesture is what the recogniser made of a press.
type Gesture uint8

// The gestures the firmware distinguishes.
const (
	// SingleTap, DoubleTap and TripleTap are presses shorter than a hold,
	// counted within the multi-tap window.
	SingleTap Gesture = iota
	DoubleTap
	TripleTap
	// Hold is a press past hold_ms, which fires while the finger is still
	// down, as button.long_hold_task does.
	Hold
	// LongHold is a press past long_hold_ms, on the inputs that have one.
	LongHold
)

// String names the gesture.
func (g Gesture) String() string {
	switch g {
	case DoubleTap:
		return "double tap"
	case TripleTap:
		return "triple tap"
	case Hold:
		return "hold"
	case LongHold:
		return "long hold"
	}
	return "single tap"
}

// Input timings. The firmware's own values, where the disassembly gives
// them; the native touch driver's validation ranges bracket the rest
// (multi_tap_window 50-2000 ms, short_hold 50-5000, long_hold 50-10000).
const (
	// holdTime is AsyncButton's default hold_ms, which both buttons are
	// built with (compass: AsyncButton(4, hold_ms=800)).
	holdTime = 800 * time.Millisecond
	// longHold is sw_sos's long_hold_ms=10000, the compass reset.
	longHold = 10 * time.Second
	// crystalPairHold is how long the Touch Crystal has to be held to start
	// pairing. The user guide says about 1.2 s.
	crystalPairHold = 1200 * time.Millisecond
	// multiTapWindow is how long the recogniser waits after a release
	// before it decides how many taps it saw. The firmware's value is in
	// bytecode; this sits inside the driver's 50-2000 ms range and is
	// long enough for a triple tap by hand.
	multiTapWindow = 400 * time.Millisecond
	// edgeLockout is v5.0.3's 30 ms timestamp lockout in the IRQ handler,
	// which replaced the debounce coroutine of 5.0.2. An edge inside it
	// is noise, not a press.
	edgeLockout = 30 * time.Millisecond
	// bootDebounce is the wait before touch is enabled after a boot
	// ("Booted: {} ago, waiting: {} before enabling touch"): a board that
	// has just been picked up is still settling.
	bootDebounce = 3 * time.Second
)

// recogniser turns presses and releases into gestures. One per input.
type recogniser struct {
	in      Input
	holdFor time.Duration
	// longFor is zero on an input with no long hold.
	longFor time.Duration

	down     bool
	downAt   time.Time
	lastEdge time.Time
	// held and longHeld record that the hold already fired, so it fires
	// once per press rather than on every poll.
	held, longHeld bool
	taps           int
	lastRelease    time.Time
	// blockedUntil is the Block Touch mechanism, and the post-boot wait.
	blockedUntil time.Time
}

// press reports a finger down. It returns no gesture: a hold is decided
// while the finger stays down, a tap when it lifts.
func (r *recogniser) press(now time.Time) {
	if now.Before(r.blockedUntil) || r.down || now.Sub(r.lastEdge) < edgeLockout {
		return
	}
	r.down, r.downAt, r.lastEdge = true, now, now
	r.held, r.longHeld = false, false
}

// release reports the finger lifted, and returns a tap gesture once the
// multi-tap window closes. A press that already fired a hold ends silently.
func (r *recogniser) release(now time.Time) {
	if !r.down || now.Sub(r.lastEdge) < edgeLockout {
		return
	}
	r.down, r.lastEdge = false, now
	if r.held || r.longHeld {
		r.taps = 0
		return
	}
	r.taps++
	r.lastRelease = now
}

// poll advances the recogniser and returns the gestures that became true
// at now: a hold while the finger is down, or the tap count once the
// window has closed.
func (r *recogniser) poll(now time.Time) []Gesture {
	var out []Gesture
	if r.down {
		if r.longFor > 0 && !r.longHeld && now.Sub(r.downAt) >= r.longFor {
			r.longHeld = true
			out = append(out, LongHold)
		}
		if !r.held && now.Sub(r.downAt) >= r.holdFor {
			r.held = true
			out = append(out, Hold)
		}
		return out
	}
	if r.taps > 0 && now.Sub(r.lastRelease) >= multiTapWindow {
		switch r.taps {
		case 1:
			out = append(out, SingleTap)
		case 2:
			out = append(out, DoubleTap)
		default:
			// Four taps in the window is still a triple tap: the firmware
			// has no fourth gesture, and a person drumming on the button
			// means the last one it knows.
			out = append(out, TripleTap)
		}
		r.taps = 0
	}
	return out
}

// next is when poll has something to do, or the zero time.
func (r *recogniser) next() time.Time {
	switch {
	case r.down && !r.held:
		return r.downAt.Add(r.holdFor)
	case r.down && r.longFor > 0 && !r.longHeld:
		return r.downAt.Add(r.longFor)
	case r.taps > 0:
		return r.lastRelease.Add(multiTapWindow)
	}
	return time.Time{}
}

// block stops the input taking presses for d, as the firmware's Block
// Touch does while an animation or a transfer needs the device left alone.
func (r *recogniser) block(now time.Time, d time.Duration) {
	r.blockedUntil = now.Add(d)
	r.down, r.taps = false, 0
}

// Press reports an input pressed. The gesture arrives from Poll, because
// a hold is a press that has lasted, and a tap count is only known once
// the window closes.
func (n *Node) Press(in Input, now time.Time) {
	if r := n.input(in); r != nil {
		r.press(now)
		n.log.Debug("input pressed", "input", in)
	}
}

// Release reports an input released.
func (n *Node) Release(in Input, now time.Time) {
	if r := n.input(in); r != nil {
		r.release(now)
	}
}

// Tap is a press and a release, for a console that has no pin to watch.
// It is a tap of the shortest length the recogniser counts.
func (n *Node) Tap(in Input, count int, now time.Time) {
	r := n.input(in)
	if r == nil {
		return
	}
	for i := 0; i < count; i++ {
		at := now.Add(time.Duration(i) * 2 * edgeLockout)
		r.press(at)
		r.release(at.Add(edgeLockout + time.Millisecond))
	}
}

// HoldFor is a press held for d, as a console command asks for.
func (n *Node) HoldFor(in Input, d time.Duration, now time.Time) []Packet {
	r := n.input(in)
	if r == nil {
		return nil
	}
	r.press(now)
	// The gestures a hold of this length would have fired, in order.
	for _, g := range r.poll(now.Add(d)) {
		n.onGesture(in, g, now)
	}
	r.release(now.Add(d))
	return n.flush()
}

// touch carries out a console gesture: the same presses and releases a
// finger would make, so the recogniser decides what it was rather than
// the console naming it.
func (n *Node) touch(c Command, now time.Time) []Packet {
	switch c.Gesture {
	case Hold:
		return n.HoldFor(c.Input, time.Duration(c.HoldMs)*time.Millisecond, now)
	case DoubleTap:
		n.Tap(c.Input, 2, now)
	case TripleTap:
		n.Tap(c.Input, 3, now)
	default:
		n.Tap(c.Input, 1, now)
	}
	// The taps are counted when the multi-tap window closes, which is
	// after this call: Poll fires the gesture.
	return n.flush()
}

func (n *Node) input(in Input) *recogniser {
	for i := range n.inputs {
		if n.inputs[i].in == in {
			return &n.inputs[i]
		}
	}
	return nil
}

// newInputs builds the three recognisers with the firmware's timings.
func newInputs(now time.Time) []recogniser {
	rs := []recogniser{
		{in: Crystal, holdFor: crystalPairHold},
		{in: PowerButton, holdFor: holdTime},
		{in: SOSButton, holdFor: holdTime, longFor: longHold},
	}
	for i := range rs {
		// Touch is enabled a moment after boot, as the firmware waits.
		rs[i].blockedUntil = now.Add(bootDebounce)
	}
	return rs
}

// pollInputs runs the recognisers and acts on what they report.
func (n *Node) pollInputs(now time.Time) {
	for i := range n.inputs {
		for _, g := range n.inputs[i].poll(now) {
			n.onGesture(n.inputs[i].in, g, now)
		}
	}
}

// onGesture is the callback wiring in compass: which gesture does what.
// The emulator carries out the ones that make sense off a Totem's own
// hardware and logs the rest.
func (n *Node) onGesture(in Input, g Gesture, now time.Time) {
	n.log.Info("input", "input", in, "gesture", g)
	if n.power.Off() {
		// A device that has powered down answers one gesture: the power
		// button held, which turns it back on. The crystal is not even
		// powered.
		if in == PowerButton && g == Hold {
			n.PowerOn(now)
		}
		return
	}
	switch {
	case in == Crystal && g == Hold:
		// The gesture a person uses to pair: hold the crystal until the
		// animation starts.
		n.Pair(now)
	case in == PowerButton && g == SingleTap:
		n.ToggleBrightness(now)
	case in == PowerButton && g == DoubleTap:
		// user_enable_ble. The emulator has no BLE yet; the flag is kept
		// so a peer sees what a Totem would advertise.
		n.cfg.PhoneConnected = !n.cfg.PhoneConnected
		n.log.Info("BLE advertising toggled", "on", n.cfg.PhoneConnected)
	case in == PowerButton && g == Hold:
		n.PowerOff(now)
	case in == SOSButton && g == SingleTap:
		n.MuteSOS(now)
	case in == SOSButton && g == Hold:
		n.SetSOS(true)
		n.log.Info("SOS started")
	case in == SOSButton && g == TripleTap:
		n.StartOTA(now)
	case in == SOSButton && g == LongHold:
		n.log.Warn("compass reset is not emulated: the bond list stays as it is")
	}
}

// ParseInput reads the console's name for an input.
func ParseInput(s string) (Input, error) {
	switch s {
	case "crystal":
		return Crystal, nil
	case "power":
		return PowerButton, nil
	case "sos":
		return SOSButton, nil
	}
	return 0, fmt.Errorf("%q: want crystal, power or sos", s)
}
