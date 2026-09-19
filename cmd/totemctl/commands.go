package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

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
		g.out.printf("%-24s %s  %d dBm\n", orUnnamed(d.Name), d.Address, d.RSSI)
		if verbose {
			for _, s := range d.Services {
				g.out.printf("    service %s\n", s)
			}
			for id, b := range d.MfgData {
				g.out.printf("    mfg 0x%04x % x\n", id, b)
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
	st, err := s.fetchStatic(g.wait(30*time.Second), nil)
	if err != nil {
		return err
	}
	if s.live == nil {
		_, _ = s.await(g.wait(15*time.Second), is[protocol.LiveData])
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
	if _, err := s.fetchStatic(g.wait(30*time.Second), nil); err != nil {
		return err
	}
	if err := s.send(protocol.RequestPeerSync()); err != nil {
		return err
	}
	m, err := s.await(g.wait(30*time.Second), is[protocol.PeerSync])
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
		limit := g.wait(time.Duration(len(want)+10) * 2 * time.Second)
		if _, err := s.await(limit, func(protocol.Message) bool { return !missing() }); err != nil {
			g.log.Warn("not every peer reported details", "reported", len(s.peers), "bonded", len(want))
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
			g.out.println(peerLine(s.peers[m]))
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
	if _, err := s.fetchStatic(g.wait(30*time.Second), func(d protocol.StaticData) bool { return d.Name == name }); err != nil {
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
	st, err := s.fetchStatic(g.wait(30*time.Second), nil)
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
		g.log.Warn("the Totem does not report peer blink; it will be set to off (pass -blink on to keep it on)")
	}
	if err := s.send(protocol.SetCompassPrefs(prefs)); err != nil {
		return err
	}
	st, err = s.fetchStatic(g.wait(30*time.Second), func(d protocol.StaticData) bool {
		return d.CompassLock == prefs.CompassLock && d.PersistentNorth == prefs.PersistentNorth
	})
	if err != nil {
		return fmt.Errorf("sent, but the Totem did not confirm the new settings: %w", err)
	}
	if ch.power != protocol.PowerUnchanged {
		if err := s.confirmPower(ch.power); err != nil {
			return err
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

// confirmPower waits for Live Data to report the power mode. Firmware 4.x
// leaves that field at 0, which only one Live record can tell, so it looks at
// one first.
func (s *session) confirmPower(want protocol.PowerMode) error {
	if s.live == nil {
		if _, err := s.await(s.g.wait(15*time.Second), is[protocol.LiveData]); err != nil {
			return fmt.Errorf("sent, but no Live Data arrived to confirm power mode %s: %w", want, err)
		}
	}
	if s.live.PowerMode == protocol.PowerUnchanged {
		s.g.log.Warn("this firmware does not report its power mode; the change was sent but cannot be confirmed")
		return nil
	}
	if s.live.PowerMode == want {
		return nil
	}
	if _, err := s.await(s.g.wait(30*time.Second), func(m protocol.Message) bool {
		l, ok := m.(protocol.LiveData)
		return ok && l.PowerMode == want
	}); err != nil {
		return fmt.Errorf("sent, but the Totem did not report power mode %s: %w", want, err)
	}
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
		m, err := s.await(g.wait(60*time.Second), is[protocol.WiFiNetworks])
		if err != nil {
			return err
		}
		if g.out.json {
			g.out.emit(m)
			return nil
		}
		for _, ssid := range m.(protocol.WiFiNetworks).SSIDs {
			g.out.println(ssid)
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
		if _, err := s.fetchStatic(g.wait(30*time.Second), func(d protocol.StaticData) bool { return d.WiFiSSID == args[1] }); err != nil {
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
	switch pos[0] {
	case "color", "colour":
		if len(pos) != 3 {
			return errors.New(use)
		}
	case "hide", "show", "delete", "select":
		if len(pos) != 2 {
			return errors.New(use)
		}
	default:
		return errors.New(use)
	}
	mac, err := protocol.ParseMAC(pos[1])
	if err != nil {
		return err
	}
	var color protocol.RGB
	if len(pos) == 3 {
		if color, err = protocol.ParseRGB(pos[2]); err != nil {
			return err
		}
	}
	s, err := open(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()
	p, err := s.fetchPeer(mac, g.wait(30*time.Second))
	if err != nil {
		return err
	}
	upd := protocol.PeerUpdate{MAC: mac, Color: p.Color, Hidden: p.Hidden}
	switch pos[0] {
	case "color", "colour":
		upd.Color = color
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
	p, err = s.fetchPeer(mac, g.wait(30*time.Second))
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
		if len(pos) != 1 || name == "" || (lat == 0 && lon == 0) {
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
			poi.ID = randomPOIID()
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
		p, err := s.fetchPeer(poi.ID, g.wait(30*time.Second))
		if err != nil {
			return fmt.Errorf("sent, but the Totem did not list the POI: %w", err)
		}
		if g.out.json {
			g.out.emit(p)
		} else {
			g.out.printf("added %s\n", peerLine(p))
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

// randomPOIID returns a unicast, locally administered address, which no
// real Totem uses.
func randomPOIID() protocol.MAC {
	var m protocol.MAC
	_, _ = rand.Read(m[:])
	m[0] = m[0]&^0x01 | 0x02
	return m
}

func cmdLocation(ctx context.Context, g *globals, args []string) error {
	var acc float64
	pos, err := subflags("location", args, func(fs *flag.FlagSet) {
		fs.Float64Var(&acc, "acc", 10, "horizontal accuracy in meters")
	})
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 2, "totemctl location <lat> <lon> [-acc meters]"); err != nil {
		return err
	}
	lat, err1 := strconv.ParseFloat(pos[0], 32)
	lon, err2 := strconv.ParseFloat(pos[1], 32)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("bad coordinates: %w", err)
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("bad coordinates %v,%v: latitude must be within ±90 and longitude within ±180", lat, lon)
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
	case <-time.After(g.wait(20 * time.Second)):
		return errors.New("the Totem did not reboot; it refuses to update unless the battery is charged")
	case <-ctx.Done():
		return ctx.Err()
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
	if len(b) < 2 {
		return errors.New("a frame needs at least the (cat_id, cmd_id) bytes")
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

// oneShot sends a single frame and waits until the device has had a chance
// to act on it.
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
