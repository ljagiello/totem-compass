package emulator

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestScratchLedNext(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	src := NewStatic(Sensors{Fix: &Fix{Lat: 1, Lon: 2, AccuracyM: 3, Time: now}, Orientation: mesh.OrientationVertical})
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, Sensors: src}, now)
	for i := 0; i < 2000; i++ {
		now = now.Add(5 * time.Millisecond)
		n.Poll(now)
	}
	t.Logf("leds.Next in %s, node.Next in %s, anim=%v", n.leds.Next().Sub(now), n.Next().Sub(now), n.leds.Animation())
	t.Logf("slept=%dms awake=%dms duty=%.3f holdSleep=%v", n.power.SleptMs(), n.power.awakeMs, n.power.Duty(), n.power.holdSleep)
	// simulate a driver that honours the reported sleep
	d := n.power.sleep(now, n.Next())
	t.Logf("power.sleep reported %s", d)
}

func TestScratchDupWindowFrames(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	other := mesh.MAC{9, 9, 9, 9, 9, 9}
	src := NewStatic(Sensors{Fix: &Fix{Lat: 1, Lon: 2, AccuracyM: 3, Time: now}, Orientation: mesh.OrientationVertical})
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, Owned: []mesh.MAC{other}, Sensors: src}, now)
	if err := n.AddBond(other, "peer", now); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := 0; i < 4000; i++ { // 20 s at 5 ms
		now = now.Add(5 * time.Millisecond)
		total += len(n.Poll(now))
	}
	t.Logf("status frames in 20 s with one peer: %d (expect ~5 at 4 s windows)", total)
}

func TestScratchSmartGroupColor(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, ColorID: int8(ColorBlue)}, now)
	t.Logf("leds default color before: %v, cfg %v", n.leds.DefaultColor(), n.cfg.ColorID)
	n.inGroup, n.smartUID, n.bonding = true, 7, true
	n.onSmartGroup(now, Received{Src: mesh.MAC{9}, RSSI: -10}, mesh.SmartGroup{
		Instruction: mesh.SmartGroupFinalize, UID: 7,
		Members: []mesh.SmartGroupMember{{MAC: n.cfg.MAC, ColorID: int8(ColorRed)}},
	})
	t.Logf("after finalize: cfg.ColorID=%v leds default=%v", n.cfg.ColorID, n.leds.DefaultColor())
}
