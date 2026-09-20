package emulator

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestScratchLedInternals(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}}, now)
	for i := 0; i < 10; i++ {
		now = now.Add(500 * time.Millisecond)
		n.Poll(now)
		l := n.leds
		t.Logf("t=%s anim=%v start=%s frame=%d next=%s ledsNextDelta=%s",
			now.Sub(n.boot), l.anim, l.start.Sub(n.boot), l.frame, l.next.Sub(n.boot), l.next.Sub(now))
	}
}

func TestScratchPowerSleepAccounting(t *testing.T) {
	p := newPower(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// a driver polling every 5 ms with the next event 4 s away
	for i := 0; i < 200; i++ {
		now = now.Add(5 * time.Millisecond)
		p.sleep(now, now.Add(4*time.Second))
	}
	t.Logf("1 s of wall clock -> slept=%dms awake=%dms duty=%.3f", p.sleptMs, p.awakeMs, p.Duty())
}
