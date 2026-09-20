package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.bug.st/serial"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/protocol"
)

// The mesh commands talk to cmd/totememu, the ESP32 that joins the Totem
// ESP-NOW mesh, over its serial console. On "format json" the firmware logs
// JSON lines, and on "log debug" it logs every frame it hears or sends as
// hex, which these commands decode with the mesh package.

const (
	emulatorBaud = 115200
	consoleReply = 3 * time.Second // time the emulator has to answer "format json"
	consoleQuiet = time.Second     // status and send stop after this much silence
	// maxHeardMs is the largest peer age worth printing: past it a
	// Duration in milliseconds overflows, and a board that has been up
	// for 292 million years is not the likelier explanation.
	maxHeardMs = float64(math.MaxInt64 / int64(time.Millisecond))
	// consoleChunk and consoleChunkGap pace a line onto the wire: 64 bytes
	// is half the board's receive FIFO, and 5 ms is longer than it takes
	// to drain at 115200 baud.
	consoleChunk    = 64
	consoleChunkGap = 5 * time.Millisecond
)

// esp32BridgeVIDs are the USB vendor ids of the serial bridges on ESP32
// boards: Silicon Labs CP210x, WCH CH34x, FTDI, and Espressif's built-in
// USB serial.
var esp32BridgeVIDs = []string{"10C4", "1A86", "0403", "303A"}

// openSerial opens the emulator's console. DTR and RTS stay asserted, the
// go.bug.st/serial default: an ESP32 board's auto-reset circuit pulls EN low
// when RTS is set without DTR, so changing them reboots the emulator.
func openSerial(name string) (io.ReadWriteCloser, error) {
	return serial.Open(name, &serial.Mode{BaudRate: emulatorBaud})
}

func newMeshCmd(g *globals) *cobra.Command {
	c := group("mesh", "Watch and drive the ESP32 mesh emulator (cmd/totememu) over USB serial",
		newMeshWatchCmd(g), newMeshStatusCmd(g), newMeshPairCmd(g), newMeshClockCmd(g), newMeshSendCmd(g))
	c.Long = `The mesh commands talk to an ESP32 running cmd/totememu, which joins the
Totem ESP-NOW mesh as another Totem, over its USB serial console. watch
decodes every frame your Totem sends the board.

The port is --port, TOTEM_PORT or port: in the config file; by default the
only USB serial port of an ESP32 board.`
	c.PersistentFlags().String("port", "", "serial port of the emulator board (default: the only ESP32 USB serial port)")
	return c
}

func newMeshWatchCmd(g *globals) *cobra.Command {
	var (
		dur     time.Duration
		raw, tx bool
	)
	c := &cobra.Command{
		Use:   "watch",
		Short: "Decode every frame the emulator hears, and its events, until Ctrl-C",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if dur > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, dur)
				defer cancel()
			}
			// ctx, not cmd.Context(): --for is how long the whole thing
			// may take, and the handshake is part of it. Against a board
			// that is not running the emulator, opening spends its own
			// three-second reply window before giving up, so a
			// --for 500ms took six times what it asked for.
			e, err := openEmulator(ctx, g, true)
			if err != nil {
				return err
			}
			defer e.close()
			g.out.status("watching %s; Ctrl-C to stop", e.name)
			for {
				l, err := e.next(ctx)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return nil // --for elapsed or Ctrl-C: how watch ends
				}
				if err != nil {
					return err
				}
				switch {
				case l.Msg == "rx" || (l.Msg == "tx" && tx):
					g.out.meshFrame(decodeFrame(l), raw)
				case l.Msg == "tx", hiddenEvents[l.Msg]:
				default:
					g.out.meshEvent(l.event())
				}
			}
		},
	}
	c.Flags().DurationVar(&dur, "for", 0, "stop after this long (default: until Ctrl-C)")
	c.Flags().BoolVar(&raw, "raw", false, "also print each frame's bytes")
	c.Flags().BoolVar(&tx, "tx", false, "also show the frames the emulator sends")
	return c
}

// hiddenEvents are log lines watch leaves out: the decoded frames already
// say it, or they answer watch's own commands.
var hiddenEvents = map[string]bool{"peer status": true, "log format": true, "log level": true}

func newMeshStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the emulator and the peers it is bonded with",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := openEmulator(cmd.Context(), g, false)
			if err != nil {
				return err
			}
			defer e.close()
			if err := e.send("status"); err != nil {
				return err
			}
			self, err := e.await(cmd.Context(), g.wait(consoleReply), func(l consoleLine) bool { return l.Msg == "self" })
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			var peers []consoleLine
			for {
				l, err := e.await(cmd.Context(), g.wait(consoleQuiet), func(l consoleLine) bool { return l.Msg == "peer" })
				if errors.Is(err, errTimeout) {
					break // no more peer lines
				}
				if err != nil {
					return fmt.Errorf("status: %w", err)
				}
				peers = append(peers, l)
			}
			g.out.meshStatus(self, peers)
			return nil
		},
	}
}

func newMeshPairCmd(g *globals) *cobra.Command {
	var dur time.Duration
	c := &cobra.Command{
		Use:   "pair",
		Short: "Wait for your Totem to pair with the emulator",
		Long: `pair waits while you hold the Totem's Touch Crystal for about 1.2 s, right
next to the emulator board, until the pairing animation starts. The emulator
answers the Totem's bond request by itself; bonding needs a signal of
-25 dBm or stronger on both sides, so the two must nearly touch.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dur <= 0 {
				return fmt.Errorf("--for %s: give it time to hold the Touch Crystal in", dur)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), dur)
			defer cancel()
			e, err := openEmulator(ctx, g, false)
			if err != nil {
				return err
			}
			defer e.close()
			g.out.status("hold the Totem's Touch Crystal for about 1.2 s next to the board…")
			for {
				l, err := e.next(ctx)
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("no bond within %s: hold the Touch Crystal until the pairing animation starts, "+
						"with the Totem touching the board (bonding needs -25 dBm or stronger)", dur)
				}
				if err != nil {
					return err
				}
				if hiddenEvents[l.Msg] || l.Msg == "rx" || l.Msg == "tx" || l.Msg == "radio" {
					continue
				}
				g.out.meshEvent(l.event())
				if l.Msg == "bonded" || l.Msg == "both devices already bonded" {
					return nil
				}
			}
		},
	}
	c.Flags().DurationVar(&dur, "for", time.Minute, "give up after this long")
	return c
}

func newMeshClockCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "clock",
		Short: "Give the emulator this computer's time, as a GNSS lock would",
		Long: `The board has no clock of its own: it counts from 1970 until a peer or a
GNSS receiver gives it the time. clock sets it from this computer, so the
emulator can hold wall-clock radio windows and advertise the time to peers
the way a Totem with a fix does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := openEmulator(cmd.Context(), g, false)
			if err != nil {
				return err
			}
			defer e.close()
			if err := e.send(fmt.Sprintf("clock %d", time.Now().UnixMilli())); err != nil {
				return err
			}
			l, err := e.await(cmd.Context(), g.wait(consoleReply), func(l consoleLine) bool {
				return l.Msg == "clock set" || l.Msg == "bad command"
			})
			if err != nil {
				return fmt.Errorf("clock: %w", err)
			}
			g.out.meshEvent(l.event())
			if l.Msg == "bad command" {
				return fmt.Errorf("the emulator rejected the clock: %v", l.attrs["err"])
			}
			return nil
		},
	}
}

func newMeshSendCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "send <console command>...",
		Short: "Run an emulator console command (pos, sos, heading, unbond, pair, selftest, …)",
		Long: `send runs one emulator console command and prints what it logs:

  pos <lat> <lon> [accuracy m] | pos off   the GNSS fix to report
  heading <deg>                            the compass azimuth to report
  sos on|off                               the SOS flag
  pair                                     open a 6 s pairing window
  unbond <mac>                             delete a peer
  selftest                                 check the radio receiver`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEmulator(cmd.Context(), g, false)
			if err != nil {
				return err
			}
			defer e.close()
			if err := e.send(strings.Join(args, " ")); err != nil {
				return err
			}
			// The command's own lines follow at once; a self test takes 6 s.
			quiet := g.wait(consoleQuiet)
			if args[0] == "selftest" {
				quiet = g.wait(8 * time.Second)
			}
			for {
				l, err := e.await(cmd.Context(), quiet, func(l consoleLine) bool {
					return !hiddenEvents[l.Msg] && l.Msg != "rx" && l.Msg != "tx" && l.Msg != "radio"
				})
				if errors.Is(err, errTimeout) {
					return nil // the command has stopped logging
				}
				if err != nil {
					return err // the port died, which is not a finished command
				}
				g.out.meshEvent(l.event())
				switch {
				case l.Level == "WARN" && l.Msg == "unknown command":
					return fmt.Errorf("the emulator does not know %q", strings.Join(args, " "))
				case l.Level == "WARN" && l.Msg == "bad command":
					return fmt.Errorf("the emulator rejected %q: %v", strings.Join(args, " "), l.attrs["err"])
				}
				quiet = g.wait(consoleQuiet)
			}
		},
	}
	// Everything after the console command belongs to it: "pos 37.5 -122"
	// must not read -122 as a flag.
	c.Flags().SetInterspersed(false)
	return c
}

// ---- console session ------------------------------------------------------

// emulator is an open console session with cmd/totememu.
type emulator struct {
	g     *globals
	name  string
	port  io.ReadWriteCloser
	lines chan consoleLine
	done  chan struct{} // closed when the reader ends
	err   error         // why the reader ended, once done is closed
	debug bool
}

var errNoEmulator = errors.New("no answer from the emulator")

// openEmulator opens the console, switches it to JSON lines and, with debug,
// makes it log every frame.
func openEmulator(ctx context.Context, g *globals, debug bool) (*emulator, error) {
	name, err := g.emulatorPort()
	if err != nil {
		return nil, err
	}
	port, err := g.openPort(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	// The USB bridge keeps what the board logged while nobody read it.
	if p, ok := port.(interface{ ResetInputBuffer() error }); ok {
		_ = p.ResetInputBuffer()
	}
	e := &emulator{g: g, name: name, port: port, lines: make(chan consoleLine, 64), done: make(chan struct{})}
	go e.read()
	if err := e.handshake(ctx); err != nil {
		e.close()
		return nil, err
	}
	if debug {
		e.debug = true
		if err := e.send("log debug"); err != nil {
			e.close()
			return nil, err
		}
	}
	return e, nil
}

// handshake switches the console to JSON. The first newline ends whatever
// partial line the board holds, and the command is repeated each second, so
// a byte lost while the port opened costs a retry, not the session.
func (e *emulator) handshake(ctx context.Context) error {
	// The caller's deadline as well as this one: --for is how long the
	// whole command may take and the handshake is part of it. Which of
	// the two ran out decides what to say — the caller's means the
	// person asked for less time than this takes, and blaming the board
	// for that sent them to reflash a board that was answering.
	outer := ctx
	ctx, cancel := context.WithTimeout(ctx, e.g.wait(consoleReply))
	defer cancel()
	if err := e.send(""); err != nil {
		return err
	}
	for {
		if err := e.send("format json"); err != nil {
			return err
		}
		_, err := e.await(ctx, e.g.wait(consoleQuiet), func(l consoleLine) bool { return l.Msg == "log format" })
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, errTimeout):
			return err
		case outer.Err() != nil:
			return fmt.Errorf("gave up waiting for %s: %w", e.name, outer.Err())
		case ctx.Err() != nil:
			return fmt.Errorf("%w on %s: is cmd/totememu flashed on this board? (tinygo flash -target esp32-generic ./cmd/totememu)", errNoEmulator, e.name)
		}
	}
}

// maxConsoleLine bounds one line from the board. The longest the firmware
// logs is a frame in hex, well under a kilobyte; a longer run means line
// noise, which is dropped rather than ending the session (bufio.Scanner
// gives up for good on such a line).
const maxConsoleLine = 16 << 10

// read turns console lines into consoleLines until the port closes. Lines
// that are not JSON (the boot log, a person's text-format session) go to the
// debug log.
func (e *emulator) read() {
	defer close(e.done)
	r := bufio.NewReaderSize(e.port, maxConsoleLine)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			e.g.log.Debug("console", "dropped", "a line over "+strconv.Itoa(maxConsoleLine)+" bytes")
			if err == nil {
				continue
			}
		}
		if err != nil {
			e.err = err
			return
		}
		text := strings.TrimRight(string(line), "\r\n")
		l, perr := parseConsoleLine([]byte(text))
		if perr != nil {
			e.g.log.Debug("console", "line", text)
			continue
		}
		select {
		case e.lines <- l:
		default:
			// Nobody is reading between commands. Dropping keeps the port
			// drained, so the board never blocks writing while this side
			// writes, which would deadlock both.
			e.g.log.Debug("console", "dropped", l.Msg)
		}
	}
}

func (e *emulator) send(line string) error {
	e.g.log.Debug("console", "send", line)
	// In chunks, with a pause between them. The board has no flow control
	// and a 128-byte receive FIFO, and it stops reading for tens of
	// milliseconds while it erases a flash sector; a long line — an
	// injected frame runs to a couple of hundred characters — would lose
	// its middle and run as a different command.
	b := []byte(line + "\r\n")
	for len(b) > 0 {
		n := min(len(b), consoleChunk)
		if _, err := e.port.Write(b[:n]); err != nil {
			return fmt.Errorf("write to %s: %w", e.name, err)
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(consoleChunkGap)
		}
	}
	return nil
}

// next returns the next console line.
func (e *emulator) next(ctx context.Context) (consoleLine, error) {
	select {
	case l := <-e.lines:
		return l, nil
	case <-ctx.Done():
		return consoleLine{}, ctx.Err()
	case <-e.done:
		return consoleLine{}, fmt.Errorf("%s: %w", e.name, e.err)
	}
}

// await returns the first line match accepts, dropping the others, or
// errTimeout after d.
func (e *emulator) await(ctx context.Context, d time.Duration, match func(consoleLine) bool) (consoleLine, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		l, err := e.next(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return l, errTimeout
		}
		if err != nil || match(l) {
			return l, err
		}
	}
}

// close puts the console back the way a person reading it expects, and
// closes the port.
func (e *emulator) close() {
	if e.debug {
		_ = e.send("log info")
	}
	_ = e.send("format text")
	_ = e.port.Close()
	<-e.done
}

// emulatorPort is --port, or else the only USB serial port with an ESP32
// board's bridge chip.
func (g *globals) emulatorPort() (string, error) {
	if g.port != "" {
		return g.port, nil
	}
	ports, err := g.ports()
	if err != nil {
		return "", fmt.Errorf("list serial ports: %w (pass --port)", err)
	}
	var found []string
	for _, p := range ports {
		if p.IsUSB && slices.Contains(esp32BridgeVIDs, strings.ToUpper(p.VID)) && !strings.HasPrefix(p.Name, "/dev/tty.") {
			found = append(found, p.Name) // on macOS the /dev/cu.* twin is the one to open
		}
	}
	switch len(found) {
	case 0:
		return "", errors.New("no ESP32 USB serial port found: plug in the emulator board or pass --port")
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("several ESP32 serial ports (%s): pick one with --port", strings.Join(found, ", "))
}

// ---- console lines ---------------------------------------------------------

// consoleLine is one JSON log line from the emulator.
type consoleLine struct {
	Level string       `json:"level"`
	Msg   string       `json:"msg"`
	Src   protocol.MAC `json:"src"`
	Dst   protocol.MAC `json:"dst"`
	RSSI  int          `json:"rssi"`
	Frame string       `json:"frame"`
	attrs map[string]any
}

func parseConsoleLine(b []byte) (consoleLine, error) {
	var l consoleLine
	if err := json.Unmarshal(b, &l.attrs); err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	if l.Msg == "" || l.Level == "" {
		return l, errors.New("not an emulator log line")
	}
	for _, k := range []string{"up", "level", "msg"} {
		delete(l.attrs, k)
	}
	return l, nil
}

// MeshEvent is something the emulator logged: a bond, the clock sync, a
// console reply.
type MeshEvent struct {
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

func (l consoleLine) event() MeshEvent { return MeshEvent{l.Level, l.Msg, l.attrs} }

// MeshFrame is an ESP-NOW frame the emulator heard (rx) or sent (tx).
type MeshFrame struct {
	Dir   string       `json:"dir"`
	Src   protocol.MAC `json:"src,omitzero"`
	Dst   protocol.MAC `json:"dst"`
	RSSI  int          `json:"rssi,omitempty"`
	Kind  string       `json:"kind"`
	Frame mesh.Message `json:"frame,omitempty"`
	Error string       `json:"error,omitempty"`
	Raw   string       `json:"raw"`
}

func decodeFrame(l consoleLine) MeshFrame {
	f := MeshFrame{Dir: l.Msg, Src: l.Src, Dst: l.Dst, RSSI: l.RSSI, Raw: l.Frame}
	b, err := hex.DecodeString(l.Frame)
	if err == nil {
		f.Frame, err = mesh.Parse(b)
	}
	if err != nil {
		f.Kind, f.Error = "invalid", err.Error()
		return f
	}
	f.Kind = kindOf(f.Frame)
	return f
}

// kindOf names a frame for the JSON output. The names are written out
// rather than taken from the Go types: they are part of what this command
// promises its readers, and a type renamed in the mesh package should not
// quietly change them.
func kindOf(m mesh.Message) string {
	switch m.(type) {
	case mesh.Peer:
		return "Peer"
	case mesh.Locate:
		return "Locate"
	case mesh.SmartGroup:
		return "SmartGroup"
	case mesh.SmartGroupReply:
		return "SmartGroupReply"
	case mesh.DemiGod:
		return "DemiGod"
	case mesh.Unknown:
		return "Unknown"
	}
	return "invalid"
}

// ---- output ----------------------------------------------------------------

func (p *printer) meshEvent(e MeshEvent) {
	if p.json {
		p.emit(e)
		return
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		v := e.Attrs[k]
		if s, ok := v.(string); ok && strings.ContainsAny(s, " \t\"=") {
			v = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&b, " %s=%v", k, v)
	}
	mark := "·"
	if e.Level == "WARN" || e.Level == "ERROR" {
		mark = "!"
	}
	p.printf("%s %s %s%s\n", time.Now().Format("15:04:05"), mark, e.Msg, b.String())
}

func (p *printer) meshFrame(f MeshFrame, raw bool) {
	if p.json {
		// A frame whose decoded fields JSON cannot hold still goes out,
		// with the decode dropped and the reason in its place. Kind and
		// Error keep the meaning decodeFrame gives them — Kind says what
		// the bytes turned out to be, and a Kind of "invalid" is the one
		// that means they did not parse — so a reader can tell a frame
		// that could not be decoded from one that could not be printed.
		// The raw hex is on the line either way, and it is the record
		// that matters.
		if _, err := json.Marshal(f.Frame); err != nil {
			f.Frame = nil
			f.Error = fmt.Sprintf("decoded frame cannot be written as JSON: %v", err)
		}
		p.emit(f)
		return
	}
	ts := time.Now().Format("15:04:05")
	if f.Dir == "rx" {
		p.printf("%s ← %s %4d dBm  %s\n", ts, f.Src, f.RSSI, frameLine(f))
	} else {
		p.printf("%s → %s           %s\n", ts, f.Dst, frameLine(f))
	}
	if raw {
		p.printf("         %s\n", f.Raw)
	}
}

func frameLine(f MeshFrame) string {
	switch m := f.Frame.(type) {
	case mesh.Peer:
		return peerFrameLine(m)
	case mesh.Locate:
		kind := "locate reply"
		if m.ReplyRequested {
			kind = "locate request"
		}
		return fmt.Sprintf("%s uid %d from %s  hop %d/%d  %s  expires %s%s", kind, m.UID, m.Origin, m.Hops, m.MaxHops,
			position(m.Lat, m.Lon, m.PosAccuracyM), time.Unix(int64(m.Expiry), 0).Local().Format("15:04:05"), flagList("sos", m.SOS))
	case mesh.SmartGroup:
		return fmt.Sprintf("smart group %s uid %d  %d members  %s left  host %s", instructionName(m.Instruction), m.UID,
			len(m.Members), time.Duration(m.TimeoutMs)*time.Millisecond, position(m.Lat, m.Lon, m.PosAccuracyM))
	case mesh.SmartGroupReply:
		verb := "join"
		if m.Leave {
			verb = "leave"
		}
		return fmt.Sprintf("smart group %s uid %d  %s", verb, m.UID, position(m.Lat, m.Lon, m.PosAccuracyM))
	case mesh.DemiGod:
		return fmt.Sprintf("demi-god command %d, %d bytes", m.Command, len(m.Payload))
	case mesh.Unknown:
		return fmt.Sprintf("unknown (%d,%d) %x", m.Category, m.Command, m.Payload)
	}
	return "invalid frame: " + f.Error
}

func peerFrameLine(m mesh.Peer) string {
	var b strings.Builder
	switch {
	case m.Command == mesh.PeerStatus:
		b.WriteString("status")
	case m.Command == mesh.PeerBond && m.Ack:
		b.WriteString("bond confirm")
	case m.Command == mesh.PeerBond:
		b.WriteString("bond request")
	case m.Command == mesh.PeerUnbond:
		b.WriteString("unbond")
	default:
		fmt.Fprintf(&b, "peer cmd %d", m.Command)
	}
	fmt.Fprintf(&b, " %q  %s", m.Name, position(m.Lat, m.Lon, m.PosAccuracyM))
	if m.AltitudeM != -500 {
		fmt.Fprintf(&b, " alt %dm", m.AltitudeM)
	}
	if m.SpeedKPH > 0 {
		fmt.Fprintf(&b, " %dkm/h", m.SpeedKPH)
	}
	fmt.Fprintf(&b, "  azimuth %d°", m.Azimuth)
	if m.HeadingOfMotion >= 0 {
		fmt.Fprintf(&b, " heading %d°", m.HeadingOfMotion)
	}
	switch m.Orientation {
	case mesh.OrientationVertical:
		b.WriteString(" upright")
	case mesh.OrientationHorizontal:
		b.WriteString(" flat")
	}
	fmt.Fprintf(&b, "  batt %d%% %.2fV  fw %d.%d.%d  up %s", m.BattPct, m.BattVolts, m.Major, m.Minor, m.Patch,
		time.Duration(m.UptimeMin)*time.Minute)
	if m.Unix > 0 && m.TimeOfDayMs >= 0 {
		clock := time.Unix(int64(m.Unix), int64(m.TimeOfDayMs%1000)*int64(time.Millisecond))
		b.WriteString("  clock " + clock.Local().Format("15:04:05.000"))
	}
	b.WriteString(flagList("sos", m.SOS, "phone", m.PhoneConnected, "phone-gnss", m.GNSSSource == mesh.GNSSPhone))
	return b.String()
}

func position(lat, lon float32, acc int8) string {
	if lat == 0 && lon == 0 {
		return "no fix"
	}
	if acc < 0 {
		return fmt.Sprintf("%.6f,%.6f", lat, lon)
	}
	return fmt.Sprintf("%.6f,%.6f ±%dm", lat, lon, acc)
}

func instructionName(i mesh.SmartGroupInstruction) string {
	switch i {
	case mesh.SmartGroupAdvertise:
		return "advertise"
	case mesh.SmartGroupFinalize:
		return "finalize"
	case mesh.SmartGroupAbandon:
		return "abandon"
	}
	return fmt.Sprintf("instruction %d", i)
}

// whereFrom renders the position in a status line, or "no fix".
//
// Both halves are asserted, not only the latitude: a line carrying one
// and not the other printed "37.586754,%!f(<nil>)" into the row.
//
// has_position is the board saying whether it accepted the position at
// all — a bond restored from flash and never heard from since has none,
// and printing its zeroes puts the peer on Null Island, a thousand
// kilometers off the coast of Ghana. A board built before that field
// existed does not send it, and for those the coordinates themselves are
// the only answer available, so its absence is not taken as a no.
func whereFrom(a map[string]any) string {
	if has, ok := a["has_position"]; ok && has != true {
		return "no fix"
	}
	lat, latOK := a["lat"].(float64)
	lon, lonOK := a["lon"].(float64)
	if !latOK || !lonOK {
		return "no fix"
	}
	return fmt.Sprintf("%.6f,%.6f", lat, lon)
}

func (p *printer) meshStatus(self consoleLine, peers []consoleLine) {
	if p.json {
		p.emit(self.event())
		for _, l := range peers {
			p.emit(l.event())
		}
		return
	}
	a := self.attrs
	pos := whereFrom(a)
	p.printf("emulator %v %q  pairing %s  sos %s  heading %v°  %s\n", a["mac"], a["name"],
		onOff(a["pairing"] == true), onOff(a["sos"] == true), a["heading"], pos)
	if len(peers) == 0 {
		p.println("no peers: run totemctl mesh pair")
		return
	}
	for _, l := range peers {
		a := l.attrs
		heard := "never"
		// A peer restored from the bond list has not been heard yet and
		// reports -1. Anything past maxHeardMs is not a real age either,
		// and multiplying it into a Duration would overflow into a
		// negative one.
		if ms, ok := a["heard_ms"].(float64); ok && ms >= 0 && ms <= maxHeardMs {
			heard = (time.Duration(ms) * time.Millisecond).Round(100*time.Millisecond).String() + " ago"
		}
		via := ""
		if a["mesh"] == true {
			via = " via mesh"
		}
		p.printf("peer     %v %q  rssi %v  heard %s%s  %s  batt %v%%\n",
			a["mac"], a["name"], a["rssi"], heard, via, whereFrom(a), a["batt"])
	}
}
