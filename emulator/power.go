package emulator

// Power: the battery curve, the power modes, sleep and the watchdog
// model, as device_power.py and wdt_manager.py run them.
//
// A Totem spends most of its life asleep between radio windows, wakes to
// send its status, and drops back. It changes power mode as the battery
// falls, holds sleep off while a radio window is near or a clock sync is
// pending, and finally powers down rather than brown out. The emulator
// does not have to save power, but a peer can tell the difference: the
// power mode is in every status frame, a device that has powered down
// stops answering, and a device that is asleep answers only in its
// windows.

import (
	"fmt"
	"log/slog"
	"time"
)

// PowerMode is modes.power_level, which a peer frame carries.
type PowerMode uint8

// The power modes, in the order the firmware moves through them as the
// battery falls ("Changing power mode from {} to: {}").
const (
	// PowerNormal is a healthy battery: every radio window is kept.
	PowerNormal PowerMode = iota
	// PowerEco is the saving mode a long run settles into.
	PowerEco
	// PowerLow is modes.power_level 2, below about 3.45 V: the device
	// keeps talking but everything that can wait, waits.
	PowerLow
	// PowerOff is after device_off or the low-voltage cutoff.
	PowerOff
)

// String names the mode as the console prints it.
func (p PowerMode) String() string {
	switch p {
	case PowerEco:
		return "eco"
	case PowerLow:
		return "low"
	case PowerOff:
		return "off"
	}
	return "normal"
}

// LogValue names the mode in a log line, in either format.
func (p PowerMode) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Battery thresholds, from device_power.py's strings and the battery
// curve. The exact numbers the firmware compares against live in
// bytecode; these are the voltages the curve and the logs imply.
const (
	// lowBattVolts is where modes.power_level becomes 2 and the touch
	// driver turns on its low-battery sensitivity.
	lowBattVolts = 3.45
	// cutoffVolts is "Voltages too low, powering down | {}v".
	cutoffVolts = 3.30
	// ecoBattPct is where a device settles into eco mode. Inferred: the
	// threshold is not in the strings.
	ecoBattPct = 40
	// otaMinPct is the OTA battery gate ("Battery too low for OTA
	// update"). The firmware's number is not recoverable; this is the
	// level below which an update that reboots the device is a bad idea.
	otaMinPct = 30
)

// battCurve is peripherals.get_batt_pct's own breakpoint table, decoded
// out of the 232-byte blob the v5.0.3 bytecode carries as a constant: 58
// pairs of millivolts and a raw level, each a little-endian u16, running
// from the top of the pack down. What stood here before was a six-point
// approximation of the same shape, inferred before the blob was read.
//
// The percentage is always round(raw / battFullScale * 100), so the
// table's own numbers are levels rather than percentages, and the
// rounding is the firmware's integer arithmetic rather than ours.
var battCurve = [...]struct{ mv, raw int32 }{
	{4120, 605}, {4100, 590}, {4080, 580}, {4070, 570},
	{4050, 560}, {4030, 550}, {4020, 540}, {4010, 530},
	{4000, 520}, {3990, 510}, {3980, 500}, {3970, 490},
	{3960, 480}, {3950, 470}, {3940, 455}, {3930, 440},
	{3920, 430}, {3910, 420}, {3900, 410}, {3890, 400},
	{3880, 390}, {3870, 380}, {3860, 370}, {3850, 360},
	{3840, 350}, {3830, 340}, {3820, 330}, {3810, 320},
	{3800, 305}, {3790, 290}, {3780, 280}, {3770, 270},
	{3760, 260}, {3750, 250}, {3740, 235}, {3730, 220},
	{3720, 210}, {3710, 200}, {3700, 190}, {3680, 180},
	{3670, 170}, {3660, 160}, {3640, 150}, {3630, 140},
	{3610, 130}, {3600, 120}, {3590, 110}, {3570, 100},
	{3550, 90}, {3540, 80}, {3530, 70}, {3510, 60},
	{3500, 50}, {3470, 40}, {3430, 30}, {3360, 20},
	{3280, 10}, {3190, 0},
}

// battFullScale is the raw level paired with the top of the table, and
// the divisor every percentage is taken against.
const battFullScale = 605

// battTopMv and battFlatMv are the two ends of the table, named because
// the algorithm compares against them directly.
const (
	battTopMv  = 4120
	battFlatMv = 3190
)

// floorDiv divides the way Python's // does, towards negative infinity.
// Go truncates towards zero instead, and the rescale below is the one
// place the difference can show: a cur_mv under the bottom of the table
// makes the numerator negative.
func floorDiv(a, b int32) int32 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// battPctFor is peripherals.get_batt_pct(max_volts, cur_volts), arguments
// swapped to read the way this package's callers do. top is
// config.batt_max_volts: the highest this pack has been seen at.
//
// The rescale is the part worth reading twice, because it runs the
// opposite way round from what the name suggests. It fires only when the
// learned maximum is *below* the table's top, and it scales the reading
// up — compensating for a pack that never reaches 4.12 V, so that its own
// ceiling still reads 100%. A pack that charges above 4.12 V gets no
// rescale at all: the clamp just below returns 100 for anything from the
// table's top upwards.
//
// This replaces a stretch that ran the other way, extending the curve out
// to a learned maximum above 4.12 V. That read 89% at 4.12 V on a pack
// last seen at 4.48, where a Totem reads 100% — and the Totem sitting
// next to this one reports exactly 100% at 4.48 V, which is what settled
// it. The old shape was the nicer gauge and the wrong one: emulating the
// device is the point, and the device holds at 100% over that last
// tenth of a volt.
func battPctFor(v float32, top float32) int8 {
	if v == 0 {
		return 0
	}
	curMv := int32(float64(v)*1000 + 0.5)
	// plausibleVolts before the conversion, not only because a maximum
	// off the flash can be anything, but because Go leaves a float to
	// int conversion implementation-defined when the value will not fit.
	// An infinity here happened to land on a large negative on this
	// machine and take the right branch by luck; there is no reason the
	// board's toolchain would agree. An implausible maximum is treated as
	// none, and the table is read as written.
	if top != 0 && plausibleVolts(top) {
		maxMv := int32(float64(top)*1000 + 0.5)
		if battFlatMv < maxMv && maxMv < battTopMv {
			curMv = battFlatMv + floorDiv((curMv-battFlatMv)*(battTopMv-battFlatMv), maxMv-battFlatMv)
		}
		if curMv >= battTopMv {
			return 100
		}
		if curMv < battFlatMv {
			return 0
		}
	}
	hiMv, hiRaw := battCurve[0].mv, battCurve[0].raw
	for _, p := range battCurve[1:] {
		if curMv >= p.mv {
			raw := p.raw + floorDiv((curMv-p.mv)*(hiRaw-p.raw), hiMv-p.mv)
			// Both clamps live inside "if max_volts" in the firmware, so
			// a device that has not learned a maximum yet can come out of
			// here above 100 — 4.48 V reads 145. On the Totem that value
			// goes on to struct.pack('<b'), which raises, so it is a fault
			// the device never survives rather than a number it sends.
			// Capped instead of reproduced: the frame's field is a signed
			// byte, and 145 in one is -111.
			return int8(min(battPctOf(raw), 100))
		}
		hiMv, hiRaw = p.mv, p.raw
	}
	return 0
}

// battPctOf turns a raw level into a percentage the way the firmware
// does: round(raw / battFullScale * 100), in integer arithmetic.
func battPctOf(raw int32) int32 { return (raw*1000/battFullScale + 5) / 10 }

// Sleep timing, from the light-sleep log line ("Slept for: {} of {} |
// sleep duty: {:.3f}") and the reasons the firmware gives for staying
// awake.
const (
	// wakeBeforeWindow is how long before a radio window the device stops
	// sleeping: "Not sleeping due radio needing to turn on soon".
	wakeBeforeWindow = 100 * time.Millisecond
	// minSleep is the shortest nap worth taking. Below this the wake
	// costs more than the sleep saves.
	minSleep = 20 * time.Millisecond
)

// WdtBlocker is one of wdt_manager's blockers: while any is set, the
// watchdog feed loop is off.
type WdtBlocker uint8

// The blockers, as the v5.0.3 tuple ('log rotate', 'vfs write', 'wlan
// kick') names them. WLAN_KICK = 2 is new in v5.0.3.
const (
	BlockLogRotate WdtBlocker = iota
	BlockVFSWrite
	BlockWLANKick
)

// String names the blocker as the firmware's tuple does.
func (b WdtBlocker) String() string {
	switch b {
	case BlockVFSWrite:
		return "vfs write"
	case BlockWLANKick:
		return "wlan kick"
	}
	return "log rotate"
}

// Power is the device's power state.
type Power struct {
	mode PowerMode
	// off is when the device powered down, so a peer sees it stop.
	off bool
	// sleptMs is dev_total_lightsleep_ms, and awakeMs its counterpart, so
	// the duty cycle the firmware logs can be reported.
	sleptMs, awakeMs int64
	lastTick         time.Time
	// blockers are the watchdog blockers currently held. The feed loop
	// runs only when there are none, as _should_enable says.
	blockers map[WdtBlocker]bool
	// holdSleep is set while something must not be interrupted, such as a
	// GNSS clock sync ("Block sleep for GNSS RTC Sync").
	holdSleep bool
	// chargeSince is when the charger was first seen without a break, and
	// chargeShown records that the powerup animation has already run for
	// this connection. Compass.power_conn_new waits for v_in to read high
	// three 100 ms samples running before it plays it, so a contact that
	// bounces on the way into the socket does not set it off.
	chargeSince time.Time
	chargeShown bool
	// maxVolts is config.batt_max_volts: the ceiling this pack was last
	// seen to charge to, which get_batt_pct scales a reading against.
	// peakVolts is modes.batt_volts_max, the highest reading of this
	// charge, and prevVolts the peak as it stood a minute ago — the two
	// one_min_coro compares to decide the pack has stopped taking charge.
	maxVolts, peakVolts, prevVolts float32
}

func newPower(now time.Time) *Power {
	return &Power{lastTick: now, blockers: map[WdtBlocker]bool{}}
}

// battMaxMargin is the 0.04 V one_min_coro subtracts from the peak
// before storing it, and the same margin it allows when deciding a new
// peak is worth recording.
const battMaxMargin = 0.04

// learn takes what a reading says about the pack itself, which is a
// different thing from working out a power mode from it. update used to
// do both, so anything that wanted a mode also, silently, taught the
// device about the battery — and a factory reset had to clear the
// calibration twice to work around it.
//
// Only while the charger is in. That is the shape of one_min_coro, where
// the whole block sits under "if modes.is_charging", and it is what
// makes the number mean anything: config.batt_max_volts is the ceiling
// this pack was last seen to charge to, so get_batt_pct can scale a pack
// that no longer reaches 4.12 V against its own top.
//
// Learning it on every reading instead — which is what this did — makes
// it the highest voltage seen at all, and a device that has been
// discharging since boot has "learned" a maximum equal to roughly where
// it started. Scaled against that, a half-flat pack reads full. The old
// curve hid this by stretching rather than scaling, so the mistake cost
// nothing until the curve was made to match the firmware's.
func (p *Power) learn(b Battery) {
	if !b.Charging {
		return
	}
	if b.Volts > p.peakVolts && plausibleVolts(b.Volts) {
		p.peakVolts = b.Volts
	}
	if p.peakVolts == 0 {
		return
	}
	// Still climbing: remember where it got to and wait.
	if p.peakVolts > p.prevVolts {
		p.prevVolts = p.peakVolts
		return
	}
	// It has stopped, so this is the top of the charge. The firmware
	// also has a slower path, counting five minutes of no rise before it
	// records the same number, for the case where the peak falls back
	// below what it already had; both store the peak less the margin.
	if p.peakVolts > p.maxVolts-battMaxMargin {
		p.maxVolts = p.peakVolts - battMaxMargin
	}
}

// LearnedMaxVolts is config.batt_max_volts: the top of the last charge,
// less battMaxMargin. Like the sleep total, the firmware keeps the peak
// behind it in modes and starts it at 0 on every boot, so it is saved as
// a snapshot and learned again from the pack rather than remembered
// across a reboot.
func (p *Power) LearnedMaxVolts() float32 { return p.maxVolts }

// ClearLearnedMaxVolts forgets the pack, as a factory reset does.
func (p *Power) ClearLearnedMaxVolts() { p.maxVolts, p.peakVolts, p.prevVolts = 0, 0, 0 }

// plausibleVolts reports whether a reading is one a single lithium cell
// could give: above the curve's own floor and not past what a charger
// will take it to.
func plausibleVolts(v float32) bool {
	// The table runs from the top of the pack down, so its last entry is
	// the flat end. It used to be the first.
	return v >= float32(battCurve[len(battCurve)-1].mv)/1000 && v <= maxPlausibleVolts
}

// maxPlausibleVolts is the highest a cell reads, with room above it.
// Past this the number is wrong rather than high.
//
// Measured, not assumed: a Totem on firmware 5.0.3 sitting on its
// charger reports 4.48 V, which the 4.4 that used to be here called
// implausible — so learn() would have refused to learn a real pack's own
// peak, and the battery curve would have been stretched against a
// maximum the device never reaches. The headroom above 4.48 is for a
// pack that reads a little higher still.
const maxPlausibleVolts = 4.6

// startRun begins a new run: the counters the firmware keeps in modes,
// whose constructor sets them to zero. A power cycle is a boot, so what
// the last run slept and what it learned about the pack go with it.
func (p *Power) startRun(now time.Time) {
	p.sleptMs, p.awakeMs = 0, 0
	p.ClearLearnedMaxVolts()
	p.lastTick = now
	// The charger is not touched at all, neither the mark nor the
	// debounce. charging() sets chargeShown while the device is down
	// precisely so the ring is not replayed when it comes back on a
	// cable that never moved, and the driver keeps polling while it is
	// down — that is how the power button works — so every connection
	// that survives a power cycle is marked.
	//
	// Starting the debounce again instead would announce such a
	// connection 300 ms after every power-up: the ring says "the charger
	// just went in", and a cable that has been in since yesterday did
	// not. The ceiling below is the same argument for the same reason.
	// The only way past both is a power cycle with no poll in it, which
	// the driver does not do and a test has to construct.
}

// Mode is the power mode a status frame carries.
func (p *Power) Mode() PowerMode { return p.mode }

// Off reports whether the device has powered down.
func (p *Power) Off() bool { return p.off }

// Block holds a watchdog blocker. A flash write is the firmware's 'vfs
// write': while it runs, nothing feeds the dog.
func (p *Power) Block(b WdtBlocker) { p.blockers[b] = true }

// Unblock releases one blocker; the feed loop comes back when the last
// one goes.
func (p *Power) Unblock(b WdtBlocker) { delete(p.blockers, b) }

// WatchdogFeeding reports whether the feed loop would be running:
// wdt_manager._should_enable is false as soon as any blocker is set.
func (p *Power) WatchdogFeeding() bool { return len(p.blockers) == 0 }

// HoldSleep keeps the device awake while something must finish.
func (p *Power) HoldSleep(on bool) { p.holdSleep = on }

// update moves the power mode for a battery reading, and reports whether
// it changed. It takes no time: the mode is a function of the reading
// alone, and a parameter it could not use read as though it were not.
func (p *Power) update(b Battery) (PowerMode, bool) {
	was := p.mode
	switch {
	case p.off:
		return p.mode, false
	case b.NoPowerChip:
		// No cell to be low: the zeroes such a board reports are the
		// absence of a reading, and driving it into PowerLow would put
		// power mode 2 in every status frame and flash the low-battery
		// ring at someone whose device cannot go flat.
		p.mode = PowerNormal
	case b.Charging:
		// On the charger first: a pack that reads flat while it charges is
		// filling up, and powering it down there would kill the radio at
		// the moment someone plugged it in to fix exactly that.
		p.mode = PowerNormal
	case b.Volts > 0 && b.Volts <= cutoffVolts:
		// Report the cutoff; the node does the powering down, which is
		// more than setting a flag — the radio windows have to stop.
		p.mode = PowerOff
	case b.Low || (b.Volts > 0 && b.Volts <= lowBattVolts):
		p.mode = PowerLow
	case b.Percent > 0 && b.Percent <= ecoBattPct:
		p.mode = PowerEco
	default:
		p.mode = PowerNormal
	}
	return p.mode, p.mode != was
}

// chargeDebounce is how long the charger has to read as connected before
// the ring says so: power_conn_new's three 100 ms samples of v_in. It is
// a length of time rather than a count of polls, because the poll rate is
// not the sample rate — the board runs the loop every few milliseconds
// and the console harness runs it when the next radio window comes round.
const chargeDebounce = 300 * time.Millisecond

// charging reports whether the charger has been connected long enough to
// show the battery level, and keeps the debounce running. The firmware
// launches powerup_animation from power_conn_new at that point, the same
// animation it plays at boot, so the ring reads as a battery gauge either
// way.
//
// It answers the same for as long as the charger stays in. What makes
// the ring play only once is takeCharger, which the caller reaches only
// if it is actually going to draw — so a connection that arrives while
// something else owns the ring is not silently spent, and plays as soon
// as the ring is free.
func (p *Power) charging(b Battery, now time.Time) bool {
	if !b.Charging {
		p.chargeSince, p.chargeShown = time.Time{}, false
		return false
	}
	if p.off {
		// Powered down, so nothing is drawn. The connection is not
		// forgotten: the charger never left the socket, and coming back
		// on should not replay the ring the power-up already played.
		p.chargeShown = true
		return false
	}
	// A clock that has stepped back would otherwise leave the debounce
	// measuring a negative stretch, and the ring would never come on
	// again for as long as the charger stayed in.
	if p.chargeSince.IsZero() || now.Before(p.chargeSince) {
		p.chargeSince = now
	}
	held := now.Sub(p.chargeSince)
	// Not for ever: the ring says "the charger just went in", and if
	// something else owned it at the time the announcement is owed, not
	// stored indefinitely. Ten minutes into an alarm it would be a lie.
	return !p.chargeShown && held >= chargeDebounce && held <= chargeAnnounce
}

// chargeAnnounce is how long after the charger settles the ring may still
// announce it, if something else held the ring in the meantime.
const chargeAnnounce = 30 * time.Second

// takeCharger spends this connection's one showing of the ring. It is
// separate from charging so that the decision to draw and the record of
// having drawn cannot come apart: a caller that asks and then does not
// draw would otherwise lose the animation for the whole connection. It
// is called once the caller has decided to draw, not as part of
// deciding — it cannot refuse, and a condition that cannot fail reads
// like one that can.
func (p *Power) takeCharger() { p.chargeShown = true }

// sleep accounts for the time between now and the next thing the device
// has to do. It returns how long a real Totem would have slept: zero when
// something holds sleep off, or when the next window is too close.
func (p *Power) sleep(now, next time.Time) time.Duration {
	elapsed := now.Sub(p.lastTick)
	p.lastTick = now
	if elapsed < 0 {
		elapsed = 0
	}
	if p.off {
		// Neither asleep nor awake: the driver keeps polling so the power
		// button works, and booking that as time awake made a device that
		// had been switched off all day report a duty cycle for it. The
		// span that ended with the button is dropped along with the rest
		// — at most one poll of it, against a total in hours, and the
		// alternative is carrying a "was awake until" through a path
		// whose whole job is that the device is not.
		return 0
	}
	if next.IsZero() || p.holdSleep {
		p.awakeMs += elapsed.Milliseconds()
		return 0
	}
	d := next.Sub(now) - wakeBeforeWindow
	if d < minSleep {
		p.awakeMs += elapsed.Milliseconds()
		return 0
	}
	// What is counted is the time that has passed since the last look,
	// not the sleep still ahead: nothing was due in it, so the device
	// could have spent it asleep. Counting the interval ahead on every
	// call would multiply it by however often the driver polls, and the
	// total would run away from the wall clock.
	p.sleptMs += elapsed.Milliseconds()
	return d
}

// Duty is the share of time the device would have spent asleep, the
// figure the firmware logs as "sleep duty".
func (p *Power) Duty() float64 {
	total := p.sleptMs + p.awakeMs
	if total == 0 {
		return 0
	}
	return float64(p.sleptMs) / float64(total)
}

// SleptMs is dev_total_lightsleep_ms, which is this run's total: the
// firmware keeps it in modes, whose constructor sets it to 0, so it
// starts again at every boot. The settings save it as a snapshot of what
// the device last reported, not as something to carry over.
func (p *Power) SleptMs() int64 { return p.sleptMs }

// Describe is the power state for a console line.
func (p *Power) Describe() string {
	s := fmt.Sprintf("mode %s, slept %s, duty %.1f%%, watchdog %s",
		p.mode, time.Duration(p.sleptMs)*time.Millisecond, p.Duty()*100,
		map[bool]string{true: "feeding", false: "held off"}[p.WatchdogFeeding()])
	if p.off {
		s += ", powered down"
	}
	return s
}
