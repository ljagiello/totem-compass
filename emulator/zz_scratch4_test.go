package emulator

import (
	"testing"
	"time"
)

func TestScratchLedsAlone(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLEDs(now, ColorBlue)
	for i := 0; i < 12; i++ {
		now = now.Add(250 * time.Millisecond)
		l.Tick(now)
		t.Logf("i=%d anim=%v start=%v frame=%d until=%v next=%v zero=%v",
			i, l.anim, l.start.Format("05.000"), l.frame,
			l.until.IsZero(), l.next.Format("05.000"), l.next.IsZero())
	}
}
