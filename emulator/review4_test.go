package emulator

// Regressions for the fourth review round. Each one is a rule the
// firmware follows that the emulator had drifted from.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestChargerPlaysThePowerupAnimation: 'disconn_animation' is the task
// name Compass.power_conn_new runs under, and what it does is wait for
// v_in to read high three samples running and then launch
// powerup_animation — the ring the device shows at boot. A contact that
// bounces once on the way into the socket must not set it off.
func TestChargerPlaysThePowerupAnimation(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if got := h.n.LEDs().Animation(); got == AnimBoot {
		t.Fatalf("the boot animation is still playing: %s", got)
	}

	// One poll on the charger is a bounce, not a connection.
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got == AnimBoot {
		t.Error("a single charging poll played the powerup animation")
	}
	if err := h.n.SetBattery(80, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))

	// Settled on the charger, it plays once and only once.
	if err := h.n.SetBattery(80, true, h.now); err != nil {
		t.Fatal(err)
	}
	played := 0
	for i := 0; i < chargeDebounce+3; i++ {
		was := h.n.LEDs().Animation()
		h.collect(h.n.Poll(h.now))
		if h.n.LEDs().Animation() == AnimBoot && was != AnimBoot {
			played++
		}
		h.now = h.now.Add(100 * time.Millisecond)
	}
	if played != 1 {
		t.Errorf("the powerup animation ran %d times on one connection, want 1", played)
	}
}

// TestUpdateWithNoPowerChip: the OTA gate refuses a flat battery
// ("Battery too low for OTA update"), and a 0% reading is as flat as it
// gets. A board with no power chip is a different case: it reports 0 V
// and 0% because nothing has told it otherwise, and Power.update already
// reads that as "no reading". The gate has to agree with it, or such a
// board could never update.
func TestUpdateWithNoPowerChip(t *testing.T) {
	// A real reading of 0% is still refused.
	h := newHarness(t, nil)
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if err := h.n.Update(h.now); err == nil {
		t.Error("a flat battery did not stop the update")
	} else if !strings.Contains(err.Error(), "battery") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	// A board that reports nothing at all is not refused for its battery.
	h = newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 0, 0 })
	h.collect(h.n.Poll(h.now))
	if b := h.n.Sensors().Battery; b.Volts != 0 || b.Percent != 0 {
		t.Fatalf("expected a board with no reading, got %+v", b)
	}
	err := h.n.Update(h.now)
	if err != nil && strings.Contains(err.Error(), "battery") {
		t.Errorf("a board with no power chip was refused for its battery: %v", err)
	}
}

// TestAbandonLeavesOtherGroupsAlone: SmartGroupAbandon ends the group it
// names. A device in no group, or in a different one, has nothing to
// abandon and must not report that it did — the flags look the same
// either way, so what gives it away is the line in the log.
func TestAbandonLeavesOtherGroupsAlone(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if h.n.inGroup {
		t.Fatal("an abandon put the device into a group")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("a device in no group reported abandoning one: %s", logged.String())
	}
	logged.Reset()

	// In a group, only its own UID ends it.
	h.n.inGroup, h.n.smartUID = true, 7
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if !h.n.inGroup {
		t.Error("another group's abandon ended this one")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("another group's abandon was reported as this one's: %s", logged.String())
	}
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 7})
	if h.n.inGroup {
		t.Error("the group's own abandon did not end it")
	}
}

// TestUnbondNoticeReachesTheAir: deleting a peer tells it so. The notice
// waits for the next window rather than leaving with the call, so this
// covers the whole path — Unbond also hands back whatever the node
// queued now, as every other command does, instead of returning nil.
func TestUnbondNoticeReachesTheAir(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.take()
	h.collect(h.n.Unbond(h.now, totem))
	if h.n.BondCount() != 0 {
		t.Fatalf("the peer is still bonded: %d", h.n.BondCount())
	}
	// The unbond notice goes out at the next window, not from the call,
	// so what matters is that nothing is stranded: drive a window and the
	// frame has to appear.
	h.advance(10 * time.Second)
	var found bool
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerUnbond {
			found = true
		}
	}
	if !found {
		t.Error("the unbond notice never reached the air")
	}
}

// TestPeerTimestampSurvivesAZeroTime: State writes each peer's last
// position time as a Unix second. The zero time is the year 1, whose Unix
// value is a large negative number, and a restore would read it back as a
// timestamp and saturate every duration measured from it.
func TestPeerTimestampSurvivesAZeroTime(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	p := h.n.peers[totem]
	p.hasCoords, p.lat, p.lon = true, 37.775, -122.42
	p.coordsAt = time.Time{}

	st := h.n.State(1)
	if len(st.Peers) == 0 {
		t.Fatal("the peer was not saved")
	}
	if got := st.Peers[0].LastSeenUnix; got < 0 {
		t.Errorf("saved a last-seen time of %d, which is before the epoch", got)
	}
}

// TestUpdateHoldsTheProgressRing: an update drives the ring itself, and
// nothing in the interface makes a transport block the loop. A poll that
// landed in the middle of a download used to put the idle animation back
// over the progress ring.
func TestUpdateHoldsTheProgressRing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.ota.state = OTADownloading
	h.n.LEDs().Play(AnimOTA, h.now)
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got != AnimOTA {
		t.Errorf("a poll during a download left the ring on %s", got)
	}
	h.n.ota.state = OTAChecking
	h.collect(h.n.Poll(h.now))
	if got := h.n.LEDs().Animation(); got != AnimWiFi {
		t.Errorf("a poll while checking for an update left the ring on %s", got)
	}
}
