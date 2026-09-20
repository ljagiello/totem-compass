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

// battCurve is peripherals.get_batt_pct: cell voltage to percentage,
// piecewise linear. v5.0.3 recalibrated it so that 4.12 V reads 100%
// (v5.0.2 read 61% there), and the points below come from that curve:
// 3.8 V is 50% and 4.0 V is 86%. The shape between them is linear, as the
// table's segments are.
var battCurve = []struct {
	volts float32
	pct   int8
}{
	{3.30, 0},
	{3.50, 10},
	{3.65, 25},
	{3.80, 50},
	{4.00, 86},
	{4.12, 100},
}

// battPctFor maps a cell voltage to a percentage along that curve. top
// is the highest this pack has been seen at, which stretches the last
// segment when a pack charges above the curve's own top — otherwise
// everything from 4.12 V upwards reads 100% and the first tenth of a
// volt of discharge looks like no discharge at all. Zero means nothing
// has been learned yet, and the curve is used as written.
func battPctFor(v float32, top float32) int8 {
	last := len(battCurve) - 1
	// The last point moves out to what this pack reaches; it does not
	// gain a segment of its own. A segment added above the curve's top
	// left the one below it unstretched, so the two met at different
	// percentages and the gauge dipped seven points at 4.12 V — 99% then
	// 92% climbing, and back up again on the way down.
	full := battCurve[last].volts
	if top > full && plausibleVolts(top) {
		full = top
	}
	switch {
	case v <= battCurve[0].volts:
		return 0
	case v >= full:
		return 100
	}
	for i := 1; i <= last; i++ {
		hi, hiVolts := battCurve[i], battCurve[i].volts
		if i == last {
			hiVolts = full
		}
		if v > hiVolts {
			continue
		}
		lo := battCurve[i-1]
		return lo.pct + int8(float32(hi.pct-lo.pct)*(v-lo.volts)/(hiVolts-lo.volts))
	}
	return 100
}

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
	// maxVolts is the highest cell voltage this pack has been seen at,
	// which device_power learns and logs as "Max volts updated". It is
	// kept across reboots, so a pack that charges above the curve's top
	// is not read as 100% for ever after.
	maxVolts float32
	// measured says maxVolts came from a reading this run took, rather
	// than from a saved record. A saved one may replace what was only
	// restored — a pack swapped for one that peaks lower has to be able
	// to bring the curve back down — but never what this run has seen
	// with its own eyes.
	measured bool
}

func newPower(now time.Time) *Power {
	return &Power{lastTick: now, blockers: map[WdtBlocker]bool{}}
}

// SetSleptMs puts back the lifetime light-sleep total a previous boot
// saved, which the settings carry like the boot count. Without it the
// figure a device reports is only this run's, next to a boot count that
// is every run's.
func (p *Power) SetSleptMs(ms int64) {
	if ms > 0 {
		p.sleptMs = ms
	}
}

// LearnedMaxVolts is the highest cell voltage seen, which the settings
// carry across reboots.
func (p *Power) LearnedMaxVolts() float32 { return p.maxVolts }

// SetLearnedMaxVolts puts back what a previous boot learned, and reports
// whether it was a voltage a cell could reach — it comes off a flash
// sector as four raw bytes, and an infinity would pin the curve for the
// life of the boot and be written straight back out.
//
// It will not lower a maximum this run measured for itself: `store open`
// on a running device would otherwise un-stretch a curve the device
// learned an hour ago. It will lower one that was only restored, because
// a pack swapped for one that peaks lower has to have a way down that is
// not a factory reset.
func (p *Power) SetLearnedMaxVolts(v float32) bool {
	if v == 0 {
		return true // nothing was saved, which is not a bad value
	}
	if !plausibleVolts(v) {
		return false
	}
	if v > p.maxVolts || !p.measured {
		p.maxVolts = v
	}
	return true
}

// ClearLearnedMaxVolts forgets the pack, as a factory reset does.
func (p *Power) ClearLearnedMaxVolts() { p.maxVolts, p.measured = 0, false }

// plausibleVolts reports whether a reading is one a single lithium cell
// could give: above the curve's own floor and not past what a charger
// will take it to.
func plausibleVolts(v float32) bool {
	return v >= battCurve[0].volts && v <= maxPlausibleVolts
}

// maxPlausibleVolts is the highest a single cell is charged to, with room
// for a pack that reads a little high. Past it the reading is wrong.
const maxPlausibleVolts = 4.4

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
	if b.Volts > p.maxVolts && plausibleVolts(b.Volts) {
		p.maxVolts, p.measured = b.Volts, true
	}
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
// draw would otherwise lose the animation for the whole connection.
func (p *Power) takeCharger() bool {
	p.chargeShown = true
	return true
}

// sleep accounts for the time between now and the next thing the device
// has to do. It returns how long a real Totem would have slept: zero when
// something holds sleep off, or when the next window is too close.
func (p *Power) sleep(now, next time.Time) time.Duration {
	elapsed := now.Sub(p.lastTick)
	p.lastTick = now
	if elapsed < 0 {
		elapsed = 0
	}
	if next.IsZero() || p.holdSleep || p.off {
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

// SleptMs is dev_total_lightsleep_ms.
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
