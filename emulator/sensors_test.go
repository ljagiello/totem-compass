package emulator

import (
	"math"
	"math/rand/v2"
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
	s := NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Motion: Walk, Bearing: 90, Percent: -1}, t0)
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
	s.SetFix(nil)
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
		c.Sensors = NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Percent: -1}, t0)
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
		if err := h.n.SetFlat(true, h.now); err != nil {
			t.Fatal(err)
		}
		if err := h.n.SetBattery(17, true, h.now); err != nil {
			t.Fatal(err)
		}
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
		c.Sensors = NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Clock: t0, Percent: -1}, t0)
	})
	if got := h.n.period(); got != 4*time.Second {
		t.Errorf("upright period = %v, want 4s", got)
	}
	if err := h.n.SetFlat(true, h.now); err != nil {
		t.Fatal(err)
	}
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

// TestHostClockKeepsRunning: the clock totemctl hands a board with no GNSS
// has to run on by itself. Stamping it into the fixed reading pinned it:
// read took the same instant back every poll, so the emulated wall clock
// stood still, and the windows, the mesh tick and the advertised time
// with it.
func TestHostClockKeepsRunning(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3, AltitudeM: -500, HeadingOfMotion: -1}
	})
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(10 * time.Minute)
	want := t0.Add(10 * time.Minute)
	if got := h.n.wall(h.now); got.Sub(want).Abs() > time.Second {
		t.Errorf("wall clock = %s ten minutes on, want %s", got.UTC(), want.UTC())
	}
	if got := h.n.Sensors().Fix.Time; got.Sub(want).Abs() > time.Second {
		t.Errorf("fix time = %s, want %s", got.UTC(), want.UTC())
	}
	// The status frames carry the running clock, not the one it started at.
	h.bond()
	h.advance(10 * time.Second)
	var seen int
	for _, s := range h.take() {
		p, ok := s.msg.(mesh.Peer)
		if !ok || p.Command != mesh.PeerStatus {
			continue
		}
		seen++
		if got := time.Unix(int64(p.Unix), 0); got.Sub(s.at).Abs() > 2*time.Second {
			t.Errorf("advertised %s in a frame sent at %s", got.UTC(), s.at.UTC())
		}
	}
	if seen == 0 {
		t.Fatal("no status frames")
	}
}

// TestSimAzimuthStaysPositive: a walk wanders, and its course drifts below
// zero sooner or later. math.Mod keeps the sign, so the compass reported
// negative degrees in the status frames.
func TestSimAzimuthStaysPositive(t *testing.T) {
	s := NewSim(SimConfig{Lat: 37.775, Lon: -122.42, Motion: Walk, Clock: t0, Rand: rand.New(rand.NewPCG(7, 9)), Percent: -1}, t0)
	s.bearing = -725.5 // a walk that has turned left twice round
	if got := s.Read(t0.Add(time.Second)); got.Azimuth < 0 || got.Azimuth > 359 {
		t.Errorf("azimuth = %d, want 0-359", got.Azimuth)
	}
	for _, deg := range []float64{-0.5, -360, -359.9, 719.5, 0} {
		if n := norm360(deg); n < 0 || n >= 360 {
			t.Errorf("norm360(%v) = %v, want 0 to just under 360", deg, n)
		}
	}
}

// TestSetHeadingSteersTheSim: heading reached the fixed reading only, so
// while a simulation ran it reported success and changed nothing.
func TestSetHeadingSteersTheSim(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3, AltitudeM: -500, HeadingOfMotion: -1}
	})
	// A drive holds its course, so the track shows the steering; a walk
	// wanders and would wander off it.
	h.n.StartSim(Drive, 0, h.now)
	if err := h.n.SetHeading(90, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.n.Sensors().Azimuth; got != 90 {
		t.Errorf("azimuth = %d, want 90", got)
	}
	if got := h.n.Config().Heading; got != 90 {
		t.Errorf("Config().Heading = %d, want 90", got)
	}
	// It steers the walk as well: a Totem someone carries points where it
	// is going.
	start := *h.n.Sensors().Fix
	h.advance(2 * time.Minute)
	if end := h.n.Sensors().Fix; end.Lon <= start.Lon {
		t.Errorf("driving east from longitude %f ended at %f", start.Lon, end.Lon)
	}
}

// TestSetBatteryRestartsTheDrain: the level set by hand is the level
// reported, not the level the simulation would have drained to by now.
func TestSetBatteryRestartsTheDrain(t *testing.T) {
	h := newHarness(t, nil)
	h.n.StartSim(Still, 0, h.now)
	h.advance(6 * time.Hour)
	if err := h.n.SetBattery(80, false, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.n.Sensors().Battery.Percent; got != 80 {
		t.Errorf("battery = %d%% right after setting 80%%", got)
	}
	h.advance(time.Hour)
	if got := h.n.Sensors().Battery.Percent; got < 68 || got > 76 {
		t.Errorf("battery = %d%% an hour later, want about 73%% (80%% over a 12 h life)", got)
	}
}

// TestSensorsCannotBeWrittenThrough: Sensors() looks like a read, and its
// Fix was a pointer into the node's own reading — so a caller could put a
// position on the air that usablePosition exists to keep out.
func TestSensorsCannotBeWrittenThrough(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	got := h.n.Sensors()
	if got.Fix == nil {
		t.Fatal("no fix to test with")
	}
	got.Fix.Lat = float32(math.NaN())
	if f := h.n.fix(); f == nil || math.IsNaN(float64(f.Lat)) {
		t.Error("a caller wrote a position through Sensors()")
	}
}

// TestAWalkThatStartsNowhere: StartSim is exported and took any course
// it was handed, and the simulation folds a course modulo 360 — so -1
// walked at 359, one degree west of north, which SetHeading refuses and
// which this package writes elsewhere to mean "no value". It was the
// door left open after the others were closed.
func TestAWalkThatStartsNowhere(t *testing.T) {
	at := func() *Position { return &Position{Lat: 52.2, Lon: 21.0, AccuracyM: 3} }
	for _, bad := range []int16{-1, 360, 900, -32768} {
		h := newHarness(t, func(c *Config) { c.Position = at() })
		h.n.StartSim(Walk, bad, h.now)
		h.advance(2 * time.Second)
		// North, within the wander a walk puts on its own course.
		if got := h.n.Sensors().Azimuth; !usableBearing(got) || (got > 45 && got < 315) {
			t.Errorf("a walk started on a course of %d reports %d, and north was the default", bad, got)
		}
	}
	// And a course that is a bearing is walked as given. A walk wanders
	// around its course, so this is a band rather than a number.
	h := newHarness(t, func(c *Config) { c.Position = at() })
	h.n.StartSim(Walk, 90, h.now)
	h.advance(2 * time.Second)
	if got := h.n.Sensors().Azimuth; got < 45 || got > 135 {
		t.Errorf("a walk started east reports %d", got)
	}
}

// TestTheSimCrossesThePoleInsteadOfSittingOnIt: the fold reflected the
// latitude and put the longitude on the far side, but left the course
// alone — so the next step set off north again from just below the pole,
// crossed it again, and the track alternated between two points forever.
// A peer watching saw the longitude flip 180 degrees at the poll rate
// and the heading alternate, while the odometer kept climbing.
//
// TestTheSimStaysOnTheGlobe passes either way: it only asks that the
// values stay finite and in range, which two points at the pole are.
func TestTheSimCrossesThePoleInsteadOfSittingOnIt(t *testing.T) {
	s := NewSim(SimConfig{
		Lat: 89.9, Lon: 0, Motion: Drive, Bearing: 0,
	}, t0)

	// Long enough to get there and well down the other side: 50 km/h for
	// 100 minutes is about 83 km, and the pole is 11 km away.
	now := t0
	var lats, lons []float64
	for range 200 {
		now = now.Add(30 * time.Second)
		s.Read(now)
		lats = append(lats, s.lat)
		lons = append(lons, s.lon)
	}

	// It must have got to the other side: a latitude that came back down
	// well past the pole, rather than hovering just below it.
	var lowest = math.Inf(1)
	for _, l := range lats {
		lowest = math.Min(lowest, l)
	}
	if lowest > 89.5 {
		t.Errorf("driving north from 89.9 for 100 minutes never left the pole: lowest latitude %.5f", lowest)
	}

	// And it is still going the same way on the ground rather than
	// oscillating: no two consecutive steps may reverse direction more
	// than once, which is the crossing itself.
	reversals := 0
	for i := 2; i < len(lats); i++ {
		a, b := lats[i-1]-lats[i-2], lats[i]-lats[i-1]
		if a != 0 && b != 0 && (a > 0) != (b > 0) {
			reversals++
		}
	}
	if reversals > 1 {
		t.Errorf("the track changed direction %d times; crossing the pole is one", reversals)
	}

	// And the longitude settles on the far side rather than flipping back
	// and forth: one change, at the crossing.
	flips := 0
	for i := 1; i < len(lons); i++ {
		if math.Abs(lons[i]-lons[i-1]) > 90 {
			flips++
		}
	}
	if flips > 1 {
		t.Errorf("the longitude jumped to the far side %d times; crossing the pole is one", flips)
	}
}

// TestAStaticSourceCopiesTheFixItWasGiven: every other ingress copies,
// and says why — a caller must not be able to write through into the
// readings. NewStatic is the exported constructor a board driver calls,
// and it kept the caller's pointer.
func TestAStaticSourceCopiesTheFixItWasGiven(t *testing.T) {
	fix := &Fix{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3}
	src := NewStatic(Sensors{Fix: fix, Battery: Battery{Volts: 4.1, Percent: 90}})

	// The caller keeps hold of it and writes something impossible.
	fix.Lat = float32(math.NaN())

	got := src.Read(t0)
	if got.Fix == nil {
		t.Fatal("the fix went away")
	}
	if math.IsNaN(float64(got.Fix.Lat)) {
		t.Error("a caller wrote through NewStatic into the readings")
	}
}

// TestSimStartsFromAPositionWeWouldUse: the simulation read the raw fix
// rather than the one the node will actually use, so a garbled configured
// position started a walk from a NaN — and every reading after it was NaN
// too, while `sim` cheerfully reported a walk in progress.
func TestSimStartsFromAPositionWeWouldUse(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: float32(math.NaN()), Lon: float32(math.NaN()), AccuracyM: 3}
	})
	h.n.StartSim(Walk, 90, h.now)
	h.advance(10 * time.Second)
	if f := h.n.fix(); f != nil && !usablePosition(f.Lat, f.Lon) {
		t.Errorf("the simulation is walking from %v, %v", f.Lat, f.Lon)
	}
}

// TestTheSimStaysOnTheGlobe: a long enough drive north ran the latitude
// past the pole, and the longitude step divides by cos(lat), so the track
// left the globe and the device silently stopped having a position at
// all — the ring back to searching, with nothing in the log to say why.
func TestTheSimStaysOnTheGlobe(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 89.9, Lon: 0, AccuracyM: 3}, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.StartSim(Drive, 0, h.now)

	// Long past the pole at 50 km/h.
	for range 60 {
		h.advance(time.Minute)
		f := h.n.fix()
		if f == nil {
			t.Fatalf("the simulation lost its fix after driving north")
		}
		if math.IsNaN(float64(f.Lat)) || math.IsInf(float64(f.Lon), 0) {
			t.Fatalf("the track reached %v, %v", f.Lat, f.Lon)
		}
		if f.Lat < -90 || f.Lat > 90 || f.Lon < -180 || f.Lon > 180 {
			t.Fatalf("the track left the globe at %v, %v", f.Lat, f.Lon)
		}
	}
}
