package emulator

// Regressions for the eighteenth review round.

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestAPowerCycleLeavesOneWindowChain: a clock arriving while the device
// was off re-armed the radio windows that PowerOff had just cleared, and
// PowerOn then started a second chain beside it — so the device sent its
// status to every peer twice a period for the rest of the run.
func TestAPowerCycleLeavesOneWindowChain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.n.PowerOff(h.now)

	// A receiver that locks on while the device is down.
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{Lat: 37.775, Lon: -122.42, AccuracyM: 3, Time: t0},
	}), h.now)
	if got := countJobs(h.n, jobWindow); got != 0 {
		t.Errorf("a powered-down device armed %d radio windows", got)
	}

	h.n.PowerOn(h.now)
	if got := countJobs(h.n, jobWindow); got != 1 {
		t.Fatalf("after a power cycle there are %d window chains, want 1", got)
	}

	// And that is what it sends: one status per peer per period.
	h.advance(bootDebounce)
	h.take()
	h.advance(9 * time.Second)
	perPeer := map[mesh.MAC]int{}
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Peer); ok && m.Command == mesh.PeerStatus {
			perPeer[s.Dst]++
		}
	}
	for dst, n := range perPeer {
		// Two periods in nine seconds, and the firmware sends a second
		// copy to a far peer — four is the ceiling, eight is two chains.
		if n > 4 {
			t.Errorf("sent %d status frames to %s in 9 s", n, dst)
		}
	}
}

func countJobs(n *Node, kind jobKind) int {
	c := 0
	for _, j := range n.jobs {
		if j.kind == kind {
			c++
		}
	}
	return c
}

// TestAPowerCycleOwesNoMeshReply: sendOutbox repeats the last locate
// reply for ten seconds. One owed at the moment the power went would go
// out on the next power-up, answering — with a fresh position and the
// old UID — a request from the far side of a power cycle the peers never
// saw.
func TestAPowerCycleOwesNoMeshReply(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.replyUID, h.n.replyAt, h.n.lastReply = 4242, h.now, h.now

	h.n.PowerOff(h.now)
	if !h.n.replyAt.IsZero() {
		t.Error("powering down left a mesh reply owed")
	}
	h.n.PowerOn(h.now)
	h.take()
	h.advance(12 * time.Second)
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Locate); ok && m.UID == 4242 {
			t.Error("a reply owed before the power cycle went out after it")
		}
	}
}

// TestAPowerCycleStartsTheCountersAgain: coming back up is a boot — it
// runs the post-boot touch wait and the power-up ring, and on the device
// the wake restarts main.py and builds modes again. The counters modes
// starts at zero start again with it.
func TestAPowerCycleStartsTheCountersAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.advance(bootDebounce)
	h.collect(h.n.Poll(h.now))
	if h.n.Power().LearnedMaxVolts() == 0 {
		t.Fatal("nothing was learned to lose")
	}

	h.n.PowerOff(h.now)
	h.now = h.now.Add(time.Hour)
	h.n.PowerOn(h.now)

	if got := h.n.Power().LearnedMaxVolts(); got != 0 {
		t.Errorf("a power cycle kept a learned maximum of %v", got)
	}
	if got := h.n.Power().SleptMs(); got != 0 {
		t.Errorf("a power cycle kept %d ms of sleep", got)
	}
	// And the hour it spent switched off is not counted as time awake.
	h.collect(h.n.Poll(h.now))
	if got := h.n.Power().Duty(); got < 0 || got > 1 {
		t.Errorf("the duty cycle is %v after a power cycle", got)
	}
}
