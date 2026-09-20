package emulator

// Regressions for the seventeenth review round.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestPoweringDownDropsWhatWasQueued: refusing to queue an unbond while
// the device is off is only half of it. One queued a moment *before* the
// power went sat in the outbox and went out on the next power-up,
// telling a peer about something from the far side of a power cycle it
// never saw.
func TestPoweringDownDropsWhatWasQueued(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.bond()
	h.take()

	h.collect(h.n.Unbond(h.now, totem))
	if len(h.n.outbox) == 0 {
		t.Fatal("the unbond notice was not queued in the first place")
	}
	h.n.PowerOff(h.now)
	if len(h.n.outbox) != 0 {
		t.Errorf("powering down left %d frames queued", len(h.n.outbox))
	}

	h.n.PowerOn(h.now)
	h.advance(10 * time.Second)
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerUnbond {
			t.Error("a notice queued before the power cycle went out after it")
		}
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

// TestARefusedHoldNamesOneReason: a hold refused because a finger is
// already on the button is not a hold refused for being too short, and
// printing both sends someone chasing a timing problem that is not there.
func TestARefusedHoldNamesOneReason(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)

	h.collect(h.n.HoldFor(SOSButton, 10*time.Millisecond, h.now))
	if !strings.Contains(logged.String(), "hold ignored") {
		t.Errorf("the refusal was not reported: %s", logged.String())
	}
	if strings.Contains(logged.String(), "too short") {
		t.Errorf("the refusal named the wrong reason as well: %s", logged.String())
	}
}
