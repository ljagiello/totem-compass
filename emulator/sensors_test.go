package emulator

import (
	"math"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestSimWalks(t *testing.T) {
	s := NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Motion: Walk, Bearing: 90, Percent: 80, Life: time.Hour, Clock: t0}, t0)
	var last Sensors
	for i := 1; i <= 60; i++ {
		last = s.Read(t0.Add(time.Duration(i) * time.Second))
	}
	if last.Fix == nil {
		t.Fatal("no fix")
	}
	// A minute at 5 km/h is about 83 m, mostly east.
	d := distance(37.775, -122.42, last.Fix.Lat, last.Fix.Lon)
	if d < 60 || d > 110 {
		t.Errorf("walked %.0f m in a minute, want about 83", d)
	}
	if last.Fix.Lon <= -122.42 {
		t.Errorf("walking east ended at longitude %f", last.Fix.Lon)
	}
	if last.Fix.SpeedKPH != 5 {
		t.Errorf("speed %d km/h, want 5", last.Fix.SpeedKPH)
	}
	// The heading of motion is the course of the last 10 m, so it tracks
	// the compass a walk wanders with.
	h := last.Fix.HeadingOfMotion
	if h < 0 || math.Abs(float64(h-last.Azimuth)) > 45 {
		t.Errorf("heading of motion %d° against compass %d°", h, last.Azimuth)
	}
	if od := last.Fix.OdometerM; od < 60 || od > 110 {
		t.Errorf("odometer %d m", od)
	}
	if last.Fix.Time.IsZero() || last.Fix.SolutionID == 0 || last.Fix.SatCount == 0 {
		t.Errorf("fix = %+v, want a GNSS clock, a solution and satellites", *last.Fix)
	}
	if last.Battery.Percent >= 80 {
		t.Errorf("battery %d%% after a minute of a one-hour life", last.Battery.Percent)
	}
}

// TestSimWanderIsPerSecond: the driver reads the sensors every few
// milliseconds, so a per-reading wander would spin the compass.
func TestSimWanderIsPerSecond(t *testing.T) {
	s := NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Motion: Walk, Bearing: 90}, t0)
	var last Sensors
	for ms := 5; ms <= 60_000; ms += 5 { // a minute of 5 ms polls
		last = s.Read(t0.Add(time.Duration(ms) * time.Millisecond))
	}
	if off := math.Abs(float64(last.Azimuth) - 90); off > 60 {
		t.Errorf("walked a minute and the compass moved %.0f° to %d°", off, last.Azimuth)
	}
	if d := distance(37.775, -122.42, last.Fix.Lat, last.Fix.Lon); d < 60 || d > 110 {
		t.Errorf("walked %.0f m in a minute of fast polling, want about 83", d)
	}
}

func TestSimStillAndFlat(t *testing.T) {
	s := NewSim(SimConfig{Lat: 1, Lon: 2, Percent: 50, Charging: true}, t0)
	got := s.Read(t0.Add(time.Hour))
	if got.Fix.Lat != 1 || got.Fix.Lon != 2 || got.Fix.SpeedKPH != 0 {
		t.Errorf("standing still moved to %+v", *got.Fix)
	}
	if got.Battery.Percent != 50 || !got.Battery.Charging {
		t.Errorf("charging battery = %+v, want it held at 50%%", got.Battery)
	}
	if got.Orientation != mesh.OrientationVertical {
		t.Errorf("orientation = %v", got.Orientation)
	}
	s.SetFlat(true)
	if got := s.Read(t0.Add(2 * time.Hour)); got.Orientation != mesh.OrientationHorizontal {
		t.Errorf("orientation = %v, want horizontal", got.Orientation)
	}
	s.SetFix(0, 0, false)
	if got := s.Read(t0.Add(3 * time.Hour)); got.Fix != nil {
		t.Errorf("fix = %+v, want none", *got.Fix)
	}
}

// TestSimWithoutAClockStaysQuiet: a board with no wall clock counts from
// 1970. Advertising that would set an unsynced peer's RTC to 1970, so a
// fix without a real time reports none.
func TestSimWithoutAClockStaysQuiet(t *testing.T) {
	// A Sim with no Clock is a receiver that has not given the time yet.
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewSim(SimConfig{Lat: 37.775, Lon: -122.42}, t0)
	})
	h.bond()
	h.advance(10 * time.Second)
	var sent int
	for _, s := range h.take() {
		p, ok := s.msg.(mesh.Peer)
		if !ok {
			continue
		}
		sent++
		if p.Unix != -1 || p.TimeOfDayMs != -1 {
			t.Fatalf("advertised a clock the board does not have: unix %d, itod %d", p.Unix, p.TimeOfDayMs)
		}
	}
	if sent == 0 {
		t.Fatal("no frames")
	}
	if h.n.gnssClock {
		t.Error("the node thinks it has a GNSS clock")
	}
	// A clock from the host starts it.
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	if !h.n.gnssClock || h.n.wall(h.now).Sub(t0).Abs() > time.Second {
		t.Errorf("after SetClock: gnss=%v wall=%s", h.n.gnssClock, h.n.wall(h.now))
	}
	if err := h.n.SetClock(time.Unix(42, 0), h.now); err == nil {
		t.Error("accepted a 1970 clock")
	}
}

// TestSimClockIsAdvertised: a Totem with its own GNSS fix tells peers the
// time (rtc_method 1), and does not take a peer's clock.
func TestSimClockIsAdvertised(t *testing.T) {
	gnss := t0.Add(3 * time.Hour)
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Percent: 90, Clock: t0}, t0)
	})
	// The simulated receiver reports the harness clock, which starts at t0.
	h.bond()
	h.advance(10 * time.Second)
	var sent int
	for _, s := range h.take() {
		p, ok := s.msg.(mesh.Peer)
		if !ok || p.Command != mesh.PeerStatus {
			continue
		}
		sent++
		if p.Unix <= 0 || p.TimeOfDayMs < 0 {
			t.Fatalf("status without a clock: unix %d, itod %d", p.Unix, p.TimeOfDayMs)
		}
		if got, want := time.Unix(int64(p.Unix), 0).UTC(), s.at.UTC(); got.Sub(want).Abs() > time.Second {
			t.Errorf("advertised %s at %s", got, want)
		}
		if p.SpeedKPH != 0 || p.SolutionID != 1 || p.Lat == 0 {
			t.Errorf("status carries no fix: %+v", p)
		}
	}
	if sent == 0 {
		t.Fatal("no status frames")
	}
	// A peer's clock must not replace the device's own.
	h.advance(rtcSyncDelay)
	h.rx(totem, self, -50, statusFrame(gnss))
	if off := h.n.clockOffset.Abs(); off > time.Second {
		t.Errorf("clock moved %v after a peer's frame", off)
	}
}

func TestStartAndStopSim(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3, AltitudeM: -500}
	})
	h.n.StartSim(Drive, 180, h.now)
	h.advance(time.Minute)
	moving := h.n.Sensors()
	if moving.Fix == nil || moving.Fix.SpeedKPH != 50 {
		t.Fatalf("driving sensors = %+v", moving)
	}
	if d := distance(37.775, -122.42, moving.Fix.Lat, moving.Fix.Lon); d < 700 || d > 1000 {
		t.Errorf("drove %.0f m in a minute, want about 833", d)
	}
	if moving.Fix.Lat >= 37.775 {
		t.Errorf("driving south ended at latitude %f", moving.Fix.Lat)
	}
	h.n.StopSim(h.now)
	held := h.n.Sensors()
	h.advance(time.Minute)
	if after := h.n.Sensors(); after.Fix.Lat != held.Fix.Lat || after.Fix.SpeedKPH != 0 {
		t.Errorf("after stopping: %+v, want it frozen at %+v", *after.Fix, *held.Fix)
	}
}

func TestSetFlatAndBattery(t *testing.T) {
	for _, sim := range []bool{false, true} {
		h := newHarness(t, nil)
		if sim {
			h.n.StartSim(Still, 0, h.now)
		}
		h.n.SetFlat(true, h.now)
		h.n.SetBattery(17, true, h.now)
		got := h.n.Sensors()
		if got.Orientation != mesh.OrientationHorizontal {
			t.Errorf("sim=%v orientation = %v", sim, got.Orientation)
		}
		if got.Battery.Percent != 17 || !got.Battery.Charging {
			t.Errorf("sim=%v battery = %+v", sim, got.Battery)
		}
		if v := got.Battery.Volts; math.Abs(float64(v-voltsFor(17))) > 0.001 {
			t.Errorf("sim=%v volts = %v", sim, v)
		}
	}
}

// TestFlatShortensTheWindow: with a clock, a Totem lying flat transmits
// every second (speed 8) instead of every four.
func TestFlatShortensTheWindow(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Clock: t0}, t0)
	})
	if got := h.n.period(); got != 4*time.Second {
		t.Errorf("upright period = %v, want 4s", got)
	}
	h.n.SetFlat(true, h.now)
	if got := h.n.period(); got != time.Second {
		t.Errorf("flat period = %v, want 1s", got)
	}
}

func TestParseSimCommands(t *testing.T) {
	tests := []struct {
		line string
		want Command
	}{
		{"sim walk", Command{Op: OpSim, Motion: Walk, On: true}},
		{"sim drive 270", Command{Op: OpSim, Motion: Drive, Heading: 270, On: true}},
		{"sim still 0", Command{Op: OpSim, Motion: Still, On: true}},
		{"sim off", Command{Op: OpSim}},
		{"flat on", Command{Op: OpFlat, On: true}},
		{"flat off", Command{Op: OpFlat}},
		{"batt 42", Command{Op: OpBattery, Percent: 42}},
		{"batt 100 charging", Command{Op: OpBattery, Percent: 100, On: true}},
	}
	for _, tt := range tests {
		got, err := ParseCommand(tt.line)
		if err != nil || got != tt.want {
			t.Errorf("ParseCommand(%q) = %+v, %v; want %+v", tt.line, got, err, tt.want)
		}
	}
	for _, line := range []string{
		"sim", "sim fly", "sim walk 360", "sim walk -1", "sim walk x", "sim off now", "sim walk 90 extra",
		"flat", "flat yes", "batt", "batt 101", "batt -1", "batt x", "batt 50 full", "batt 50 charging extra",
	} {
		if c, err := ParseCommand(line); err == nil {
			t.Errorf("ParseCommand(%q) = %+v, want an error", line, c)
		}
	}
}
