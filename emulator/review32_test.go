package emulator

// Regressions for the thirty-first and thirty-second review rounds.
//
// The thirty-first landed eleven fixes and one test, which is how a fix
// that was never applied — the drain on Receive's unparseable-frame exit
// — came to ship with a comment saying it had been. These cover the rest
// of that round as well as this one.

import (
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestAHeadingThatIsNotABearing: SetHeading refuses its own argument,
// but that is one door of several — a config, a driver or a simulation
// all reach the reading, and an azimuth of 900 went into every status
// frame through any of them.
func TestAHeadingThatIsNotABearing(t *testing.T) {
	// The exported setter says no.
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	for _, deg := range []int16{-1, 360, 900, -30} {
		if err := h.n.SetHeading(deg, h.now); err == nil {
			t.Errorf("SetHeading(%d) was accepted", deg)
		}
	}
	if err := h.n.SetHeading(187, h.now); err != nil {
		t.Errorf("SetHeading(187): %v", err)
	}

	// And a reading that arrives another way is not sent either.
	h = newHarness(t, func(c *Config) { c.Heading = 900 })
	if got := h.n.Sensors().Azimuth; got < 0 || got > 359 {
		t.Errorf("a configured heading of 900 came back as %d", got)
	}
	if got := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; got < 0 || got > 359 {
		t.Errorf("a status frame carried an azimuth of %d", got)
	}
}

// TestUptimeHasBothEnds: the field is a uint16 of minutes. A board up
// 45 days wrapped to zero and told every peer it had just started; a
// clock that runs backwards made it negative, and uint16 of that is 45
// days of uptime rather than none.
func TestUptimeHasBothEnds(t *testing.T) {
	h := newHarness(t, nil)
	for _, d := range []time.Duration{
		0, time.Minute, 24 * time.Hour,
		46 * 24 * time.Hour, // past what the field holds
		400 * 24 * time.Hour,
	} {
		got := h.n.status(t0.Add(d), mesh.PeerStatus, false).UptimeMin
		want := uint16(min(d/time.Minute, math.MaxUint16))
		if got != want {
			t.Errorf("after %v the frame says %d minutes, want %d", d, got, want)
		}
	}
	// And a stamp before the node was built says none, not 45 days.
	if got := h.n.status(t0.Add(-time.Hour), mesh.PeerStatus, false).UptimeMin; got != 0 {
		t.Errorf("a clock that ran backwards gave an uptime of %d minutes", got)
	}
}

// TestConfigReportsWhereTheDeviceIsNow: cfg.Position is what the node
// was built or last told, and the sources hand out a fresh reading every
// time. A walking simulation moved and Config() went on answering with
// where it set off from.
func TestConfigReportsWhereTheDeviceIsNow(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 52.2, Lon: 21.0, AccuracyM: 3}, h.now); err != nil {
		t.Fatal(err)
	}
	start := h.n.Config().Position
	if start == nil || start.Lat != 52.2 {
		t.Fatalf("the position did not take: %+v", start)
	}

	h.n.SetSensors(NewSim(SimConfig{
		Lat: 52.2, Lon: 21.0, Motion: Walk, Bearing: 90, Percent: -1,
	}, h.now), h.now)
	h.advance(10 * time.Minute)

	got := h.n.Config().Position
	if got == nil {
		t.Fatal("a walking device reports no position")
	}
	if got.Lon == start.Lon {
		t.Errorf("after ten minutes of walking east the position is still %v", got.Lon)
	}
}

// TestApplyRefusesAnEmptyRXCommand: Command and Apply are both exported,
// and String() already answers "rx" for one with no frame. On the board
// a nil dereference is a boot loop rather than a refused command.
func TestApplyRefusesAnEmptyRXCommand(t *testing.T) {
	h := newHarness(t, nil)
	_, handled, err := h.n.Apply(Command{Op: OpRX}, h.now)
	if !handled {
		t.Error("an rx command with no frame was not handled at all")
	}
	if err == nil {
		t.Error("an rx command with no frame was accepted")
	}
}

// TestSetBatteryAndHeadingReportTheirRefusals: the setters say no rather
// than recording something else, which is the rule SetPosition states.
func TestSetBatteryAndHeadingReportTheirRefusals(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	was := h.n.Sensors()
	if err := h.n.SetBattery(120, false, h.now); err == nil {
		t.Error("SetBattery(120) was accepted")
	}
	if err := h.n.SetHeading(900, h.now); err == nil {
		t.Error("SetHeading(900) was accepted")
	}
	// And nothing moved on the way to being refused — asked of the frame
	// the device would send, which is what a peer would see, rather than
	// of the reading: both setters return before anything reads the
	// sensors at all, so a check there cannot fail whatever they do.
	f := h.n.status(h.now, mesh.PeerStatus, false)
	if f.BattPct != was.Battery.Percent || f.Azimuth != was.Azimuth {
		t.Errorf("a refused setting reached the air: batt %d%%, azimuth %d", f.BattPct, f.Azimuth)
	}
	if err := h.n.SetPosition(&Position{Lat: 91, Lon: 0}, h.now); err == nil {
		t.Error("SetPosition(91) was accepted")
	}
}
