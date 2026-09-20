package emulator

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/protocol"
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
	// OpFlash and OpStore reach the board's own flash, so only the
	// firmware answers them. On the host the node leaves them unhandled.
	OpFlash Op = "flash"
	OpStore Op = "store"
	// OpRX feeds the node a frame as though the radio had heard it. It is
	// how the mesh paths that need a second Totem — a locate relay, a
	// Smart Group invitation — get exercised on the board itself.
	OpRX Op = "rx"
	// The parts of a Totem that are not the radio.
	OpTouch Op = "touch"
	OpLEDs  Op = "leds"
	OpColor Op = "color"
	OpPower Op = "power"
	OpOTA   Op = "ota"
)

// Help is the console's command summary.
const Help = "commands: pair | unbond <mac> | pos <lat> <lon> [accuracy m] | pos off | heading <deg> | " +
	"sos on|off | sim still|walk|drive [bearing] | sim off | flat on|off | batt <0-100> [charging] | " +
	"clock <unix ms> | " +
	"touch crystal|power|sos tap|double|triple|hold [ms] | leds | color <name> | power [on|off] | ota [update] | " +
	"rx <src mac> self|all <rssi> <hex frame> | status | store [forget|open] | flash | log debug|info|warn|error | format text|json | selftest"

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
	Sub      string     // sub-command, as in "store forget"
	RX       *Received  // rx: the frame to feed the node
	Input    Input      // touch: which input
	Gesture  Gesture    // touch: what it did
	HoldMs   int64      // touch: how long a hold lasted
	Color    Color      // color
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
	case OpPair, OpStatus, OpSelfTest, OpHelp, OpFlash:
		err = want(0)
	case OpStore:
		// store prints the saved settings; store forget wipes them, as a
		// factory reset does; store open reads the sector on a board that
		// did not read it while starting.
		if len(args) == 1 {
			if args[0] != "forget" && args[0] != "open" {
				err = fmt.Errorf("store takes nothing, forget or open, got %q", args[0])
			}
			c.Sub = args[0]
		} else {
			err = want(0)
		}
	case OpUnbond:
		if err = want(1); err == nil {
			c.MAC, err = mesh.ParseMAC(args[0])
		}
	case OpRX:
		c.RX, err = parseRX(args)
	case OpTouch:
		c.Input, c.Gesture, c.HoldMs, err = parseTouch(args)
	case OpColor:
		if err = want(1); err == nil {
			c.Color, err = ParseColor(args[0])
		}
	case OpLEDs:
		err = want(0)
	case OpPower:
		// power reports the state; power off and power on are what the
		// button held does. The modes in between follow the battery —
		// setting one by hand would last until the next reading, 5 ms
		// later — so the way to reach them is to set the battery.
		if len(args) == 1 {
			if args[0] != "on" && args[0] != "off" {
				err = fmt.Errorf("power takes nothing, on or off, got %q "+
					"(the mode follows the battery: change that with batt)", args[0])
			}
			c.Sub = args[0]
		} else {
			err = want(0)
		}
	case OpOTA:
		// ota reports the last update; ota update runs one.
		if len(args) == 1 {
			if args[0] != "update" {
				err = fmt.Errorf("ota takes nothing or update, got %q", args[0])
			}
			c.Sub = args[0]
		} else {
			err = want(0)
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

// parseTouch reads "<crystal|power|sos> <tap|double|triple|hold> [ms]".
// A hold takes how long the finger stays down, because that is what
// decides which gesture the firmware reports.
func parseTouch(args []string) (Input, Gesture, int64, error) {
	if len(args) < 2 || len(args) > 3 {
		return 0, 0, 0, errors.New("want crystal|power|sos tap|double|triple|hold [ms]")
	}
	in, err := ParseInput(args[0])
	if err != nil {
		return 0, 0, 0, err
	}
	var g Gesture
	switch args[1] {
	case "tap":
		g = SingleTap
	case "double":
		g = DoubleTap
	case "triple":
		g = TripleTap
	case "hold":
		g = Hold
	default:
		return 0, 0, 0, fmt.Errorf("%q: want tap, double, triple or hold", args[1])
	}
	var ms int64
	if len(args) == 3 {
		if g != Hold {
			return 0, 0, 0, fmt.Errorf("only a hold takes a length, not %s", args[1])
		}
		ms, err = strconv.ParseInt(args[2], 10, 32)
		if err != nil || ms <= 0 || ms > 60000 {
			return 0, 0, 0, fmt.Errorf("hold %q: want 1 to 60000 ms", args[2])
		}
	}
	if g == Hold && ms == 0 {
		// Long enough for the longest hold any input reports.
		ms = int64(longHold / time.Millisecond)
	}
	return in, g, ms, nil
}

// parseRX reads "<src mac> self|all <rssi> <hex frame>". The frame is
// untrusted in the same way an on-air one is: it goes to the same parser,
// and the node drops what it does not like.
func parseRX(args []string) (*Received, error) {
	if len(args) < 4 {
		return nil, errors.New("want <src mac> self|all <rssi dBm> <hex frame>")
	}
	src, err := mesh.ParseMAC(args[0])
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	var dst mesh.MAC
	switch args[1] {
	case "all":
		dst = mesh.Broadcast
	case "self":
		// The node fills its own address in; leaving it zero would look
		// like a frame addressed to nobody.
	default:
		return nil, fmt.Errorf("%q: want self or all", args[1])
	}
	rssi, err := strconv.ParseInt(args[2], 10, 8)
	if err != nil || rssi > 0 || rssi < -127 {
		return nil, fmt.Errorf("rssi %q: want 0 to -127 dBm", args[2])
	}
	// Everything after the RSSI is the frame, joined: a paste that has a
	// space in it — the non-breaking one a document leaves behind, or the
	// grouping someone typed — arrives here already split into fields,
	// and refusing it for having the wrong number of arguments is the
	// answer least likely to be understood. totemctl raw joins the same
	// way.
	//
	// The same cleaner the CLI and ParseMAC use: a frame is pasted out of
	// a log or a chat window as often as it is typed, so it arrives with
	// colons, dashes or a non-breaking space in it.
	data, err := hex.DecodeString(protocol.CleanHex(strings.Join(args[3:], "")))
	if err != nil {
		return nil, fmt.Errorf("frame: %w", err)
	}
	if len(data) == 0 || len(data) > mesh.MaxFrame {
		return nil, fmt.Errorf("frame of %d bytes: want 1 to %d", len(data), mesh.MaxFrame)
	}
	return &Received{Src: src, Dst: dst, RSSI: int8(rssi), Data: data}, nil
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
	case OpTouch:
		if c.Gesture == Hold {
			return fmt.Sprintf("touch %s hold %d", c.Input, c.HoldMs)
		}
		return fmt.Sprintf("touch %s %s", c.Input,
			map[Gesture]string{SingleTap: "tap", DoubleTap: "double", TripleTap: "triple"}[c.Gesture])
	case OpColor:
		return "color " + c.Color.String()
	case OpPower, OpOTA:
		if c.Sub != "" {
			return string(c.Op) + " " + c.Sub
		}
		return string(c.Op)
	case OpRX:
		if c.RX == nil {
			return "rx"
		}
		to := "self"
		if c.RX.Dst == mesh.Broadcast {
			to = "all"
		}
		return fmt.Sprintf("rx %s %s %d %x", c.RX.Src, to, c.RX.RSSI, c.RX.Data)
	case OpStore:
		if c.Sub != "" {
			return "store " + c.Sub
		}
		return "store"
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
		err = n.SetPosition(c.Position, now)
	case OpHeading:
		err = n.SetHeading(c.Heading, now)
	case OpSOS:
		n.SetSOS(c.On)
	case OpTouch:
		return n.touch(c, now), true, nil
	case OpColor:
		n.SetColor(c.Color, now)
	case OpPower:
		switch c.Sub {
		case "":
			return nil, false, nil // the firmware prints the state
		case "on":
			n.PowerOn(now)
		case "off":
			n.PowerOff(now)
		}
	case OpOTA:
		if c.Sub != "update" {
			return nil, false, nil // the firmware prints the state
		}
		return nil, true, n.Update(now)
	case OpFlat:
		err = n.SetFlat(c.On, now)
	case OpBattery:
		err = n.SetBattery(c.Percent, c.On, now)
	case OpSim:
		if c.On {
			n.StartSim(c.Motion, c.Heading, now)
		} else {
			n.StopSim(now)
		}
	case OpClock:
		return nil, true, n.SetClock(time.UnixMilli(c.ClockMs), now)
	case OpRX:
		// A command with no frame in it is a caller's mistake, not a
		// frame: String() already answers "rx" for one, and every other
		// op that carries a pointer is safe to build by hand. On the
		// board the alternative is a nil dereference, which is a boot
		// loop rather than a refused command.
		if c.RX == nil {
			return nil, true, errors.New("rx: no frame to inject")
		}
		// The frame goes in where the radio's would, so the scope rules,
		// the duplicate check and the relay decision all still apply: an
		// injected frame is treated exactly as an overheard one.
		rx := *c.RX
		if rx.Dst != mesh.Broadcast {
			rx.Dst = n.cfg.MAC
		}
		return n.Receive(now, rx), true, nil
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return n.flush(), true, nil
}

// MaxCommandLine is the longest console line the emulator runs. It holds
// the longest frame the rx command can inject, which is what parseRX
// bounds: mesh.MaxFrame bytes of hex, two characters each, plus the
// words around them — "rx", a MAC, a destination, an RSSI and their
// spaces come to a couple of dozen.
//
// Sized from that rather than from the 108-byte peer frame it used to
// name: the two disagreed, so the largest frames parseRX accepts were
// refused first as a line too long, by a limit whose own comment
// described a shorter frame.
const MaxCommandLine = 2*mesh.MaxFrame + 64

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
