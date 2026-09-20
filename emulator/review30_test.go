package emulator

// Regressions for the thirtieth review round.

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

// TestABatteryWarningIsSaidOnce: read() runs on every pass of a loop
// that polls every 5 ms, and the check moved into it says its piece
// there. A driver stuck out of range filled the only diagnostic channel
// the board has with two hundred copies a second of the same line,
// drowning everything else and burning serial time in the loop that has
// to keep feeding the watchdog.
func TestABatteryWarningIsSaidOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(*Config)
		want string
	}{
		{"a voltage no frame can carry", func(c *Config) { c.BattVolts = 131072 }, "battery voltage"},
		{"a percentage that is not one", func(c *Config) { c.BattPct = 120 }, "battery percentage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			h := newHarness(t, func(c *Config) {
				tc.cfg(c)
				c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
			})
			// A second of the board's own polling.
			for range 200 {
				h.now = h.now.Add(5 * time.Millisecond)
				h.collect(h.n.Poll(h.now))
			}
			if got := strings.Count(logged.String(), tc.want); got != 1 {
				t.Errorf("the warning was said %d times in a second of polling", got)
			}
		})
	}
}

// TestARealReadingIsNotZeroed: the check in read() is about what a peer
// frame can carry, not about what a cell is likely to read. A board
// whose divider reads a little high is still telling the truth, and a
// real Totem on a charger reads 4.48 — zeroing either would be this
// device lying about its own battery over a tenth of a volt.
func TestARealReadingIsNotZeroed(t *testing.T) {
	for _, v := range []float32{3.1, 4.2, 4.48, 4.62, 5.0} {
		h := newHarness(t, func(c *Config) { c.BattVolts = v })
		if got := h.n.Sensors().Battery.Volts; got != v {
			t.Errorf("a reading of %v V came back as %v", v, got)
		}
	}
	// And what the frame cannot hold is refused, whatever it claims.
	for _, v := range []float32{131072, 1e6, float32(math.NaN()), float32(math.Inf(1))} {
		h := newHarness(t, func(c *Config) { c.BattVolts = v })
		if got := h.n.Sensors().Battery.Volts; got != 0 {
			t.Errorf("a reading of %v was kept as %v", v, got)
		}
	}
}

// TestSetBatterySaysNo: a caller that asked for something impossible
// should be told, which is the rule SetPosition already states. Left to
// read(), the percentage was quietly zeroed and the caller went on
// believing the device was at 120%.
func TestSetBatterySaysNo(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	for _, pct := range []int8{-50, 101, 120} {
		if err := h.n.SetBattery(pct, false, h.now); err == nil {
			t.Errorf("SetBattery(%d) was accepted", pct)
		}
	}
	if err := h.n.SetBattery(55, false, h.now); err != nil {
		t.Errorf("SetBattery(55): %v", err)
	}
	if got := h.n.Sensors().Battery.Percent; got != 55 {
		t.Errorf("the battery reads %d%% after being set to 55", got)
	}
}

// TestFixIsWhatTheAirSees: a driver may report a position this node
// refuses, and a caller showing one should show the same one every frame
// carries. The board's console writes JSON, which cannot hold a NaN at
// all, so a receiver reporting one turned the whole self line into an
// error string.
func TestFixIsWhatTheAirSees(t *testing.T) {
	nan := float32(math.NaN())
	for _, tc := range []struct {
		name     string
		lat, lon float32
		want     bool
	}{
		{"a real position", 37.7749, -122.4194, true},
		{"null island", 0, 0, false},
		{"not a number", nan, nan, false},
		{"off the globe", 400, 900, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				c.Sensors = NewStatic(Sensors{
					Fix:     &Fix{Lat: tc.lat, Lon: tc.lon, AccuracyM: 3},
					Battery: Battery{Volts: 4.1, Percent: 90},
				})
			})
			if got := h.n.Fix() != nil; got != tc.want {
				t.Errorf("Fix() reports a position: %v, want %v", got, tc.want)
			}
		})
	}
}
