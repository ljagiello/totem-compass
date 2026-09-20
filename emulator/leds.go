package emulator

// The halo and the Touch Crystal.
//
// A Totem's visible output is a ring of 60 APA106 pixels around the top
// (RING_PX_COUNT) and 7 more inside the crystal (CRYSTAL_PX_COUNT),
// driven by leds.py and animations.py. Subsystems raise events and the
// LED task plays the matching effect: pairing, a peer bonded, a peer
// gone, SOS, a demi-god command, an OTA download.
//
// This models what is lit rather than how the bits reach the strip: the
// animation playing, its frame, and the colour of every pixel. A board
// with a ring can render it; a board with one LED can mirror the crystal;
// the console can print it. The frame timings the firmware keeps in
// bytecode are not recoverable, so the ones here are chosen to look like
// the device and are marked as such.

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
)

// Ring and crystal sizes, RING_PX_COUNT and CRYSTAL_PX_COUNT in
// project_data.py.
const (
	RingPixels    = 60
	CrystalPixels = 7
)

// RGB is one pixel.
type RGB struct{ R, G, B uint8 }

// Off is the cleared pixel. The firmware has no named "off" colour: it
// fills with a literal (0, 0, 0).
var Off = RGB{}

// Color names a palette entry. The Colors class in project_data.py binds
// 13 names, and their order is the colour-id order the frames carry.
type Color int8

// The palette, in the order the firmware stores it, which is also the
// colour id a peer frame carries.
const (
	ColorRed Color = iota
	ColorOrange
	ColorYellow
	ColorYellowGreen
	ColorGreen
	ColorBlueGreen
	ColorTeal
	ColorWhite
	ColorAqua
	ColorBlue
	ColorIndigo
	ColorMagenta
	ColorHotPink
)

// palette is the RGB of each colour, read from the class's STORE_ATTRs.
var palette = [...]struct {
	name string
	rgb  RGB
}{
	{"red", RGB{255, 0, 0}},
	{"orange", RGB{255, 128, 0}},
	{"yellow", RGB{255, 255, 0}},
	{"yl_green", RGB{128, 255, 0}},
	{"green", RGB{0, 255, 0}},
	{"bl_green", RGB{0, 255, 128}},
	{"teal", RGB{0, 255, 255}},
	{"white", RGB{255, 255, 255}},
	{"aqua", RGB{0, 128, 255}},
	{"blue", RGB{0, 0, 255}},
	{"indigo", RGB{128, 0, 255}},
	{"magenta", RGB{255, 0, 255}},
	{"hot_pink", RGB{255, 0, 128}},
}

// InPalette reports whether the colour is one of the thirteen. An id off
// the air or off the flash is a number, not a promise.
func (c Color) InPalette() bool { return c >= 0 && int(c) < len(palette) }

// paletteColor is a colour id from outside — a flash sector, a frame off
// the air, a driver — checked against the thirteen. Anything else renders
// as an unlit pixel and, once saved, comes back that way at every boot,
// so it is refused and the caller keeps what it had.
//
// One helper rather than the check written out at each ingress: it was
// written out three times with three different fallbacks and three
// different messages, and the fourth ingress was missed entirely.
func paletteColor(log *slog.Logger, source string, id int8, fallback Color) Color {
	if c := Color(id); c.InPalette() {
		return c
	}
	log.Warn("colour is not one of the thirteen, keeping what we had",
		"source", source, "id", id, "using", fallback)
	return fallback
}

// RGB is the colour's pixel value.
func (c Color) RGB() RGB {
	if !c.InPalette() {
		return Off
	}
	return palette[c].rgb
}

// String is the firmware's name for the colour.
func (c Color) String() string {
	if !c.InPalette() {
		return fmt.Sprintf("color(%d)", int(c))
	}
	return palette[c].name
}

// LogValue names the colour in a log line, in either format.
func (c Color) LogValue() slog.Value { return slog.StringValue(c.String()) }

// dur is a length of time for a log line. slog on the board renders a
// time.Duration as a float of nanoseconds — "held=5e+06" for five
// milliseconds — which is no use to anyone reading the console, so every
// duration that reaches a log line goes through this instead.
func dur(d time.Duration) slog.Value { return slog.StringValue(d.String()) }

// ParseColor reads a palette name.
//
// The loop indexes the array rather than ranging over its values. Written
// as `for i, p := range palette { if p.name == s }` this never matched on
// the board: TinyGo 0.42 on xtensa compares the string field of a
// range-copied struct wrongly, and every colour was refused with an error
// that listed the name it had just failed to match. Printing the bytes on
// both sides showed 726564 == 726564 and the comparison still false. The
// host build has always been fine, so no test here can catch it — only
// the board can, which is why this note is here.
func ParseColor(s string) (Color, error) {
	for i := range palette {
		if palette[i].name == s {
			return Color(i), nil
		}
	}
	return 0, fmt.Errorf("%q: want one of %s", s, ColorNames())
}

// ColorNames lists the palette, for a console message.
func ColorNames() string {
	names := make([]string, 0, len(palette))
	// Indexed, for the reason in ParseColor.
	for i := range palette {
		names = append(names, palette[i].name)
	}
	return strings.Join(names, ", ")
}

// PeerCrystalMap is PEER_CRYSTAL_MAP = (1, 2, 4, 6): the crystal colour a
// peer is shown in, by bond order.
var PeerCrystalMap = [4]Color{ColorOrange, ColorYellow, ColorGreen, ColorTeal}

// bondColors is the 9-colour subset shuffle_bond_colors draws from when
// Totems auto-bond, as its index list gives it.
var bondColors = [9]Color{ColorHotPink, ColorGreen, ColorBlue, ColorYellow, ColorIndigo, ColorOrange, ColorTeal, ColorWhite, ColorMagenta}

// BondColor is the colour a peer is given when it bonds, drawn from the
// subset shuffle_bond_colors uses. The draw is by bond order and the
// device's own seed, so two Totems bonded in the same order to the same
// device do not both come out hot pink.
func BondColor(order int, seed uint32) Color {
	if order < 0 {
		order = 0
	}
	return bondColors[(order+int(seed%uint32(len(bondColors))))%len(bondColors)]
}

// Animation is an effect the LED task plays.
type Animation uint8

// The animations, named as animations.py names them.
const (
	// AnimIdle is the resting state: the crystal in its default colour
	// and the ring dark, or the compass dial when there is someone to
	// point at.
	AnimIdle Animation = iota
	AnimBoot
	AnimGNSSSearch
	AnimPairing
	AnimBonded
	AnimPeerDeleteCountdown
	AnimSOS
	AnimDemiGod
	AnimOTA
	// AnimOTAFailed fills the ring with ERROR_RGB, the orange the update
	// guide describes and ota_callback.py defines.
	AnimOTAFailed
	AnimWiFi
	AnimLowBattery
)

// String names the animation.
func (a Animation) String() string {
	switch a {
	case AnimBoot:
		return "boot"
	case AnimGNSSSearch:
		return "gnss search"
	case AnimPairing:
		return "pairing"
	case AnimBonded:
		return "bonded"
	case AnimPeerDeleteCountdown:
		return "peer delete countdown"
	case AnimSOS:
		return "sos"
	case AnimDemiGod:
		return "demi-god"
	case AnimOTA:
		return "ota"
	case AnimOTAFailed:
		return "ota failed"
	case AnimWiFi:
		return "wifi"
	case AnimLowBattery:
		return "low battery"
	}
	return "idle"
}

// LogValue names the animation in a log line, in either format.
func (a Animation) LogValue() slog.Value { return slog.StringValue(a.String()) }

// Animation lengths and frame rates. The firmware's own values live in
// undisassembled bytecode, so these are chosen to match what the device
// looks like: a 25 ms frame is 40 a second, fast enough that a spin or a
// breathe looks smooth.
const (
	ledFrame     = 25 * time.Millisecond
	bootAnim     = 2 * time.Second
	bondedAnim   = 3 * time.Second
	demiGodBlink = 3 * time.Second
	otaFailAnim  = 4 * time.Second
	peerDelAnim  = 2 * time.Second
	lowBattAnim  = 4 * time.Second
	// sosPeriod is one blink of the crystal in SOS: on for half of it.
	sosPeriod = 700 * time.Millisecond
)

// progressRGB is PROGRESS_RGB in ota_callback.py: white at 30%, the ring
// filling as an update downloads.
var progressRGB = scale(ColorWhite.RGB(), 0.3)

// errorRGB is ERROR_RGB: orange at the global brightness, filled over the
// ring when an update fails.
var errorRGB = ColorOrange.RGB()

// LEDs is the ring and the crystal, and what they are playing.
type LEDs struct {
	ring    [RingPixels]RGB
	crystal [CrystalPixels]RGB

	anim  Animation
	start time.Time
	until time.Time
	// frame counts the frames played, which the effects step through.
	frame int
	// next is when the next frame is due, and paces the redraw. resting
	// is whether anyone should be woken for it: a picture that does not
	// change still has a frame deadline, so Tick knows not to redraw
	// before it, but nothing has to be woken up to watch it. The two were
	// one field, and zeroing next to mean "do not wake" turned the pacing
	// off with it — now.Before(the zero time) is never true, so every
	// call redrew the whole strip.
	next    time.Time
	resting bool

	// brightness is GLOBAL_BRT, 0 to 1. A tap of the power button drops
	// it and another restores it (toggle_brightness).
	brightness float64
	dimmed     bool
	// defaultColor is crystal_default, the colour the crystal returns to.
	defaultColor Color
	// dial is where the compass points, -1 when it points at nothing.
	dial int16
	// dialColor is the peer's colour, which the lit pixel takes.
	dialColor Color
	// progress is the OTA download, 0 to 1.
	progress float64
	// off is a device that has powered down: the strip is dark and stays
	// dark, however often it is ticked.
	off bool
}

// fullBrightness and dimBrightness are the two levels toggle_brightness
// moves between. The firmware's GLOBAL_BRT is a fraction like these.
const (
	fullBrightness = 1.0
	dimBrightness  = 0.25
)

// newLEDs starts the strip at boot, with the power-up animation the
// device plays.
func newLEDs(now time.Time, def Color) *LEDs {
	l := &LEDs{brightness: fullBrightness, defaultColor: def, dial: -1}
	l.Play(AnimBoot, now)
	return l
}

// openEnded reports whether an animation runs until something stops it,
// rather than for a fixed time.
func openEnded(a Animation) bool {
	switch a {
	case AnimIdle, AnimPairing, AnimSOS, AnimOTA, AnimWiFi, AnimGNSSSearch:
		return true
	}
	return false
}

// Play starts an animation. A shorter one over a longer one wins: the
// firmware's LED task plays the newest event. When a timed one ends the
// strip goes idle, and the node puts back whatever its own state still
// calls for — an alarm, a pairing, a search for a fix — rather than the
// strip trying to remember.
func (l *LEDs) Play(a Animation, now time.Time) {
	if l.off {
		// A device that has powered down shows nothing, so there is
		// nothing to play and nothing to remember having played. Without
		// this the strip reported the animation for ever: device() does
		// not tick a dark strip, so nothing ever cleared it, and a
		// refusal that flashes the ring left `leds` and `status` saying a
		// powered-down board was showing an update failure.
		return
	}
	l.anim, l.start, l.frame, l.next, l.resting = a, now, 0, now, false
	switch a {
	case AnimBoot:
		l.until = now.Add(bootAnim)
	case AnimBonded:
		l.until = now.Add(bondedAnim)
	case AnimDemiGod:
		l.until = now.Add(demiGodBlink)
	case AnimOTAFailed:
		l.until = now.Add(otaFailAnim)
	case AnimPeerDeleteCountdown:
		l.until = now.Add(peerDelAnim)
	case AnimLowBattery:
		// A reminder, not a state: it plays when the mode drops and then
		// gives the ring back. Left running it would hold the device
		// awake for a single red pixel.
		l.until = now.Add(lowBattAnim)
	default:
		// Pairing, SOS, OTA and the searches run until something stops
		// them.
		l.until = time.Time{}
	}
}

// Stop ends an animation that runs until told, and falls back to idle.
func (l *LEDs) Stop(a Animation, now time.Time) {
	if l.anim == a {
		l.Play(AnimIdle, now)
	}
}

// Dark turns the strip off and keeps it off, for a device that has
// powered down: nothing is drawn and no frame is ever due until it comes
// back on.
func (l *LEDs) Dark(off bool, now time.Time) {
	if off {
		// Idle first, then dark: after l.off is set, Play is a no-op by
		// design, and the strip would otherwise keep reporting whatever
		// was on it when the device went down.
		l.Play(AnimIdle, now)
		l.off = true
		l.fillRing(Off)
		l.fillCrystal(Off)
		l.next, l.resting = time.Time{}, true
		return
	}
	l.off = false
	// From now, not from l.start: that is when the device went down, and
	// a first tick measuring frames from it would start an animation
	// hours into its own sequence.
	l.start, l.next, l.resting = now, now, false
}

// Animation is what is playing.
func (l *LEDs) Animation() Animation { return l.anim }

// SetDefaultColor changes the crystal's resting colour, as the app does
// ("Change default crystal color: {}").
func (l *LEDs) SetDefaultColor(c Color) {
	if c != l.defaultColor {
		l.redraw()
	}
	l.defaultColor = c
}

// DefaultColor is the crystal's resting colour.
func (l *LEDs) DefaultColor() Color { return l.defaultColor }

// redraw says the picture has changed, so a frame is due even if the
// strip had gone quiet. Everything that changes what a resting strip
// looks like has to call it: a resting strip is paced by next and woken
// by nothing, so without this the change waits for whatever happens to
// tick next, or for ever.
func (l *LEDs) redraw() {
	if l.off {
		// Nothing is due on a strip that is not lit. This is the one
		// place pacing is armed, so it is the one place that has to know.
		return
	}
	l.next, l.resting = l.start, false
}

// SetDial points the compass at a bearing in degrees, in a peer's colour.
// A negative bearing points at nothing.
func (l *LEDs) SetDial(deg int16, c Color) {
	if deg != l.dial || c != l.dialColor {
		l.redraw()
	}
	l.dial, l.dialColor = deg, c
}

// SetProgress sets the OTA download's share, 0 to 1.
func (l *LEDs) SetProgress(f float64) {
	// NaN survives min and max — every comparison with it is false — and
	// would reach the uint8 conversion in scale(), which Go leaves
	// implementation-defined. Nothing is a safer share than something
	// that is not a number.
	if math.IsNaN(f) {
		return
	}
	if p := min(max(f, 0), 1); p != l.progress {
		l.progress = p
		l.redraw()
	}
}

// ToggleBrightness is the power button's single tap: dim, then full
// again ("Revert to full brightness").
func (l *LEDs) ToggleBrightness() float64 {
	l.dimmed = !l.dimmed
	l.brightness = fullBrightness
	if l.dimmed {
		l.brightness = dimBrightness
	}
	l.redraw()
	return l.brightness
}

// Brightness is the global scale, 0 to 1.
func (l *LEDs) Brightness() float64 { return l.brightness }

// SetBrightness puts the scale back to a level the device was left at,
// which is what a saved setting restores after a reboot.
func (l *LEDs) SetBrightness(f float64) {
	// See SetProgress: a NaN would go through min and max untouched and
	// then into every pixel.
	if math.IsNaN(f) {
		return
	}
	if b := min(max(f, 0), 1); b != l.brightness {
		l.brightness = b
		l.redraw()
	}
	l.dimmed = l.brightness < fullBrightness
}

// Next is when Tick next has a frame to draw.
func (l *LEDs) Next() time.Time {
	// l.off as well as l.resting, though Play and redraw both refuse a
	// dark strip: the property is worth stating where it is read, so a
	// future writer of resting cannot hand a driver a deadline on a
	// powered-down device — which is a busy loop at full power.
	if l.off || l.resting {
		return time.Time{}
	}
	return l.next
}

// Tick draws the frames due at now. It reports whether anything changed,
// so a driver can skip writing an unchanged strip.
func (l *LEDs) Tick(now time.Time) bool {
	if l.off {
		// A powered-down device draws nothing: the pixels stay as Dark
		// left them, and no frame is ever due.
		l.next, l.resting = time.Time{}, true
		return false
	}
	if now.Before(l.next) {
		return false
	}
	if !l.until.IsZero() && !now.Before(l.until) {
		// Idle, and the node decides on its next poll what should be
		// showing instead.
		l.Play(AnimIdle, now)
	}
	// A frame number indexes the ring, so it must never go negative: a
	// caller whose clock has stepped back would otherwise index out of
	// the strip.
	l.frame = max(int(now.Sub(l.start)/ledFrame), 0)
	l.next = l.start.Add(time.Duration(l.frame+1) * ledFrame)
	l.draw(now)
	// Resting when nothing is moving: the crystal holds its colour, and
	// the ring is dark or holding one lit point at the peer. Waking for
	// another frame would keep the device up for a picture that does not
	// change — and a Totem with someone to point at is the normal case,
	// so this is where the sleep is won. SetDial asks for a frame when
	// the bearing or the colour moves. next keeps its deadline either
	// way: it is what stops the strip being redrawn on every poll, which
	// on the board is every few milliseconds.
	l.resting = l.anim == AnimIdle
	return true
}

// draw fills the pixels for the current animation and frame.
func (l *LEDs) draw(now time.Time) {
	l.fillRing(Off)
	l.fillCrystal(l.dim(l.defaultColor.RGB()))
	switch l.anim {
	case AnimBoot:
		// powerup_animation: a point runs round the ring once.
		l.ring[l.frame%RingPixels] = l.dim(ColorWhite.RGB())
	case AnimGNSSSearch:
		// anim_gnss_search: a slow sweep of two opposite points.
		p := l.frame % RingPixels
		l.ring[p] = l.dim(ColorAqua.RGB())
		l.ring[(p+RingPixels/2)%RingPixels] = l.dim(ColorAqua.RGB())
	case AnimPairing:
		// pairing_animation: the ring breathes, so the other device's
		// owner can see it is looking.
		c := scale(ColorMagenta.RGB(), breathe(l.frame))
		l.fillRing(l.dim(c))
	case AnimBonded:
		// add_peer_animation: the ring fills in the new peer's colour.
		n := min(l.frame, RingPixels)
		for i := 0; i < n; i++ {
			l.ring[i] = l.dim(ColorGreen.RGB())
		}
	case AnimPeerDeleteCountdown:
		// anim_peer_del_ctdwn: the ring empties.
		n := max(RingPixels-l.frame, 0)
		for i := 0; i < n; i++ {
			l.ring[i] = l.dim(ColorRed.RGB())
		}
	case AnimSOS:
		// crystal_sos_blink: the crystal blinks red, and the ring with it.
		if now.Sub(l.start)%sosPeriod < sosPeriod/2 {
			l.fillCrystal(l.dim(ColorRed.RGB()))
			l.fillRing(l.dim(ColorRed.RGB()))
		} else {
			l.fillCrystal(Off)
		}
	case AnimDemiGod:
		// demi_god_blink: the ring blinks indigo, fast.
		if (l.frame/4)%2 == 0 {
			l.fillRing(l.dim(ColorIndigo.RGB()))
		}
	case AnimOTA:
		// The download's progress ring, white at 30% (PROGRESS_RGB).
		n := int(l.progress * RingPixels)
		for i := 0; i < n; i++ {
			// Dimmed with everything else: a tap of the power button dims
			// the whole strip, not all of it but this.
			l.ring[i] = l.dim(progressRGB)
		}
	case AnimOTAFailed:
		// ring.fill(ERROR_RGB, True): the whole ring in orange.
		l.fillRing(l.dim(errorRGB))
	case AnimWiFi:
		// wifi_animation: a point spins while the station is up.
		l.ring[l.frame%RingPixels] = l.dim(ColorBlue.RGB())
	case AnimLowBattery:
		// A single red pixel, so a nearly flat device still says so.
		l.ring[0] = l.dim(ColorRed.RGB())
	case AnimIdle:
		l.drawDial()
	}
}

// drawDial lights the ring pixel that points at the peer the compass is
// following, as _spin_deg does, with its neighbors feathered.
func (l *LEDs) drawDial() {
	if l.dial < 0 {
		return
	}
	c := l.dialColor.RGB()
	p := int(float64(l.dial%360) / 360 * RingPixels)
	l.ring[p%RingPixels] = l.dim(c)
	l.ring[(p+1)%RingPixels] = l.dim(scale(c, 0.3))
	l.ring[(p+RingPixels-1)%RingPixels] = l.dim(scale(c, 0.3))
}

// Ring is the halo's pixels, as they would go to the strip.
func (l *LEDs) Ring() []RGB { return l.ring[:] }

// Crystal is the Touch Crystal's pixels.
func (l *LEDs) Crystal() []RGB { return l.crystal[:] }

func (l *LEDs) fillRing(c RGB) {
	for i := range l.ring {
		l.ring[i] = c
	}
}

func (l *LEDs) fillCrystal(c RGB) {
	for i := range l.crystal {
		l.crystal[i] = c
	}
}

// dim applies the global brightness, as dim_leds(rgb, GLOBAL_BRT) does.
func (l *LEDs) dim(c RGB) RGB { return scale(c, l.brightness) }

func scale(c RGB, f float64) RGB {
	f = min(max(f, 0), 1)
	return RGB{uint8(float64(c.R) * f), uint8(float64(c.G) * f), uint8(float64(c.B) * f)}
}

// breathe is the brightness curve of breathe_effect: up and down over
// about a second and a half.
func breathe(frame int) float64 {
	const period = 60 // frames, so 1.5 s at 25 ms
	p := frame % period
	if p > period/2 {
		p = period - p
	}
	return 0.15 + 0.85*float64(p)/float64(period/2)
}

// Describe renders the strip for a console: the animation, the crystal's
// colour and the lit ring pixels.
func (l *LEDs) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, crystal %s", l.anim, describeRGB(l.crystal[0]))
	lit := 0
	for _, p := range l.ring {
		if p != Off {
			lit++
		}
	}
	fmt.Fprintf(&b, ", ring %d/%d lit", lit, RingPixels)
	if l.dial >= 0 && l.anim == AnimIdle {
		fmt.Fprintf(&b, ", pointing %d° in %s", l.dial, l.dialColor)
	}
	fmt.Fprintf(&b, ", brightness %.0f%%", l.brightness*100)
	return b.String()
}

func describeRGB(c RGB) string {
	if c == Off {
		return "off"
	}
	// Indexed, for the reason in ParseColor.
	for i := range palette {
		if palette[i].rgb == c {
			return palette[i].name
		}
	}
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}
