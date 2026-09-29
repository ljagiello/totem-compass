package emulator

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestCandidateFor(t *testing.T) {
	for _, tt := range []struct {
		name        string
		pitch, roll float64
		want        mesh.Orientation
	}{
		{"upright", -60, 10, mesh.OrientationVertical},
		{"upright, just steep enough", -35, 1, mesh.OrientationVertical},
		{"flat", 0, 0, mesh.OrientationHorizontal},
		{"flat, just shallow enough", -34.9, 0, mesh.OrientationHorizontal},
		{"flat, rolled to the edge", 0, 34.9, mesh.OrientationHorizontal},
		{"flat, rolled the other way", 0, -34.9, mesh.OrientationHorizontal},
		// Steep but rolled the wrong way is neither: the firmware's
		// vertical test wants a roll between 0 and 35, exclusive of zero.
		{"steep and rolled negative", -60, -10, mesh.OrientationUnknown},
		{"steep and rolled level", -60, 0, mesh.OrientationUnknown},
		{"steep and rolled far", -60, 40, mesh.OrientationUnknown},
		{"shallow and rolled far", 0, 40, mesh.OrientationUnknown},
		{"upside down", 0, 180, mesh.OrientationUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidateFor(tt.pitch, tt.roll); got != tt.want {
				t.Errorf("candidateFor(%v, %v) = %v, want %v", tt.pitch, tt.roll, got, tt.want)
			}
		})
	}
}

// TestOrientationNeedsTheDwell: a pose has to persist before it is
// committed. Without this every wobble of a Totem in a hand would be a
// status frame to every peer.
func TestOrientationNeedsTheDwell(t *testing.T) {
	var tr OrientationTracker
	now := time.Now()
	// Flat takes 300 ms.
	if got := tr.Update(0, 0, now); got != mesh.OrientationUnknown {
		t.Errorf("the first flat reading committed straight to %v", got)
	}
	if got := tr.Update(0, 0, now.Add(299*time.Millisecond)); got != mesh.OrientationUnknown {
		t.Errorf("flat committed after 299 ms, before its 300 ms dwell: %v", got)
	}
	if got := tr.Update(0, 0, now.Add(300*time.Millisecond)); got != mesh.OrientationHorizontal {
		t.Errorf("flat did not commit after its 300 ms dwell: %v", got)
	}
}

// TestOrientationHoldsTheOldState: the state being replaced has to have had
// its turn, and upright's turn is 2.8 s where flat's is 800 ms.
func TestOrientationHoldsTheOldState(t *testing.T) {
	var tr OrientationTracker
	now := time.Now()
	// Commit flat.
	tr.Update(0, 0, now)
	if got := tr.Update(0, 0, now.Add(300*time.Millisecond)); got != mesh.OrientationHorizontal {
		t.Fatalf("flat did not commit: %v", got)
	}
	at := now.Add(300 * time.Millisecond)

	// Upright's own dwell is 100 ms, but flat has to have lasted 800 ms
	// first, so a reading 100 ms later is not enough.
	tr.Update(-60, 10, at)
	if got := tr.Update(-60, 10, at.Add(100*time.Millisecond)); got != mesh.OrientationHorizontal {
		t.Errorf("upright took over 100 ms into flat's 800 ms hold: %v", got)
	}
	if got := tr.Update(-60, 10, at.Add(800*time.Millisecond)); got != mesh.OrientationVertical {
		t.Errorf("upright did not take over once flat had its 800 ms: %v", got)
	}
	up := at.Add(800 * time.Millisecond)

	// And now upright holds for 2.8 s, which is much longer.
	tr.Update(0, 0, up)
	if got := tr.Update(0, 0, up.Add(1*time.Second)); got != mesh.OrientationVertical {
		t.Errorf("flat took over a second into upright's 2.8 s hold: %v", got)
	}
	if got := tr.Update(0, 0, up.Add(2800*time.Millisecond)); got != mesh.OrientationHorizontal {
		t.Errorf("flat did not take over once upright had its 2.8 s: %v", got)
	}
}

// TestOrientationIgnoresReadingsForNeither: a pose that is neither leaves
// the candidate as it was, rather than resetting it. The firmware has no
// else branch, so a single odd reading mid-transition must not restart the
// dwell it was most of the way through.
func TestOrientationIgnoresReadingsForNeither(t *testing.T) {
	var tr OrientationTracker
	now := time.Now()
	tr.Update(0, 0, now) // flat starts arguing
	// A reading for neither, 200 ms in.
	tr.Update(-60, -10, now.Add(200*time.Millisecond))
	// The flat case should still be running, so its 300 ms is up.
	if got := tr.Update(0, 0, now.Add(300*time.Millisecond)); got != mesh.OrientationHorizontal {
		t.Errorf("a reading for neither restarted the dwell: %v", got)
	}
}

func TestOrientationReportsWithoutAReading(t *testing.T) {
	var tr OrientationTracker
	if got := tr.Orientation(); got != mesh.OrientationUnknown {
		t.Errorf("a tracker with no readings says %v, want unknown", got)
	}
	now := time.Now()
	tr.Update(0, 0, now)
	tr.Update(0, 0, now.Add(300*time.Millisecond))
	if got := tr.Orientation(); got != mesh.OrientationHorizontal {
		t.Errorf("after committing flat, Orientation says %v", got)
	}
}

// driverSensors stands in for a board's own drivers: a source that is not a
// held reading and cannot be rebuilt from one, because the real thing owns
// a power chip, a serial port and an IMU opened once at startup.
type driverSensors struct{ reads int }

func (d *driverSensors) Read(time.Time) Sensors {
	d.reads++
	return Sensors{Battery: Battery{Volts: 4.0, Percent: 80}}
}

// TestSimGivesTheHardwareBack: starting a simulation on a board sets its
// drivers aside, and stopping one has to give them back. Installing a fresh
// held reading instead left the board inventing a battery, a position and an
// orientation while its own parts sat powered and unread until a reboot.
func TestSimGivesTheHardwareBack(t *testing.T) {
	drv := &driverSensors{}
	h := newHarness(t, func(c *Config) { c.Sensors = drv })
	h.collect(h.n.Poll(h.now))
	if drv.reads == 0 {
		t.Fatal("the driver was never read to begin with")
	}

	// No position set first: this source is a driver and takes no console
	// settings, which is exactly the case being tested. The simulation
	// warns that it has nowhere to walk from and starts anyway.
	h.n.StartSim(Walk, 90, h.now)
	if h.n.sim() == nil {
		t.Fatal("the simulation did not start")
	}
	was := drv.reads
	h.collect(h.n.Poll(h.now.Add(time.Second)))
	if drv.reads != was {
		t.Error("the driver was still read while the simulation ran")
	}

	h.n.StopSim(h.now.Add(2 * time.Second))
	if h.n.sim() != nil {
		t.Fatal("the simulation did not stop")
	}
	h.collect(h.n.Poll(h.now.Add(3 * time.Second)))
	if drv.reads == was {
		t.Error("the board's drivers were not read again after the simulation stopped")
	}
	if got := h.n.Sensors().Battery.Percent; got != 80 {
		t.Errorf("battery %d%% after the simulation stopped, want the driver's 80%%", got)
	}
}

// TestSetSensorsSettlesWhatStopSimRestores: the stash is only for the source
// StartSim itself pushed aside. A source arriving by any other route while a
// simulation runs settles the question, and a later StopSim must not
// reinstate a driver that something else had already replaced — that would
// discard both it and the simulation's reading.
func TestSetSensorsSettlesWhatStopSimRestores(t *testing.T) {
	drv := &driverSensors{}
	h := newHarness(t, func(c *Config) { c.Sensors = drv })
	h.collect(h.n.Poll(h.now))
	h.n.StartSim(Walk, 90, h.now)

	// Something else installs a source while the simulation runs.
	other := NewStatic(Sensors{Battery: Battery{Volts: 3.9, Percent: 61}})
	h.n.SetSensors(other, h.now.Add(time.Second))

	h.n.StopSim(h.now.Add(2 * time.Second))
	h.collect(h.n.Poll(h.now.Add(3 * time.Second)))
	if got := h.n.Sensors().Battery.Percent; got != 61 {
		t.Errorf("battery %d%% after stopping, want the 61%% the other source reports", got)
	}
}

// TestSimOnAHeldReadingStillFreezes: the other half of the same rule. With
// no driver to give back, stopping a simulation keeps its last reading,
// which is what it has always done.
func TestSimOnAHeldReadingStillFreezes(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 1, Lon: 2}, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.StartSim(Drive, 90, h.now)
	h.collect(h.n.Poll(h.now.Add(30 * time.Second)))
	moved := h.n.Fix()
	if moved == nil {
		t.Fatal("the simulation produced no position")
	}
	h.n.StopSim(h.now.Add(31 * time.Second))
	frozen := h.n.Fix()
	if frozen == nil {
		t.Fatal("stopping the simulation lost the position")
	}
	if frozen.Lat != moved.Lat || frozen.Lon != moved.Lon {
		t.Errorf("stopped at %v,%v but the simulation had reached %v,%v",
			frozen.Lat, frozen.Lon, moved.Lat, moved.Lon)
	}
}
