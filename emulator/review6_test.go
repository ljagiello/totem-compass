package emulator

// Regressions for the sixth review round. Three of these are damage the
// fifth round's own fixes did.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// TestAPoweredDownDeviceAsksForNothing: a device that has switched itself
// off has no timers and nothing to draw, so Next has nothing to offer. A
// driver waits on Next and polls immediately when it is in the past — so
// a deadline that never moves is a busy loop at full power on a device
// that is supposed to be off.
//
// An update refused for being powered down used to leave exactly that:
// the refusal flashes the ring, which is a Play, and device() does not
// tick a strip that is off, so nothing ever cleared it.
func TestAPoweredDownDeviceAsksForNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.PowerOff(h.now)
	if got := h.n.Next(); !got.IsZero() {
		t.Fatalf("a powered-down device wants waking at %v", got)
	}

	// The refusal that flashes the ring must not change that.
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("an update on a powered-down device was allowed")
	}
	if got := h.n.Next(); !got.IsZero() {
		t.Errorf("a refused update left a deadline of %v on a device that is off", got)
	}
	// And it stays that way however many times it is polled.
	for range 5 {
		h.collect(h.n.Poll(h.now))
		h.now = h.now.Add(5 * time.Millisecond)
		if got := h.n.Next(); !got.IsZero() {
			t.Fatalf("polling a powered-down device produced a deadline of %v", got)
		}
	}
}

// TestAPictureChangeWakesARestingStrip: a resting strip is paced by its
// frame deadline and woken by nobody, so anything that changes what it
// looks like has to say so. The dial did; the crystal colour and the
// brightness did not, and a `color blue` on a resting strip drew nothing
// until the next radio window happened to tick it.
func TestAPictureChangeWakesARestingStrip(t *testing.T) {
	rest := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
		h.bond()
		h.rx(totem, self, -40, statusFrame(t0))
		h.advance(bootAnim + time.Second)
		if got := h.n.LEDs().Animation(); got != AnimIdle {
			t.Fatalf("the strip is playing %s, want it resting", got)
		}
		// Settle onto a frame, so the deadline is in the future.
		h.n.LEDs().Tick(h.now)
		return h
	}

	t.Run("crystal colour", func(t *testing.T) {
		h := rest(t)
		before := h.n.LEDs().Crystal()[0]
		h.n.SetColor(ColorBlue, h.now)
		if got := h.n.LEDs().Crystal()[0]; got == before {
			t.Errorf("the crystal is still %v after being set to blue", got)
		}
	})

	t.Run("brightness", func(t *testing.T) {
		h := rest(t)
		before := h.n.LEDs().Crystal()[0]
		h.n.ToggleBrightness(h.now)
		if got := h.n.LEDs().Crystal()[0]; got == before {
			t.Errorf("the crystal is still %v after the brightness changed", got)
		}
	})
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

// TestAShortHoldIsNotATap: HoldFor used to stretch a hold shorter than
// the edge lockout past it, because a release inside the lockout was
// being dropped and left the input latched down. The release is honored
// now, so the stretch is not only unnecessary — it turns "hold for 10 ms"
// into a counted tap, and on the power button into a brightness toggle.
func TestAShortHoldIsNotATap(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	full := h.n.LEDs().Brightness()

	h.collect(h.n.HoldFor(PowerButton, edgeLockout/3, h.now))
	h.advance(multiTapWindow + time.Second)

	if got := h.n.LEDs().Brightness(); got != full {
		t.Errorf("a %s hold of power changed brightness to %v", edgeLockout/3, got)
	}
	if h.n.Power().Off() {
		t.Error("a hold shorter than the edge lockout switched the device off")
	}
}

// TestRestoreRefusesACrystalColourOutsideThePalette: the device's own
// colour comes off the same flash sector as its peers'. An id outside the
// thirteen renders unlit, and State writes it straight back, so one bad
// byte would leave the crystal dark for good.
func TestRestoreRefusesACrystalColourOutsideThePalette(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.ColorID = int8(ColorTeal)
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.n.Restore(store.State{ColorID: 120}, h.now)

	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d, which is not in the palette", int(got))
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorTeal {
		t.Errorf("the crystal is %s, want the default it started with", got)
	}
	if !strings.Contains(logged.String(), "thirteen") {
		t.Errorf("nothing was said about the bad colour: %s", logged.String())
	}
	// And it is not written back out, so it cannot survive the next boot.
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved again")
	}
}

// TestABrokenPeerClockIsReportedOnce: a peer whose RTC never started
// broadcasts status every few seconds. Warning on each would fill the log
// — on the board the rotate that follows holds a watchdog blocker — but
// saying nothing at all leaves someone wondering why a Totem never picks
// up a clock with nothing to go on.
func TestABrokenPeerClockIsReportedOnce(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.bond()
	h.advance(rtcSyncDelay + time.Second)

	old := statusFrame(t0)
	old.Unix, old.TimeOfDayMs = 1, 1
	for range 10 {
		h.rx(totem, self, -40, old)
		h.advance(2 * time.Second)
	}
	if n := strings.Count(logged.String(), "before 2020"); n != 1 {
		t.Errorf("a peer with a broken clock was reported %d times, want once", n)
	}
	if h.n.clockSet {
		t.Error("the node took a clock from before 2020")
	}
}

// TestUsablePositionIsOneRule: every path that takes a position from
// outside asks the same two things — that something was reported, and
// that it is somewhere a device could be. The pair was written out four
// times, and the fourth was added because the third had been missed.
func TestUsablePositionIsOneRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lat, lon float32
		want     bool
	}{
		{"a real place", 37.775, -122.42, true},
		{"null island is no fix", 0, 0, false},
		{"a latitude with a zero longitude", 37.775, 0, true},
		{"past the pole", 91, 0, false},
		{"past the meridian", 0, 181, false},
	} {
		if got := usablePosition(tc.lat, tc.lon); got != tc.want {
			t.Errorf("%s: usablePosition(%v, %v) = %v, want %v", tc.name, tc.lat, tc.lon, got, tc.want)
		}
	}
	// And a frame carrying one of the refused pairs leaves the peer
	// without coordinates, whichever path it came in on.
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	f := statusFrame(t0)
	f.Lat, f.Lon = 0, 0
	h.rx(totem, self, -40, f)
	if p := h.n.peers[totem]; p.hasCoords {
		t.Errorf("a frame reporting no fix gave the peer a position of %v, %v", p.lat, p.lon)
	}
}
