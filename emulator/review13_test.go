package emulator

// Regressions for the thirteenth review round.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestTheRestoreBondWarningIsThrottled: a Totem that still holds us in
// its config.json unicasts a status every one to four seconds. On a
// device already at eight bonds that warning fired on every one of them
// — the same flood the pairing warning was quietened for, in the branch
// beside it.
func TestTheRestoreBondWarningIsThrottled(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for _, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatal(err)
		}
	}
	// A ninth Totem that kept us, unicasting status as the firmware does.
	ninth := mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x99}
	h.n.cfg.Owned = append(h.n.cfg.Owned, ninth)
	for range 6 {
		h.rx(ninth, self, -40, statusFrame(t0))
		h.advance(2 * time.Second)
	}
	if n := strings.Count(logged.String(), "bond not restored"); n != 1 {
		t.Errorf("the bond-limit warning fired %d times, want once", n)
	}
}

// TestRestorePutsThePowerModeInStep: Restore takes a fresh reading, and
// the mode has to follow it. Left to the next poll, `power` and `status`
// report the mode from before the restore in between — and on a device
// coming up on a flat pack that is the interval in which nothing says so.
func TestRestorePutsThePowerModeInStep(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.4, 5 })
	// No poll: this is a node as the driver has it at boot, with the
	// settings about to be put back.
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Fatalf("a node starts in mode %s, want normal", got)
	}
	h.n.Restore(&store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerLow {
		t.Errorf("after restoring on a 3.4 V pack the mode is %s, want low", got)
	}
}
