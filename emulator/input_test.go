package emulator

// The two buttons and the Touch Crystal: taps, holds, and the edges a
// bouncing pin or a jittering capacitive reading produces.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestASyntheticTapDoesNotEndARealPress: press() refuses when a finger is
// already down, but release() had no such guard — so a `touch sos tap`
// arriving while someone held the board's button ended that press before
// it matured into a hold, and the alarm never started.
func TestASyntheticTapDoesNotEndARealPress(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)

	// A finger goes down on the real button.
	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)
	// A tap arrives from the console while it is held.
	h.n.Tap(SOSButton, 1, h.now)
	if r := h.n.input(SOSButton); !r.down {
		t.Fatal("the console tap ended a press someone was making")
	}
	// The hold still matures.
	h.advance(holdTime + time.Second)
	if !h.n.Config().SOS {
		t.Error("the hold never fired, so the alarm never started")
	}
}

// TestASyntheticHoldDoesNotEndARealPress: the same rule Tap got last
// round, on the path beside it. A `touch sos hold 10` arriving while
// someone held the board's button ended that press before it matured,
// so the alarm never started.
func TestASyntheticHoldDoesNotEndARealPress(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)

	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)
	h.collect(h.n.HoldFor(SOSButton, 10*time.Millisecond, h.now))
	if r := h.n.input(SOSButton); !r.down {
		t.Fatal("the console hold ended a press someone was making")
	}
	if !strings.Contains(logged.String(), "hold ignored") {
		t.Errorf("the console was told nothing: %s", logged.String())
	}
	h.advance(holdTime + time.Second)
	if !h.n.Config().SOS {
		t.Error("the real hold never fired, so the alarm never started")
	}
}

// TestARefusedHoldNamesOneReason: a hold refused because a finger is
// already on the button is not a hold refused for being too short, and
// printing both sends someone chasing a timing problem that is not there.
func TestARefusedHoldNamesOneReason(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	h.n.Press(SOSButton, h.now)
	h.now = h.now.Add(400 * time.Millisecond)

	h.collect(h.n.HoldFor(SOSButton, 10*time.Millisecond, h.now))
	if !strings.Contains(logged.String(), "hold ignored") {
		t.Errorf("the refusal was not reported: %s", logged.String())
	}
	if strings.Contains(logged.String(), "too short") {
		t.Errorf("the refusal named the wrong reason as well: %s", logged.String())
	}
}

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

		if r := h.n.input(in); r.down {
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

// TestAShortHoldIsNotATap: HoldFor used to stretch a hold shorter than
// the edge lockout past it, because a release inside the lockout was
// being dropped and left the input latched down. The release is honored
// now, so the stretch is not only unnecessary — it turns "hold for 10 ms"
// into a counted tap, and on the power button into a brightness toggle.
func TestAShortHoldIsNotATap(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	full := h.n.LEDs().Brightness()

	h.collect(h.n.HoldFor(PowerButton, edgeLockout/3, h.now))
	h.advance(multiTapWindow + time.Second)

	if got := h.n.LEDs().Brightness(); got != full {
		t.Errorf("a %s hold of power changed brightness to %v", edgeLockout/3, got)
	}
	if h.n.Power().Off() {
		t.Error("a hold shorter than the edge lockout switched the device off")
	}
}

// TestAPendingTapDoesNotSurvivePowerOff: a device that is off asks for
// nothing. A tap still inside its multi-tap window kept a deadline alive
// on a device whose only answer was to throw the gesture away.
func TestAPendingTapDoesNotSurvivePowerOff(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.n.Tap(Crystal, 1, h.now)
	h.n.PowerOff(h.now)
	if got := h.n.Next(); !got.IsZero() {
		t.Errorf("a powered-down device wants waking at %v", got)
	}
	// And the power button still works: the wait is cleared, not re-armed.
	h.collect(h.n.HoldFor(PowerButton, holdTime+100*time.Millisecond, h.now))
	if h.n.Power().Off() {
		t.Error("holding power did not bring the device back")
	}
}

// TestAShortHoldSaysSo: the console takes a hold of 1 ms, and nothing
// registers — which is right, but silence looks the same as the console
// having missed the line.
func TestAShortHoldSaysSo(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	h.collect(h.n.HoldFor(PowerButton, time.Millisecond, h.now))
	if !strings.Contains(logged.String(), "too short") {
		t.Errorf("a hold too short to register said nothing: %s", logged.String())
	}
}

// TestAHoldOfExactlyTheLockoutCounts: the "too short to register" notice
// used <=, and release() uses <. At exactly the lockout the console was
// told nothing had happened and then the gesture fired — on the power
// button, the device dimmed.
func TestAHoldOfExactlyTheLockoutCounts(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.advance(bootDebounce)
	full := h.n.LEDs().Brightness()

	h.collect(h.n.HoldFor(PowerButton, edgeLockout, h.now))
	h.advance(multiTapWindow + time.Second)

	dimmed := h.n.LEDs().Brightness() != full
	said := bytes.Contains(logged.Bytes(), []byte("too short"))
	if said && dimmed {
		t.Error("the console was told nothing happened, and then the brightness changed")
	}
}
