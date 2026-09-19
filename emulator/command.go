package emulator

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/ljagiello/totem-compass/mesh"
)

// Op is a console command.
type Op string

// Console commands. Help lists them.
const (
	OpPair     Op = "pair"
	OpUnbond   Op = "unbond"
	OpPos      Op = "pos"
	OpHeading  Op = "heading"
	OpSOS      Op = "sos"
	OpStatus   Op = "status"
	OpLog      Op = "log"
	OpFormat   Op = "format"
	OpSelfTest Op = "selftest"
	OpHelp     Op = "help"
)

// Help is the console's command summary.
const Help = "commands: pair | unbond <mac> | pos <lat> <lon> [accuracy m] | pos off | heading <deg> | " +
	"sos on|off | status | log debug|info|warn|error | format text|json | selftest"

// ErrUnknownCommand is returned for a line that names no command.
var ErrUnknownCommand = errors.New("unknown command")

// Command is one parsed console line.
type Command struct {
	Op       Op
	MAC      mesh.MAC   // unbond
	Position *Position  // pos; nil clears the fix
	Heading  int16      // heading, 0-359
	On       bool       // sos
	Level    slog.Level // log
	JSON     bool       // format
}

// defaultAccuracyM is the accuracy pos reports when none is given.
const defaultAccuracyM = 5

// ParseCommand parses a console line. The console is untrusted input: every
// value is range-checked, so a typo cannot put a NaN or an impossible
// position, heading or accuracy on the air.
func ParseCommand(line string) (Command, error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return Command{}, ErrUnknownCommand
	}
	c := Command{Op: Op(f[0])}
	args := f[1:]
	want := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s takes %d argument(s), got %d", c.Op, n, len(args))
		}
		return nil
	}
	var err error
	switch c.Op {
	case OpPair, OpStatus, OpSelfTest, OpHelp:
		err = want(0)
	case OpUnbond:
		if err = want(1); err == nil {
			c.MAC, err = mesh.ParseMAC(args[0])
		}
	case OpPos:
		c.Position, err = parsePosition(args)
	case OpHeading:
		if err = want(1); err == nil {
			var d int64
			d, err = strconv.ParseInt(args[0], 10, 16)
			if err == nil && (d < 0 || d > 359) {
				err = fmt.Errorf("heading %d: want 0-359 degrees", d)
			}
			c.Heading = int16(d)
		}
	case OpSOS:
		if err = want(1); err == nil {
			c.On, err = parseChoice(args[0], "on", "off")
		}
	case OpFormat:
		if err = want(1); err == nil {
			c.JSON, err = parseChoice(args[0], "json", "text")
		}
	case OpLog:
		if err = want(1); err == nil {
			err = c.Level.UnmarshalText([]byte(args[0]))
		}
	default:
		return Command{}, fmt.Errorf("%w %q", ErrUnknownCommand, f[0])
	}
	if err != nil {
		return Command{}, fmt.Errorf("%s: %w", c.Op, err)
	}
	return c, nil
}

func parseChoice(s, yes, no string) (bool, error) {
	switch s {
	case yes:
		return true, nil
	case no:
		return false, nil
	}
	return false, fmt.Errorf("%q: want %s or %s", s, yes, no)
}

func parsePosition(args []string) (*Position, error) {
	if len(args) == 1 && args[0] == "off" {
		return nil, nil
	}
	if len(args) < 2 || len(args) > 3 {
		return nil, errors.New("want <lat> <lon> [accuracy m] or off")
	}
	lat, err := parseDegrees(args[0], 90)
	if err != nil {
		return nil, fmt.Errorf("latitude: %w", err)
	}
	lon, err := parseDegrees(args[1], 180)
	if err != nil {
		return nil, fmt.Errorf("longitude: %w", err)
	}
	p := &Position{Lat: lat, Lon: lon, AccuracyM: defaultAccuracyM, AltitudeM: -500}
	if len(args) == 3 {
		acc, err := strconv.ParseInt(args[2], 10, 8)
		if err != nil || acc < 0 {
			return nil, fmt.Errorf("accuracy %q: want 0-127 m", args[2])
		}
		p.AccuracyM = int8(acc)
	}
	return p, nil
}

// parseDegrees parses a coordinate within ±limit. The frames carry float32,
// so the check runs on the value that will be sent.
func parseDegrees(s string, limit float32) (float32, error) {
	v, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	d := float32(v)
	if math.IsNaN(v) || d < -limit || d > limit {
		return 0, fmt.Errorf("%q: want -%g to %g degrees", s, limit, limit)
	}
	return d, nil
}

// String is the command as a console line that ParseCommand reads back.
func (c Command) String() string {
	switch c.Op {
	case OpUnbond:
		return fmt.Sprintf("unbond %s", c.MAC)
	case OpPos:
		if c.Position == nil {
			return "pos off"
		}
		return fmt.Sprintf("pos %s %s %d", formatDegrees(c.Position.Lat), formatDegrees(c.Position.Lon), c.Position.AccuracyM)
	case OpHeading:
		return fmt.Sprintf("heading %d", c.Heading)
	case OpSOS:
		return "sos " + map[bool]string{true: "on", false: "off"}[c.On]
	case OpLog:
		return "log " + strings.ToLower(c.Level.String())
	case OpFormat:
		return "format " + map[bool]string{true: "json", false: "text"}[c.JSON]
	}
	return string(c.Op)
}

func formatDegrees(d float32) string { return strconv.FormatFloat(float64(d), 'g', -1, 32) }

// MaxCommandLine is the longest console line the emulator runs.
const MaxCommandLine = 128

// ErrLineTooLong is returned for a console line over MaxCommandLine bytes.
var ErrLineTooLong = fmt.Errorf("console line over %d bytes dropped", MaxCommandLine)

// LineReader assembles console lines from the bytes typed on the serial
// port. Non-printable bytes, such as noise while a host opens the port, are
// dropped. A line longer than MaxCommandLine is dropped whole, so a
// truncated line never runs as a different command.
type LineReader struct {
	buf  []byte
	long bool
}

// Feed adds one byte. At the end of a non-empty line it returns the line;
// otherwise it returns "".
func (r *LineReader) Feed(b byte) (string, error) {
	switch {
	case b == '\r' || b == '\n':
		line, long := string(r.buf), r.long
		r.buf, r.long = r.buf[:0], false
		if long {
			return "", ErrLineTooLong
		}
		return line, nil
	case b >= ' ' && b <= '~':
		if len(r.buf) < MaxCommandLine {
			r.buf = append(r.buf, b)
		} else {
			r.long = true
		}
	}
	return "", nil
}
