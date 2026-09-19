package emulator

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

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
	OpSim      Op = "sim"
	OpClock    Op = "clock"
	OpFlat     Op = "flat"
	OpBattery  Op = "batt"
)

// Help is the console's command summary.
const Help = "commands: pair | unbond <mac> | pos <lat> <lon> [accuracy m] | pos off | heading <deg> | " +
	"sos on|off | sim still|walk|drive [bearing] | sim off | flat on|off | batt <0-100> [charging] | " +
	"clock <unix ms> | " +
	"status | log debug|info|warn|error | format text|json | selftest"

// ErrUnknownCommand is returned for a line that names no command.
var ErrUnknownCommand = errors.New("unknown command")

// Command is one parsed console line.
type Command struct {
	Op       Op
	MAC      mesh.MAC   // unbond
	Position *Position  // pos; nil clears the fix
	Heading  int16      // heading and sim bearing, 0-359
	On       bool       // sos, flat, batt charging
	Level    slog.Level // log
	JSON     bool       // format
	Motion   Motion     // sim
	Percent  int8       // batt
	ClockMs  int64      // clock, milliseconds since the Unix epoch
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
	case OpFlat:
		if err = want(1); err == nil {
			c.On, err = parseChoice(args[0], "on", "off")
		}
	case OpSim:
		c.Motion, c.Heading, c.On, err = parseSim(args)
	case OpBattery:
		c.Percent, c.On, err = parseBattery(args)
	case OpClock:
		if err = want(1); err == nil {
			c.ClockMs, err = strconv.ParseInt(args[0], 10, 64)
			if err != nil || c.ClockMs <= 0 {
				err = fmt.Errorf("%q: want milliseconds since the Unix epoch", args[0])
			}
		}
	default:
		return Command{}, fmt.Errorf("%w %q", ErrUnknownCommand, f[0])
	}
	if err != nil {
		return Command{}, fmt.Errorf("%s: %w", c.Op, err)
	}
	return c, nil
}

// parseSim reads "still|walk|drive [bearing]" or "off"; on reports whether
// the simulation runs.
func parseSim(args []string) (m Motion, bearing int16, on bool, err error) {
	if len(args) == 1 && args[0] == "off" {
		return Still, 0, false, nil
	}
	if len(args) < 1 || len(args) > 2 {
		return 0, 0, false, errors.New("want still|walk|drive [bearing] or off")
	}
	switch args[0] {
	case "still":
		m = Still
	case "walk":
		m = Walk
	case "drive":
		m = Drive
	default:
		return 0, 0, false, fmt.Errorf("%q: want still, walk, drive or off", args[0])
	}
	if len(args) == 2 {
		d, err := strconv.ParseInt(args[1], 10, 16)
		if err != nil || d < 0 || d > 359 {
			return 0, 0, false, fmt.Errorf("bearing %q: want 0-359 degrees", args[1])
		}
		bearing = int16(d)
	}
	return m, bearing, true, nil
}

// parseBattery reads "<0-100> [charging]".
func parseBattery(args []string) (percent int8, charging bool, err error) {
	if len(args) < 1 || len(args) > 2 {
		return 0, false, errors.New("want <0-100> [charging]")
	}
	v, err := strconv.ParseInt(args[0], 10, 8)
	if err != nil || v < 0 || v > 100 {
		return 0, false, fmt.Errorf("%q: want 0-100 percent", args[0])
	}
	if len(args) == 2 {
		if args[1] != "charging" {
			return 0, false, fmt.Errorf("%q: want charging", args[1])
		}
		charging = true
	}
	return int8(v), charging, nil
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
	p := &Position{Lat: lat, Lon: lon, AccuracyM: defaultAccuracyM, AltitudeM: -500, HeadingOfMotion: -1}
	if len(args) == 3 {
		acc, err := strconv.ParseInt(args[2], 10, 8)
		if err != nil || acc < 0 {
			return nil, fmt.Errorf("accuracy %q: want 0-127 m", args[2])
		}
		p.AccuracyM = int8(acc)
	}
	p.SolutionID = solutionFor(p.AccuracyM)
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
	case OpFlat:
		return "flat " + map[bool]string{true: "on", false: "off"}[c.On]
	case OpSim:
		if !c.On {
			return "sim off"
		}
		return fmt.Sprintf("sim %s %d", c.Motion, c.Heading)
	case OpBattery:
		if c.On {
			return fmt.Sprintf("batt %d charging", c.Percent)
		}
		return fmt.Sprintf("batt %d", c.Percent)
	case OpClock:
		return fmt.Sprintf("clock %d", c.ClockMs)
	}
	return string(c.Op)
}

func formatDegrees(d float32) string { return strconv.FormatFloat(float64(d), 'g', -1, 32) }

// Apply runs the commands that change the device: pairing, peers, the
// sensors and the clock. The firmware handles the rest (status, log,
// format, selftest, help), which touch its console rather than the node.
// It reports whether the command belonged here.
func (n *Node) Apply(c Command, now time.Time) (out []Packet, handled bool, err error) {
	switch c.Op {
	case OpPair:
		return n.Pair(now), true, nil
	case OpUnbond:
		return n.Unbond(now, c.MAC), true, nil
	case OpPos:
		n.SetPosition(c.Position)
	case OpHeading:
		n.SetHeading(c.Heading)
	case OpSOS:
		n.SetSOS(c.On)
	case OpFlat:
		n.SetFlat(c.On, now)
	case OpBattery:
		n.SetBattery(c.Percent, c.On, now)
	case OpSim:
		if c.On {
			n.StartSim(c.Motion, c.Heading, now)
		} else {
			n.StopSim(now)
		}
	case OpClock:
		return nil, true, n.SetClock(time.UnixMilli(c.ClockMs), now)
	default:
		return nil, false, nil
	}
	return n.flush(), true, nil
}

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
