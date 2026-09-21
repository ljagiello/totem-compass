package emulator

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		line string
		want Command
	}{
		{"pair", Command{Op: OpPair}},
		{"  status  ", Command{Op: OpStatus}},
		{"unbond 8C:94:DF:7B:04:78", Command{Op: OpUnbond, MAC: totem}},
		{"pos 37.5868 -122.0074", Command{Op: OpPos, Position: &Position{Lat: 37.5868, Lon: -122.0074, AccuracyM: 5, AltitudeM: -500, SolutionID: 2, HeadingOfMotion: -1}}},
		{"pos -90 180 0", Command{Op: OpPos, Position: &Position{Lat: -90, Lon: 180, AltitudeM: -500, SolutionID: 1, HeadingOfMotion: -1}}},
		{"pos 1 2 127", Command{Op: OpPos, Position: &Position{Lat: 1, Lon: 2, AccuracyM: 127, AltitudeM: -500, HeadingOfMotion: -1}}},
		{"pos off", Command{Op: OpPos}},
		{"heading 359", Command{Op: OpHeading, Heading: 359}},
		{"sos on", Command{Op: OpSOS, On: true}},
		{"sos off", Command{Op: OpSOS}},
		{"log debug", Command{Op: OpLog, Level: slog.LevelDebug}},
		{"log WARN", Command{Op: OpLog, Level: slog.LevelWarn}},
		{"format json", Command{Op: OpFormat, JSON: true}},
		{"format text", Command{Op: OpFormat}},
		{"selftest", Command{Op: OpSelfTest}},
		{"help", Command{Op: OpHelp}},
	}
	for _, tt := range tests {
		got, err := ParseCommand(tt.line)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseCommand(%q) = %+v, %v; want %+v", tt.line, got, err, tt.want)
		}
	}
}

// TestParseCommandRejects: the firmware used to take these, and put NaN,
// impossible positions or headings on the air.
func TestParseCommandRejects(t *testing.T) {
	for _, line := range []string{
		"", "   ", "fly", "Pair",
		"pair now",
		"unbond", "unbond 12345", "unbond 8c94df7b0478 extra",
		"pos", "pos 1", "pos off now", "pos 1 2 3 4",
		"pos NaN 0", "pos 0 NaN", "pos Inf 0", "pos 0 -Inf", "pos 1e39 0",
		"pos 90.001 0", "pos -91 0", "pos 0 180.5", "pos 0 -200",
		"pos 1 2 -1", "pos 1 2 128", "pos 1 2 x", "pos 1 2 1.5", "pos x 2",
		"heading", "heading -1", "heading 360", "heading 9999", "heading 1.5", "heading 0x10",
		"sos", "sos yes", "sos on off",
		"log", "log loud",
		"format", "format xml",
	} {
		if c, err := ParseCommand(line); err == nil {
			t.Errorf("ParseCommand(%q) = %+v, want an error", line, c)
		}
	}
	if _, err := ParseCommand("fly"); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown command: err = %v, want ErrUnknownCommand", err)
	}
}

// FuzzParseCommand feeds arbitrary console lines: a parsed command must be
// in range and print as a line that parses back to the same command.
func FuzzParseCommand(f *testing.F) {
	for _, s := range []string{
		"pair", "unbond 8c94df7b0478", "pos 37.5868 -122.0074 3", "pos off", "pos NaN 1",
		"heading 90", "sos on", "log debug", "log info+2", "format json", "status", "selftest", "help", "pos 1e-45 -0 0",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		c, err := ParseCommand(line)
		if err != nil {
			if c != (Command{}) {
				t.Fatalf("ParseCommand(%q) failed but returned %+v", line, c)
			}
			return
		}
		if p := c.Position; p != nil {
			if math.IsNaN(float64(p.Lat)) || math.IsNaN(float64(p.Lon)) || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
				t.Fatalf("ParseCommand(%q): position %+v out of range", line, *p)
			}
			if p.AccuracyM < 0 {
				t.Fatalf("ParseCommand(%q): accuracy %d", line, p.AccuracyM)
			}
			if _, err := New(Config{MAC: self, Position: p}, t0).status(t0, mesh.PeerStatus, false).MarshalBinary(); err != nil {
				t.Fatalf("ParseCommand(%q): a status with this position does not encode: %v", line, err)
			}
		}
		if c.Heading < 0 || c.Heading > 359 {
			t.Fatalf("ParseCommand(%q): heading %d", line, c.Heading)
		}
		s := c.String()
		if strings.ContainsAny(s, "\r\n") {
			t.Fatalf("String() = %q spans lines", s)
		}
		back, err := ParseCommand(s)
		if err != nil || !reflect.DeepEqual(back, c) {
			t.Fatalf("ParseCommand(%q) = %+v, but its String %q parses as %+v, %v", line, c, s, back, err)
		}
	})
}

func feed(r *LineReader, s string) (lines []string, errs int) {
	for i := range len(s) {
		line, err := r.Feed(s[i])
		if err != nil {
			errs++
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, errs
}

func TestLineReader(t *testing.T) {
	var r LineReader
	lines, errs := feed(&r, "\x00\xffpair\r\n\r\nsos\x07 on\n")
	if want := []string{"pair", "sos on"}; !reflect.DeepEqual(lines, want) || errs != 0 {
		t.Errorf("lines = %q, %d errors; want %q", lines, errs, want)
	}
	// A line one byte too long is dropped whole: cut at 128 bytes, this one
	// would run as "pos 1 2 3".
	long := "pos 1 2 3" + strings.Repeat(" ", MaxCommandLine-len("pos 1 2 3")) + "9"
	lines, errs = feed(&r, long+"\nstatus\n")
	if want := []string{"status"}; !reflect.DeepEqual(lines, want) || errs != 1 {
		t.Errorf("after an over-long line: lines = %q, %d errors; want %q and 1 error", lines, errs, want)
	}
	exact := strings.Repeat("x", MaxCommandLine)
	if lines, errs = feed(&r, exact+"\n"); len(lines) != 1 || lines[0] != exact || errs != 0 {
		t.Errorf("a %d-byte line: %q, %d errors", MaxCommandLine, lines, errs)
	}
}

// FuzzLineReader feeds arbitrary serial input: every line that comes out is
// printable ASCII, fits MaxCommandLine and is what was typed on that line.
func FuzzLineReader(f *testing.F) {
	f.Add([]byte("pair\r\nsos on\n"))
	f.Add([]byte("\x00\xff\r\n" + strings.Repeat("a", 200) + "\nhelp\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		var r LineReader
		var typed []byte
		for _, b := range in {
			line, err := r.Feed(b)
			if b != '\r' && b != '\n' {
				if line != "" || err != nil {
					t.Fatalf("Feed(%q) mid-line = %q, %v", b, line, err)
				}
				if b >= ' ' && b <= '~' {
					typed = append(typed, b)
				}
				continue
			}
			switch {
			case len(typed) > MaxCommandLine:
				if !errors.Is(err, ErrLineTooLong) || line != "" {
					t.Fatalf("%d-byte line: %q, %v", len(typed), line, err)
				}
			case err != nil || line != string(typed):
				t.Fatalf("line %q: got %q, %v", typed, line, err)
			}
			typed = typed[:0]
		}
	})
}

// TestInjectedFrameFollowsTheSameRules: rx feeds the node a frame as the
// radio would. It is how the paths that need a second Totem get tested on
// the board, so it must not be a way around the scope rules: a frame from
// a stranger is dropped exactly as an overheard one is.
func TestInjectedFrameFollowsTheSameRules(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(61 * time.Second)
	h.take()

	req := mesh.Locate{
		Origin: totem, Lat: 37.7749, Lon: -122.4194, UID: 909, ReplyRequested: true,
		MinRSSI: -127, MaxHops: 99, Expiry: int32(h.n.wall(h.now).Unix()) + 120, RelayMinDistM: 10,
	}
	frame, err := req.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("rx %s all -70 %x", totem, frame)
	c, err := ParseCommand(line)
	if err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	if got := c.String(); got != line {
		t.Errorf("round trip gave %q, want %q", got, line)
	}
	out, handled, err := h.n.Apply(c, h.now)
	if !handled || err != nil {
		t.Fatalf("apply: handled=%v err=%v", handled, err)
	}
	h.collect(out)
	got := h.take()
	if len(got) != 2 {
		t.Fatalf("an injected locate request sent %d frames, want a reply and a relay", len(got))
	}

	// The same frame from a Totem this node does not own goes nowhere.
	req.UID = 910
	frame, err = req.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	c, err = ParseCommand(fmt.Sprintf("rx %s all -70 %x", stranger, frame))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = h.n.Apply(c, h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.collect(out)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("an injected frame from a stranger sent %d frames", len(s))
	}
}

func TestParseRXRejects(t *testing.T) {
	for _, line := range []string{
		"rx",
		"rx 8c94df7b0478 all -70",
		"rx nothex all -70 a774",
		"rx 8c94df7b0478 somewhere -70 a774",
		"rx 8c94df7b0478 all 12 a774",   // a positive RSSI
		"rx 8c94df7b0478 all -200 a774", // below the floor
		"rx 8c94df7b0478 all -70 zz",    // not hex
		"rx 8c94df7b0478 all -70 a7740", // an odd number of hex digits
		"rx 8c94df7b0478 all -70 ",      // no frame
	} {
		if c, err := ParseCommand(line); err == nil {
			t.Errorf("%q parsed to %+v", line, c)
		}
	}
	// A frame longer than an ESP-NOW payload is refused rather than sent.
	long := fmt.Sprintf("rx %s all -70 %x", totem, make([]byte, mesh.MaxFrame+1))
	if _, err := ParseCommand(long); err == nil {
		t.Error("a frame over the ESP-NOW limit parsed")
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

// TestApplyRefusesAnEmptyRXCommand: Command and Apply are both exported,
// and String() already answers "rx" for one with no frame. On the board
// a nil dereference is a boot loop rather than a refused command.
func TestApplyRefusesAnEmptyRXCommand(t *testing.T) {
	h := newHarness(t, nil)
	_, handled, err := h.n.Apply(Command{Op: OpRX}, h.now)
	if !handled {
		t.Error("an rx command with no frame was not handled at all")
	}
	if err == nil {
		t.Error("an rx command with no frame was accepted")
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

// TestPosZeroZeroIsRefused: fix() reads 0,0 as no fix, so SetPosition has
// to refuse it rather than store something every reader ignores — the
// device reporting a position it does not have.
func TestPosZeroZeroIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 0, Lon: 0, AccuracyM: 5}, h.now); err == nil {
		t.Error("SetPosition accepted Null Island, which fix() reads as no fix")
	}
}
