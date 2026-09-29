//go:build tinygo && (esp32 || esp32s3)

// Command totememu turns an ESP32 into a Totem on the ESP-NOW mesh. It
// bonds with, and only talks to, the Totems listed in owned.
//
//	tinygo flash -target esp32-generic -ldflags "-X main.owned=8c94df7b0478" ./cmd/totememu
//
// Adding -X main.relayUnowned=1 also carries locate frames for Totems that
// are not in owned, as a real Totem does. See docs/reference/esp32-emulator.
//
// Commands on the serial console (115200 baud): pair, unbond <mac>,
// pos <lat> <lon> [accuracy m] | pos off, heading <deg>, sos on|off,
// status, log debug|info, format text|json, selftest, help.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"tinygo.org/x/espradio"

	"github.com/ljagiello/totem-compass/cmd/totememu/settings"
	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
)

// Console log settings: the level ("log debug|info") and whether lines
// are JSON for totemctl mesh ("format json|text").
var (
	level   slog.LevelVar
	jsonLog atomic.Bool
)

// Build-time settings (-ldflags "-X main.name=...").
var (
	owned = "8c94df7b0478" // comma-separated ESP-NOW MACs of the Totems to bond with
	name  = ""             // default emu_totem_<last four hex digits of the MAC>
	// relayUnowned set to "1" carries locate frames for Totems that are
	// not in owned, which is what a real Totem does. Off unless asked for,
	// and asked for at build time rather than from the console: turning it
	// on means this board transmits on behalf of devices its owner does
	// not own, and that should be a decision made while flashing, not a
	// keystroke away on a board sitting in someone else's crowd.
	relayUnowned = ""
	// bondingRSSI overrides the weakest bond request the board will accept,
	// in dBm, as a negative number: -X main.bondingRSSI=-45.
	//
	// The firmware's own limit is -25, and it is there so a Totem cannot
	// bond with a stranger across a room. This board has a stronger rule
	// already: it only bonds with the MACs in owned, so no signal level lets
	// a stranger in. What the limit costs here is a bench pairing, because
	// the hand that has to hold the Touch Crystal to start pairing absorbs
	// the Totem's own signal — measured on this board, locate frames from a
	// Totem sitting still arrive at -21 to -25 dBm and its bond requests,
	// sent while it is being held, at -33 to -36.
	//
	// Empty keeps the firmware's number, so a board flashed without thinking
	// about it behaves like a Totem.
	bondingRSSI = ""
)

// simWatcher is a sensor source that wants to know when the node has been
// switched to a simulation, because it is then not being read.
type simWatcher interface{ watchSim(func() bool) }

// bondingLimit reads the bondingRSSI build flag. Anything unparseable, or
// out of the range a signal strength can be, is reported and ignored rather
// than silently turning the limit off.
func bondingLimit(log *slog.Logger) int8 {
	if bondingRSSI == "" {
		return 0 // the node fills in the firmware's own -25
	}
	v, err := strconv.Atoi(bondingRSSI)
	if err != nil || v >= 0 || v < -100 {
		log.Warn("main.bondingRSSI is not a signal strength in dBm; keeping the firmware's limit",
			"value", bondingRSSI, "want", "a negative number no lower than -100")
		return 0
	}
	log.Warn("the bonding signal limit has been loosened from the firmware's -25",
		"dbm", v, "note", "only the MACs in owned can bond at any strength")
	return int8(v)
}

// magConsole is a sensor source with a compass the console can reach: read
// it, or run the calibration turn it needs before it can be believed.
type magConsole interface {
	magReport()
	calibrateMag(now func() time.Time)
}

// talkerSource is a sensor source backed by a real receiver, which can say
// which constellations it is solving from. Reported with the rest of the
// status rather than only at the first fix: a line logged once at boot is
// gone by the time anyone wonders, and this is the number that bounds how
// accurate every position the board sends can be. See gnss.Reader.Talker.
type talkerSource interface{ Talker() string }

// The battery a board reports when nothing measures one: a healthy cell,
// not a flat one. It is both what the node is configured with and what the
// board's sensor source falls back to, so the two cannot drift apart.
//
// A healthy reading on purpose. Zeroes would put the node into its low
// power mode and hold the OTA gate shut, and neither is true of a board
// sitting on a bench with no cell in it.
const (
	restingVolts = 4.1
	restingPct   = 95
)

func main() {
	time.Sleep(2 * time.Second)
	boot := time.Now()
	opts := &slog.HandlerOptions{
		Level: &level,
		// The board has no wall clock: log the uptime instead.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Duration("up", time.Since(boot).Round(time.Millisecond))
			}
			return a
		},
	}
	log := slog.New(newConsoleHandler(slog.NewTextHandler(os.Stdout, opts), slog.NewJSONHandler(os.Stdout, opts), &jsonLog))

	var allow []mesh.MAC
	for _, s := range strings.Split(owned, ",") {
		m, err := mesh.ParseMAC(s)
		if err != nil {
			fail(log, err)
		}
		allow = append(allow, m)
	}
	// The board's own parts, where it has any, before the radio rather than
	// after it. Nil on a board with none, and then the reading configured
	// below is what the node reports.
	//
	// Before, because this is slow: a quarter second of rail cycling, three
	// rail settles and a second spent listening to the GNSS receiver to see
	// whether it is there. Done after startRadio, all of that ran with
	// ESP-NOW already delivering into a sixteen-slot ring that nothing pops
	// until the main loop — so a pairing broadcast arriving in that window
	// was dropped. Nothing in the bring-up needs the radio.
	//
	// Moving it out of the emulator.Config literal was not enough on its
	// own, and the first attempt at this claimed otherwise: the literal is
	// evaluated in the same place relative to startRadio and the loop, so
	// the window was exactly as wide as before.
	sensors := newSensorSource(log, emulator.Sensors{
		Battery: emulator.Battery{Volts: restingVolts, Percent: restingPct},
	})
	mac, err := startRadio()
	if err != nil {
		fail(log, err)
	}
	if err := registerRX(); err != nil {
		fail(log, err)
	}
	radio := newRadio(log)
	for _, m := range append([]mesh.MAC{mesh.Broadcast}, allow...) {
		if err := radio.addPeer(m); err != nil {
			fail(log, err)
		}
	}

	// The settings sector is read before the node starts, so the name and
	// the colour it was given last are the ones it comes up with.
	// One call or the other, in a branch, because each of them says what
	// it did on the console and does the work to match. Assigning over
	// the other one has now been wrong in both directions: first Open
	// ran either way, taking the 4 KB read with the cache and interrupts
	// off that storeAtBoot exists to skip; then Unread ran either way,
	// logging "settings not read at boot" one line above "settings
	// restored".
	var saved *settings.Store
	if storeAtBoot {
		saved = settings.Open(log, settingsSector())
	} else {
		saved = settings.Unread(log)
	}
	// One copy: State() clones the peers it hands out, and this is the
	// window in which the board is also starting the radio and the ring.
	boot0 := saved.State()
	// len, not a string comparison: TinyGo 0.42 on xtensa gets == and !=
	// wrong on a string field of a copied struct, which is what boot0 is
	// — the same miscompile that made every `color <name>` fail on the
	// board. A device whose saved name silently did not come back would
	// look exactly like a device that had not saved one, and no host
	// test can see it, because this file only builds for the board.
	if len(boot0.Name) != 0 && name == "" {
		name = boot0.Name
	}
	// A separate instant for the node, because boot is what the console's
	// timestamps count from: rebasing it made every line logged during the
	// bring-up print an `up` of up to 1.5 s and then the next line print
	// 0.00s, so the board's only diagnostic channel ran backwards across
	// exactly the window that had just been made slow.
	nodeBoot := time.Now()
	node := emulator.New(emulator.Config{
		MAC: mac, Owned: allow, Name: name, AutoPair: true,
		BondingRSSI: bondingLimit(log),
		BattVolts:   restingVolts, BattPct: restingPct,
		Sensors: sensors,
		ColorID: boot0.ColorID, RelayUnowned: relayUnowned == "1",
		// The update client runs the firmware's exchange against a
		// transport that answers from memory: this board has no
		// credentials for a network, and nothing it does should depend on
		// having one.
		OTATransport: newLocalOTA(),
		Logger:       log, Rand: rand.New(rand.NewPCG(hwRandom(), hwRandom())),
	}, nodeBoot)
	// The compass needs to know when the node is reading a simulation rather
	// than this source, because a calibration turn would then collect
	// nothing. Set after New, which is when there is a node to ask.
	if w, ok := sensors.(simWatcher); ok {
		w.watchSim(node.Simulating)
	}
	saved.Restore(node, nodeBoot)
	saved.Save(node) // records this boot, and writes nothing if nothing changed
	log.Info("totem emulator ready", "mac", mac, "name", node.Config().Name, "owned", owned,
		"relay_unowned", relayUnowned == "1", "bonding_rssi", node.Config().BondingRSSI,
		"channel", mesh.Channel, "phy", "LR 250K", "tx_dbm", TxPowerDBm(), "boots", boot0.BootCount)
	log.Info("hold your Totem's button for 1.2 s next to this board to pair, or type help")

	front := newPanel(log)
	cmds := make(chan string, 4)
	go readLines(log, cmds)
	// bonds is what the settings on flash say. A bond gained or lost is
	// the only thing worth a write: an erase stalls the radio, and flash
	// wears out.
	bonds := node.BondCount()
	stats := time.NewTicker(time.Minute)
	// One timer for the life of the loop. The loop runs every 5 ms, so a
	// fresh timer per pass would put 200 allocations a second through a
	// heap of a few hundred kilobytes.
	timer := time.NewTimer(rxPoll)
	for {
		for {
			r, ok := popRX()
			if !ok {
				break
			}
			radio.received++
			radio.send(node.Receive(time.Now(), r))
		}
		now := time.Now()
		front.poll(node, now)
		radio.send(node.Poll(now))
		if n := node.BondCount(); n != bonds {
			bonds = n
			saved.Save(node)
		}
		// The receive ring is polled: the WiFi task must not call into Go.
		wait := rxPoll
		if next := node.Next(); !next.IsZero() {
			wait = min(max(next.Sub(now), 0), rxPoll)
		}
		if !timer.Stop() {
			// It fired while another branch was taken; drop that tick.
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case line := <-cmds:
			radio.send(command(log, node, saved, sensors, line))
			// A command is the usual way a setting changes — the colour,
			// the brightness, the name — so look for something to save
			// right after one. save writes nothing when nothing changed.
			saved.Save(node)
		case <-stats.C:
			radio.report()
			// And once a minute, for the settings a gesture changed: a
			// tap of the power button goes through no command at all.
			saved.Save(node)
		case <-timer.C:
		}
	}
}

// rxPoll is how often the main loop drains the receive ring.
const rxPoll = 5 * time.Millisecond

// radio moves frames between espradio and the node.
type radio struct {
	log   *slog.Logger
	peers map[mesh.MAC]bool
	// failed counts unicasts the radio did not see acknowledged. Against
	// a real Totem that is a burst while the two are pairing and then
	// nothing at all: zero across twenty-five minutes of steady traffic
	// from a boot that restored its bond, against a couple of hundred
	// while pairing windows were being opened over and over. A rising
	// count means the link is unhappy now, not that frames are being
	// lost — the peer acts on what it is sent either way.
	//
	// Atomic because the send handler below is a callback and nothing
	// here establishes which context runs it. Not a claim that it runs
	// on the WiFi task: rx.go explains that Go called from there raises
	// AllocaCause and is fatal under TinyGo's vector, which is why the
	// receive path is a C ring — so if this closure ran there the board
	// would not survive its first unicast, and it plainly does. The
	// atomic costs nothing and is the right shape for a counter written
	// from a callback and read from the loop; the counter is only ever
	// read for a log line, so nothing depends on it beyond being a
	// number rather than a torn one.
	failed   atomic.Int32
	received int
}

func newRadio(log *slog.Logger) *radio {
	r := &radio{log: log, peers: map[mesh.MAC]bool{}}
	espradio.ESPNowSetSendHandler(func(s espradio.ESPNowSendReport) {
		if s.Status != espradio.ESPNowSendSuccess && s.DestinationAddress != mesh.Broadcast {
			r.failed.Add(1)
		}
	})
	return r
}

func (r *radio) addPeer(m mesh.MAC) error {
	if r.peers[m] {
		return nil
	}
	if err := espradio.ESPNowAddPeer(espradio.ESPNowPeer{Address: m, If: espradio.WiFiInterfaceSTA}); err != nil {
		return fmt.Errorf("add ESP-NOW peer %s: %w", m, err)
	}
	r.peers[m] = true
	return nil
}

func (r *radio) send(ps []emulator.Packet) {
	for _, p := range ps {
		// "tx failed", not "tx": totemctl reads a "tx" line as a frame it
		// should decode, and hides it unless asked for. A board that
		// cannot transmit at all would have looked silent rather than
		// broken, and with --tx it printed "invalid frame" instead of
		// the radio's error.
		if err := r.addPeer(p.Dst); err != nil {
			r.log.Error("tx failed", "dst", p.Dst, "err", err)
			continue
		}
		if err := espradio.ESPNowSend((*[6]byte)(&p.Dst), p.Data); err != nil {
			r.log.Error("tx failed", "dst", p.Dst, "cat", p.Data[2], "cmd", p.Data[3], "err", err)
			continue
		}
		r.log.Debug("tx", "dst", p.Dst, "len", len(p.Data), "frame", fmt.Sprintf("%x", p.Data))
	}
}

func (r *radio) report() {
	r.log.Info("radio", "rx", r.received, "rx_lost", rxLost(), "unacked_unicasts", r.failed.Load())
}

// command runs one console line.
// sensors is the board's own source, passed in rather than taken from the
// node: the node's source is whatever is being read right now, which a
// running simulation replaces, while the compass belongs to the board
// whatever the node is reading.
func command(log *slog.Logger, n *emulator.Node, saved *settings.Store,
	sensors emulator.SensorSource, line string,
) []emulator.Packet {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	c, err := emulator.ParseCommand(line)
	if errors.Is(err, emulator.ErrUnknownCommand) {
		log.Warn("unknown command", "line", line)
		log.Info(emulator.Help)
		return nil
	}
	if err != nil {
		log.Warn("bad command", "err", err)
		return nil
	}
	now := time.Now()
	if out, handled, err := n.Apply(c, now); handled {
		switch {
		case err != nil:
			log.Warn("bad command", "err", err)
		default:
			// The message is the command's own name, except for rx:
			// totemctl reads a line whose msg is "rx" as a frame the
			// radio heard and tries to decode it, so this echo came out
			// as "invalid frame" — and `mesh send rx …` dropped its own
			// acknowledgement, which is why injecting one looked silent.
			msg := string(c.Op)
			if c.Op == emulator.OpRX {
				msg = "rx injected"
			}
			log.Info(msg, "cmd", c.String())
		}
		return out
	}
	switch c.Op {
	case emulator.OpStatus:
		cfg := n.Config()
		sense := n.Sensors()
		self := []any{"mac", cfg.MAC, "name", cfg.Name, "pairing", n.Pairing(), "sos", cfg.SOS,
			"heading", sense.Azimuth, "color", cfg.ColorID, "flat", sense.Orientation == mesh.OrientationHorizontal,
			"batt", sense.Battery.Percent, "volts", sense.Battery.Volts, "charging", sense.Battery.Charging}
		// Why those are zero, when they are. A board on USB with no cell
		// fitted reports nought percent at nought volts, which is exactly
		// what a flat pack reports, and the difference decides whether the
		// device is about to power itself down. Said only when it applies,
		// so an ordinary status line does not carry it — the same trap
		// has_position is spelled out for below.
		if sense.Battery.NoBattery {
			self = append(self, "no_battery", true)
		}
		// n.Fix(), not sense.Fix: the reading is whatever a receiver
		// reported, and a driver may report a NaN. This handler writes
		// JSON, which cannot hold one — a single such reading turns the
		// whole line into "lat":"!ERROR:json: unsupported value: NaN"
		// and totemctl then reads a string where it wants a number. The
		// node's own answer is the one every frame it sends uses.
		//
		// has_position says which it is, so a reader does not have to
		// take a pair of zeroes for a place: Null Island is how this
		// firmware says it has no fix.
		self = append(self, "has_position", n.Fix() != nil)
		if p := n.Fix(); p != nil {
			self = append(self, "lat", p.Lat, "lon", p.Lon, "acc", p.AccuracyM,
				"speed", p.SpeedKPH, "sats", p.SatCount, "odometer_m", p.OdometerM)
		}
		if t, ok := sensors.(talkerSource); ok {
			if id := t.Talker(); id != "" {
				self = append(self, "talker", id)
			}
		}
		log.Info("self", self...)
		for _, p := range n.Peers() {
			// A peer restored from the bond list has not been heard since
			// the board booted. -1 says so; the age since the zero time is
			// 56 years of milliseconds, which reads as nonsense.
			heard := int64(-1)
			if !p.LastHeard.IsZero() {
				heard = time.Since(p.LastHeard).Milliseconds()
			}
			log.Info("peer", "mac", p.MAC, "name", p.Status.Name, "rssi", p.RSSI,
				"heard_ms", heard, "mesh", p.ViaMesh,
				"lat", p.Lat, "lon", p.Lon, "has_position", p.HasPosition,
				"distance_m", int(p.DistanceM), "batt", p.Status.BattPct)
		}
	case emulator.OpFormat:
		jsonLog.Store(c.JSON)
		log.Info("log format", "format", map[bool]string{true: "json", false: "text"}[c.JSON])
	case emulator.OpLog:
		level.Set(c.Level)
		log.Info("log level", "set", level.Level())
	case emulator.OpSelfTest:
		lr, bgn, err := selfTest()
		log.Info("self test: 802.11 frames heard on the mesh channel in 3 s", "lr_only", lr, "with_bgn", bgn, "err", err)
	case emulator.OpHelp:
		log.Info(emulator.Help)
	case emulator.OpLEDs:
		log.Info("leds", "state", n.LEDs().Describe())
	case emulator.OpPower:
		log.Info("power", "state", n.Power().Describe(), "batt", n.Sensors().Battery.Percent,
			"volts", n.Sensors().Battery.Volts)
	case emulator.OpOTA:
		o := n.OTA()
		log.Info("ota", "state", o.State(), "detail", o.Describe())
	case emulator.OpFlash:
		flashSelfTest(log)
	case emulator.OpI2C:
		i2cScan(log)
	case emulator.OpMag:
		// The sensor source owns the compass, so the command goes to it.
		// A board without one — or a host build — finds nothing here and
		// says so rather than pretending.
		m, ok := sensors.(magConsole)
		switch {
		case !ok:
			log.Warn("no magnetometer on this build")
		case c.Sub == "calibrate":
			m.calibrateMag(time.Now)
		default:
			m.magReport()
		}
	case emulator.OpStore:
		switch c.Sub {
		case "forget":
			if err := saved.Forget(n, now); err != nil {
				log.Warn("settings could not be forgotten", "err", err)
			} else {
				log.Info("settings forgotten: the bonds are gone at the next boot")
			}
		case "open":
			saved.Reopen(n, now, settingsSector())
		default:
			saved.Report()
		}
	}
	return nil
}

// readLines reads console lines from the USB serial port.
func readLines(log *slog.Logger, out chan<- string) {
	var r emulator.LineReader
	for {
		b, ok := consoleByte()
		if !ok {
			// The smaller of the two receive FIFOs behind consoleByte holds
			// 64 bytes: the ESP32's UART holds 128, the S3's USB endpoint
			// 64, which is 5.5 ms of traffic at 115200 baud. Sleeping
			// longer than that loses the middle of a long line — an
			// injected frame is 200-odd characters.
			time.Sleep(2 * time.Millisecond)
			continue
		}
		switch line, err := r.Feed(b); {
		case err != nil:
			log.Warn("console", "err", err)
		case line != "":
			out <- line
		}
	}
}

// consoleByte and hwRandom live in chip_esp32.go and chip_esp32s3.go: the
// console reaches a different peripheral on each chip, and only one of the
// two has a random number generator TinyGo will drive for us.

func fail(log *slog.Logger, err error) {
	for {
		log.Error("fatal", "err", err)
		time.Sleep(5 * time.Second)
	}
}
