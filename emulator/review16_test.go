package emulator

// Regressions for the sixteenth review round.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestASyntheticHoldDoesNotEndARealPress: the same rule Tap got last
// round, on the path beside it. A `touch sos hold 10` arriving while
// someone held the board's button ended that press before it matured,
// so the alarm never started.
func TestASyntheticHoldDoesNotEndARealPress(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)

	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)
	h.collect(h.n.HoldFor(SOSButton, 10*time.Millisecond, h.now))
	if r := h.n.input(SOSButton); !r.down {
		t.Fatal("the console hold ended a press someone was making")
	}
	if !strings.Contains(logged.String(), "hold ignored") {
		t.Errorf("the console was told nothing: %s", logged.String())
	}
	h.advance(holdTime + time.Second)
	if !h.n.Config().SOS {
		t.Error("the real hold never fired, so the alarm never started")
	}
}

// TestAPoweredDownDeviceDoesNoWork: dropping the bytes in sendRaw is not
// the same as not doing the work. A pairing window opened on a device
// that is off still armed its timers and built a full status frame every
// 50-99 ms for six seconds, for the radio to throw away.
func TestAPoweredDownDeviceDoesNoWork(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	h.bond()
	h.take()
	h.n.PowerOff(h.now)

	h.collect(h.n.Pair(h.now))
	if h.n.Pairing() {
		t.Error("a powered-down device opened a pairing window")
	}
	if got := h.n.Next(); !got.IsZero() {
		t.Errorf("a powered-down device armed a timer for %v", got)
	}
	if !strings.Contains(logged.String(), "cannot pair") {
		t.Errorf("nothing was said about why: %s", logged.String())
	}

	// And an unbond leaves nothing queued to go out when it comes back.
	logged.Reset()
	h.collect(h.n.Unbond(h.now, totem))
	if len(h.n.outbox) != 0 {
		t.Errorf("a powered-down device queued %d frames for later", len(h.n.outbox))
	}
	if !strings.Contains(logged.String(), "cannot delete a peer") {
		t.Errorf("nothing was said about why: %s", logged.String())
	}
	h.n.PowerOn(h.now)
	h.advance(10 * time.Second)
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerUnbond {
			t.Error("an unbond from while the device was off went out on power-up")
		}
	}
}

// TestTheDutyCycleIsThisRun: the lifetime sleep total comes back from the
// settings, and the time awake beside it does not — so adding the total
// to one half of the ratio reported 99% sleep on a device that had been
// awake the whole time.
func TestTheDutyCycleIsThisRun(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(10 * time.Second)
	h.n.Restore(store.State{SleepMs: 9_000_000}, h.now)

	if got := h.n.Power().Duty(); got > 0.5 {
		t.Errorf("the duty cycle is %.3f after restoring a lifetime total", got)
	}
	// The total is still reported, and still saved.
	if got := h.n.Power().SleptMs(); got < 9_000_000 {
		t.Errorf("the lifetime total is %d ms", got)
	}
}

// TestASavedSleepTotalIsChecked: eight bytes off a flash sector. The
// all-ones an erased or torn record gives came back as a negative
// duration and a duty of -100%.
func TestASavedSleepTotalIsChecked(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.n.Restore(store.State{SleepMs: ^uint64(0)}, h.now)
	if got := h.n.Power().SleptMs(); got < 0 {
		t.Errorf("the device reports %d ms of sleep", got)
	}
	if !strings.Contains(h.n.Power().Describe(), "slept") {
		t.Fatal("the power line stopped mentioning sleep")
	}
	if strings.Contains(h.n.Power().Describe(), "-") {
		t.Errorf("the power line reads %q", h.n.Power().Describe())
	}
	if !strings.Contains(logged.String(), "sleep total") {
		t.Errorf("nothing was said about it: %s", logged.String())
	}
}

// TestASavedSleepTotalOnlyGoesUp: `store open` on a running device reads
// a record written before this run added to the counter.
func TestASavedSleepTotalOnlyGoesUp(t *testing.T) {
	h := newHarness(t, nil)
	h.n.Restore(store.State{SleepMs: 9_000_000}, h.now)
	h.n.Restore(store.State{SleepMs: 5}, h.now)
	if got := h.n.Power().SleptMs(); got < 9_000_000 {
		t.Errorf("a stale record moved the lifetime total to %d ms", got)
	}
}
