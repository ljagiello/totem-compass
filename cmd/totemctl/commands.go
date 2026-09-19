package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

func newScanCmd(g *globals) *cobra.Command {
	var all, verbose bool
	var dur time.Duration
	c := &cobra.Command{
		Use:   "scan",
		Short: "List nearby Totems",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), dur)
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
		},
	}
	c.Flags().BoolVar(&all, "all", false, "list every BLE device, not just Totems")
	c.Flags().BoolVarP(&verbose, "verbose", "v", false, "also print advertised services and manufacturer data")
	c.Flags().DurationVar(&dur, "for", 10*time.Second, "scan duration")
	return c
}

func newInfoCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show device identity, settings, battery and position",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(cmd.Context(), g)
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
		},
	}
}

func newWatchCmd(g *globals) *cobra.Command {
	var dur time.Duration
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stream live data and peer updates until Ctrl-C",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(cmd.Context(), g)
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
		},
	}
	c.Flags().DurationVar(&dur, "for", 0, "stop after this long (default: until Ctrl-C)")
	return c
}

func newPeersCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "List bonded Totems and points of interest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(cmd.Context(), g)
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
		},
	}
}

func newNameCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "name <new name>",
		Short: "Rename the Totem",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.Join(args, " ")
			f, err := protocol.SetName(name)
			if err != nil {
				return err
			}
			s, err := open(cmd.Context(), g)
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
		},
	}
}

type compassChange struct {
	lock, north, blink *bool
	power              protocol.PowerMode
}

func newCompassCmd(g *globals) *cobra.Command {
	var lock, north, blink switchFlag
	var power powerFlag
	c := &cobra.Command{
		Use:     "compass",
		Short:   "Change compass settings",
		Example: "  totemctl compass --lock on --north off",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return applyCompass(cmd.Context(), g, compassChange{lock.val, north.val, blink.val, protocol.PowerMode(power)})
		},
	}
	c.Flags().Var(&lock, "lock", "compass lock")
	c.Flags().Var(&north, "north", "persistent north")
	c.Flags().Var(&blink, "blink", "blink peers on the ring")
	c.Flags().Var(&power, "power", "power mode")
	c.MarkFlagsOneRequired("lock", "north", "blink", "power")
	return c
}

func newPowerCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:       "power eco|normal",
		Short:     "Switch power mode (eco dims the LEDs)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"eco", "normal"},
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := parsePower(args[0])
			if err != nil {
				return err
			}
			return applyCompass(cmd.Context(), g, compassChange{power: p})
		},
	}
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
		g.log.Warn("the Totem does not report peer blink; it will be set to off (pass --blink on to keep it on)")
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

func newWiFiCmd(g *globals) *cobra.Command {
	scan := &cobra.Command{
		Use:   "scan",
		Short: "List the networks the Totem can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(cmd.Context(), g)
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
		},
	}
	set := &cobra.Command{
		Use:   "set <ssid> [password]",
		Short: "Save the network the Totem uses for firmware updates",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ssid, pass := args[0], ""
			if len(args) == 2 {
				pass = args[1]
			}
			f, err := protocol.SaveWiFi(ssid, pass)
			if err != nil {
				return err
			}
			s, err := open(cmd.Context(), g)
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.send(f); err != nil {
				return err
			}
			if _, err := s.fetchStatic(g.wait(30*time.Second), func(d protocol.StaticData) bool { return d.WiFiSSID == ssid }); err != nil {
				return fmt.Errorf("sent, but the Totem did not report the new network: %w", err)
			}
			g.out.status("saved WiFi network %q", ssid)
			return nil
		},
	}
	return group("wifi", "Scan for WiFi networks or save the one used for updates", scan, set)
}

func newPeerCmd(g *globals) *cobra.Command {
	color := &cobra.Command{
		Use:     "color <mac> <colour>",
		Aliases: []string{"colour"},
		Short:   "Change the peer's colour on the ring (palette name, #rrggbb or r,g,b)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mac, err := protocol.ParseMAC(args[0])
			if err != nil {
				return err
			}
			c, err := protocol.ParseRGB(args[1])
			if err != nil {
				return err
			}
			return updatePeer(cmd.Context(), g, mac, func(u *protocol.PeerUpdate) { u.Color = c })
		},
	}
	// byMAC builds a command that takes just the peer's MAC.
	byMAC := func(use, short string, change func(*protocol.PeerUpdate)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <mac>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				mac, err := protocol.ParseMAC(args[0])
				if err != nil {
					return err
				}
				return updatePeer(cmd.Context(), g, mac, change)
			},
		}
	}

	var stop bool
	sel := &cobra.Command{
		Use:   "select <mac>",
		Short: "Open the Totem's peer-management UI on a peer (--stop closes it)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 || stop == (len(args) == 1) {
				return errors.New("pass one peer MAC, or --stop")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if stop {
				return oneShot(cmd.Context(), g, protocol.StopPeerManagement(), "peer management closed")
			}
			mac, err := protocol.ParseMAC(args[0])
			if err != nil {
				return err
			}
			s, err := open(cmd.Context(), g)
			if err != nil {
				return err
			}
			defer s.close()
			p, err := s.fetchPeer(mac, g.wait(30*time.Second))
			if err != nil {
				return err
			}
			if err := s.send(protocol.SelectPeer(mac)); err != nil {
				return err
			}
			g.out.status("selected %q on the Totem", p.Name)
			return s.settle()
		},
	}
	sel.Flags().BoolVar(&stop, "stop", false, "close the peer-management UI")

	return group("peer", "Manage a bonded peer",
		color,
		byMAC("hide", "Hide the peer from the ring", func(u *protocol.PeerUpdate) { u.Hidden = true }),
		byMAC("show", "Show a hidden peer on the ring again", func(u *protocol.PeerUpdate) { u.Hidden = false }),
		byMAC("delete", "Forget the peer", func(u *protocol.PeerUpdate) { u.Delete = true }),
		sel,
	)
}

// updatePeer applies change to the peer's current ring entry and confirms
// the result.
func updatePeer(ctx context.Context, g *globals, mac protocol.MAC, change func(*protocol.PeerUpdate)) error {
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
	change(&upd)
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

func newPOICmd(g *globals) *cobra.Command {
	var name, color, id string
	var lat, lon float64
	var sticky bool
	add := &cobra.Command{
		Use:     "add",
		Short:   "Add a point of interest the compass can point to",
		Example: `  totemctl poi add --name "Main Stage" --lat 50.0671 --lon 19.9124 --sticky`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkCoords(lat, lon); err != nil {
				return err
			}
			poi := protocol.POI{Name: name, Lat: float32(lat), Lon: float32(lon), Sticky: sticky}
			var err error
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
			s, err := open(cmd.Context(), g)
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
		},
	}
	add.Flags().StringVar(&name, "name", "", "label shown in the app")
	add.Flags().Float64Var(&lat, "lat", 0, "latitude")
	add.Flags().Float64Var(&lon, "lon", 0, "longitude")
	add.Flags().StringVar(&color, "color", "white", "ring colour: palette name, #rrggbb or r,g,b")
	add.Flags().BoolVar(&sticky, "sticky", false, "keep pointing at this POI")
	add.Flags().StringVar(&id, "id", "", "6-byte id (default: random, locally administered)")
	for _, f := range []string{"name", "lat", "lon"} {
		_ = add.MarkFlagRequired(f) // the flags exist, so this cannot fail
	}

	del := &cobra.Command{
		Use:   "delete <id>",
		Short: "Remove a point of interest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mac, err := protocol.ParseMAC(args[0])
			if err != nil {
				return err
			}
			return oneShot(cmd.Context(), g, protocol.UpdatePeer(protocol.PeerUpdate{MAC: mac, Delete: true}), "deleted "+mac.String())
		},
	}
	return group("poi", "Add or remove a point of interest to navigate to", add, del)
}

// randomPOIID returns a unicast, locally administered address, which no
// real Totem uses.
func randomPOIID() protocol.MAC {
	var m protocol.MAC
	_, _ = rand.Read(m[:])
	m[0] = m[0]&^0x01 | 0x02
	return m
}

func checkCoords(lat, lon float64) error {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("bad coordinates %v,%v: latitude must be within ±90 and longitude within ±180", lat, lon)
	}
	return nil
}

func newLocationCmd(g *globals) *cobra.Command {
	var lat, lon, acc float64
	c := &cobra.Command{
		Use:     "location",
		Short:   "Send a phone GNSS fix and the time (used when the Totem has no fix)",
		Example: "  totemctl location --lat 37.3349 --lon -122.009",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkCoords(lat, lon); err != nil {
				return err
			}
			now := time.Now()
			f := protocol.SendPhoneFix(protocol.PhoneFix{
				Lat: float32(lat), Lon: float32(lon), HAcc: acc,
				Unix: int32(now.Unix()), UnixMS: int16(now.Nanosecond() / 1e6), Focused: true,
			})
			return oneShot(cmd.Context(), g, f, fmt.Sprintf("sent fix %.6f,%.6f ±%.0fm", lat, lon, acc))
		},
	}
	c.Flags().Float64Var(&lat, "lat", 0, "latitude")
	c.Flags().Float64Var(&lon, "lon", 0, "longitude")
	c.Flags().Float64Var(&acc, "acc", 10, "horizontal accuracy in meters")
	for _, f := range []string{"lat", "lon"} {
		_ = c.MarkFlagRequired(f) // the flags exist, so this cannot fail
	}
	return c
}

func newOTACmd(g *globals) *cobra.Command {
	var yes bool
	req := protocol.OTARequest{Cmd: 1, EndpointID: 1}
	c := &cobra.Command{
		Use:   "ota",
		Short: "Reboot the Totem into a WiFi firmware update",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("this reboots the Totem and updates it over its saved WiFi network (see `wifi set`); it needs a charged battery. Re-run with --yes")
			}
			f, err := protocol.StartOTA(req)
			if err != nil {
				return err
			}
			s, err := open(cmd.Context(), g)
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
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "really reboot into the updater")
	c.Flags().StringVar(&req.Version, "version", "latest", "release code to install")
	c.Flags().StringVar(&req.Branch, "branch", "totem", "release branch")
	return c
}

func newRawCmd(g *globals) *cobra.Command {
	var conn bool
	var listen time.Duration
	c := &cobra.Command{
		Use:   "raw <hex>",
		Short: "Send a raw frame and print what comes back",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := hex.DecodeString(strings.NewReplacer(" ", "", ":", "").Replace(strings.Join(args, "")))
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
			s, err := open(cmd.Context(), g)
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
		},
	}
	c.Flags().BoolVar(&conn, "conn", false, "write to the conn-status characteristic instead of data")
	c.Flags().DurationVar(&listen, "listen", 10*time.Second, "how long to print replies")
	return c
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
