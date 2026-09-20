//go:build tinygo && esp32

// Command totememu turns an ESP32 into a Totem on the ESP-NOW mesh. It
// bonds with, and only talks to, the Totems listed in owned.
//
//	tinygo flash -target esp32-generic -ldflags "-X main.owned=8c94df7b0478" ./cmd/totememu
//
// Commands on the serial console (115200 baud): pair, unbond <mac>,
// pos <lat> <lon> [accuracy m] | pos off, heading <deg>, sos on|off,
// status, log debug|info, format text|json, selftest, help.
package main

import (
	"device/esp"
	"errors"
	"fmt"
	"log/slog"
	"machine"
	"math/rand/v2"
	"os"
	"runtime/volatile"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

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
	node := emulator.New(emulator.Config{
		MAC: mac, Owned: allow, Name: name, AutoPair: true, BattVolts: 4.1, BattPct: 95,
		ColorID: boot0.ColorID,
		// The update client runs the firmware's exchange against a
		// transport that answers from memory: this board has no
		// credentials for a network, and nothing it does should depend on
		// having one.
		OTATransport: newLocalOTA(),
		Logger:       log, Rand: rand.New(rand.NewPCG(hwRandom(), hwRandom())),
	}, boot)
	saved.Restore(node, boot)
	saved.Save(node) // records this boot, and writes nothing if nothing changed
	log.Info("totem emulator ready", "mac", mac, "name", node.Config().Name, "owned", owned,
		"channel", mesh.Channel, "phy", "LR 250K", "boots", boot0.BootCount)
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
			radio.send(command(log, node, saved, line))
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
	// nothing at all: 250 during the pairing window, and flat across the
	// ten minutes of steady traffic afterwards while rx climbed by sixty
	// a minute. A rising count means the link is unhappy now, not that
	// frames are being lost — the peer acts on what it is sent either
	// way — so it is worth watching rather than worth alarm.
	failed   int
	received int
}

func newRadio(log *slog.Logger) *radio {
	r := &radio{log: log, peers: map[mesh.MAC]bool{}}
	espradio.ESPNowSetSendHandler(func(s espradio.ESPNowSendReport) {
		if s.Status != espradio.ESPNowSendSuccess && s.DestinationAddress != mesh.Broadcast {
			r.failed++
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
	r.log.Info("radio", "rx", r.received, "rx_lost", rxLost(), "unacked_unicasts", r.failed)
}

// command runs one console line.
func command(log *slog.Logger, n *emulator.Node, saved *settings.Store, line string) []emulator.Packet {
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
		if p := sense.Fix; p != nil {
			self = append(self, "lat", p.Lat, "lon", p.Lon, "acc", p.AccuracyM,
				"speed", p.SpeedKPH, "sats", p.SatCount, "odometer_m", p.OdometerM)
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
			// The UART FIFO holds 128 bytes, which is 11 ms of traffic at
			// 115200 baud. Sleeping longer than that loses the middle of a
			// long line — an injected frame is 200-odd characters.
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

// consoleByte returns the next byte typed on the console. The UART receive
// interrupt stops firing once the WiFi blob runs, so the RX FIFO is polled
// as well, through its AHB address (UART0 base + 0x200C0000) as TinyGo's
// driver reads it to avoid an ESP32 silicon erratum.
func consoleByte() (byte, bool) {
	if b, err := machine.Serial.ReadByte(); err == nil {
		return b, true
	}
	if esp.UART0.GetSTATUS_RXFIFO_CNT() == 0 {
		return 0, false
	}
	return (*volatile.Register8)(unsafe.Add(unsafe.Pointer(esp.UART0), 0x200C0000)).Get(), true
}

// hwRandom reads RNG_DATA_REG, which gives true random numbers while the
// radio runs (ESP32 Technical Reference Manual, Random Number Generator
// chapter, register RNG_DATA_REG at 0x3FF75144).
func hwRandom() uint64 {
	reg := (*volatile.Register32)(unsafe.Pointer(uintptr(0x3ff75144)))
	return uint64(reg.Get())<<32 | uint64(reg.Get())
}

func fail(log *slog.Logger, err error) {
	for {
		log.Error("fatal", "err", err)
		time.Sleep(5 * time.Second)
	}
}
