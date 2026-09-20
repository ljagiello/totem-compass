package emulator

// Regressions for the thirty-seventh review round, which found these in
// code older than this session's work rather than in its fixes.

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// TestAnOdometerHasBothEnds: the field on the air is an int16 and the
// reading is an int32, so a negative odometer past -32768 wrapped into a
// large positive distance — a device reporting -100000 told its peers it
// had traveled 31 km. The uptime beside it was given both ends; this
// had only the top.
func TestAnOdometerHasBothEnds(t *testing.T) {
	for _, m := range []int32{0, 1, 32767, 100000, -1, -100000, math.MinInt32, math.MaxInt32} {
		h := newHarness(t, func(c *Config) {
			c.Sensors = NewStatic(Sensors{
				Fix:     &Fix{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3, OdometerM: m},
				Battery: Battery{Volts: 4.1, Percent: 90},
			})
		})
		got := h.n.status(h.now, mesh.PeerStatus, false).OdometerM
		if got < 0 {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
		if m >= 0 && m <= math.MaxInt16 && int32(got) != m {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
	}
}

// TestTheConsoleTakesTheLongestFrameItAccepts: the rx command bounds
// what it injects by mesh.MaxFrame, and the line limit was sized for a
// 108-byte peer frame — so the largest frames the validator accepts were
// refused first, as a line too long, by a limit whose own comment
// described something shorter.
//
// Fed through a LineReader, because that is where the limit lives:
// ParseCommand has no length check of its own, so asking it whether a
// long line is "too long" can only ever answer no. Both spellings of
// the hex, since parseRX cleans separators out and the separated form
// is half as long again.
func TestTheConsoleTakesTheLongestFrameItAccepts(t *testing.T) {
	frame := make([]byte, mesh.MaxFrame)
	for i := range frame {
		frame[i] = 0xab
	}
	bare := hex.EncodeToString(frame)
	var sep strings.Builder
	for i, b := range frame {
		if i > 0 {
			sep.WriteByte(':')
		}
		fmt.Fprintf(&sep, "%02x", b)
	}

	for _, tt := range []struct{ how, hex string }{
		{"typed", bare},
		{"pasted", sep.String()},
	} {
		line := "rx 8c:94:df:7b:04:78 all -25 " + tt.hex
		// The console reads it as a line at all.
		var r LineReader
		var got string
		var err error
		for i := 0; i < len(line) && err == nil && got == ""; i++ {
			got, err = r.Feed(line[i])
		}
		if err == nil {
			got, err = r.Feed('\n')
		}
		if err != nil {
			t.Errorf("%s, %d characters: the console refused the line: %v", tt.how, len(line), err)
			continue
		}
		if got != line {
			t.Errorf("%s: the console read back %d of %d characters", tt.how, len(got), len(line))
			continue
		}
		// And what it read is a frame rx will take.
		c, err := ParseCommand(got)
		if err != nil {
			t.Errorf("%s: %v", tt.how, err)
			continue
		}
		if c.RX == nil || len(c.RX.Data) != mesh.MaxFrame {
			t.Errorf("%s: the line parsed to %#v, not a %d-byte frame", tt.how, c.RX, mesh.MaxFrame)
		}
	}
}

// TestAVoltageTheFrameCannotCarry: the field is a half float and the
// encoder is a port with no range check of its own, so a finite voltage
// past its limit runs off the exponent into the sign bit. The emulator
// guards its own readings; mesh is an exported package, and its encoder
// refused an over-long name while taking any voltage at all.
//
// The first version of this check also refused NaN, the infinities and
// every negative — which CI caught, because those are patterns the half
// holds and decodeHalf hands back, so a frame we could read became one
// we could not write. Both halves of that are pinned below.
func TestAVoltageTheFrameCannotCarry(t *testing.T) {
	// Finite and past the half's largest: silently becomes something else.
	for _, v := range []float32{100000, 131072, 1e6, -100000} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err == nil {
			t.Errorf("a battery of %v V was encoded", v)
		}
	}
	// Every voltage a cell reads, and every other pattern the half holds:
	// a frame carrying one of these must re-encode.
	for _, v := range []float32{
		0, 3.1, 4.1, 4.48, 4.6, 65504, -5,
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)),
	} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err != nil {
			t.Errorf("a battery of %v V was refused: %v", v, err)
		}
	}
}

// TestTheChargerDebounceRunsWhileTheRingIsBusy: charging() is the only
// writer of when the cable went in, and it used to be the first operand
// of an && chain — so the debounce kept running only because of the
// order the operands happened to be in. Plugging in during an alarm is
// the case that breaks when someone tidies that.
func TestTheChargerDebounceRunsWhileTheRingIsBusy(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)

	// The alarm owns the strip, and the cable goes in underneath it.
	h.n.SetSOS(true)
	h.advance(time.Second)
	if err := h.n.SetBattery(70, true, h.now); err != nil {
		t.Fatal(err)
	}
	// Long enough that a debounce started now would have finished.
	h.advance(2 * time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Fatalf("the alarm lost the ring to something else: %s", got)
	}

	// The alarm ends, and the connection is announced — which needs the
	// debounce to have been running the whole time it was held back.
	//
	// Less than chargeDebounce, and that is the whole point: if asking
	// only starts the debounce now, the ring cannot be announced yet,
	// and the test fails. Advancing past it instead — which this test
	// did when it was written — lets a debounce that started late
	// finish inside the window, and then it passes either way.
	h.n.SetSOS(false)
	h.advance(chargeDebounce / 2)
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Errorf("the ring shows %s: the charger was never announced", got)
	}
}
