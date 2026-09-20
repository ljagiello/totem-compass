package emulator

// Regressions for the ninth review round.

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestASmartGroupBondOutlivesThePairingWindow: a group can finalize while
// a pairing window is still open. The window ends by deleting whatever
// bond it had half-made — and that is often a Totem the group has just
// bonded us to, so the timer undid the group's own work seconds later.
func TestASmartGroupBondOutlivesThePairingWindow(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.collect(h.n.Pair(h.now))

	// A bond request from the owned Totem: half-made, held as tempBond.
	h.rx(totem, self, -20, bondFrame(false))
	if h.n.tempBond == nil {
		t.Fatal("the bond request did not start a bond")
	}

	// The group forms and names both of us.
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 20000,
		Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: self}},
	})
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupFinalize, UID: 7,
		Members: []mesh.SmartGroupMember{{MAC: self}, {MAC: totem}},
	})
	if h.n.BondCount() != 1 {
		t.Fatalf("the group left %d bonds", h.n.BondCount())
	}

	// Well past where the pairing window would have closed.
	h.advance(pairingWindow + 2*time.Second)
	if got := h.n.BondCount(); got != 1 {
		t.Errorf("the pairing window deleted the group's bond: %d bonds left", got)
	}
	if h.n.Pairing() {
		t.Error("the group finalized and the device is still pairing")
	}
}

// TestTheSavedNameComesBack: State saves the device name, so Restore has
// to put it back. It did not, and the `store open` path — the one that
// exists to recover settings — read the saved name off the flash and then
// wrote the default straight back over it.
func TestTheSavedNameComesBack(t *testing.T) {
	h := newHarness(t, nil)
	was := h.n.Config().Name
	h.n.Restore(&store.State{Name: "lcfs_totem"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_totem" {
		t.Errorf("the name is %q after a restore, want the saved one (was %q)", got, was)
	}
	// And what it saves next carries the restored name, so it survives.
	if got := h.n.State(1).Name; got != "lcfs_totem" {
		t.Errorf("the state saves %q", got)
	}
}

// TestPosZeroZeroIsRefused: fix() reads 0,0 as no fix, so SetPosition has
// to refuse it rather than store something every reader ignores — the
// device reporting a position it does not have.
func TestPosZeroZeroIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 0, Lon: 0, AccuracyM: 5}, h.now); err == nil {
		t.Error("SetPosition accepted Null Island, which fix() reads as no fix")
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

// TestASecondUpdateDoesNotReportTheFirstOne: a run that fails at the
// first request used to report the release and package the last one
// installed — and that report is posted back to the server as what this
// device is running.
func TestASecondUpdateDoesNotReportTheFirstOne(t *testing.T) {
	h := newHarness(t, nil)
	h.n.ota.release = Release{Version: "5.0.3", ReleaseID: 339}
	h.n.ota.pkg = "firmware_v5.0.3.bin"
	h.n.ota.state = OTADone

	// No transport, so it fails at the first step.
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("an update with no transport was allowed")
	}
	if got := h.n.ota.Release().Version; got != "" {
		t.Errorf("a failed update still reports release %q", got)
	}
	if got := h.n.ota.Package(); got != "" {
		t.Errorf("a failed update still reports package %q", got)
	}
}

// TestTheClockWarningReachesTheConsole: the warning explains why a Totem
// never picks up a clock, and totemctl decodes "src" as a MAC — so a line
// putting a name there failed to parse and was dropped whole, which is
// the one place it had to arrive.
func TestTheClockWarningReachesTheConsole(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.bond()
	h.advance(rtcSyncDelay + time.Second)

	old := statusFrame(t0)
	old.Unix, old.TimeOfDayMs = 1, 1
	h.rx(totem, self, -40, old)

	var line string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "ignoring a peer's clock") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("the warning was not logged at all: %s", logged.String())
	}
	// totemctl decodes src as a MAC, so a line carrying anything else
	// there does not parse and never reaches the person watching. The
	// name has a key of its own.
	if strings.Contains(line, "src=") {
		t.Errorf("the warning uses src, which totemctl reads as a MAC: %s", line)
	}
	if !strings.Contains(line, "name=Lukasz") {
		t.Errorf("the warning does not name the peer: %s", line)
	}
	if !strings.Contains(line, "mac=") {
		t.Errorf("the warning does not say which peer: %s", line)
	}
}

// TestAClockThePeerFrameCannotCarry: the Unix field in a peer frame is an
// int32, as it is in the firmware. Fuzzing set a clock in 2055 and the
// node advertised 1919 — the cast wrapped — which is exactly the value a
// peer with no clock of its own would have taken as real.
func TestAClockThePeerFrameCannotCarry(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()

	// Past what the frame can hold: refused, like one from before 2020.
	tooLate := maxClock.Add(time.Hour)
	if err := h.n.SetClock(tooLate, h.now); err == nil {
		t.Errorf("accepted a clock of %s, which a peer frame cannot carry", tooLate)
	}
	if h.n.gnssClock {
		t.Error("a clock the frame cannot carry was taken anyway")
	}

	// One just inside the ceiling is fine, and what goes out is never a
	// second from the wrong side of the epoch — not even once the device
	// has been up long enough to cross it.
	ok := maxClock.Add(-time.Hour)
	if err := h.n.SetClock(ok, h.now); err != nil {
		t.Fatalf("a clock an hour inside the ceiling was refused: %v", err)
	}
	h.take()
	h.advance(2 * time.Hour)
	for _, s := range h.take() {
		m, isPeer := s.msg.(mesh.Peer)
		if !isPeer || (m.Unix == -1 && m.TimeOfDayMs == -1) {
			continue
		}
		if got := time.Unix(int64(m.Unix), 0); got.Before(minClock) {
			t.Errorf("advertised %s, which is before %s", got, minClock)
		}
	}
}
