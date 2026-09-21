package emulator

// The battery curve, the power modes, the charger and the watchdog:
// power.go, and the parts of device.go that act on a reading.

import (
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// TestVoltsAloneGiveAPercentage: a real power chip reports a cell
// voltage, and get_batt_pct is what makes a percentage of it. A source
// that gives one and not the other left every reader — the status frame,
// the power mode, the OTA gate — looking at a battery of 0%.
func TestVoltsAloneGiveAPercentage(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 4.0}})
	})
	h.collect(h.n.Poll(h.now))
	if got := h.n.Sensors().Battery.Percent; got < 80 || got > 92 {
		t.Errorf("4.0 V read as %d%%, want about 86%% from the curve", got)
	}
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Errorf("a battery at 4.0 V put the device in power mode %s", got)
	}
}

// TestTheLearnedMaximumScalesTheCurve: what the learned maximum is for,
// now that it does what the firmware's does.
//
// This test used to assert the opposite — that a pack charging above
// 4.12 V made 4.15 V read under 100%, so the first tenth of a volt of
// discharge would show. get_batt_pct does not do that. Its rescale fires
// only when the learned maximum is *below* the table's top, and it lifts
// the reading so an aging pack's own ceiling still reads full; above the
// top there is no rescale and a flat 100%. The Totem next to this one
// reports 4.48 V and 100%, which is the same statement from the device.
func TestTheLearnedMaximumScalesTheCurve(t *testing.T) {
	// Above the table's top: no rescale, and full all the way down to it.
	const high = float32(4.25)
	for _, v := range []float32{4.12, 4.15, high} {
		if got := battPctFor(v, high); got != 100 {
			t.Errorf("a pack that reaches %v V reads %d%% at %v V, want 100%%", high, got, v)
		}
	}
	// Below it: the reading is scaled up against the pack's own ceiling.
	const worn = float32(3.95)
	if got := battPctFor(worn, worn); got != 100 {
		t.Errorf("a pack that tops out at %v V reads %d%% there, want 100%%", worn, got)
	}
	plain, scaled := battPctFor(3.80, 0), battPctFor(3.80, worn)
	if scaled <= plain {
		t.Errorf("3.80 V reads %d%% unscaled and %d%% against a %v V pack: not lifted",
			plain, scaled, worn)
	}
}

// TestAFactoryResetForgetsThePack: the learned maximum describes a
// battery. A reset that left it behind would keep stretching the curve
// for a pack the device is being told it never met — and it has to take
// a fresh reading with it, or the percentage worked out with the
// forgotten stretch stands until the next poll and then jumps.
func TestAFactoryResetForgetsThePack(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	// On the charger and settled there: the maximum is learned from the
	// top of a charge, so it takes a poll to see the peak rise and
	// another to see it stop.
	if err := h.n.SetBattery(100, true, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	h.advance(time.Second)
	h.collect(h.n.Poll(h.now))
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Fatal("nothing was learned to forget")
	}
	// Dropping bonds is not a reset: the name promises the bonds, and a
	// caller that wants those should not lose the battery calibration.
	h.n.ForgetPeers(h.now)
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Error("ForgetPeers threw away the battery calibration")
	}

	// The pack is swapped for one that peaks lower, and the device is
	// told to forget what it knew. What a reset throws away is the
	// history: it keeps whatever the present reading says, because that
	// is a measurement and not a memory.
	// Low, but above the cutoff: at 0% the device switches itself off,
	// and a device that is off does not learn from the pack it is on —
	// which is the point of the reading below, not of this line.
	if err := h.n.SetBattery(20, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Sensors() // no-op read, so the harness state is settled
	if err := h.n.SetPosition(nil, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.SetSensors(NewStatic(Sensors{Battery: Battery{Volts: 4.15, Percent: 100}}), h.now)
	h.n.FactoryReset(h.now)
	// The reset drops the memory, and nothing takes its place until the
	// pack is charged again: a maximum is the top of a charge, so a
	// device merely running on a battery has nothing to say about where
	// that pack tops out. This used to read it straight off the next
	// reading, which is what made the old learned maximum meaningless —
	// it was the highest voltage seen rather than a charge ceiling.
	if got := h.n.Power().LearnedMaxVolts(); got != 0 {
		t.Errorf("a reset kept %v, which is the pack it was told to forget", got)
	}
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().LearnedMaxVolts(); got != 0 {
		t.Errorf("a reading off the battery taught it a maximum of %v", got)
	}

	// On the charger, settled, it learns the new pack — and the new pack
	// only, less the margin the firmware keeps.
	h.n.SetSensors(NewStatic(Sensors{Battery: Battery{Volts: 4.15, Percent: 100, Charging: true}}), h.now)
	h.collect(h.n.Poll(h.now))
	h.advance(time.Second)
	h.collect(h.n.Poll(h.now))
	got := h.n.Power().LearnedMaxVolts()
	if got == 0 {
		t.Fatal("a settled charge taught it nothing")
	}
	if got > 4.15-battMaxMargin+0.001 || got < 4.15-battMaxMargin-0.001 {
		t.Errorf("a %v V pack was learned as %v, want %v", 4.15, got, 4.15-battMaxMargin)
	}
}

// TestAPoweredDownDeviceLearnsNothing: the driver keeps polling a device
// that is off so the power button still works, and read() runs every
// time. A device left switched off on a charger went on stretching its
// battery curve from a pack nobody was using.
func TestAPoweredDownDeviceLearnsNothing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.8, 50 })
	h.collect(h.n.Poll(h.now))
	was := h.n.Power().LearnedMaxVolts()

	h.n.PowerOff(h.now)
	if err := h.n.SetBattery(100, true, h.now); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		h.collect(h.n.Poll(h.now))
		h.now = h.now.Add(time.Second)
	}
	if got := h.n.Power().LearnedMaxVolts(); got != was {
		t.Errorf("a powered-down device learned %v, up from %v", got, was)
	}
}

// TestTheCountersStartAgainAtEveryBoot: the firmware keeps the sleep
// total and the learned maximum in modes, whose constructor sets both to
// 0, so neither survives a reboot. Restoring them made the emulator
// report something its own firmware never does — and the round trip
// double-counted the sleep total every time the settings were re-read.
func TestTheCountersStartAgainAtEveryBoot(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.8, 50 })
	h.collect(h.n.Poll(h.now))
	h.n.Restore(&store.State{SleepMs: 9_000_000, LearnedMaxVolts: 4.35}, h.now)

	if got := h.n.Power().SleptMs(); got >= 9_000_000 {
		t.Errorf("the sleep total came back as %d ms", got)
	}
	if got := h.n.Power().LearnedMaxVolts(); got > 4.3 {
		t.Errorf("the learned maximum came back as %v", got)
	}
	// And what it saves is this run's, so reading it back cannot inflate
	// anything.
	first := h.n.State(1).SleepMs
	saved := h.n.State(1)
	h.n.Restore(&saved, h.now)
	if got := h.n.State(1).SleepMs; got != first {
		t.Errorf("a save and restore moved the sleep total from %d to %d", first, got)
	}
}

// TestAPowerCycleStartsTheCountersAgain: coming back up is a boot — it
// runs the post-boot touch wait and the power-up ring, and on the device
// the wake restarts main.py and builds modes again. The counters modes
// starts at zero start again with it.
func TestAPowerCycleStartsTheCountersAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.advance(bootDebounce)
	// On the charger and settled: the maximum comes from the top of a
	// charge, so one poll sees the peak rise and the next sees it hold.
	if err := h.n.SetBattery(100, true, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	h.advance(time.Second)
	h.collect(h.n.Poll(h.now))
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Fatal("nothing was learned to lose")
	}

	h.n.PowerOff(h.now)
	h.now = h.now.Add(time.Hour)
	h.n.PowerOn(h.now)

	if got := h.n.Power().LearnedMaxVolts(); got != 0 {
		t.Errorf("a power cycle kept a learned maximum of %v", got)
	}
	if got := h.n.Power().SleptMs(); got != 0 {
		t.Errorf("a power cycle kept %d ms of sleep", got)
	}
	// And the hour it spent switched off is not counted as time awake.
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().Duty(); got < 0 || got > 1 {
		t.Errorf("the duty cycle is %v after a power cycle", got)
	}
}

// TestTheChargerRingIsNotReplayedAfterAPowerCycle: charging() marks the
// connection shown while the device is down precisely so the ring is not
// replayed when it comes back on a cable that never moved. Starting a
// run cleared that mark.
func TestTheChargerRingIsNotReplayedAfterAPowerCycle(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	for end := h.now.Add(time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
	}
	h.advance(bootAnim + time.Second)

	h.n.PowerOff(h.now)
	h.now = h.now.Add(time.Second)
	h.n.PowerOn(h.now)
	// The power-up ring plays; what must not happen is a second one from
	// the charger once it ends.
	h.advance(bootAnim + time.Second)
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatal("the charger replayed the ring after a power cycle on the same cable")
		}
	}
}

// TestACableThatNeverMovedIsNotAnnouncedAgain: the driver keeps polling
// while the device is down, which is how the power button works, and
// those polls are what mark a connection as already announced. A day on
// the same cable must not produce a second ring, and the debounce must
// not be restarted into one either.
func TestACableThatNeverMovedIsNotAnnouncedAgain(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(70, true, h.now); err != nil {
		t.Fatal(err)
	}
	// The connection announces itself once, while the device is on.
	h.advance(2 * time.Second)

	h.n.PowerOff(h.now)
	// A day down, polled throughout, exactly as the loop does.
	for end := h.now.Add(24 * time.Hour); h.now.Before(end); h.now = h.now.Add(time.Second) {
		h.collect(h.n.Poll(h.now))
	}
	h.n.PowerOn(h.now)
	h.advance(bootAnim + time.Second)
	for end := h.now.Add(chargeAnnounce + time.Second); h.now.Before(end); h.now = h.now.Add(10 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatal("a cable that never moved played the charger ring again")
		}
	}
}

// TestABoardBuiltOnAFlatPackComesUpOff: New takes a reading, and the
// mode has to follow it there as much as anywhere else. Left to the
// first poll, a node built on a pack under the cutoff reports power mode
// normal until something else looks — and on the board that was masked
// only by main happening to call restore two lines later.
func TestABoardBuiltOnAFlatPackComesUpOff(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.1, 40 })
	if !h.n.Power().Off() {
		t.Error("a node built on a pack under the cutoff came up running")
	}

	// The reading is taken before the window is armed and acted on where
	// it is taken, so a device that came up under the cutoff is already
	// down by then and New does not arm one at all: a device that is off
	// must not transmit.
	if h.n.hasJob(jobWindow) {
		t.Error("a device that came up switched off still has a radio window armed")
	}

	// And one on a good pack is unaffected.
	h = newHarness(t, nil)
	if h.n.Power().Off() {
		t.Error("a node on a full pack came up switched off")
	}
	if !h.n.hasJob(jobWindow) {
		t.Error("a node on a full pack came up with no radio window")
	}
}

// TestTheLowBatteryReminderWaitsForTheRing: it is a reminder, and a boot
// animation, an alarm or a download is the picture someone is actually
// looking at. Owed rather than drawn over them, and not lost either.
func TestTheLowBatteryReminderWaitsForTheRing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.4, 8 })
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Fatalf("a device on a low pack came up showing %s, not its power-up ring", got)
	}
	h.advance(bootAnim + time.Second)
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("once the strip was free the ring showed %s", got)
	}

	// And a reminder owed while something else holds the ring is not
	// spent on it: the alarm keeps the strip, and the reminder follows.
	h = newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(8, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Errorf("the low-battery reminder took the ring from the alarm: %s", got)
	}
	h.n.SetSOS(false)
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("after the alarm ended the ring showed %s, and the reminder was owed", got)
	}
}

// TestARealTotemsChargingVoltageIsPlausible: the plausibility band was a
// guess, and it was too low. A Totem on firmware 5.0.3 sitting on its
// charger reports 4.48 V, which the old ceiling of 4.4 called wrong — so
// learn() would refuse a real pack's own peak, the battery curve would
// be stretched against a maximum the device never reaches, and the peer
// line printed volts=0 for a device plainly reporting volts.
func TestARealTotemsChargingVoltageIsPlausible(t *testing.T) {
	// Measured on the bench, in a log full of frames from a real Totem.
	const observed = 4.48
	if !plausibleVolts(observed) {
		t.Errorf("a real Totem reads %v V on its charger, and this build calls that implausible", observed)
	}
	// And such a reading reaches the peer line rather than being zeroed.
	if got := loggableVolts(observed); got != observed {
		t.Errorf("a peer reporting %v V is logged as %v", observed, got)
	}
	// What cannot be logged is still kept out: slog's JSON handler takes
	// neither, and one frame carrying one takes the whole line with it.
	for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if got := loggableVolts(v); got != 0 {
			t.Errorf("loggableVolts(%v) = %v", v, got)
		}
	}
}

// TestAReminderGoesWhenTheModeDoes: the low-battery reminder is owed
// while the ring is busy, which means it can outlive the reason for it.
// One owed during an alarm and not cleared when the pack came back up
// flashed "battery low" at someone whose pack was full and on the
// charger.
func TestAReminderGoesWhenTheModeDoes(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)

	// The alarm holds the ring, and the pack goes low underneath it.
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(8, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)

	// Then the pack comes back up — swapped, or simply read higher.
	// Not on the charger: plugging in plays the power-up ring, which
	// would sit on top of the reminder and hide it from this test.
	if err := h.n.SetBattery(100, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.SetSOS(false)
	// Two seconds, not five: the reminder is a timed animation and runs
	// out after about four, so a test that looks afterwards sees the
	// resting ring either way and proves nothing.
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got == AnimLowBattery {
		t.Errorf("a pack back at 100%% was told its battery is low (ring: %s)", got)
	}
}

// TestAReminderDoesNotSurviveAPowerCycle: the power button does not go
// through applyPowerMode — PowerOff sets the mode straight to off and
// PowerOn straight back to normal — so a reminder owed before the button
// was cleared by nothing at all. The pack is charged while the device is
// down, and the ring says "battery low" as soon as the power-up
// animation ends.
func TestAReminderDoesNotSurviveAPowerCycle(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)

	// Owed while the alarm holds the ring, so it cannot be shown.
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(8, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)

	// The button, an hour on a charger, and the button again.
	h.n.PowerOff(h.now)
	h.now = h.now.Add(time.Hour)
	if err := h.n.SetBattery(100, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.PowerOn(h.now)
	h.n.SetSOS(false)
	h.advance(bootAnim + 2*time.Second)

	if got := h.n.LEDs().Animation(); got == AnimLowBattery {
		t.Error("a device that came back on a full pack was told its battery is low")
	}
}

// TestTheChargerDebounceRunsWhileTheRingIsBusy: charging() is the only
// writer of when the cable went in, and it used to be the first operand
// of an && chain — so the debounce kept running only because of the
// order the operands happened to be in. Plugging in during an alarm is
// the case that breaks when someone tidies that.
func TestTheChargerDebounceRunsWhileTheRingIsBusy(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)

	// The alarm owns the strip, and the cable goes in underneath it.
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(70, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Long enough that a debounce started now would have finished.
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Fatalf("the alarm lost the ring to something else: %s", got)
	}

	// The alarm ends, and the connection is announced — which needs the
	// debounce to have been running the whole time it was held back.
	//
	// Less than chargeDebounce, and that is the whole point: if asking
	// only starts the debounce now, the ring cannot be announced yet,
	// and the test fails. Advancing past it instead — which this test
	// did when it was written — lets a debounce that started late
	// finish inside the window, and then it passes either way.
	h.n.SetSOS(false)
	h.advance(chargeDebounce / 2)
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Errorf("the ring shows %s: the charger was never announced", got)
	}
}

// TestChargerPlaysThePowerupAnimation: 'disconn_animation' is the task
// name Compass.power_conn_new runs under, and what it does is wait for
// v_in to read high three 100 ms samples running and then launch
// powerup_animation — the ring the device shows at boot.
//
// The wait is a length of time, not a number of polls: the board runs its
// loop every few milliseconds and this harness runs it when the next
// radio window comes round, so counting polls made the debounce either
// far too short or far too long depending on who was driving.
func TestChargerPlaysThePowerupAnimation(t *testing.T) {
	// poll drives the node the way a board does, in small steps, and
	// reports whether the powerup ring started.
	poll := func(h *harness, d time.Duration) bool {
		played := false
		for end := h.now.Add(d); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
			was := h.n.LEDs().Animation()
			h.collect(h.n.Poll(h.now))
			if was != AnimBoot && h.n.LEDs().Animation() == AnimBoot {
				played = true
			}
		}
		return played
	}

	// The lengths here are real ones, not chargeDebounce ± a margin:
	// scaling the test with the constant is how a debounce of fifteen
	// milliseconds passed a test meant to prove it was three hundred.
	const (
		bounce = 200 * time.Millisecond // a contact rattling into a socket
		settle = 500 * time.Millisecond // long enough that it is plugged in
	)
	if chargeDebounce < bounce || chargeDebounce > settle {
		t.Fatalf("chargeDebounce is %s; power_conn_new waits three 100 ms samples", chargeDebounce)
	}

	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if got := h.n.LEDs().Animation(); got == AnimBoot {
		t.Fatalf("the boot animation is still playing: %s", got)
	}

	// A contact that bounces on the way into the socket is not a
	// connection, however many polls fall inside it.
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	if poll(h, bounce) {
		t.Error("a bouncing contact played the powerup animation")
	}
	if err := h.n.SetBattery(80, false, h.now); err != nil {
		t.Fatal(err)
	}
	if poll(h, 100*time.Millisecond) {
		t.Error("letting go of the charger played the powerup animation")
	}

	// Settled on the charger, it plays once and stays played.
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	if !poll(h, settle) {
		t.Fatal("a settled charger never played the powerup animation")
	}
	h.advance(bootAnim + time.Second)
	if poll(h, 2*time.Second) {
		t.Error("the powerup animation played twice on one connection")
	}
}

// TestChargerDoesNotStealTheRing: the powerup ring is a timed animation,
// so once it starts nothing can put back what was underneath for two
// seconds. Plugging in is the obvious thing to do during an update or an
// alarm — the battery is low, that is why you reached for the cable — and
// neither may lose the ring to it.
func TestChargerDoesNotStealTheRing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		want  Animation
	}{
		{"an alarm", func(h *harness) { h.n.SetSOS(true) }, AnimSOS},
		{"an update", func(h *harness) { h.n.ota.state = OTADownloading }, AnimOTA},
	} {
		h := newHarness(t, nil)
		h.advance(bootAnim + time.Second)
		tc.setup(h)
		h.collect(h.n.Poll(h.now))
		if got := h.n.LEDs().Animation(); got != tc.want {
			t.Fatalf("%s: the ring shows %s before the charger, want %s", tc.name, got, tc.want)
		}
		if err := h.n.SetBattery(5, true, h.now); err != nil {
			t.Fatal(err)
		}
		for end := h.now.Add(chargeDebounce + time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
			h.collect(h.n.Poll(h.now))
			if got := h.n.LEDs().Animation(); got != tc.want {
				t.Fatalf("%s: the charger put %s over it", tc.name, got)
			}
		}
	}
}

// TestAChargerSuppressedByTheRingPlaysLater: the powerup ring is shown
// once per connection, and the fifth round made it give way to an alarm
// or an update. Those two together used to lose it: the one-shot was
// spent deciding not to draw, so the ring never came back for the rest of
// the connection. Plugging in during the low-battery flash — the likeliest
// moment of all — meant no charger ring at all.
func TestAChargerSuppressedByTheRingPlaysLater(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.SetSOS(true)
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Fatalf("the ring shows %s, want the alarm", got)
	}

	if err := h.n.SetBattery(50, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Well past the debounce, with the alarm holding the ring.
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
	}
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Fatalf("the charger took the ring from the alarm: %s", got)
	}

	// The alarm ends. The charger is still in, so the ring it never got
	// to show is still owed.
	h.n.SetSOS(false)
	played := false
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			played = true
			break
		}
	}
	if !played {
		t.Error("the charger ring was spent while the alarm held it, and never played")
	}
}

// TestTheChargerRingGoesStale: the ring says the charger has just gone
// in. Owed indefinitely, it would play ten minutes into an alarm as if
// the cable had only then been connected.
func TestTheChargerRingGoesStale(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.SetSOS(true)
	h.collect(h.n.Poll(h.now))
	if err := h.n.SetBattery(50, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Long past the point where the announcement would still be true.
	for end := h.now.Add(chargeAnnounce + time.Minute); h.now.Before(end); h.now = h.now.Add(time.Second) {
		h.collect(h.n.Poll(h.now))
	}
	h.n.SetSOS(false)
	for end := h.now.Add(2 * time.Second); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot {
			t.Fatalf("the charger ring played %s after the charger went in", chargeAnnounce+time.Minute)
		}
	}
}

// TestNoPowerChipIsNotAFlatBattery: a board with no battery monitor
// reports zeroes. Read as a reading, they drove it into PowerLow, which
// goes out in every status frame and flashes the low-battery ring at
// someone whose device cannot go flat.
func TestNoPowerChipIsNotAFlatBattery(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NoPowerChip, c.BattVolts, c.BattPct = true, 0, 0 })
	h.collect(h.n.Poll(h.now))
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Errorf("a board with no power chip is in power mode %s", got)
	}
	if h.n.Power().Off() {
		t.Error("a board with no power chip powered itself down")
	}
}
