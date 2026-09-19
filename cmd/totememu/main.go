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

	node := emulator.New(emulator.Config{
		MAC: mac, Owned: allow, Name: name, AutoPair: true, BattVolts: 4.1, BattPct: 95,
		Logger: log, Rand: rand.New(rand.NewPCG(hwRandom(), hwRandom())),
	}, time.Now())
	log.Info("totem emulator ready", "mac", mac, "name", node.Config().Name, "owned", owned,
		"channel", mesh.Channel, "phy", "LR 250K")
	log.Info("hold your Totem's button for 1.2 s next to this board to pair, or type help")

	cmds := make(chan string, 4)
	go readLines(cmds)
	stats := time.NewTicker(time.Minute)
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
		radio.send(node.Poll(now))
		// The receive ring is polled: the WiFi task must not call into Go.
		wait := rxPoll
		if next := node.Next(); !next.IsZero() {
			wait = min(max(next.Sub(now), 0), rxPoll)
		}
		timer := time.NewTimer(wait)
		select {
		case line := <-cmds:
			radio.send(command(log, node, line))
		case <-stats.C:
			radio.report()
		case <-timer.C:
		}
		timer.Stop()
	}
}

// rxPoll is how often the main loop drains the receive ring.
const rxPoll = 5 * time.Millisecond

// radio moves frames between espradio and the node.
type radio struct {
	log      *slog.Logger
	peers    map[mesh.MAC]bool
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
		if err := r.addPeer(p.Dst); err != nil {
			r.log.Error("tx", "err", err)
			continue
		}
		if err := espradio.ESPNowSend((*[6]byte)(&p.Dst), p.Data); err != nil {
			r.log.Error("tx", "dst", p.Dst, "cat", p.Data[2], "cmd", p.Data[3], "err", err)
			continue
		}
		r.log.Debug("tx", "dst", p.Dst, "len", len(p.Data), "frame", fmt.Sprintf("%x", p.Data))
	}
}

func (r *radio) report() {
	r.log.Info("radio", "rx", r.received, "rx_lost", rxLost(), "unacked_unicasts", r.failed)
}

// command runs one console line.
func command(log *slog.Logger, n *emulator.Node, line string) []emulator.Packet {
	f := strings.Fields(line)
	if len(f) == 0 {
		return nil
	}
	now := time.Now()
	switch f[0] {
	case "pair":
		return n.Pair(now)
	case "unbond":
		if len(f) != 2 {
			break
		}
		m, err := mesh.ParseMAC(f[1])
		if err != nil {
			log.Error("unbond", "err", err)
			return nil
		}
		return n.Unbond(now, m)
	case "pos":
		if len(f) == 2 && f[1] == "off" {
			n.SetPosition(nil)
			log.Info("position cleared")
			return nil
		}
		var p emulator.Position
		p.AccuracyM = 5
		if len(f) < 3 {
			break
		}
		if _, err := fmt.Sscan(f[1], &p.Lat); err != nil {
			break
		}
		if _, err := fmt.Sscan(f[2], &p.Lon); err != nil {
			break
		}
		if len(f) > 3 {
			fmt.Sscan(f[3], &p.AccuracyM)
		}
		n.SetPosition(&p)
		log.Info("position set", "lat", p.Lat, "lon", p.Lon, "acc", p.AccuracyM)
		return nil
	case "heading":
		var d int16
		if len(f) == 2 {
			if _, err := fmt.Sscan(f[1], &d); err == nil {
				n.SetHeading(d)
				log.Info("heading set", "deg", d)
				return nil
			}
		}
	case "sos":
		if len(f) == 2 && (f[1] == "on" || f[1] == "off") {
			n.SetSOS(f[1] == "on")
			log.Info("sos", "on", f[1] == "on")
			return nil
		}
	case "status":
		c := n.Config()
		self := []any{"mac", c.MAC, "name", c.Name, "pairing", n.Pairing(), "sos", c.SOS, "heading", c.Heading, "color", c.ColorID}
		if p := c.Position; p != nil {
			self = append(self, "lat", p.Lat, "lon", p.Lon, "acc", p.AccuracyM)
		}
		log.Info("self", self...)
		for _, p := range n.Peers() {
			log.Info("peer", "mac", p.MAC, "name", p.Status.Name, "rssi", p.RSSI,
				"heard_ms", time.Since(p.LastHeard).Milliseconds(), "mesh", p.ViaMesh,
				"lat", p.Status.Lat, "lon", p.Status.Lon, "distance_m", int(p.DistanceM), "batt", p.Status.BattPct)
		}
		return nil
	case "format":
		if len(f) == 2 && (f[1] == "json" || f[1] == "text") {
			jsonLog.Store(f[1] == "json")
			log.Info("log format", "format", f[1])
			return nil
		}
	case "selftest":
		lr, bgn, err := selfTest()
		log.Info("self test: 802.11 frames heard on the mesh channel in 3 s", "lr_only", lr, "with_bgn", bgn, "err", err)
		return nil
	case "log":
		if len(f) == 2 {
			if err := level.UnmarshalText([]byte(f[1])); err == nil {
				log.Info("log level", "set", level.Level())
				return nil
			}
		}
	case "help":
	default:
		log.Warn("unknown command", "line", line)
	}
	log.Info("commands: pair | unbond <mac> | pos <lat> <lon> [acc] | pos off | heading <deg> | sos on|off | status | log debug|info | format text|json | selftest")
	return nil
}

// readLines reads console lines from the USB serial port.
func readLines(out chan<- string) {
	var line []byte
	for {
		b, ok := consoleByte()
		if !ok {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		switch {
		case b == '\r' || b == '\n':
			if len(line) > 0 {
				out <- string(line)
				line = line[:0]
			}
		case b >= ' ' && b <= '~' && len(line) < 128:
			// Only printable ASCII: a glitch while the host opens the port
			// must not corrupt the next command.
			line = append(line, b)
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
