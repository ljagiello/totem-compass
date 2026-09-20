package emulator

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestScratchTapCount(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}}, now)
	now = now.Add(bootDebounce + time.Second)
	n.Tap(PowerButton, 2, now)
	r := n.input(PowerButton)
	t.Logf("after Tap(2): taps=%d", r.taps)
	gs := r.poll(now.Add(2 * time.Second))
	t.Logf("gestures after Tap(2): %v", gs)

	n.Tap(SOSButton, 3, now)
	r3 := n.input(SOSButton)
	t.Logf("after Tap(3): taps=%d", r3.taps)
	t.Logf("gestures after Tap(3): %v", r3.poll(now.Add(2*time.Second)))
}

func TestScratchDoubleWindowJob(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	src := NewStatic(Sensors{Fix: &Fix{Lat: 1, Lon: 2, AccuracyM: 3, Time: now}, Orientation: mesh.OrientationVertical})
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, Sensors: src}, now)
	win := 0
	for _, j := range n.jobs {
		t.Logf("job kind=%d at=%s", j.kind, j.at.Sub(now))
		if j.kind == jobWindow {
			win++
		}
	}
	t.Logf("window jobs: %d, clockSet=%v", win, n.clockSet)
}

func TestScratchPairFlush(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, Owned: []mesh.MAC{{9, 9, 9, 9, 9, 9}}}, now)
	now = now.Add(bootDebounce + time.Second)
	out := n.HoldFor(Crystal, 2*time.Second, now)
	t.Logf("HoldFor crystal returned %d packets, pairing=%v", len(out), n.Pairing())
}

func TestScratchOTAFlatBattery(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, BattPct: 0, BattVolts: 3.4}, now)
	err := n.Update(now)
	t.Logf("Update at 0%%: err=%v", err)
	n2 := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, BattPct: 10, BattVolts: 3.5}, now)
	t.Logf("Update at 10%%: err=%v", n2.Update(now))
}

func TestScratchSleep(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}}, now)
	for i := 0; i < 400; i++ {
		now = now.Add(5 * time.Millisecond)
		n.Poll(now)
	}
	t.Logf("after 2s: slept=%dms duty=%.3f next=%s", n.power.SleptMs(), n.power.Duty(), n.Next().Sub(now))
}

func TestScratchPowerOffDuringPairing(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	n := New(Config{MAC: mesh.MAC{1, 2, 3, 4, 5, 6}, Owned: []mesh.MAC{{9, 9, 9, 9, 9, 9}}}, now)
	n.Pair(now)
	n.PowerOff(now)
	n.PowerOn(now.Add(time.Second))
	later := now.Add(30 * time.Second)
	n.Poll(later)
	t.Logf("pairing after power cycle: %v, jobs=%d", n.Pairing(), len(n.jobs))
	out := n.Poll(later.Add(5 * time.Second))
	t.Logf("packets after: %d", len(out))
}
