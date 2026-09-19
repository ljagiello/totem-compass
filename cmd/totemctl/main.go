// Command totemctl controls a Totem Compass over Bluetooth LE, speaking the
// same protocol as the official phone app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

type globals struct {
	device      string
	scanTimeout time.Duration
	halfDuplex  bool
	out         *printer
	log         *slog.Logger // warnings; with -trace also frames and phases
	start       time.Time

	// connect opens a client session; tests replace it with a simulator.
	connect func(ctx context.Context, opts client.Options) (*client.Client, error)
	// timeScale shrinks every wait for the device (tests); 0 means 1.
	timeScale float64
}

// wait scales a device timeout.
func (g *globals) wait(d time.Duration) time.Duration {
	if g.timeScale > 0 {
		return time.Duration(float64(d) * g.timeScale)
	}
	return d
}

// setLogger makes l the destination for warnings and -trace output.
func (g *globals) setLogger(l *slog.Logger) {
	g.log = l
	g.out.log = l
}

// newLogger logs to w at warn level, or with trace at debug level, which
// adds every BLE frame and the phase timings.
func newLogger(w io.Writer, trace bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelWarn}
	if trace {
		opts.Level = slog.LevelDebug
	} else {
		// Timestamps matter for a trace, not for an occasional warning.
		opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// phase logs the time since startup at debug level, to show where a command
// spends its time.
func (g *globals) phase(name string) {
	g.log.Debug(name, "elapsed", time.Since(g.start).Round(time.Millisecond))
}

type command struct {
	name, usage, help string
	run               func(ctx context.Context, g *globals, args []string) error
}

var commands = []command{
	{"scan", "[-all] [-v] [-for 10s]", "list nearby Totems", cmdScan},
	{"info", "", "show device identity, settings, battery and position", cmdInfo},
	{"watch", "[-for 30s]", "stream live data and peer updates until Ctrl-C", cmdWatch},
	{"peers", "", "list bonded Totems and points of interest", cmdPeers},
	{"name", "<new name>", "rename the Totem", cmdName},
	{"compass", "[-lock on|off] [-north on|off] [-blink on|off] [-power eco|normal]", "change compass settings", cmdCompass},
	{"power", "eco|normal", "switch power mode (eco dims the LEDs)", cmdPower},
	{"wifi", "scan | set <ssid> [password]", "list networks the Totem sees / save the network used for updates", cmdWiFi},
	{"peer", "color <mac> <colour> | hide <mac> | show <mac> | delete <mac> | select <mac> | select -stop", "manage a bonded peer", cmdPeer},
	{"poi", "add -name N -lat X -lon Y [-color C] [-sticky] [-id MAC] | delete <id>", "add or remove a point of interest to navigate to", cmdPOI},
	{"location", "<lat> <lon> [-acc meters]", "send a phone GNSS fix + time (used when the Totem has no fix)", cmdLocation},
	{"ota", "-yes [-version latest] [-branch totem]", "reboot the Totem into a WiFi firmware update", cmdOTA},
	{"raw", "[-conn] [-listen 10s] <hex>", "send a raw frame and print what comes back", cmdRaw},
}

const wakeHelp = `The Totem keeps Bluetooth off to save power and switches it off again
after every session. To make it connectable, double-press the power button:
the crystal breathes blue while it advertises, for 60 seconds. The
double-press toggles, so if it doesn't breathe blue, press again.

A Totem connected to another device, such as the Totem phone app, stops
advertising and can't be found. Close the app (or turn off the phone's
Bluetooth) before using totemctl.

On macOS the first run asks for Bluetooth access for your terminal
(System Settings → Privacy & Security → Bluetooth).`

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: totemctl [flags] <command> [args]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-9s %s\n  %-9s   %s\n", c.name, c.help, "", c.usage)
	}
	fmt.Fprintf(os.Stderr, "\nFlags:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\n%s\n", wakeHelp)
}

func main() { os.Exit(realMain()) }

// realMain returns the exit status, so deferred cleanup runs before exit.
func realMain() int {
	g := &globals{out: &printer{out: os.Stdout, err: os.Stderr}, start: time.Now()}
	g.connect = g.connectBLE
	flag.StringVar(&g.device, "d", os.Getenv("TOTEM_DEVICE"), "Totem to use: name or address substring (default: first found; env TOTEM_DEVICE)")
	flag.DurationVar(&g.scanTimeout, "scan-timeout", 60*time.Second, "how long to look for the Totem")
	flag.BoolVar(&g.out.json, "json", false, "print results as JSON lines")
	flag.BoolVar(&g.out.quiet, "q", false, "suppress progress messages")
	trace := flag.Bool("trace", false, "log every BLE frame and phase timing to stderr")
	flag.BoolVar(&g.halfDuplex, "half-duplex", false, "use the device's half-duplex (TX handoff) protocol; stalls on macOS, whose Bluetooth stack does not confirm its indications")
	flag.Usage = usage
	flag.Parse()
	g.setLogger(newLogger(os.Stderr, *trace))

	if flag.NArg() == 0 || flag.Arg(0) == "help" {
		usage()
		return 0
	}
	if lookup(flag.Arg(0)) == nil {
		fmt.Fprintf(os.Stderr, "totemctl: unknown command %q\n\n", flag.Arg(0))
		usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, g, flag.Args()); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "totemctl %s: %v\n", flag.Arg(0), err)
		return 1
	}
	return 0
}

func lookup(name string) *command {
	if i := slices.IndexFunc(commands, func(c command) bool { return c.name == name }); i >= 0 {
		return &commands[i]
	}
	return nil
}

// run executes one command line (without the global flags).
func run(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("no command")
	}
	c := lookup(args[0])
	if c == nil {
		return fmt.Errorf("unknown command %q", args[0])
	}
	return c.run(ctx, g, args[1:])
}

// connectBLE finds the Totem over Bluetooth and connects to it.
func (g *globals) connectBLE(ctx context.Context, opts client.Options) (*client.Client, error) {
	scanCtx, cancel := context.WithTimeout(ctx, g.scanTimeout)
	defer cancel()
	g.out.status("scanning for %s…", describeMatch(g.device))
	dev, err := client.Find(scanCtx, g.device)
	if errors.Is(err, client.ErrNotFound) {
		return nil, fmt.Errorf("%w. A Totem is only visible for 60 s after a double-press of the power button, "+
			"while its crystal breathes blue (the double-press toggles, so press again if it doesn't). "+
			"If the Totem phone app is open nearby, close it: a connected Totem stops advertising", err)
	}
	if err != nil {
		return nil, err
	}
	g.phase("found")
	g.out.status("connecting to %s (%s, %d dBm)…", orUnnamed(dev.Name), dev.Address, dev.RSSI)
	return client.Connect(ctx, dev, opts)
}

// subflags parses a subcommand's flags; Go's flag package stops at the first
// positional argument, so flags are also accepted after positionals.
func subflags(name string, args []string, define func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	define(fs)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func onOffValue(fs *flag.FlagSet, name, usage string) *string {
	return fs.String(name, "", usage+" (on|off)")
}

func parseOnOff(name, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("-%s must be on or off, not %q", name, v)
}

func parsePower(v string) (protocol.PowerMode, error) {
	switch strings.ToLower(v) {
	case "eco":
		return protocol.PowerEco, nil
	case "normal", "performance", "full":
		return protocol.PowerNormal, nil
	}
	return 0, fmt.Errorf("power mode must be eco or normal, not %q", v)
}

func wantArgs(args []string, n int, usage string) error {
	if len(args) != n {
		return fmt.Errorf("usage: %s", usage)
	}
	return nil
}
