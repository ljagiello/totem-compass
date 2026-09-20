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
	"log/slog"
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

// LogValue makes a log line say "sos" rather than 2, in JSON as well as
// in text: the CLI reads the JSON, and a number there means nothing.
func (i Input) LogValue() slog.Value { return slog.StringValue(i.String()) }

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

// LogValue names the gesture in a log line, in either format.
func (g Gesture) LogValue() slog.Value { return slog.StringValue(g.String()) }

// Input timings. The firmware's own values, where the disassembly gives
// them; the native touch driver's validation ranges bracket the rest
// (multi_tap_window 50-2000 ms, short_hold 50-5000, long_hold 50-10000).
const (
	// holdTime is AsyncButton's default hold_ms, which both buttons are
	// built with (compass: AsyncButton(4, hold_ms=800)).
	holdTime = 800 * time.Millisecond
	// longHold is sw_sos's long_hold_ms=10000, the compass reset.
	longHold = 10 * time.Second
	// maxTaps is as far as the tap count is worth keeping: the firmware's
	// last gesture is the triple tap, and poll reads anything at or past
	// three as one.
	maxTaps = 4
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
func (r *recogniser) press(now time.Time) bool {
	if now.Before(r.blockedUntil) || r.down || now.Sub(r.lastEdge) < edgeLockout {
		return false
	}
	r.down, r.downAt, r.lastEdge = true, now, now
	r.held, r.longHeld = false, false
	return true
}

// release reports the finger lifted, and returns a tap gesture once the
// multi-tap window closes. A press that already fired a hold ends silently.
//
// A release always ends the press, even one inside the edge lockout. The
// lockout is there to swallow contact bounce, and swallowing the release
// with it is what a bouncing button actually produces: the recogniser
// would stay down with nobody touching it, and 800 ms later poll would
// report a hold no one made — on the power button, a device that switches
// itself off; on SOS, an alarm that starts by itself. The driver calls
// this straight off a GPIO edge, so the pair arrives exactly that way.
func (r *recogniser) release(now time.Time) {
	if !r.down {
		return
	}
	bounce := now.Sub(r.lastEdge) < edgeLockout
	r.down, r.lastEdge = false, now
	if r.held || r.longHeld {
		r.taps = 0
		return
	}
	if bounce {
		// Ended, but not a tap: nothing a finger did that fast is one.
		return
	}
	// Counted up to the last gesture there is and no further. The
	// firmware has no fourth, and poll already reads anything past three
	// as a triple tap, so the extra counting buys nothing — and a caller
	// that presses and releases faster than it polls, which fuzzing does
	// and a stalled loop could, would otherwise grow this without limit.
	if r.taps < maxTaps {
		r.taps++
	}
	r.lastRelease = now
}

// poll advances the recogniser and returns the gestures that became true
// at now: a hold while the finger is down, or the tap count once the
// window has closed.
func (r *recogniser) poll(now time.Time) []Gesture {
	var out []Gesture
	if r.down {
		// In the order a finger makes them: the hold at 800 ms, the long
		// hold at ten seconds. One poll can land past both — a blocking
		// update or a flash erase stalls the loop — and reporting the
		// long hold first would run the compass reset before the alarm.
		if !r.held && now.Sub(r.downAt) >= r.holdFor {
			r.held = true
			out = append(out, Hold)
		}
		if r.longFor > 0 && !r.longHeld && now.Sub(r.downAt) >= r.longFor {
			r.longHeld = true
			out = append(out, LongHold)
		}
		return out
	}
	// A tap count is only decided once the finger is up: a press that is
	// still down may yet become a double tap.
	if !r.down && r.taps > 0 && now.Sub(r.lastRelease) >= multiTapWindow {
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

// next is when poll has something to do, or the zero time. While the
// finger is down it is the next hold; the tap count cannot resolve until
// the finger lifts, so offering its deadline here would hand a caller a
// time at which poll does nothing — and a caller that walks time forward
// would never get past it.
func (r *recogniser) next() time.Time {
	if r.down {
		switch {
		case !r.held:
			return r.downAt.Add(r.holdFor)
		case r.longFor > 0 && !r.longHeld:
			return r.downAt.Add(r.longFor)
		}
		return time.Time{}
	}
	if r.taps > 0 {
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
	r := n.input(in)
	if r == nil {
		return
	}
	// What the recogniser did with it, not what was asked: an edge
	// inside the lockout, during the post-boot wait or while touch is
	// blocked for an update is refused, and a driver chasing a dead
	// button should not find presses in the log that never happened.
	if r.press(now) {
		n.log.Debug("input pressed", "input", in)
		return
	}
	// At debug, unlike the console's own tap and hold: this is driven
	// straight off a pin, and the edge lockout exists precisely to
	// swallow the bounce of one physical press. Announcing each swallowed
	// edge would put the noise on the console and leave the press that
	// took it at a lower level than the ones that did not.
	n.log.Debug("input press ignored: already down, too soon after the last edge, or touch is blocked",
		"input", in)
}

// Release reports an input released.
func (n *Node) Release(in Input, now time.Time) {
	if r := n.input(in); r != nil {
		r.release(now)
	}
}

// tapHold is how long a synthetic tap holds the input down, and tapStep
// how far apart two of them start. Both clear the edge lockout on the
// edge before them: a press inside that window is noise, so taps made
// any closer together are not counted at all and a double tap arrives as
// a single one.
const (
	tapHold = edgeLockout + time.Millisecond
	tapStep = tapHold + edgeLockout + time.Millisecond
)

// Tap is a press and a release, for a console that has no pin to watch.
// It is a tap of the shortest length the recogniser counts.
func (n *Node) Tap(in Input, count int, now time.Time) {
	r := n.input(in)
	if r == nil {
		return
	}
	for i := 0; i < count; i++ {
		at := now.Add(time.Duration(i) * tapStep)
		// Only released if this press was ours: a finger may already be
		// on the button — a real one on the board, or a hold the console
		// started — and releasing that would end someone else's press
		// before it matured into a hold.
		if !r.press(at) {
			// Which one, because the ones before it did land and the
			// recogniser will report the count it actually saw.
			n.log.Info("tap ignored: the input is already down or blocked",
				"input", in, "tap", i+1, "of", count)
			return
		}
		r.release(at.Add(tapHold))
	}
}

// HoldFor is a press held for d, as a console command asks for. The
// gestures come out in the order a finger would have made them — the
// hold at 800 ms, the long hold at ten seconds — and each is dispatched
// with the time it would have fired, not the time of the press.
func (n *Node) HoldFor(in Input, d time.Duration, now time.Time) []Packet {
	r := n.input(in)
	if r == nil {
		return nil
	}
	// A hold shorter than the edge lockout is left exactly as asked: the
	// release ends the press either way, and the press is then too short
	// to be a gesture at all. Stretching it past the lockout — which this
	// did, back when a release inside the lockout was dropped and left
	// the input latched down — turned "hold for 10 ms" into a counted tap
	// and, on the power button, a brightness toggle.
	//
	// It is said out loud, because doing nothing quietly looks the same
	// as the console having missed the line.
	end := now.Add(d)
	if !r.press(now) {
		// A finger is already on it — a real one on the board, or a hold
		// the console started — or touch is blocked. Releasing now would
		// end that press before it matured, which is the thing Tap was
		// fixed for; this is the same path.
		n.log.Info("hold ignored: the input is already down or blocked", "input", in)
		return n.flush()
	}
	// Said after the press was taken, so it names the reason that
	// actually applies: a hold refused for being too short and one
	// refused because a finger is already there are different answers,
	// and printing both sends someone chasing a timing problem that is
	// not there.
	if d < edgeLockout {
		n.log.Info("hold too short to register, as on the device",
			"input", in, "held", dur(d), "shortest", dur(edgeLockout))
	}
	// Walk the press forward, firing what each moment brings, rather than
	// jumping to the end: one poll at the end reports the long hold
	// before the hold, which is backwards.
	for {
		at := r.next()
		if at.IsZero() || at.After(end) {
			break
		}
		for _, g := range r.poll(at) {
			n.onGesture(in, g, at)
		}
	}
	r.release(end)
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

// clearPending drops whatever a finger had started — a press still down,
// a tap still inside its window — without re-arming the post-boot wait.
// A device powering down forgets them: none of it means anything after
// the power goes, and a pending tap would keep asking to be woken on a
// device whose only answer is to throw the gesture away. The power
// button has to keep working, so the block is left alone.
func clearPending(rs []recogniser) {
	for i := range rs {
		rs[i].down, rs[i].held, rs[i].longHeld = false, false, false
		rs[i].taps = 0
		rs[i].lastRelease = time.Time{}
	}
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
		// animation starts. Pair flushes what it queued, so keep it: the
		// caller's own flush would otherwise find nothing and the first
		// bond broadcast would be dropped. The call goes in its own
		// statement because it empties n.out as a side effect, and the
		// order the two operands of an append are evaluated in is not
		// something the language promises.
		ps := n.Pair(now)
		n.out = append(n.out, ps...)
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
