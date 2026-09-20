package emulator

// The halo and the Touch Crystal: what is lit, what is playing, and
// how the strip paces its frames.

import (
	"strings"
	"testing"
	"time"
)

// TestTheErrorRingIsPlayedAtTheRightTime: an update blocks the loop, so
// the time Update was entered on is not the time the node is on when it
// finishes. A download that ran for longer than the error ring's own four
// seconds left it already expired, and the next tick replaced it with
// idle before a frame was drawn.
func TestTheErrorRingIsPlayedAtTheRightTime(t *testing.T) {
	// Long enough to be measurable, short enough that the suite does not
	// wait for it: the property is where the animation was armed, not how
	// long the test takes.
	const slow = 200 * time.Millisecond
	h := newHarness(t, func(c *Config) {
		c.OTATransport = slowFailingTransport{delay: slow}
	})
	h.advance(bootAnim + time.Second)
	began := h.now
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("the update did not fail")
	}
	if got := h.n.LEDs().Animation(); got != AnimOTAFailed {
		t.Fatalf("the ring shows %s after a failure, want the error ring", got)
	}
	// The node's clock has not moved — Update blocked the loop — so the
	// ring has to have been armed for where the node will be when it next
	// polls, not for where it was when Update was entered.
	if l := h.n.LEDs(); !l.start.After(began) {
		t.Errorf("the error ring was armed at %v, the instant the update began", l.start)
	}
	if l := h.n.LEDs(); !l.until.After(began.Add(otaFailAnim)) {
		t.Errorf("the error ring expires at %v, within its own length of the start", l.until)
	}
}

func litPixels(l *LEDs) int {
	n := 0
	for _, px := range l.Ring() {
		if px != Off {
			n++
		}
	}
	return n
}

// TestTheCrystalIsNamedWhileDimmed: Describe read the pixel, which has
// been through dim(), and looked it up in a palette of undimmed values —
// so one tap of the power button turned "crystal red" into
// "crystal #3f0000" for as long as the strip stayed dimmed.
func TestTheCrystalIsNamedWhileDimmed(t *testing.T) {
	l := newLEDs(t0, ColorRed)
	l.Tick(t0)
	if got := l.Describe(); !strings.Contains(got, "crystal red") {
		t.Fatalf("at full brightness: %q", got)
	}
	l.ToggleBrightness()
	l.Tick(t0.Add(time.Second))
	if got := l.Describe(); !strings.Contains(got, "crystal red") {
		t.Errorf("dimmed: %q, want the colour named", got)
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

// TestAPictureChangeWakesARestingStrip: a resting strip is paced by its
// frame deadline and woken by nobody, so anything that changes what it
// looks like has to say so. The dial did; the crystal colour and the
// brightness did not, and a `color blue` on a resting strip drew nothing
// until the next radio window happened to tick it.
func TestAPictureChangeWakesARestingStrip(t *testing.T) {
	rest := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
		h.bond()
		h.rx(totem, self, -40, statusFrame(t0))
		h.advance(bootAnim + time.Second)
		if got := h.n.LEDs().Animation(); got != AnimIdle {
			t.Fatalf("the strip is playing %s, want it resting", got)
		}
		// Settle onto a frame, so the deadline is in the future.
		h.n.LEDs().Tick(h.now)
		return h
	}

	t.Run("crystal colour", func(t *testing.T) {
		h := rest(t)
		before := h.n.LEDs().Crystal()[0]
		h.n.SetColor(ColorBlue, h.now)
		if got := h.n.LEDs().Crystal()[0]; got == before {
			t.Errorf("the crystal is still %v after being set to blue", got)
		}
	})

	t.Run("brightness", func(t *testing.T) {
		h := rest(t)
		before := h.n.LEDs().Crystal()[0]
		h.n.ToggleBrightness(h.now)
		if got := h.n.LEDs().Crystal()[0]; got == before {
			t.Errorf("the crystal is still %v after the brightness changed", got)
		}
	})
}

// TestAPoweredDownStripRemembersNothing: a dark strip shows nothing, so
// there is nothing to play on it and nothing to remember having played.
// Guarding only the reader left the fields saying one thing and Next
// another, and `leds` reported a powered-down board as showing an update
// failure for ever.
func TestAPoweredDownStripRemembersNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	h.n.PowerOff(h.now)
	was := h.n.LEDs().Describe()

	// The refusal that flashes the ring.
	if err := h.n.Update(h.now); err == nil {
		t.Fatal("an update on a powered-down device was allowed")
	}
	if got := h.n.LEDs().Animation(); got != AnimIdle {
		t.Errorf("a dark strip is playing %s", got)
	}
	if got := h.n.LEDs().Describe(); got != was {
		t.Errorf("a dark strip changed from %q to %q", was, got)
	}
	if got := h.n.LEDs().Next(); !got.IsZero() {
		t.Errorf("a dark strip asked to be woken at %v", got)
	}
	// Every pixel is still off.
	for i, p := range h.n.LEDs().Ring() {
		if p != Off {
			t.Fatalf("ring pixel %d is lit on a powered-down device: %v", i, p)
		}
	}
}

// TestADarkStripComesBackAtNow: Dark(false) armed the frame deadline from
// the instant the device went down, so the first tick measured the whole
// off-duration in frames and started the animation mid-sequence.
func TestADarkStripComesBackAtNow(t *testing.T) {
	l := newLEDs(t0, ColorTeal)
	l.Tick(t0)
	l.Dark(true, t0)

	// No Play afterwards: PowerOn happens to call one, and that is what
	// hid this. A caller of the exported Dark gets whatever this leaves.
	later := t0.Add(2 * time.Hour)
	l.Dark(false, later)
	if l.next.Before(later) {
		t.Errorf("a frame is due at %v, %v before the strip came back",
			l.next, later.Sub(l.next))
	}
	l.Tick(later)
	if l.frame != 0 {
		t.Errorf("the strip came back at frame %d, two hours of frames in", l.frame)
	}
}
