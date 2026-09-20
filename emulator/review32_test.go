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
	// Nothing is asserted about the reading here. Both setters return
	// before anything touches the sensors, so a check on them — or on
	// the frame, which status() builds from the same sensors — cannot
	// fail whatever the setters do. What matters is that a value that
	// gets past a setter does not reach the air, and that is
	// TestAHeadingThatIsNotABearing, which drives one in through a door
	// with no check on it at all.
	_ = was
	if err := h.n.SetPosition(&Position{Lat: 91, Lon: 0}, h.now); err == nil {
		t.Error("SetPosition(91) was accepted")
	}
}

// TestASentinelIsNotABearing: the compass field has no value meaning
// "unknown", and -1 is what this package writes when it means that —
// HeadingOfMotion, SpeedKPH and PosAccuracyM all use it. Folding an
// out-of-range reading modulo 360 turned that -1 into 359, one degree
// west of north, and put it on the air as a direction every peer's
// compass would point in. SetHeading refuses -1 outright, so the two
// doors disagreed about the same value.
func TestASentinelIsNotABearing(t *testing.T) {
	pack := Battery{Volts: 4.1, Percent: 90}
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Azimuth: 187, Battery: pack})
	})
	h.advance(time.Second)
	if got := h.n.Sensors().Azimuth; got != 187 {
		t.Fatalf("the compass reads %d before anything went wrong", got)
	}

	for _, bad := range []int16{-1, -30, 360, 900, -32768} {
		h.n.SetSensors(NewStatic(Sensors{Azimuth: bad, Battery: pack}), h.now)
		got := h.n.Sensors().Azimuth
		if got < 0 || got > 359 {
			t.Errorf("a reading of %d came back as %d, which is not a bearing", bad, got)
		}
		if got != 187 {
			t.Errorf("a reading of %d became %d; the last bearing the compass saw was 187", bad, got)
		}
		// And it is not on the air either.
		if f := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; f != 187 {
			t.Errorf("a reading of %d reached the air as %d", bad, f)
		}
	}

	// A real bearing still takes.
	h.n.SetSensors(NewStatic(Sensors{Azimuth: 42, Battery: pack}), h.now)
	if got := h.n.Sensors().Azimuth; got != 42 {
		t.Errorf("a bearing of 42 came back as %d", got)
	}
}

// TestAnOrientationTheFrameHasAMeaningFor: New settles this for what it
// is given, and a source set later never passes through New — so a
// driver that leaves the field alone told every peer "unknown", and
// Config() handed the same out through the front door.
func TestAnOrientationTheFrameHasAMeaningFor(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 4.1, Percent: 90}})
	})
	if got := h.n.Sensors().Orientation; got == mesh.OrientationUnknown {
		t.Error("a source that said nothing about orientation was left saying unknown")
	}
	if got := h.n.Config().Orientation; got == mesh.OrientationUnknown {
		t.Error("Config reports an orientation of unknown")
	}
	if got := h.n.status(h.now, mesh.PeerStatus, false).Orientation; got == mesh.OrientationUnknown {
		t.Error("a status frame carried an orientation of unknown")
	}
}
