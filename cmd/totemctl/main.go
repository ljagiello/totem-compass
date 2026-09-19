// Command totemctl controls a Totem Compass over Bluetooth LE, speaking the
// same protocol as the official phone app.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

type globals struct {
	device      string
	scanTimeout time.Duration
	halfDuplex  bool
	blink       *bool // TOTEM_BLINK or blink in the config file, if set
	out         *printer
	log         *slog.Logger // warnings; with --trace also frames and phases
	start       time.Time

	// configPath is the default --config file, which may be missing; empty
	// (tests) reads no file.
	configPath string
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

// setLogger makes l the destination for warnings and --trace output.
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

const wakeHelp = `The Totem keeps Bluetooth off to save power and switches it off again
after every session. To make it connectable, double-press the power button:
the crystal breathes blue while it advertises, for 60 seconds. The
double-press toggles, so if it doesn't breathe blue, press again.

A Totem connected to another device, such as the Totem phone app, stops
advertising and can't be found. Close the app (or turn off the phone's
Bluetooth) before using totemctl.

On macOS the first run asks for Bluetooth access for your terminal
(System Settings → Privacy & Security → Bluetooth).`

func main() { os.Exit(realMain()) }

// realMain returns the exit status, so deferred cleanup runs before exit.
func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g := &globals{start: time.Now(), configPath: defaultConfigPath()}
	g.connect = g.connectBLE
	cmd, err := newRootCmd(g).ExecuteContextC(ctx)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, context.Canceled) {
			msg = "interrupted"
		}
		_, _ = fmt.Fprintf(os.Stderr, "%s: %s\n", cmd.CommandPath(), msg)
	}
	return exitCode(err)
}

// exitCode maps a command's error to the process status: 130 (128+SIGINT)
// when Ctrl-C cut it short, so scripts cannot mistake it for success.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return 130
	}
	return 1
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "totemctl", "config.yaml")
}

func newRootCmd(g *globals) *cobra.Command {
	v := viper.New()
	root := &cobra.Command{
		Use:   "totemctl",
		Short: "Control a Totem Compass over Bluetooth LE",
		Long: `totemctl controls a Totem Compass over Bluetooth LE, speaking the same
protocol as the official phone app.

Each global flag can also be set as TOTEM_<FLAG> in the environment
(TOTEM_DEVICE, TOTEM_HALF_DUPLEX, ...) or as a key in the config file
(device: <address from totemctl scan>). A flag beats the environment,
which beats the file. So can blink, the peer-blink setting that compass
and power must send.

` + wakeHelp,
		SilenceErrors: true, // realMain prints them
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true // the arguments parsed; what fails now is not a usage mistake
			return g.configure(cmd, v)
		},
	}
	pf := root.PersistentFlags()
	pf.StringP("device", "d", "", "Totem to use: an address substring, as totemctl scan prints (default: the first found)")
	pf.Duration("scan-timeout", 60*time.Second, "how long to look for the Totem")
	pf.Bool("json", false, "print results as JSON lines")
	pf.BoolP("quiet", "q", false, "suppress progress messages")
	pf.Bool("trace", false, "log every BLE frame and phase timing to stderr")
	pf.Bool("half-duplex", false, "use the device's half-duplex (TX handoff) protocol; stalls on macOS, whose Bluetooth stack does not confirm its indications")
	pf.String("config", g.configPath, "config file (YAML, TOML or JSON) with the long flag names as keys")
	root.AddCommand(
		newScanCmd(g), newInfoCmd(g), newWatchCmd(g), newPeersCmd(g),
		newNameCmd(g), newCompassCmd(g), newPowerCmd(g), newWiFiCmd(g),
		newPeerCmd(g), newPOICmd(g), newLocationCmd(g), newOTACmd(g), newRawCmd(g),
	)
	return root
}

// configure resolves the global settings, each from its flag, TOTEM_*
// environment variable or config file key, and sets up output.
func (g *globals) configure(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Root().PersistentFlags()); err != nil {
		return err
	}
	v.SetEnvPrefix("TOTEM")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	if path := v.GetString("config"); path != "" {
		v.SetConfigFile(path)
		// Only the default file may be missing.
		if err := v.ReadInConfig(); err != nil && (path != g.configPath || !errors.Is(err, fs.ErrNotExist)) {
			return fmt.Errorf("config: %w", err)
		}
	}
	g.device = v.GetString("device")
	g.scanTimeout = v.GetDuration("scan-timeout")
	g.halfDuplex = v.GetBool("half-duplex")
	g.blink = nil
	if b := v.GetString("blink"); b != "" {
		on, err := parseOnOff(b)
		if err != nil {
			return fmt.Errorf("blink setting: %w", err)
		}
		g.blink = &on
	}
	g.out = &printer{json: v.GetBool("json"), quiet: v.GetBool("quiet"), out: cmd.OutOrStdout(), err: cmd.ErrOrStderr()}
	g.setLogger(newLogger(cmd.ErrOrStderr(), v.GetBool("trace")))
	return nil
}

// connectBLE finds the Totem over Bluetooth and connects to it.
func (g *globals) connectBLE(ctx context.Context, opts client.Options) (*client.Client, error) {
	scanCtx, cancel := context.WithTimeout(ctx, g.scanTimeout)
	defer cancel()
	g.out.status("scanning for %s…", describeMatch(g.device))
	dev, err := client.Find(scanCtx, g.device)
	if errors.Is(err, client.ErrNotFound) {
		hint := ""
		if g.device != "" {
			hint = ". Every Totem advertises as \"totem\", not by the name `totemctl name` sets: " +
				"pick one by the address `totemctl scan` shows"
		}
		return nil, fmt.Errorf("%w%s. A Totem is only visible for 60 s after a double-press of the power button, "+
			"while its crystal breathes blue (the double-press toggles, so press again if it doesn't). "+
			"If the Totem phone app is open nearby, close it: a connected Totem stops advertising", err, hint)
	}
	if err != nil {
		return nil, err
	}
	g.phase("found")
	g.out.status("connecting to %s (%s, %d dBm)…", orUnnamed(dev.Name), dev.Address, dev.RSSI)
	return client.Connect(ctx, dev, opts)
}

// group is a command that only holds subcommands.
func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs, // so an unknown subcommand is an error, not an argument
		RunE: func(cmd *cobra.Command, _ []string) error {
			var names []string
			for _, s := range cmd.Commands() {
				names = append(names, s.Name())
			}
			return fmt.Errorf("missing subcommand: one of %s", strings.Join(names, ", "))
		},
	}
	c.AddCommand(subs...)
	return c
}

// switchFlag is an on|off flag that remembers whether it was given.
type switchFlag struct{ val *bool }

func (s *switchFlag) Set(v string) error {
	b, err := parseOnOff(v)
	if err != nil {
		return err
	}
	s.val = &b
	return nil
}

func (s *switchFlag) String() string {
	if s.val == nil {
		return ""
	}
	return onOff(*s.val)
}

func (s *switchFlag) Type() string { return "on|off" }

// powerFlag is an eco|normal flag; PowerUnchanged when not given.
type powerFlag protocol.PowerMode

func (p *powerFlag) Set(v string) error {
	m, err := parsePower(v)
	*p = powerFlag(m)
	return err
}

func (p *powerFlag) String() string {
	if protocol.PowerMode(*p) == protocol.PowerUnchanged {
		return ""
	}
	return protocol.PowerMode(*p).String()
}

func (p *powerFlag) Type() string { return "eco|normal" }

func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("must be on or off, not %q", v)
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
