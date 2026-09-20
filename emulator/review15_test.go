package emulator

// Regressions for the fifteenth review round.

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// TestAPoweredDownDeviceSendsNothing: a device that has switched itself
// off has no radio, and Receive already refuses to answer while it is
// off. The console can still reach Pair and Unbond, though, and without
// the other half of that guard the device broadcast bond requests every
// 50-99 ms for six seconds while being deaf to the replies.
func TestAPoweredDownDeviceSendsNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.bond()
	h.take()
	h.n.PowerOff(h.now)

	if got := h.n.Pair(h.now); len(got) != 0 {
		t.Errorf("pairing a powered-down device sent %d frames", len(got))
	}
	if got := h.n.Unbond(h.now, totem); len(got) != 0 {
		t.Errorf("unbonding on a powered-down device sent %d frames", len(got))
	}
	// And nothing leaks out of a later poll either.
	h.advance(pairingWindow + 2*time.Second)
	if got := h.take(); len(got) != 0 {
		t.Errorf("a powered-down device sent %d frames: %v", len(got), got)
	}

	// Back on, it works again.
	h.n.PowerOn(h.now)
	h.advance(bootDebounce)
	if got := h.n.Pair(h.now); len(got) == 0 {
		t.Error("pairing sent nothing once the device was back on")
	}
}

// TestTheFirstWindowAfterAFixIsAligned: startAligned exists to put the
// radio windows on wall-clock slots. Setting clockSet after calling it
// meant the first window — the one being re-slotted — was scheduled on
// the no-clock period, at an arbitrary phase.
func TestTheFirstWindowAfterAFixIsAligned(t *testing.T) {
	h := newHarness(t, nil)
	// Off the slot boundary, or an unaligned window would land on one by
	// accident and the test would pass either way.
	h.advance(1300 * time.Millisecond)
	// A receiver that arrives with a fix and a clock, as a lock does.
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{Lat: 37.775, Lon: -122.42, AccuracyM: 3, Time: t0},
	}), h.now)
	if !h.n.clockSet {
		t.Fatal("the node did not take the clock")
	}
	// The radio window itself, not whatever the strip wants next.
	var next time.Time
	for _, j := range h.n.jobs {
		if j.kind == jobWindow {
			next = j.at
		}
	}
	if next.IsZero() {
		t.Fatal("no radio window was scheduled")
	}
	// An aligned window lands on a whole radio period of the wall clock,
	// plus the fixed transmit delay. Unaligned it lands on whatever phase
	// the last transmission left, which the no-clock period gives.
	period := h.n.period().Milliseconds()
	off := (h.n.wall(next).UnixMilli() - windowTXDelay.Milliseconds()) % period
	if off != 0 {
		t.Errorf("the first window after a fix is %d ms off its %d ms slot", off, period)
	}
}

// TestTheSleepTotalSurvivesAReboot: the settings save it, the boot count
// beside it is restored, and this was not — so a device reported a
// lifetime sleep of one run next to a boot count of hundreds.
func TestTheSleepTotalSurvivesAReboot(t *testing.T) {
	h := newHarness(t, nil)
	h.n.Restore(store.State{SleepMs: 9_000_000}, h.now)
	if got := h.n.Power().SleptMs(); got < 9_000_000 {
		t.Errorf("the restored sleep total is %d ms, want at least 9000000", got)
	}
	// And it is still there to save again.
	if got := h.n.State(1).SleepMs; got < 9_000_000 {
		t.Errorf("the state saves %d ms", got)
	}
}

// TestASyntheticTapDoesNotEndARealPress: press() refuses when a finger is
// already down, but release() had no such guard — so a `touch sos tap`
// arriving while someone held the board's button ended that press before
// it matured into a hold, and the alarm never started.
func TestASyntheticTapDoesNotEndARealPress(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)

	// A finger goes down on the real button.
	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)
	// A tap arrives from the console while it is held.
	h.n.Tap(SOSButton, 1, h.now)
	if r := h.n.input(SOSButton); !r.down {
		t.Fatal("the console tap ended a press someone was making")
	}
	// The hold still matures.
	h.advance(holdTime + time.Second)
	if !h.n.Config().SOS {
		t.Error("the hold never fired, so the alarm never started")
	}
}
