package emulator

// Regressions for the twenty-fifth review round.

import (
	"testing"
	"time"
)

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

// TestAFrameThatArrivesAsThePackDies: the reading Receive takes can
// switch the device off, and the check that a powered-down device does
// not answer runs before it. The frame was then taken into the peer
// table, and could set the clock, on a device that was down.
func TestAFrameThatArrivesAsThePackDies(t *testing.T) {
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	h.bond()
	h.advance(time.Second)
	h.take()

	was := h.n.Peers()[0]

	// The pack collapses, and the next thing to happen is a frame.
	pack.b = Battery{Volts: 3.1, Percent: 40}
	f := statusFrame(h.now)
	f.Name, f.BattPct = "SomeoneElse", 42
	h.rx(totem, self, -40, f)

	if !h.n.Power().Off() {
		t.Fatal("the reading taken on the frame did not switch the device off")
	}
	got := h.n.Peers()[0]
	if got.Status.Name != was.Status.Name || got.Status.BattPct != was.Status.BattPct {
		t.Errorf("a device that had just powered down took the frame in: the peer is now %q at %d%%",
			got.Status.Name, got.Status.BattPct)
	}
	if !got.LastHeard.Equal(was.LastHeard) {
		t.Errorf("a device that had just powered down recorded hearing from a peer at %v", got.LastHeard)
	}
	if h.n.clockSet {
		t.Error("a device that had just powered down took a clock off a frame")
	}
	if s := h.take(); len(s) != 0 {
		t.Errorf("a device that had just powered down sent %d frames", len(s))
	}
}
