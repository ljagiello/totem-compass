package emulator

// Regressions for the eleventh review round.

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

// TestPressingPairAlwaysAnswers: the bond-limit warning is said once so a
// Totem pairing next to a full device does not fill the log — but the
// person who pressed the button has to be told why nothing happened, or
// the button looks dead.
func TestPressingPairAlwaysAnswers(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.AutoPair = true
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for i, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatalf("bond %d: %v", i, err)
		}
	}
	h.advance(bootDebounce)

	// The automatic path — an owned Totem pairing next to us, which
	// repeats every 50-99 ms — says it once.
	for range 5 {
		h.rx(h.n.Config().Owned[0], mesh.Broadcast, -20, bondFrame(false))
	}
	if n := strings.Count(logged.String(), "already at the limit"); n != 1 {
		t.Errorf("the automatic path said it %d times, want once", n)
	}
	// A press says it every time, however often the radio already did.
	logged.Reset()
	h.collect(h.n.Pair(h.now))
	if !strings.Contains(logged.String(), "more than 8 bonds") {
		t.Errorf("pressing pair on a full device said nothing: %s", logged.String())
	}
	logged.Reset()
	h.collect(h.n.Pair(h.now))
	if !strings.Contains(logged.String(), "more than 8 bonds") {
		t.Errorf("pressing pair a second time said nothing: %s", logged.String())
	}
}

func manyOwned() []mesh.MAC {
	out := make([]mesh.MAC, 0, maxBonds)
	for i := range maxBonds {
		out = append(out, mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, byte(0x80 + i)})
	}
	return out
}

// TestTheLearnedMaximumStretchesTheCurve: a pack that charges above the
// curve's own top read 100% all the way down to 4.12 V, so the first
// tenth of a volt of discharge looked like none at all. That is what
// learning the maximum is for.
func TestTheLearnedMaximumStretchesTheCurve(t *testing.T) {
	const top = float32(4.25)
	if got := battPctFor(4.15, 0); got != 100 {
		t.Errorf("without a learned maximum, 4.15 V reads %d%%, want 100%%", got)
	}
	if got := battPctFor(4.15, top); got >= 100 {
		t.Errorf("a pack that reaches %v V still reads %d%% at 4.15 V", top, got)
	}
	if got := battPctFor(top, top); got != 100 {
		t.Errorf("a pack at its own maximum reads %d%%", got)
	}
}

// TestASavedPositionIsNeverFromTheFuture: a board that saved with a GNSS
// clock and came back up on a peer's slower one has a saved second later
// than its own wall time. Used as it stands, the peer is never stale and
// the mesh is never asked where it went — no corruption required.
func TestASavedPositionIsNeverFromTheFuture(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Restore(store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", Lat: 37.775, Lon: -122.42,
		LastSeenUnix: t0.Add(5 * 365 * 24 * time.Hour).Unix(),
	}}}, h.now)

	p := h.n.peers[totem]
	if p == nil || !p.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := h.now.Sub(p.coordsAt); age < 0 {
		t.Errorf("the position is dated %v into the future", -age)
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

// TestRXTakesPastedHex: a frame is pasted out of a log or a chat window
// as often as it is typed, so it arrives with colons or a non-breaking
// space in it. The CLI learned to take those; the console's own rx did
// not, so the same paste half-worked in one command.
func TestRXTakesPastedHex(t *testing.T) {
	plain, err := ParseCommand("rx 8c94df7b0478 all -40 a774020011223344")
	if err != nil {
		t.Fatalf("plain hex: %v", err)
	}
	for _, in := range []string{
		"rx 8c:94:df:7b:04:78 all -40 a7:74:02:00:11:22:33:44",
		"rx 8c94df7b0478 all -40 a77402 0011223344",
	} {
		c, err := ParseCommand(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !bytes.Equal(c.RX.Data, plain.RX.Data) {
			t.Errorf("%q gave % x, want % x", in, c.RX.Data, plain.RX.Data)
		}
	}
}
