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

// battPctFor maps a cell voltage to a percentage along that curve.
func battPctFor(v float32) int8 {
	switch {
	case v <= battCurve[0].volts:
		return 0
	case v >= battCurve[len(battCurve)-1].volts:
		return 100
	}
	for i := 1; i < len(battCurve); i++ {
		hi := battCurve[i]
		if v > hi.volts {
			continue
		}
		lo := battCurve[i-1]
		span := hi.volts - lo.volts
		return lo.pct + int8(float32(hi.pct-lo.pct)*(v-lo.volts)/span)
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
}

func newPower(now time.Time) *Power {
	return &Power{lastTick: now, blockers: map[WdtBlocker]bool{}}
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
// it changed.
func (p *Power) update(b Battery, now time.Time) (PowerMode, bool) {
	was := p.mode
	switch {
	case p.off:
		return p.mode, false
	case b.Volts > 0 && b.Volts <= cutoffVolts:
		// Report the cutoff; the node does the powering down, which is
		// more than setting a flag — the radio windows have to stop.
		p.mode = PowerOff
	case b.Charging:
		p.mode = PowerNormal
	case b.Low || (b.Volts > 0 && b.Volts <= lowBattVolts):
		p.mode = PowerLow
	case b.Percent > 0 && b.Percent <= ecoBattPct:
		p.mode = PowerEco
	default:
		p.mode = PowerNormal
	}
	_ = now
	return p.mode, p.mode != was
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
	p.sleptMs += d.Milliseconds()
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

// ParsePowerMode reads the console's name for a mode.
func ParsePowerMode(s string) (PowerMode, error) {
	switch s {
	case "normal":
		return PowerNormal, nil
	case "eco":
		return PowerEco, nil
	case "low":
		return PowerLow, nil
	case "off":
		return PowerOff, nil
	}
	return 0, fmt.Errorf("%q: want normal, eco, low or off", s)
}
