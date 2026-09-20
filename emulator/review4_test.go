package emulator

// Regressions for the fourth review round. Each one is a rule the
// firmware follows that the emulator had drifted from.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

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

	// A board with no power chip reports zeroes because nothing has told
	// it otherwise, and says so — the gate reads that rather than
	// guessing from the zeroes, which a failed reading also produces.
	h = newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 0, 0 })
	h.collect(h.n.Poll(h.now))
	if b := h.n.Sensors().Battery; b.Present {
		t.Fatalf("expected a board with no power chip, got %+v", b)
	}
	if err := h.n.Update(h.now); errors.Is(err, ErrBatteryLow) {
		t.Error("a board with no power chip was refused for its battery")
	}

	// A device that has switched itself off is not one to reboot into a
	// new image.
	h = newHarness(t, nil)
	h.n.PowerOff(h.now)
	if err := h.n.Update(h.now); !errors.Is(err, ErrPoweredDown) {
		t.Errorf("a powered-down device was refused with %v, want %v", err, ErrPoweredDown)
	}
}

// TestAbandonLeavesOtherGroupsAlone: SmartGroupAbandon ends the group it
// names. A device in no group, or in a different one, has nothing to
// abandon and must not report that it did — the flags look the same
// either way, so what gives it away is the line in the log.
func TestAbandonLeavesOtherGroupsAlone(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if h.n.inGroup {
		t.Fatal("an abandon put the device into a group")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("a device in no group reported abandoning one: %s", logged.String())
	}
	logged.Reset()

	// In a group, only its own UID ends it.
	h.n.inGroup, h.n.smartUID = true, 7
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if !h.n.inGroup {
		t.Error("another group's abandon ended this one")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("another group's abandon was reported as this one's: %s", logged.String())
	}
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 7})
	if h.n.inGroup {
		t.Error("the group's own abandon did not end it")
	}
}

// TestUnbondNoticeReachesTheAir: deleting a peer tells it so. The notice
// waits for the next window rather than leaving with the call, so this
// covers the whole path — Unbond also hands back whatever the node
// queued now, as every other command does, instead of returning nil.
func TestUnbondNoticeReachesTheAir(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.take()
	h.collect(h.n.Unbond(h.now, totem))
	if h.n.BondCount() != 0 {
		t.Fatalf("the peer is still bonded: %d", h.n.BondCount())
	}
	// The unbond notice goes out at the next window, not from the call,
	// so what matters is that nothing is stranded: drive a window and the
	// frame has to appear.
	h.advance(10 * time.Second)
	var found bool
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerUnbond {
			found = true
		}
	}
	if !found {
		t.Error("the unbond notice never reached the air")
	}
}

// TestPeerTimeIsAWallSecond: State writes each peer's position time as a
// Unix second, but coordsAt is in the device's own base — on a board with
// no RTC that base starts near the epoch at every boot. Saving it raw put
// a number like 126 in a field everything else reads as a wall time. It
// is saved only when there is a clock to convert it with, and the zero
// time, whose Unix value is a large negative number, is not saved at all.
func TestPeerTimeIsAWallSecond(t *testing.T) {
	// No clock: nothing is claimed.
	h := newHarness(t, nil)
	h.bond()
	p := h.n.peers[totem]
	p.hasCoords, p.lat, p.lon, p.coordsAt = true, 37.775, -122.42, h.now
	if got := h.n.State(1).Peers[0].LastSeenUnix; got != 0 {
		t.Errorf("a node with no clock saved a last-seen time of %d", got)
	}

	// The zero time is not a time either.
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	p.coordsAt = time.Time{}
	if got := h.n.State(1).Peers[0].LastSeenUnix; got != 0 {
		t.Errorf("the zero time was saved as %d", got)
	}

	// With a clock, it is the wall second the position was reported.
	p.coordsAt = h.now
	got := h.n.State(1).Peers[0].LastSeenUnix
	if want := t0.Unix(); got < want-5 || got > want+5 {
		t.Errorf("saved %d (%s), want about %d (%s)",
			got, time.Unix(got, 0).UTC(), want, t0.UTC())
	}

	// And it comes back where it went in. A node restoring without a
	// clock cannot place it, so the position ages from the restore rather
	// than from a second it cannot read.
	st := h.n.State(1)
	fresh := newHarness(t, nil)
	if errs := fresh.n.Restore(st, fresh.now); len(errs) != 0 {
		t.Fatalf("restore: %v", errs)
	}
	rp := fresh.n.peers[totem]
	if !rp.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := fresh.now.Sub(rp.coordsAt); age < 0 || age > time.Second {
		t.Errorf("a restored position without a clock is %v old, want about 0", age)
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
