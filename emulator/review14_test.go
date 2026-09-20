package emulator

// Regressions for the fourteenth review round.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestARestoreOnAFlatPackPowersDown: moving the power mode is not acting
// on it. Restore called update and dropped what it reported, so the mode
// went to off while the device stayed on — and because update only
// reports a change once, every poll afterwards saw nothing to do. The
// radio windows kept running on a pack below the cutoff.
func TestARestoreOnAFlatPackPowersDown(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.2, 1 })
	h.n.Restore(store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerOff {
		t.Fatalf("a 3.2 V pack reads mode %s, want off", got)
	}
	if !h.n.Power().Off() {
		t.Error("the device reports power mode off while still running")
	}
	// And it stays down: a poll must not find it half-off.
	h.advance(30 * time.Second)
	if !h.n.Power().Off() {
		t.Error("the device came back up by itself")
	}
}

// TestARestoreOnALowPackSaysSo: the same for the low-battery ring, which
// is the only thing that tells someone holding the device.
func TestARestoreOnALowPackSaysSo(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.BattVolts, c.BattPct = 3.4, 5
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.n.Restore(store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerLow {
		t.Fatalf("a 3.4 V pack reads mode %s, want low", got)
	}
	if !strings.Contains(logged.String(), "power mode") {
		t.Errorf("the mode changed without a word: %s", logged.String())
	}
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("the ring shows %s, want the low-battery flash", got)
	}
}

// TestAResetDuringPairingStays: a reset while a window is open used to
// leave the sibling still broadcasting bond requests, so the bond came
// straight back inside the same six seconds — and the board saved a
// record still holding the bond it was told to wipe.
func TestAResetDuringPairingStays(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.bond()
	h.collect(h.n.Pair(h.now))
	if !h.n.Pairing() {
		t.Fatal("the pairing window did not open")
	}

	h.n.FactoryReset(h.now)
	if h.n.Pairing() {
		t.Error("a factory reset left the pairing window open")
	}
	if got := h.n.BondCount(); got != 0 {
		t.Fatalf("the reset left %d bonds", got)
	}
	// The sibling keeps asking for the rest of the window it thinks is
	// open. None of it may take.
	for range 5 {
		h.rx(totem, self, -20, bondFrame(false))
		h.advance(200 * time.Millisecond)
	}
	h.advance(pairingWindow + time.Second)
	if got := h.n.BondCount(); got != 0 {
		t.Errorf("the bond came back after the reset: %d bonds", got)
	}
}

// TestAResetLeavesNothingToSave: the board snapshots the state right
// after the reset, so anything the reset re-learns lands in the record
// meant to be empty. Clearing before the fresh reading put the pack
// straight back — and marked it measured, so a later restore could no
// longer correct it.
func TestAResetLeavesNothingToSave(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.collect(h.n.Poll(h.now))
	h.n.FactoryReset(h.now)
	if got := h.n.State(0).LearnedMaxVolts; got != 0 {
		t.Errorf("the state saved right after a reset carries %v", got)
	}
	// And what it learns next can still be corrected by a saved value,
	// because the reset left nothing measured behind it.
	h.collect(h.n.Poll(h.now))
	h.n.Power().SetLearnedMaxVolts(4.15)
	if got := h.n.Power().LearnedMaxVolts(); got > 4.26 {
		t.Errorf("after a reset the learned maximum is stuck at %v", got)
	}
}

// TestTheTwoBondLimitWarningsAreIndependent: one latch for two
// diagnostics meant whichever fired first silenced the other for the
// rest of the run.
func TestTheTwoBondLimitWarningsAreIndependent(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.AutoPair = true
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for _, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(bootDebounce)

	// One Totem pairing beside us.
	h.rx(h.n.Config().Owned[0], mesh.Broadcast, -20, bondFrame(false))
	if !strings.Contains(logged.String(), "pairing next to us") {
		t.Fatalf("the pairing warning did not fire: %s", logged.String())
	}
	// And another that still holds us, which is a different thing to say.
	ninth := mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x99}
	h.n.cfg.Owned = append(h.n.cfg.Owned, ninth)
	h.rx(ninth, self, -40, statusFrame(t0))
	if !strings.Contains(logged.String(), "bond not restored") {
		t.Errorf("the first warning silenced the second: %s", logged.String())
	}
}
