package emulator

// WiFi updates: the release poll, the package index, the battery gate
// and what the ring shows while one is running.

import (
	"errors"
	"testing"
	"time"
)

// slowFailingTransport takes its time and then fails, as a stalled server
// or a reset connection does.
type slowFailingTransport struct{ delay time.Duration }

func (s slowFailingTransport) Post(string, []byte) ([]byte, error) {
	time.Sleep(s.delay)
	return nil, errSlowTransport
}

func (s slowFailingTransport) Get(string) ([]byte, error) { return nil, errSlowTransport }

func (s slowFailingTransport) Download(string, func(int64, int64)) (int64, string, error) {
	return 0, "", errSlowTransport
}

var errSlowTransport = errors.New("the server gave up")

// TestADownloadDoesNotOpenOnTheLastOnesRing: the progress ring holds the
// share of the download before it, so an update starting after one that
// reached 80% opened with four fifths of the ring lit for a file it had
// not asked for.
func TestADownloadDoesNotOpenOnTheLastOnesRing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.OTATransport = nil })
	h.advance(bootAnim + time.Second)
	h.n.LEDs().Play(AnimOTA, h.now)
	h.n.LEDs().SetProgress(0.8)
	h.n.LEDs().Tick(h.now)
	if litPixels(h.n.LEDs()) == 0 {
		t.Fatal("the ring did not show the first download at all")
	}

	// No transport, so this fails at the first step — after the reset.
	_ = h.n.Update(h.now)
	h.n.LEDs().Play(AnimOTA, h.now)
	h.n.LEDs().Tick(h.now)
	if got := litPixels(h.n.LEDs()); got != 0 {
		t.Errorf("a new download opened with %d pixels of the last one's lit", got)
	}
}

// TestAnUpdateOnAFlatPackPowersDown: Update takes a fresh reading before
// the battery gate, and read() leaves the node holding a reading nothing
// has acted on. Every other caller pairs the two — a pack under the
// cutoff has to power the device down, not merely lose it an update —
// and this one did not, so a device that asked for an update on a dying
// pack carried on as though the pack were fine until the next poll.
func TestAnUpdateOnAFlatPackPowersDown(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if h.n.Power().Off() {
		t.Fatal("the device was down before the test started")
	}

	// 0% is not under the cutoff — the table reads 0% from 3.19 V down
	// and the device only powers off below 3.15 — so the pack is put
	// under it directly rather than by asking for the flattest level.
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.SetSensors(NewStatic(Sensors{
		Battery: Battery{Volts: cutoffVolts - 0.01, Percent: 0},
	}), h.now)
	h.collect(h.n.Poll(h.now))
	if v := h.n.Sensors().Battery.Volts; v > cutoffVolts {
		t.Fatalf("the pack reads as %v V, above the cutoff of %v", v, cutoffVolts)
	}
	// Acted on where it was read, which is what read() does now: the
	// device is down before anything asks it for an update.
	if !h.n.Power().Off() {
		t.Error("a pack below the cutoff was read and then left running")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrPoweredDown) {
		t.Fatalf("update on a flat pack: %v", err)
	}

	// And the gate itself, on a pack that is low and still alive: under
	// the 30% an update needs, above the cutoff that stops everything.
	h = newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(10, false, h.now); err != nil {
		t.Fatal(err)
	}
	if h.n.Power().Off() {
		t.Fatal("a pack at 10 percent switched the device off")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("update on a pack at 10 percent: %v", err)
	}
}

// collapsingPack is a sensor source whose battery the test changes
// without the node being told: a driver reports what the hardware says
// at the moment it is read, and between two reads a pack can go flat.
type collapsingPack struct{ b Battery }

func (c *collapsingPack) Read(time.Time) Sensors { return Sensors{Battery: c.b} }

// TestAnUpdateStopsWhenTheReadingSwitchesTheDeviceOff: Update takes a
// fresh reading and acts on it, and acting on it can switch the device
// off — under a cutoff in volts, where the gate beside it reads a
// percentage. The check at the top of Update was passed by a device that
// was still on a poll ago, so without a second look the update ran on a
// board whose ring, jobs and outbox had just been cleared: it held the
// touch inputs for the whole exchange and ended saying "ota complete".
func TestAnUpdateStopsWhenTheReadingSwitchesTheDeviceOff(t *testing.T) {
	// A pack that still reports a percentage the gate is happy with, so
	// that what stops the update is the cutoff and not the gate.
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 80}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	h.advance(bootAnim + time.Second)
	if h.n.Power().Off() {
		t.Fatal("the device was down before the pack collapsed")
	}

	// The pack collapses, and nothing has read it yet.
	// Under the cutoff: 3.15, not the 3.30 this used to assume.
	pack.b = Battery{Volts: 3.1, Percent: 80}
	if h.n.Power().Off() {
		t.Fatal("the node was told about the collapse before it read it")
	}

	err := h.n.Update(h.now)
	if !h.n.Power().Off() {
		t.Fatal("the reading Update took did not switch the device off")
	}
	if !errors.Is(err, ErrPoweredDown) {
		t.Errorf("update on a board the reading switched off: %v", err)
	}
	// The positive: it went through fail(), so the ring shows an update
	// that did not happen rather than one still in flight. A plain
	// return would leave the state at OTAChecking, and the ring holding
	// AnimWiFi on a board that is off.
	if st := h.n.OTA().State(); st != OTAFailed {
		t.Errorf("the update ended in state %v, want it failed", st)
	}
}

// TestUpdateWithNoPowerChip: the OTA gate refuses a flat battery
// ("Battery too low for OTA update"), and a 0% reading is as flat as it
// gets. A board with no power chip is a different case: it reports 0 V
// and 0% because nothing has told it otherwise, and Power.update already
// reads that as "no reading". The gate has to agree with it, or such a
// board could never update.
func TestUpdateWithNoPowerChip(t *testing.T) {
	// A real reading below the limit is refused. It has to be above the
	// low-voltage cutoff to be a battery case at all: a poll at 0% powers
	// the device down, and then it is refused for that instead.
	h := newHarness(t, nil)
	if err := h.n.SetBattery(3, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if h.n.Power().Off() {
		t.Fatal("3% powered the device down; pick a level above the cutoff")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("a flat battery was refused with %v, want %v", err, ErrBatteryLow)
	}

	// A board with no power chip is exempt, and only because it was
	// configured to say so.
	h = newHarness(t, func(c *Config) { c.BattVolts, c.BattPct, c.NoPowerChip = 0, 0, true })
	h.collect(h.n.Poll(h.now))
	if err := h.n.Update(h.now); errors.Is(err, ErrBatteryLow) {
		t.Error("a board with no power chip was refused for its battery")
	}

	// A board that reads zeroes and has not said it lacks a chip is a
	// flat cell or a failed reading, which is what the gate is for. The
	// zero value of the flag has to be the one that keeps the gate on.
	h = newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 0, 0 })
	h.collect(h.n.Poll(h.now))
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("a board reading 0 V and 0%% was refused with %v, want %v", err, ErrBatteryLow)
	}

	// A device that has switched itself off is not one to reboot into a
	// new image.
	h = newHarness(t, nil)
	h.n.PowerOff(h.now)
	if err := h.n.Update(h.now); !errors.Is(err, ErrPoweredDown) {
		t.Errorf("a powered-down device was refused with %v, want %v", err, ErrPoweredDown)
	}
}

// TestUpdateHoldsTheProgressRing: an update drives the ring itself, and
// nothing in the interface makes a transport block the loop. A poll that
// landed in the middle of a download used to put the idle animation back
// over the progress ring.
func TestUpdateHoldsTheProgressRing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.ota.state = OTADownloading
	h.n.LEDs().Play(AnimOTA, h.now)
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got != AnimOTA {
		t.Errorf("a poll during a download left the ring on %s", got)
	}
	h.n.ota.state = OTAChecking
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got != AnimWiFi {
		t.Errorf("a poll while checking for an update left the ring on %s", got)
	}
}

// TestAnUpdateHoldsSleepThroughout: the firmware holds sleep off for the
// whole update, not only while bytes are arriving. The ring was moved to
// cover every state an update passes through; the sleep hold was left on
// the download alone, so a transport that yields during the release poll
// or between download and install let the device be accounted asleep
// across it.
func TestAnUpdateHoldsSleepThroughout(t *testing.T) {
	for _, st := range []OTAState{OTAChecking, OTADownloading, OTAVerifying, OTAInstalling} {
		h := newHarness(t, nil)
		if err := h.n.SetClock(t0, h.now); err != nil {
			t.Fatal(err)
		}
		h.advance(bootAnim + time.Second)
		h.n.ota.state = st
		h.collect(h.n.Poll(h.now))
		// The hold itself, not the sleep it prevents: an update also
		// animates the ring, which keeps a frame due and so keeps the
		// device awake on its own. That is not the guarantee — a
		// transport that yields is — so assert on the thing that is.
		if !h.n.power.holdSleep {
			t.Errorf("sleep was not held off while an update was %s", st)
		}
	}
}

// TestASecondUpdateCannotClobberOneInFlight: the re-entrancy guard named
// two states by hand while an update passes through four, so a second
// call during verify or install reset the first one's progress under it.
func TestASecondUpdateCannotClobberOneInFlight(t *testing.T) {
	for _, st := range []OTAState{OTAChecking, OTADownloading, OTAVerifying, OTAInstalling} {
		h := newHarness(t, nil)
		h.n.ota.state, h.n.ota.done, h.n.ota.total = st, 1234, 5678
		if err := h.n.Update(h.now); err == nil {
			t.Errorf("a second update was allowed while one was %s", st)
		}
		if h.n.ota.done != 1234 || h.n.ota.total != 5678 {
			t.Errorf("a second update during %s reset the first one's progress to %d of %d",
				st, h.n.ota.done, h.n.ota.total)
		}
	}
}

// TestASecondUpdateDoesNotReportTheFirstOne: a run that fails at the
// first request used to report the release and package the last one
// installed — and that report is posted back to the server as what this
// device is running.
func TestASecondUpdateDoesNotReportTheFirstOne(t *testing.T) {
	h := newHarness(t, nil)
	h.n.ota.release = Release{Version: "5.0.3", ReleaseID: 339}
	h.n.ota.pkg = "firmware_v5.0.3.bin"
	h.n.ota.state = OTADone

	// No transport, so it fails at the first step.
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("an update with no transport was allowed")
	}
	if got := h.n.ota.Release().Version; got != "" {
		t.Errorf("a failed update still reports release %q", got)
	}
	if got := h.n.ota.Package(); got != "" {
		t.Errorf("a failed update still reports package %q", got)
	}
}
