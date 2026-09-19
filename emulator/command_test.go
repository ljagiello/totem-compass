package emulator

import (
	"errors"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

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
		{"pos 37.5868 -122.0074", Command{Op: OpPos, Position: &Position{Lat: 37.5868, Lon: -122.0074, AccuracyM: 5, AltitudeM: -500}}},
		{"pos -90 180 0", Command{Op: OpPos, Position: &Position{Lat: -90, Lon: 180, AltitudeM: -500}}},
		{"pos 1 2 127", Command{Op: OpPos, Position: &Position{Lat: 1, Lon: 2, AccuracyM: 127, AltitudeM: -500}}},
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
