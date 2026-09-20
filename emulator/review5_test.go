package emulator

// Regressions for the fifth review round.

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// TestAQuickTapDoesNotLatchTheButton: the driver calls Press and Release
// straight off a GPIO edge, and a quick tap — or a contact that settles
// within the 30 ms edge lockout — produces exactly that pair. Dropping
// the release along with the bounce left the recogniser down with nobody
// touching the button, and 800 ms later poll reported a hold no one made:
// on the power button, a device that switches itself off; on SOS, an
// alarm that starts by itself.
func TestAQuickTapDoesNotLatchTheButton(t *testing.T) {
	for _, in := range []Input{PowerButton, SOSButton, Crystal} {
		h := newHarness(t, nil)
		h.advance(bootDebounce)
		h.n.Press(in, h.now)
		h.now = h.now.Add(edgeLockout / 3)
		h.n.Release(in, h.now)

		if r := &h.n.inputs[in]; r.down {
			t.Errorf("%s is still down after the release", in)
		}
		// Long enough for a hold and a long hold to mature, if the
		// recogniser thought a finger was still there.
		h.advance(longHold + 2*time.Second)
		if h.n.Power().Off() {
			t.Errorf("a quick tap of %s switched the device off", in)
		}
		if h.n.Config().SOS {
			t.Errorf("a quick tap of %s started the alarm", in)
		}
		if h.n.Pairing() {
			t.Errorf("a quick tap of %s started pairing", in)
		}
	}
}

// TestRestingStripStillPacesItsFrames: an idle strip asks for no wake-up,
// which is where the sleep is won. That was done by zeroing the frame
// deadline — but now.Before(the zero time) is never true, so the pacing
// went with it and every poll redrew the whole strip. On the board that
// is a few hundred redraws a second while the device rests.
func TestRestingStripStillPacesItsFrames(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.rx(totem, self, -40, statusFrame(t0))
	h.advance(bootAnim + time.Second)

	l := h.n.LEDs()
	if got := l.Animation(); got != AnimIdle {
		t.Fatalf("the strip is playing %s, want it resting", got)
	}
	if !l.Next().IsZero() {
		t.Error("a resting strip asked to be woken for another frame")
	}
	// A resting strip is still paced at the frame rate, whoever asks it.
	// The board polls every few milliseconds, so what matters is that
	// ticks far more often than the frame rate do not each redraw.
	const (
		run  = time.Second
		step = 5 * time.Millisecond
	)
	var drew, ticks int
	for end := h.now.Add(run); h.now.Before(end); h.now = h.now.Add(step) {
		ticks++
		if l.Tick(h.now) {
			drew++
		}
	}
	want := int(run / ledFrame)
	if drew > want+2 {
		t.Errorf("%d of %d ticks in %s redrew the strip, want about %d — one every %s",
			drew, ticks, run, want, ledFrame)
	}
	if drew == 0 {
		t.Error("the strip never drew a frame at all")
	}
	if !l.Next().IsZero() {
		t.Error("a resting strip asked to be woken after drawing")
	}
}

// TestRestoreRefusesImpossiblePositions: Restore is an ingress path like
// any other, and its bytes come off a flash sector that a torn write, a
// bad block or another firmware's layout can leave saying anything. NaN
// is not equal to zero, so the "did we save a position" check passed it
// through into the distance, the compass and the relay decision.
func TestRestoreRefusesImpossiblePositions(t *testing.T) {
	for _, bad := range []struct {
		name     string
		lat, lon float32
	}{
		{"nan latitude", float32(math.NaN()), -122.42},
		{"infinite longitude", 37.775, float32(math.Inf(-1))},
		{"latitude past the pole", 900, -122.42},
	} {
		h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
		st := store.State{Peers: []store.PeerState{{
			MAC: [6]byte(totem), Name: "totem", Lat: bad.lat, Lon: bad.lon,
		}}}
		h.n.Restore(st, h.now)

		for _, p := range h.n.Peers() {
			if math.IsNaN(p.DistanceM) || math.IsInf(p.DistanceM, 0) {
				t.Errorf("%s: the peer is %v away", bad.name, p.DistanceM)
			}
		}
		h.advance(bootAnim + time.Second)
		if desc := h.n.LEDs().Describe(); strings.Contains(desc, "NaN") {
			t.Errorf("%s: reached the ring: %s", bad.name, desc)
		}
	}
}

// TestRestoreRefusesAColourOutsideThePalette: a colour id off the flash
// is a number, not a promise. One outside the thirteen renders as an
// unlit pixel, so the peer would get a dial point that cannot be seen.
func TestRestoreRefusesAColourOutsideThePalette(t *testing.T) {
	h := newHarness(t, nil)
	st := store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", ColorID: 120,
	}}}
	h.n.Restore(st, h.now)
	if c := h.n.peers[totem].color; !c.InPalette() {
		t.Errorf("restored colour %d, which is not in the palette", int(c))
	}
}
