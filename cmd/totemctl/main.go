// Command totemctl controls a Totem Compass over Bluetooth LE, speaking the
// same protocol as the official phone app.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

type globals struct {
	device      string
	scanTimeout time.Duration
	trace       bool
	halfDuplex  bool
	out         *printer
}

type command struct {
	name, usage, help string
	run               func(ctx context.Context, g *globals, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"scan", "[-all] [-for 10s]", "list nearby Totems", cmdScan},
		{"info", "", "show device identity, settings, battery and position", cmdInfo},
		{"watch", "[-for 30s]", "stream live data and peer updates until Ctrl-C", cmdWatch},
		{"peers", "", "list bonded Totems and points of interest", cmdPeers},
		{"name", "<new name>", "rename the Totem", cmdName},
		{"compass", "[-lock on|off] [-north on|off] [-blink on|off] [-power eco|normal]", "change compass settings", cmdCompass},
		{"power", "eco|normal", "switch power mode (eco dims the LEDs)", cmdPower},
		{"wifi", "scan | set <ssid> [password]", "list networks the Totem sees / save the network used for updates", cmdWiFi},
		{"peer", "color <mac> <colour> | hide <mac> | show <mac> | delete <mac> | select <mac> | select -stop", "manage a bonded peer", cmdPeer},
		{"poi", "add -name N -lat X -lon Y [-color C] [-sticky] [-id MAC] | delete <id>", "add or remove a point of interest to navigate to", cmdPOI},
		{"location", "<lat> <lon> [-acc metres]", "send a phone GNSS fix + time (used when the Totem has no fix)", cmdLocation},
		{"ota", "-yes [-version latest] [-branch totem]", "reboot the Totem into a WiFi firmware update", cmdOTA},
		{"raw", "[-conn] [-listen 10s] <hex>", "send a raw frame and print what comes back", cmdRaw},
	}
}

const wakeHelp = `The Totem keeps Bluetooth off to save power and switches it off again
after every session. To make it connectable, double-press the power button:
the crystal breathes blue while it advertises. The double-press toggles, so
if it doesn't breathe blue, press again.

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

func main() {
	g := &globals{out: &printer{out: os.Stdout}}
	flag.StringVar(&g.device, "d", os.Getenv("TOTEM_DEVICE"), "Totem to use: name or address substring (default: first found; env TOTEM_DEVICE)")
	flag.DurationVar(&g.scanTimeout, "scan-timeout", 60*time.Second, "how long to look for the Totem")
	flag.BoolVar(&g.out.json, "json", false, "print results as JSON lines")
	flag.BoolVar(&g.out.quiet, "q", false, "suppress progress messages")
	flag.BoolVar(&g.trace, "trace", false, "print every BLE frame to stderr")
	flag.BoolVar(&g.halfDuplex, "half-duplex", false, "use the device's half-duplex (TX handoff) protocol; stalls on macOS, whose Bluetooth stack does not confirm its indications")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() == 0 || flag.Arg(0) == "help" {
		usage()
		return
	}
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == flag.Arg(0) })
	if i < 0 {
		fmt.Fprintf(os.Stderr, "totemctl: unknown command %q\n\n", flag.Arg(0))
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := commands[i].run(ctx, g, flag.Args()[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "totemctl %s: %v\n", commands[i].name, err)
		os.Exit(1)
	}
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

// ---- commands ---------------------------------------------------------------

func cmdScan(ctx context.Context, g *globals, args []string) error {
	var all, verbose bool
	var dur time.Duration
	if _, err := subflags("scan", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&all, "all", false, "list every BLE device, not just Totems")
		fs.BoolVar(&verbose, "v", false, "also print advertised services and manufacturer data")
		fs.DurationVar(&dur, "for", 10*time.Second, "scan duration")
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	g.out.status("scanning for %s…", dur)
	n := 0
	err := client.Scan(ctx, all, func(d client.Device) {
		n++
		if g.out.json {
			g.out.emit(struct {
				Name     string   `json:"name"`
				Address  string   `json:"address"`
				RSSI     int16    `json:"rssi"`
				Services []string `json:"services,omitempty"`
			}{d.Name, d.Address.String(), d.RSSI, d.Services})
			return
		}
		fmt.Fprintf(g.out.out, "%-24s %s  %d dBm\n", orUnnamed(d.Name), d.Address, d.RSSI)
		if verbose {
			for _, s := range d.Services {
				fmt.Fprintf(g.out.out, "    service %s\n", s)
			}
			for id, b := range d.MfgData {
				fmt.Fprintf(g.out.out, "    mfg 0x%04x % x\n", id, b)
			}
		}
	})
	if err != nil {
		return err
	}
	if n == 0 && !all {
		g.out.status("no Totems found.\n\n%s", wakeHelp)
	}
	return nil
}

func cmdInfo(ctx context.Context, g *globals, args []string) error {
	if err := wantArgs(args, 0, "totemctl info"); err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	st, err := s.fetchStatic(30*time.Second, nil)
	if err != nil {
		return err
	}
	if s.live == nil {
		_, _ = s.await(15*time.Second, is[protocol.LiveData])
	}
	g.out.info(st, s.live)
	return nil
}

func cmdWatch(ctx context.Context, g *globals, args []string) error {
	var dur time.Duration
	pos, err := subflags("watch", args, func(fs *flag.FlagSet) {
		fs.DurationVar(&dur, "for", 0, "stop after this long (default: until Ctrl-C)")
	})
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, "totemctl watch [-for 30s]"); err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	s.echo = true
	g.out.status("connected; Ctrl-C to stop")
	all, _ := protocol.RequestPeerDetails()
	if err := s.send(protocol.RequestStaticData(), all); err != nil {
		return err
	}
	if _, err = s.await(dur, nil); errors.Is(err, errTimeout) {
		return nil
	}
	return err
}

func cmdPeers(ctx context.Context, g *globals, args []string) error {
	if err := wantArgs(args, 0, "totemctl peers"); err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	// The device only sends Peer Sync once Static Data was delivered.
	if _, err := s.fetchStatic(30*time.Second, nil); err != nil {
		return err
	}
	if err := s.send(protocol.RequestPeerSync()); err != nil {
		return err
	}
	m, err := s.await(30*time.Second, is[protocol.PeerSync])
	if err != nil {
		return fmt.Errorf("no peer list from the Totem: %w", err)
	}
	want := m.(protocol.PeerSync).Peers
	if s.c.HalfDuplex() { // legacy mode requests details as the Peer Sync ack
		all, _ := protocol.RequestPeerDetails()
		if err := s.send(all); err != nil {
			return err
		}
	}
	// One Peer Ping per peer follows, roughly one per second.
	missing := func() bool {
		for _, mac := range want {
			if _, ok := s.peers[mac]; !ok {
				return true
			}
		}
		return false
	}
	if missing() {
		if _, err := s.await(time.Duration(len(want)+10)*2*time.Second, func(protocol.Message) bool { return !missing() }); err != nil {
			g.out.warn("only %d of %d peers reported details", len(s.peers), len(want))
		}
	}
	if len(s.peers) == 0 {
		g.out.status("no bonded peers or points of interest")
		return nil
	}
	macs := make([]protocol.MAC, 0, len(s.peers))
	for m := range s.peers {
		macs = append(macs, m)
	}
	slices.SortFunc(macs, func(a, b protocol.MAC) int { return strings.Compare(s.peers[a].Name, s.peers[b].Name) })
	for _, m := range macs {
		if g.out.json {
			g.out.emit(s.peers[m])
		} else {
			fmt.Fprintln(g.out.out, peerLine(s.peers[m]))
		}
	}
	return nil
}

func cmdName(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: totemctl name <new name>")
	}
	name := strings.Join(args, " ")
	f, err := protocol.SetName(name)
	if err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.send(f); err != nil {
		return err
	}
	if _, err := s.fetchStatic(30*time.Second, func(d protocol.StaticData) bool { return d.Name == name }); err != nil {
		return fmt.Errorf("sent, but the Totem did not report the new name: %w", err)
	}
	g.out.status("renamed to %q", name)
	return nil
}

type compassChange struct {
	lock, north, blink *bool
	power              protocol.PowerMode
}

func cmdCompass(ctx context.Context, g *globals, args []string) error {
	var lock, north, blink, power *string
	if _, err := subflags("compass", args, func(fs *flag.FlagSet) {
		lock = onOffValue(fs, "lock", "compass lock")
		north = onOffValue(fs, "north", "persistent north")
		blink = onOffValue(fs, "blink", "blink peers on the ring")
		power = fs.String("power", "", "power mode (eco|normal)")
	}); err != nil {
		return err
	}
	var ch compassChange
	for _, o := range []struct {
		name string
		val  *string
		dst  **bool
	}{{"lock", lock, &ch.lock}, {"north", north, &ch.north}, {"blink", blink, &ch.blink}} {
		if *o.val == "" {
			continue
		}
		b, err := parseOnOff(o.name, *o.val)
		if err != nil {
			return err
		}
		*o.dst = &b
	}
	if *power != "" {
		p, err := parsePower(*power)
		if err != nil {
			return err
		}
		ch.power = p
	}
	if ch == (compassChange{}) {
		return errors.New("nothing to change; pass at least one of -lock, -north, -blink, -power")
	}
	return applyCompass(ctx, g, ch)
}

func cmdPower(ctx context.Context, g *globals, args []string) error {
	if err := wantArgs(args, 1, "totemctl power eco|normal"); err != nil {
		return err
	}
	p, err := parsePower(args[0])
	if err != nil {
		return err
	}
	return applyCompass(ctx, g, compassChange{power: p})
}

// applyCompass merges the requested change into the Totem's current
// settings: the (7,3) frame always carries every setting at once.
func applyCompass(ctx context.Context, g *globals, ch compassChange) error {
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	st, err := s.fetchStatic(30*time.Second, nil)
	if err != nil {
		return err
	}
	prefs := protocol.CompassPrefs{PersistentNorth: st.PersistentNorth, CompassLock: st.CompassLock, PowerMode: ch.power}
	if ch.lock != nil {
		prefs.CompassLock = *ch.lock
	}
	if ch.north != nil {
		prefs.PersistentNorth = *ch.north
	}
	if ch.blink != nil {
		prefs.PeerBlink = *ch.blink
	} else {
		g.out.warn("the Totem does not report peer blink; it will be set to off (pass -blink on to keep it on)")
	}
	if err := s.send(protocol.SetCompassPrefs(prefs)); err != nil {
		return err
	}
	st, err = s.fetchStatic(30*time.Second, func(d protocol.StaticData) bool {
		return d.CompassLock == prefs.CompassLock && d.PersistentNorth == prefs.PersistentNorth
	})
	if err != nil {
		return fmt.Errorf("sent, but the Totem did not confirm the new settings: %w", err)
	}
	if ch.power != protocol.PowerUnchanged && s.live != nil && s.live.PowerMode == protocol.PowerUnchanged {
		g.out.warn("this firmware does not report its power mode; the change was sent but cannot be confirmed")
	} else if ch.power != protocol.PowerUnchanged {
		if _, err := s.await(30*time.Second, func(m protocol.Message) bool {
			l, ok := m.(protocol.LiveData)
			return ok && l.PowerMode == ch.power
		}); err != nil && (s.live == nil || s.live.PowerMode != ch.power) {
			return fmt.Errorf("sent, but the Totem did not report power mode %s: %w", ch.power, err)
		}
	}
	msg := fmt.Sprintf("compass lock %s, persistent north %s, peer blink %s",
		onOff(st.CompassLock), onOff(st.PersistentNorth), onOff(prefs.PeerBlink))
	if ch.power != protocol.PowerUnchanged {
		msg += ", power " + ch.power.String()
	}
	g.out.status("%s", msg)
	return nil
}

func cmdWiFi(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: totemctl wifi scan | set <ssid> [password]")
	}
	switch args[0] {
	case "scan":
		s, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer s.close()
		if err := s.send(protocol.ClearWiFiScan(), protocol.ScanWiFi()); err != nil {
			return err
		}
		g.out.status("scanning (the Totem needs a few seconds)…")
		m, err := s.await(60*time.Second, is[protocol.WiFiNetworks])
		if err != nil {
			return err
		}
		if g.out.json {
			g.out.emit(m)
			return nil
		}
		for _, ssid := range m.(protocol.WiFiNetworks).SSIDs {
			fmt.Fprintln(g.out.out, ssid)
		}
		return nil
	case "set":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: totemctl wifi set <ssid> [password]")
		}
		pass := ""
		if len(args) == 3 {
			pass = args[2]
		}
		f, err := protocol.SaveWiFi(args[1], pass)
		if err != nil {
			return err
		}
		s, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer s.close()
		if err := s.send(f); err != nil {
			return err
		}
		if _, err := s.fetchStatic(30*time.Second, func(d protocol.StaticData) bool { return d.WiFiSSID == args[1] }); err != nil {
			return fmt.Errorf("sent, but the Totem did not report the new network: %w", err)
		}
		g.out.status("saved WiFi network %q", args[1])
		return nil
	}
	return fmt.Errorf("unknown wifi subcommand %q", args[0])
}

func cmdPeer(ctx context.Context, g *globals, args []string) error {
	const use = "usage: totemctl peer color <mac> <colour> | hide <mac> | show <mac> | delete <mac> | select <mac> | select -stop"
	var stopSel bool
	pos, err := subflags("peer", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&stopSel, "stop", false, "close the peer-management UI (select only)")
	})
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New(use)
	}
	if pos[0] == "select" && stopSel {
		return oneShot(ctx, g, protocol.StopPeerManagement(), "peer management closed")
	}
	if len(pos) < 2 {
		return errors.New(use)
	}
	mac, err := protocol.ParseMAC(pos[1])
	if err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	p, err := s.fetchPeer(mac, 30*time.Second)
	if err != nil {
		return err
	}
	upd := protocol.PeerUpdate{MAC: mac, Color: p.Color, Hidden: p.Hidden}
	switch pos[0] {
	case "color", "colour":
		if len(pos) != 3 {
			return errors.New(use)
		}
		if upd.Color, err = protocol.ParseRGB(pos[2]); err != nil {
			return err
		}
	case "hide", "show":
		upd.Hidden = pos[0] == "hide"
	case "delete":
		upd.Delete = true
	case "select":
		if err := s.send(protocol.SelectPeer(mac)); err != nil {
			return err
		}
		g.out.status("selected %q on the Totem", p.Name)
		return s.settle()
	default:
		return errors.New(use)
	}
	if err := s.send(protocol.UpdatePeer(upd)); err != nil {
		return err
	}
	if upd.Delete {
		if err := s.settle(); err != nil {
			return err
		}
		g.out.status("deleted %q", p.Name)
		return nil
	}
	p, err = s.fetchPeer(mac, 30*time.Second)
	if err != nil {
		return err
	}
	if p.Color != upd.Color || p.Hidden != upd.Hidden {
		return fmt.Errorf("sent, but the Totem reports colour %s hidden %v", p.Color, p.Hidden)
	}
	g.out.status("%s", peerLine(p))
	return nil
}

func cmdPOI(ctx context.Context, g *globals, args []string) error {
	const use = "usage: totemctl poi add -name N -lat X -lon Y [-color C] [-sticky] [-id MAC] | delete <id>"
	var name, color, id string
	var lat, lon float64
	var sticky bool
	pos, err := subflags("poi", args, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "label shown in the app")
		fs.Float64Var(&lat, "lat", 0, "latitude")
		fs.Float64Var(&lon, "lon", 0, "longitude")
		fs.StringVar(&color, "color", "white", "ring colour: palette name, #rrggbb or r,g,b")
		fs.BoolVar(&sticky, "sticky", false, "keep pointing at this POI")
		fs.StringVar(&id, "id", "", "6-byte id (default: random, locally administered)")
	})
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New(use)
	}
	switch pos[0] {
	case "add":
		if name == "" || (lat == 0 && lon == 0) {
			return errors.New(use)
		}
		poi := protocol.POI{Name: name, Lat: float32(lat), Lon: float32(lon), Sticky: sticky}
		if poi.Color, err = protocol.ParseRGB(color); err != nil {
			return err
		}
		if id != "" {
			if poi.ID, err = protocol.ParseMAC(id); err != nil {
				return err
			}
		} else {
			_, _ = rand.Read(poi.ID[:])
			poi.ID[0] = poi.ID[0]&^0x01 | 0x02 // unicast, locally administered: never a real Totem
		}
		f, err := protocol.AddPOI(poi)
		if err != nil {
			return err
		}
		s, err := open(ctx, g)
		if err != nil {
			return err
		}
		defer s.close()
		if err := s.send(f); err != nil {
			return err
		}
		p, err := s.fetchPeer(poi.ID, 30*time.Second)
		if err != nil {
			return fmt.Errorf("sent, but the Totem did not list the POI: %w", err)
		}
		if g.out.json {
			g.out.emit(p)
		} else {
			fmt.Fprintf(g.out.out, "added %s\n", peerLine(p))
		}
		return nil
	case "delete":
		if len(pos) != 2 {
			return errors.New(use)
		}
		mac, err := protocol.ParseMAC(pos[1])
		if err != nil {
			return err
		}
		return oneShot(ctx, g, protocol.UpdatePeer(protocol.PeerUpdate{MAC: mac, Delete: true}), "deleted "+mac.String())
	}
	return errors.New(use)
}

func cmdLocation(ctx context.Context, g *globals, args []string) error {
	var acc float64
	pos, err := subflags("location", args, func(fs *flag.FlagSet) {
		fs.Float64Var(&acc, "acc", 10, "horizontal accuracy in metres")
	})
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 2, "totemctl location <lat> <lon> [-acc metres]"); err != nil {
		return err
	}
	lat, err1 := strconv.ParseFloat(pos[0], 32)
	lon, err2 := strconv.ParseFloat(pos[1], 32)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("bad coordinates: %w", err)
	}
	now := time.Now()
	f := protocol.SendPhoneFix(protocol.PhoneFix{
		Lat: float32(lat), Lon: float32(lon), HAcc: acc,
		Unix: int32(now.Unix()), UnixMS: int16(now.Nanosecond() / 1e6), Focused: true,
	})
	return oneShot(ctx, g, f, fmt.Sprintf("sent fix %.6f,%.6f ±%.0fm", lat, lon, acc))
}

func cmdOTA(ctx context.Context, g *globals, args []string) error {
	var yes bool
	req := protocol.OTARequest{Cmd: 1, EndpointID: 1}
	if _, err := subflags("ota", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&yes, "yes", false, "really reboot into the updater")
		fs.StringVar(&req.Version, "version", "latest", "release code to install")
		fs.StringVar(&req.Branch, "branch", "totem", "release branch")
	}); err != nil {
		return err
	}
	if !yes {
		return errors.New("this reboots the Totem and updates it over its saved WiFi network (see `wifi set`); it needs a charged battery. Re-run with -yes")
	}
	f, err := protocol.StartOTA(req)
	if err != nil {
		return err
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.send(f); err != nil {
		return err
	}
	select {
	case <-s.c.Done():
		g.out.status("the Totem disconnected to reboot into the updater")
	case <-time.After(20 * time.Second):
		return errors.New("the Totem did not reboot; it refuses to update unless the battery is charged")
	case <-ctx.Done():
	}
	return nil
}

func cmdRaw(ctx context.Context, g *globals, args []string) error {
	var conn bool
	var listen time.Duration
	pos, err := subflags("raw", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&conn, "conn", false, "write to the conn-status characteristic instead of data")
		fs.DurationVar(&listen, "listen", 10*time.Second, "how long to print replies")
	})
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: totemctl raw [-conn] <hex>")
	}
	b, err := hex.DecodeString(strings.NewReplacer(" ", "", ":", "").Replace(strings.Join(pos, "")))
	if err != nil {
		return err
	}
	f := protocol.Frame{Channel: protocol.Data, Bytes: b}
	if conn {
		f.Channel = protocol.ConnStatus
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.send(f); err != nil {
		return err
	}
	s.echo = true
	if _, err := s.await(listen, nil); !errors.Is(err, errTimeout) {
		return err
	}
	return nil
}

// oneShot sends a single frame in the next TX window and waits until the
// device has had a full window to act on it.
func oneShot(ctx context.Context, g *globals, f protocol.Frame, done string) error {
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.send(f); err != nil {
		return err
	}
	if err := s.settle(); err != nil {
		return err
	}
	g.out.status("%s", done)
	return nil
}
