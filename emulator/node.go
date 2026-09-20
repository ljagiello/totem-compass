// Package emulator is a Totem on the ESP-NOW mesh: it bonds, sends status
// in the firmware's radio windows, answers and relays mesh locate
// requests and joins Smart Groups, the way firmware 5.0.3 does
// (espnow_conn_v2, compass, peer_management).
//
// The package is pure logic: it takes received frames and the time, and
// returns the frames to send. A radio driver (cmd/totememu on an ESP32)
// moves the bytes.
//
// The node only talks to the Totems in Config.Owned. Frames from any other
// sender are dropped unread, it never relays a frame that another Totem
// originated, it never hosts a Smart Group and it never sends demi-god
// commands.
package emulator

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// Firmware constants (project_data, compass, espnow_conn_v2).
const (
	BondingRSSI    = -25             // BONDING_RSSI: weakest bond request accepted
	pairingWindow  = 6 * time.Second // start_pairing timeout_sec
	maxBonds       = 8
	alreadyBonded  = 6 * time.Second // "Both devices already bonded" if heard this recently
	ackBurstCount  = 5
	ackBurstGap    = 80 * time.Millisecond
	joinGap        = 200 * time.Millisecond
	rtcSyncDelay   = 30 * time.Second // rtc_no_peer_sync_until
	windowTXDelay  = 75 * time.Millisecond
	meshReplyHold  = 60 * time.Second
	meshRepeatMax  = 10 * time.Second
	meshSendFreq   = 30 * time.Second // MESH_SEND_FREQ_MS
	meshPeerLimit  = 999              // MESH_PEER_MSG_LIMIT
	meshStaleHeard = 15 * time.Second
	meshAckLimit   = 20
	meshHopRange   = 75 // MESH_MIN_HOP_RANGE, meters
	farPeerM       = 30 // a second status copy when the furthest peer is further
	noSatPeriod    = 5 * time.Second
	noSatOnMs      = 2500
	noSatBursts    = 3
	dedupeMin      = 15 * time.Second
	dedupeMax      = 400 * time.Second
	recentMax      = 512
)

// Config describes the emulated Totem.
type Config struct {
	MAC   mesh.MAC   // the ESP-NOW (station) address of this device
	Owned []mesh.MAC // the only Totems this node listens and talks to
	Name  string     // default DefaultName(MAC)
	// Sensors reads the GNSS receiver, magnetometer, motion sensor and
	// power chip. Without one the node reports the fixed Position, Heading
	// and battery below, as a board with no sensors does.
	Sensors     SensorSource
	Position    *Position
	Heading     int16 // compass azimuth, degrees
	BattVolts   float32
	BattPct     int8
	Version     [3]uint8
	ReleaseID   uint16
	ColorID     int8
	SOS         bool
	Orientation mesh.Orientation
	GNSSSource  mesh.GNSSSource
	AutoPair    bool // start pairing when an owned Totem pairs right next to us
	// PhoneConnected is what a peer frame reports about a phone being
	// attached over BLE; the power button's double tap toggles it.
	PhoneConnected bool
	// OTATransport reaches the update server. A board with no network
	// leaves it nil, and an update then says so instead of hanging.
	OTATransport OTATransport
	Logger       *slog.Logger
	Rand         *rand.Rand
	BondingRSSI  int8 // default BondingRSSI
	SmartGrpRSSI int8 // default mesh.SmartGroupRSSI
}

// Received is one ESP-NOW frame as the radio delivered it.
type Received struct {
	Src, Dst mesh.MAC
	RSSI     int8
	Data     []byte
}

// Packet is a frame to send. Dst is a peer or mesh.Broadcast.
type Packet struct {
	Dst  mesh.MAC
	Data []byte
}

// PeerInfo is what the node knows about a bonded peer.
type PeerInfo struct {
	MAC       mesh.MAC
	Status    mesh.Peer // last status frame
	RSSI      int8
	LastHeard time.Time
	ViaMesh   bool
	DistanceM float64 // -1 without both positions
}

type peer struct {
	mac        mesh.MAC
	status     mesh.Peer
	heard      bool
	lastHeard  time.Time // any direct frame (peers_table[mac][1])
	rssi       int8
	hasCoords  bool
	lat, lon   float32
	coordsAt   time.Time
	viaMesh    bool
	meshGrp    int
	meshNext   time.Time
	meshCount  int
	firstStale time.Time
	stale      bool
	// color is the colour this peer is shown in, drawn as
	// shuffle_bond_colors draws one when Totems bond.
	color Color
}

type jobKind uint8

const (
	jobOnce jobKind = iota
	jobWindow
	jobMeshTick
)

type job struct {
	at   time.Time
	seq  int
	kind jobKind
	run  func(now time.Time)
}

// Node is one emulated Totem.
type Node struct {
	cfg  Config
	log  *slog.Logger
	rng  *rand.Rand
	boot time.Time

	peers map[mesh.MAC]*peer
	order []mesh.MAC // bond order

	// clock is the device's own (rtc_method 1, from GNSS) or a peer's
	// (rtc_method 2): wall = local + clockOffset.
	clockSet    bool
	gnssClock   bool
	clockOffset time.Duration

	source  SensorSource
	sensors Sensors
	// sensorsAt is when the sensors were last read: the node's idea of
	// now for the parts that no frame or timer drives.
	sensorsAt time.Time

	pairing  bool
	pairEnd  time.Time
	bondMAC  *mesh.MAC
	tempBond *mesh.MAC
	bonding  bool // modes.bonding_start

	smartUID   uint16
	inGroup    bool
	groupUntil time.Time

	outbox     map[mesh.MAC][]byte
	lastTX     time.Time
	meshGrp    int
	originUID  uint16
	recent     map[uint16]time.Time
	replyUID   uint16
	replyAt    time.Time
	lastReply  time.Time
	meshAcks   int
	noSatPhase int

	jobs []job
	seq  int
	out  []Packet

	// The parts of a Totem that are not the radio: the three inputs, the
	// ring and crystal, the power state and an update in flight.
	inputs []recogniser
	leds   *LEDs
	power  *Power
	ota    *OTA
	// sosMuted is is_sos_mute: the alarm still goes out, the device just
	// stops blinking about it.
	sosMuted bool
}

// New starts a node at time now.
func New(cfg Config, now time.Time) *Node {
	if cfg.Name == "" {
		cfg.Name = DefaultName(cfg.MAC)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if short := store.SanitizeName(cfg.Name); short != cfg.Name {
		// Every status frame carries the name, so one the frame cannot
		// hold would fail at the point of sending it, and one that is not
		// text would travel on into whatever reads it.
		cfg.Logger.Warn("name cut to what a peer frame holds",
			"name", cfg.Name, "bytes", len(cfg.Name), "max", mesh.MaxPeerName, "using", short)
		cfg.Name = short
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(uint64(now.UnixNano()), binary.BigEndian.Uint64(append([]byte{0, 0}, cfg.MAC[:]...))))
	}
	if cfg.BondingRSSI == 0 {
		cfg.BondingRSSI = BondingRSSI
	}
	if cfg.SmartGrpRSSI == 0 {
		cfg.SmartGrpRSSI = mesh.SmartGroupRSSI
	}
	if cfg.Version == ([3]uint8{}) {
		cfg.Version = [3]uint8{5, 0, 3}
		cfg.ReleaseID = 339
	}
	if cfg.Orientation == mesh.OrientationUnknown {
		cfg.Orientation = mesh.OrientationVertical
	}
	if cfg.GNSSSource == 0 {
		cfg.GNSSSource = mesh.GNSSDevice
	}
	n := &Node{
		cfg: cfg, log: cfg.Logger, rng: cfg.Rand, boot: now,
		peers: map[mesh.MAC]*peer{}, outbox: map[mesh.MAC][]byte{}, recent: map[uint16]time.Time{},
		meshGrp: meshGroup(cfg.MAC),
	}
	n.source = cfg.Sensors
	if n.source == nil {
		n.source = &staticSensors{s: Sensors{
			Fix: cfg.Position, Orientation: cfg.Orientation, Azimuth: cfg.Heading,
			Battery: Battery{Volts: cfg.BattVolts, Percent: cfg.BattPct},
		}}
	}
	n.inputs = newInputs(now)
	n.leds = newLEDs(now, Color(cfg.ColorID))
	n.power = newPower(now)
	n.ota = newOTA()
	n.read(now)
	// read may already have scheduled one: a sensor source that arrives
	// with a clock takes the GNSS path, which aligns the windows itself.
	// A second one here would leave two window jobs rescheduling each
	// other, and every peer would see every status twice.
	if !n.hasJob(jobWindow) {
		n.scheduleWindow(now)
	}
	return n
}

// hasJob reports whether a job of this kind is already scheduled.
func (n *Node) hasJob(k jobKind) bool {
	return slices.ContainsFunc(n.jobs, func(j job) bool { return j.kind == k })
}

// DefaultName is "emu_totem_" and the last four hex digits of the MAC.
func DefaultName(m mesh.MAC) string {
	return fmt.Sprintf("emu_totem_%02x%02x", m[4], m[5])
}

// meshGroup is peer_helpers.get_mesh_group: the device's 1-of-5 mesh slot,
// int.from_bytes(sha256(mac).digest()[:4], 'big') % 5.
func meshGroup(m mesh.MAC) int {
	h := sha256.Sum256(m[:])
	return int(binary.BigEndian.Uint32(h[:4]) % 5)
}

// Next is when Poll next has work.
func (n *Node) Next() time.Time {
	var next time.Time
	if len(n.jobs) > 0 {
		next = slices.MinFunc(n.jobs, cmpJob).at
	}
	// The LEDs and the inputs need polling too: a frame to draw, or a
	// press that has lasted long enough to count as a hold. A driver
	// that woke only for radio work would report a gesture late, or not
	// at all on a device with nothing else scheduled.
	if t := n.leds.Next(); !t.IsZero() && (next.IsZero() || t.Before(next)) {
		next = t
	}
	for i := range n.inputs {
		if t := n.inputs[i].next(); !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	return next
}

func cmpJob(a, b job) int {
	if c := a.at.Compare(b.at); c != 0 {
		return c
	}
	return a.seq - b.seq
}

// Poll runs every timer due at now and returns the frames to send.
func (n *Node) Poll(now time.Time) []Packet {
	n.read(now)
	n.device(now)
	for {
		i := -1
		for j, jb := range n.jobs {
			if !jb.at.After(now) && (i < 0 || cmpJob(jb, n.jobs[i]) < 0) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		jb := n.jobs[i]
		n.jobs = slices.Delete(n.jobs, i, i+1)
		jb.run(now)
	}
	return n.flush()
}

func (n *Node) at(t time.Time, run func(time.Time)) { n.atKind(t, jobOnce, run) }

func (n *Node) atKind(t time.Time, kind jobKind, run func(time.Time)) {
	n.seq++
	n.jobs = append(n.jobs, job{t, n.seq, kind, run})
}

func (n *Node) flush() []Packet {
	out := n.out
	n.out = nil
	return out
}

// send queues a frame. It is the one place frames leave the node, and it
// enforces the safety scope.
func (n *Node) send(dst mesh.MAC, m mesh.Message) {
	b, err := m.MarshalBinary()
	if err != nil {
		n.log.Error("encode", "err", err)
		return
	}
	n.sendRaw(dst, b)
}

func (n *Node) sendRaw(dst mesh.MAC, b []byte) {
	if !allowedTX(dst, b, n.cfg.Owned) {
		n.log.Error("refusing to send outside the owned scope", "dst", dst, "frame", fmt.Sprintf("%x", b))
		return
	}
	n.out = append(n.out, Packet{dst, b})
}

// allowedTX: unicast only to owned Totems, never a demi-god command and
// never a Smart Group host beacon.
func allowedTX(dst mesh.MAC, b []byte, owned []mesh.MAC) bool {
	if len(b) < 4 {
		return false
	}
	if dst != mesh.Broadcast && !slices.Contains(owned, dst) {
		return false
	}
	switch cat, cmd := b[2], b[3]; cat {
	case mesh.CatPeer, mesh.CatMesh:
		return true
	case mesh.CatSmartGroup:
		return cmd == 1
	}
	return false
}

// Receive handles one frame and returns the frames to send right away.
func (n *Node) Receive(now time.Time, rx Received) []Packet {
	if !slices.Contains(n.cfg.Owned, rx.Src) {
		return nil
	}
	if n.power.Off() {
		// A device that has powered down does not answer. Its radio is
		// off, so a peer hears nothing at all from it.
		return nil
	}
	n.read(now)
	n.log.Debug("rx", "src", rx.Src, "dst", rx.Dst, "rssi", rx.RSSI, "len", len(rx.Data), "frame", fmt.Sprintf("%x", rx.Data))
	m, err := mesh.Parse(rx.Data)
	if err != nil {
		n.log.Debug("unparsed frame", "src", rx.Src, "err", err)
		return nil
	}
	switch m := m.(type) {
	case mesh.Peer:
		n.onPeer(now, rx, m)
	case mesh.Locate:
		n.onLocate(now, rx, m)
	case mesh.SmartGroup:
		n.onSmartGroup(now, rx, m)
	case mesh.SmartGroupReply:
		// Only a host acts on replies, and this node never hosts.
	case mesh.DemiGod:
		// The command is not acted on, but the ring says one landed, as
		// demi_god_blink does on a Totem.
		n.leds.Play(AnimDemiGod, now)
		n.log.Info("demi-god command ignored", "src", rx.Src, "cmd", m.Command)
	default:
		n.log.Debug("unknown frame", "src", rx.Src, "frame", fmt.Sprintf("%x", rx.Data))
	}
	return n.flush()
}

// ---- sensors and time ------------------------------------------------------

// read takes a new sensor reading. A GNSS fix carries the time, which the
// firmware treats as its own clock (rtc_method 1) and advertises to peers.
// minClock is the earliest time the node treats as a real clock. A board
// with no wall clock counts from 1970, and advertising that to a peer that
// has no clock of its own would set its RTC to 1970 too.
var minClock = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func (n *Node) read(now time.Time) {
	n.sensors, n.sensorsAt = n.source.Read(now), now
	if f := n.sensors.Fix; f != nil && f.Time.After(minClock) {
		n.clockOffset = f.Time.Sub(now)
		if !n.gnssClock {
			n.gnssClock = true
			n.log.Info("clock from GNSS", "wall", f.Time.UTC().Format(time.RFC3339Nano))
			n.startAligned(now)
		}
		n.clockSet = true
	}
}

// fix is the current GNSS solution, or nil.
func (n *Node) fix() *Fix { return n.sensors.Fix }

func (n *Node) wall(now time.Time) time.Time { return now.Add(n.clockOffset) }

// startAligned moves the radio windows and the mesh tick onto wall-clock
// slots, once the node has a clock.
func (n *Node) startAligned(now time.Time) {
	n.jobs = slices.DeleteFunc(n.jobs, func(j job) bool { return j.kind != jobOnce })
	n.scheduleWindow(now)
	n.scheduleMeshTick(now)
}

// adoptClock is the RTC block of Parser._peer: a Totem without a clock
// takes the time from a peer whose clock came from GNSS.
func (n *Node) adoptClock(now time.Time, p mesh.Peer) {
	if n.clockSet || n.gnssClock || p.TimeOfDayMs <= 0 || p.Unix <= 0 || now.Sub(n.boot) < rtcSyncDelay {
		return
	}
	wall := time.UnixMilli(int64(p.Unix)*1000 + int64(p.TimeOfDayMs%1000))
	n.clockOffset = wall.Sub(now)
	n.clockSet = true
	n.log.Info("RTC set via peer", "wall", wall.UTC().Format(time.RFC3339Nano))
	n.startAligned(now)
}

// scheduleMeshTick arms the next one_sec_mesh_coro wake-up, 200 ms before
// the next 4 s wall-clock boundary.
func (n *Node) scheduleMeshTick(now time.Time) {
	w := n.wall(now).UnixMilli()
	next := now.Add(time.Duration(4000-w%4000)*time.Millisecond - 200*time.Millisecond)
	if !next.After(now) {
		next = next.Add(4 * time.Second)
	}
	n.atKind(next, jobMeshTick, n.meshTick)
}

// ---- radio windows --------------------------------------------------------

// period is COMM_SPEED[speed_id] for an upright Totem: speed 1 (4 s) with
// a clock, speed 8 (1 s) when a peer lies flat, and speed 6 (5 s windows,
// not aligned) without a clock or satellites.
func (n *Node) period() time.Duration {
	if !n.clockSet {
		return noSatPeriod
	}
	if n.sensors.Orientation == mesh.OrientationHorizontal {
		return time.Second
	}
	for _, p := range n.peers {
		if p.status.Orientation == mesh.OrientationHorizontal {
			return time.Second
		}
	}
	return 4 * time.Second
}

// scheduleWindow arms the next radio window (EspConn.communicate_v2 and
// radio_timer): wall-clock aligned, transmitting 75 ms after the slot.
func (n *Node) scheduleWindow(now time.Time) {
	period := n.period()
	var start time.Time
	if n.clockSet {
		w := n.wall(now).UnixMilli()
		p := period.Milliseconds()
		start = now.Add(time.Duration(p-w%p) * time.Millisecond)
	} else {
		start = n.lastTX.Add(noSatPeriod)
		if start.Before(now) {
			start = now
		}
	}
	n.atKind(start.Add(windowTXDelay), jobWindow, n.window1)
}

func (n *Node) window1(now time.Time) {
	n.meshAcks = 0
	if n.inGroup && now.After(n.groupUntil) {
		// Parser.auto_bond_client_timeout
		n.log.Info("smart group timed out", "uid", n.smartUID)
		n.inGroup, n.smartUID = false, 0
	}
	if n.clockSet {
		n.sendOutbox(now)
		n.lastTX = now
		n.scheduleWindow(now)
		return
	}
	// Speed 6: up to three bursts at rotating sub-second phases within
	// 2.5 s of the window start (EspConn.cycle_ms_after_sec).
	start := now
	n.lastTX = now
	var burst func(time.Time)
	count := 0
	burst = func(t time.Time) {
		n.sendOutbox(t)
		count++
		if count >= noSatBursts {
			return
		}
		n.noSatPhase = (n.noSatPhase + 1) % 10
		off := time.Duration(900-100*n.noSatPhase) * time.Millisecond
		ms := time.Duration(t.Sub(n.boot).Milliseconds()%1000) * time.Millisecond
		wait := off - ms
		if wait <= 0 {
			wait += time.Second
		}
		if t.Add(wait).Sub(start) <= noSatOnMs*time.Millisecond {
			n.atKind(t.Add(wait), jobWindow, burst)
		}
	}
	burst(now)
	n.atKind(start.Add(noSatPeriod), jobWindow, n.window1)
}

// sendOutbox is EspConn._send_outbox: status to every bonded peer, a second
// copy when a peer is far, then the queued broadcasts. Nothing goes out
// while pairing: the bonding UI clears modes.evt_bonded, which ping_peers
// waits on.
func (n *Node) sendOutbox(now time.Time) {
	if n.pairing {
		return
	}
	if len(n.order) > 0 {
		st := n.status(now, mesh.PeerStatus, false)
		for _, mac := range n.order {
			n.send(mac, st)
		}
		if n.furthestPeer() > farPeerM {
			for _, mac := range n.order {
				n.send(mac, st)
			}
		}
	}
	for dst, b := range n.outbox {
		n.sendRaw(dst, b)
		delete(n.outbox, dst)
	}
	// One repeat of our last locate reply, same UID, in a window whose
	// second is a multiple of 4 and within 10 s of the reply.
	if !n.replyAt.IsZero() && n.clockSet && n.wall(now).Second()%4 == 0 {
		if now.Sub(n.replyAt) < meshRepeatMax {
			n.send(mesh.Broadcast, n.locate(now, false, n.replyUID))
		}
		n.replyAt = time.Time{}
	} else if !n.replyAt.IsZero() && now.Sub(n.replyAt) >= meshRepeatMax {
		n.replyAt = time.Time{}
	}
}

// status is Messages.gen_peer_msg.
func (n *Node) status(now time.Time, cmd mesh.PeerCommand, ack bool) mesh.Peer {
	c, sense := n.cfg, n.sensors
	p := mesh.Peer{
		Command: cmd, PosAccuracyM: -1, SpeedKPH: -1, Azimuth: sense.Azimuth, SOS: c.SOS,
		Orientation: sense.Orientation, Ack: ack,
		// Time goes out only with the device's own GNSS clock
		// (rtc_method 1); a clock borrowed from a peer is not passed on.
		TimeOfDayMs: -1, Unix: -1,
		Major: c.Version[0], Minor: c.Version[1], Patch: c.Version[2],
		AltitudeM: -500, UptimeMin: uint16(now.Sub(n.boot) / time.Minute), BattVolts: sense.Battery.Volts,
		HeadingOfMotion: -1, Name: c.Name, GNSSSource: c.GNSSSource, ReleaseID: c.ReleaseID,
		BattPct: sense.Battery.Percent,
		// Flags bit 0: whether a phone is attached over BLE, which the
		// power button's double tap toggles. Leaving it out meant no peer
		// ever saw the flag change.
		PhoneConnected: c.PhoneConnected,
	}
	if f := sense.Fix; f != nil {
		p.Lat, p.Lon, p.PosAccuracyM = f.Lat, f.Lon, f.AccuracyM
		p.SpeedKPH, p.AltitudeM, p.SolutionID = f.SpeedKPH, f.AltitudeM, f.SolutionID
		p.HeadingOfMotion = f.HeadingOfMotion
		p.OdometerM = int16(min(f.OdometerM, math.MaxInt16))
	}
	if n.gnssClock {
		w := n.wall(now).UTC()
		p.Unix = int32(w.Unix())
		p.TimeOfDayMs = int32(w.Hour()*3600000 + w.Minute()*60000 + w.Second()*1000 + w.Nanosecond()/1e6)
	}
	return p
}

// ---- pairing ----------------------------------------------------------------

// Pair starts pairing, as holding a Totem's button for 1.2 s does
// (Compass.start_pairing): for six seconds the node broadcasts bond
// requests and bonds with an owned Totem doing the same right next to it.
func (n *Node) Pair(now time.Time) []Packet {
	n.startPairing(now)
	return n.flush()
}

func (n *Node) startPairing(now time.Time) {
	switch {
	case n.pairing:
		return
	case len(n.peers) >= maxBonds:
		n.log.Warn("cannot have more than 8 bonds")
		return
	}
	n.pairing, n.bonding = true, true
	n.bondMAC, n.tempBond = nil, nil
	n.pairEnd = now.Add(pairingWindow)
	n.leds.Play(AnimPairing, now)
	n.log.Info("pairing started", "for", pairingWindow)
	n.pairLoop(now)
	n.at(n.pairEnd, n.cancelPairing)
}

// pairLoop is Compass.pair_nearby: ack 0 broadcasts until a partner is
// chosen, then ack 1 unicasts to it, every 50-99 ms.
func (n *Node) pairLoop(now time.Time) {
	if !n.pairing {
		return
	}
	if n.bondMAC != nil {
		n.send(*n.bondMAC, n.status(now, mesh.PeerBond, true))
	} else {
		n.send(mesh.Broadcast, n.status(now, mesh.PeerBond, false))
	}
	n.at(now.Add(time.Duration(50+n.rng.IntN(50))*time.Millisecond), n.pairLoop)
}

// cancelPairing is Compass.cancel_pairing: a bond that never got its ack
// is rolled back.
func (n *Node) cancelPairing(now time.Time) {
	if now.Before(n.pairEnd) {
		return // a later pairing session owns this timer
	}
	n.stopPairing(now)
}

func (n *Node) stopPairing(now time.Time) {
	if !n.pairing {
		return
	}
	n.pairing, n.bonding, n.bondMAC = false, false, nil
	if n.tempBond != nil {
		n.log.Warn("deleting peer with failed bond", "mac", *n.tempBond)
		n.deletePeer(*n.tempBond)
		n.tempBond = nil
	}
	n.leds.Stop(AnimPairing, now)
	n.log.Info("pairing ended", "peers", len(n.peers))
}

func (n *Node) addPeer(mac mesh.MAC) *peer {
	if p, ok := n.peers[mac]; ok {
		return p
	}
	p := &peer{mac: mac, meshGrp: meshGroup(mac)}
	p.color = BondColor(len(n.order), binary.BigEndian.Uint32(n.cfg.MAC[2:6]))
	n.peers[mac] = p
	n.order = append(n.order, mac)
	return p
}

func (n *Node) deletePeer(mac mesh.MAC) {
	delete(n.peers, mac)
	n.order = slices.DeleteFunc(n.order, func(m mesh.MAC) bool { return m == mac })
}

// ForgetPeers drops every bond without telling anyone, as a factory
// reset does. No unbond notice goes out: this is the device forgetting
// them, not a decision about what they should hold.
func (n *Node) ForgetPeers(now time.Time) {
	if len(n.peers) == 0 {
		return
	}
	for _, mac := range slices.Clone(n.order) {
		n.deletePeer(mac)
	}
	n.leds.Play(AnimPeerDeleteCountdown, now)
	n.log.Info("all bonds forgotten")
}

// Unbond forgets a peer and tells it (EspConn.del_peer with is_unbond),
// which 5.0.3 receivers ignore.
func (n *Node) Unbond(now time.Time, mac mesh.MAC) []Packet {
	if _, ok := n.peers[mac]; !ok {
		return nil
	}
	n.outbox[mac] = mustMarshal(n.status(now, mesh.PeerUnbond, false))
	n.deletePeer(mac)
	n.leds.Play(AnimPeerDeleteCountdown, now)
	n.log.Info("peer deleted", "mac", mac)
	return nil
}

func mustMarshal(m mesh.Message) []byte {
	b, err := m.MarshalBinary()
	if err != nil {
		panic(err) // the node only builds frames that fit
	}
	return b
}

// ---- category 0 ---------------------------------------------------------------

// onPeer is Parser._peer.
func (n *Node) onPeer(now time.Time, rx Received, m mesh.Peer) {
	if m.Command == mesh.PeerBond && !n.pairing && n.cfg.AutoPair && !m.Ack {
		if rx.RSSI < n.cfg.BondingRSSI {
			n.log.Info("owned Totem is pairing but too far away", "mac", rx.Src, "rssi", rx.RSSI, "need", n.cfg.BondingRSSI)
			return
		}
		n.log.Info("owned Totem is pairing next to us", "mac", rx.Src, "rssi", rx.RSSI)
		n.startPairing(now)
	}
	bonded := false
	switch m.Command {
	case mesh.PeerBond:
		var done bool
		if bonded, done = n.onBond(now, rx, m); done {
			return
		}
	case mesh.PeerUnbond:
		// Receivers ignore unbond notices in 5.0.3.
		n.log.Info("peer says it deleted us (ignored, as on a Totem)", "mac", rx.Src)
		return
	}

	n.adoptClock(now, m)
	p, ok := n.peers[rx.Src]
	if !ok && m.Command == mesh.PeerStatus && rx.Dst == n.cfg.MAC {
		// Status unicast to us means the sender kept us in its
		// config.json while this device rebooted, or was flashed.
		if len(n.peers) >= maxBonds {
			// The same limit every other path applies. Past it the bond
			// would be lost at the next boot anyway, because the saved
			// list is read back through it.
			n.log.Warn("bond not restored: already at the bond limit",
				"mac", rx.Src, "bonds", len(n.peers), "max", maxBonds)
			return
		}
		n.log.Info("restoring bond the peer still holds", "mac", rx.Src)
		p, ok = n.addPeer(rx.Src), true
	}
	if !ok {
		return
	}
	p.heard, p.lastHeard, p.rssi, p.viaMesh, p.stale = true, now, rx.RSSI, false, false
	p.status = m
	if m.Lat != 0 || m.Lon != 0 {
		p.hasCoords, p.lat, p.lon, p.coordsAt = true, m.Lat, m.Lon, now
	}
	if bonded {
		n.leds.Play(AnimBonded, now)
		n.log.Info("bonded", "mac", p.mac, "name", m.Name, "peers", len(n.peers))
		return
	}
	if m.Command == mesh.PeerStatus {
		n.log.Info("peer status", "mac", p.mac, "name", m.Name, "rssi", rx.RSSI,
			"lat", m.Lat, "lon", m.Lon, "acc", m.PosAccuracyM, "azimuth", m.Azimuth,
			"orientation", m.Orientation, "sos", m.SOS, "batt", m.BattPct, "volts", m.BattVolts,
			"phone", m.PhoneConnected, "version", fmt.Sprintf("%d.%d.%d", m.Major, m.Minor, m.Patch))
	}
}

// onBond is the bond branch of Parser._peer. It reports whether the frame
// completed a bond, and whether handling stops here.
func (n *Node) onBond(now time.Time, rx Received, m mesh.Peer) (bonded, done bool) {
	if !n.pairing {
		return false, true
	}
	if rx.RSSI < n.cfg.BondingRSSI {
		n.log.Info("RSSI too poor to bond", "mac", rx.Src, "rssi", rx.RSSI, "need", n.cfg.BondingRSSI)
		return false, true
	}
	if p, ok := n.peers[rx.Src]; ok && (n.tempBond == nil || *n.tempBond != rx.Src) {
		n.log.Info("already a peer, allow re-bonding", "mac", rx.Src)
		if n.bondMAC == nil {
			n.bondMAC = &p.mac
			if p.heard && now.Sub(p.lastHeard) < alreadyBonded {
				n.log.Info("both devices already bonded", "mac", rx.Src)
				n.send(p.mac, n.status(now, mesh.PeerBond, true))
				for i := 1; i < ackBurstCount; i++ {
					n.at(now.Add(time.Duration(i)*ackBurstGap), func(t time.Time) {
						n.send(p.mac, n.status(t, mesh.PeerBond, true))
					})
				}
				n.stopPairing(now)
				return false, true
			}
		}
	} else if n.bondMAC == nil {
		n.log.Info("register sender as peer to bond to", "mac", rx.Src, "rssi", rx.RSSI)
		mac := rx.Src
		n.tempBond, n.bondMAC = &mac, &mac
		n.addPeer(mac)
	}
	if n.bondMAC != nil && *n.bondMAC == rx.Src && m.Ack {
		if n.tempBond == nil {
			// A Totem keeps confirming until its pairing window closes.
			n.log.Debug("bond confirmed again", "mac", rx.Src)
			return false, false
		}
		n.log.Info("bonding to peer", "mac", rx.Src, "name", m.Name)
		n.tempBond = nil
		return true, false
	}
	return false, false
}

// ---- mesh ---------------------------------------------------------------------

// locate is Messages.gen_mesh_msg.
func (n *Node) locate(now time.Time, request bool, uid uint16) mesh.Locate {
	if uid == 0 {
		uid = uint16(1 + n.rng.IntN(65534))
	}
	l := mesh.Locate{
		Origin: n.cfg.MAC, PosAccuracyM: -1, SOS: n.cfg.SOS, UID: uid, ReplyRequested: request,
		MinRSSI: mesh.DefaultMinRSSI, MinDistM: -1, MaxDistM: -1, MaxHops: mesh.DefaultMaxHops,
		Expiry: int32(n.wall(now).Unix()) + mesh.LocateLifetimeSec, RelayMinDistM: mesh.DefaultRelayMinDist,
	}
	if f := n.fix(); f != nil {
		l.Lat, l.Lon, l.PosAccuracyM = f.Lat, f.Lon, f.AccuracyM
		l.LastHopLat, l.LastHopLon = f.Lat, f.Lon
	}
	return l
}

// onLocate is EspConn.handle_mesh_msg and Parser.compass_mesh for a frame
// heard from an owned Totem.
func (n *Node) onLocate(now time.Time, rx Received, m mesh.Locate) {
	switch {
	case n.fix() == nil:
		return // a Totem without a fix ignores the mesh
	case n.seen(now, m.UID):
		return
	case !n.clockSet:
		return
	}
	defer n.addRecent(now, m.UID, m.Expiry)
	if m.Origin == n.cfg.MAC {
		if m.UID == n.replyUID {
			n.replyAt = time.Time{}
		}
		if m.UID == n.originUID {
			n.log.Info("our locate request was relayed back", "uid", m.UID)
			n.originUID = 0
		}
		return
	}
	if int(rx.RSSI) < int(m.MinRSSI) || int(rx.RSSI) > int(m.MaxRSSI) {
		return
	}
	p, bonded := n.peers[m.Origin]
	if !bonded {
		return // never relay a frame another Totem originated
	}
	replyOK := (p.viaMesh || !p.heard || now.Sub(p.lastHeard) >= meshReplyHold) &&
		(n.lastReply.IsZero() || now.Sub(n.lastReply) >= meshReplyHold)
	p.viaMesh, p.stale = true, false
	p.heard, p.lastHeard = true, now
	if m.Lat != 0 || m.Lon != 0 {
		// Either half being set is a position: a Totem on the meridian or
		// the equator sends one coordinate as a true zero. The status and
		// reply paths already read it this way.
		p.hasCoords, p.lat, p.lon, p.coordsAt = true, m.Lat, m.Lon, now
	}
	n.log.Info("locate", "origin", m.Origin, "via", rx.Src, "request", m.ReplyRequested, "hops", m.Hops, "uid", m.UID)
	if !m.ReplyRequested {
		return
	}
	if replyOK {
		reply := n.locate(now, false, 0)
		n.send(mesh.Broadcast, reply)
		n.replyUID, n.replyAt, n.lastReply = reply.UID, now, now
		n.log.Info("answered locate request", "origin", m.Origin, "uid", reply.UID)
	}
	if n.meshAcks < meshAckLimit && n.relay(now, rx.Data, m) {
		n.meshAcks++
	}
}

// relay is Parser._relay_frame: the same frame with our position as the
// last hop and one more hop.
func (n *Node) relay(now time.Time, frame []byte, m mesh.Locate) bool {
	if int(m.Hops)+1 >= int(m.MaxHops) || (m.Expiry != 0 && n.wall(now).Unix() > int64(m.Expiry)) {
		return false
	}
	pos := n.fix()
	if m.RelayMinDistM > 0 && distance(pos.Lat, pos.Lon, m.LastHopLat, m.LastHopLon) < float64(m.RelayMinDistM) {
		return false
	}
	b := slices.Clone(frame)
	binary.LittleEndian.PutUint32(b[35:], math.Float32bits(pos.Lat))
	binary.LittleEndian.PutUint32(b[39:], math.Float32bits(pos.Lon))
	b[29]++
	n.sendRaw(mesh.Broadcast, b)
	return true
}

func (n *Node) seen(now time.Time, uid uint16) bool {
	t, ok := n.recent[uid]
	return ok && now.Before(t)
}

// addRecent is Parser.add_recent.
func (n *Node) addRecent(now time.Time, uid uint16, expiry int32) {
	keep := time.Duration(int64(expiry)-n.wall(now).Unix())*time.Second + dedupeMin
	keep = min(max(keep, dedupeMin), dedupeMax)
	if len(n.recent) >= recentMax {
		for k, t := range n.recent {
			if !now.Before(t) {
				delete(n.recent, k)
			}
		}
	}
	if len(n.recent) >= recentMax {
		var oldest uint16
		var at time.Time
		for k, t := range n.recent {
			if at.IsZero() || t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(n.recent, oldest)
	}
	n.recent[uid] = now.Add(keep)
}

// meshTick is Compass.one_sec_mesh_coro: in our 1-of-5 slot, ask the mesh
// for bonded peers we have lost.
func (n *Node) meshTick(now time.Time) {
	n.scheduleMeshTick(now)
	if n.fix() == nil || len(n.peers) == 0 || n.lastTX.IsZero() || n.wall(now).Second()%5 != n.meshGrp {
		return
	}
	if _, queued := n.outbox[mesh.Broadcast]; queued {
		return
	}
	send := false
	for _, mac := range n.order {
		p := n.peers[mac]
		n.updateStale(now, p)
		if p.viaMesh && now.Sub(p.lastHeard) >= meshStaleHeard {
			send = true
		}
		if !p.stale || now.Before(p.meshNext) || p.meshCount > meshPeerLimit {
			continue
		}
		d := n.peerDistance(p)
		if d < 0 {
			d = 50
		}
		delay := meshDelivery(d)
		if delay.Milliseconds()%6 == 0 {
			delay -= time.Second
		}
		// The lower MAC asks first. Comparing the bytes orders them the
		// same way as the firmware's integer compare, and allocates
		// nothing on a tick that runs every second.
		if p.meshGrp != n.meshGrp || now.Sub(p.firstStale) >= delay || bytes.Compare(p.mac[:], n.cfg.MAC[:]) <= 0 {
			send = true
		} else {
			p.meshNext = now.Add(delay)
		}
	}
	if !send {
		return
	}
	for _, p := range n.peers {
		if p.stale {
			p.meshNext = now.Add(meshSendFreq)
			p.meshCount++
		}
	}
	req := n.locate(now, true, 0)
	n.originUID = req.UID
	n.outbox[mesh.Broadcast] = mustMarshal(req)
	n.log.Info("asking the mesh for lost peers", "uid", req.UID)
}

// meshDelivery is Parser.cal_mesh_delivery(dist, range=75).
func meshDelivery(distM float64) time.Duration {
	h := int(distM) / meshHopRange
	ms := (h/2+1)*50 + (h-h/2)*4050
	return time.Duration(max(ms, 6000)) * time.Millisecond
}

// updateStale is peer_helpers.is_peer_stale.
func (n *Node) updateStale(now time.Time, p *peer) {
	was := p.stale
	switch {
	case !p.hasCoords:
		p.stale = true
	default:
		w := 600 * time.Second
		if d := n.peerDistance(p); d >= 0 {
			w = 2 * time.Duration(min(max(int(d/meshHopRange*60), 60), 600)) * time.Second
		}
		p.stale = now.Sub(p.coordsAt) > w
	}
	if p.stale && !was {
		p.firstStale = now
	}
}

func (n *Node) peerDistance(p *peer) float64 {
	f := n.fix()
	if f == nil || !p.hasCoords {
		return -1
	}
	return distance(f.Lat, f.Lon, p.lat, p.lon)
}

func (n *Node) furthestPeer() float64 {
	far := 0.0
	for _, p := range n.peers {
		far = max(far, n.peerDistance(p))
	}
	return far
}

// distance is the great-circle distance in meters. The firmware's
// get_distance lives in the native c_stats module; a haversine is
// assumed.
func distance(lat1, lon1, lat2, lon2 float32) float64 {
	const r = 6371000
	φ1, φ2 := float64(lat1)*math.Pi/180, float64(lat2)*math.Pi/180
	dφ, dλ := φ2-φ1, float64(lon2-lon1)*math.Pi/180
	a := math.Sin(dφ/2)*math.Sin(dφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// ---- Smart Group client ------------------------------------------------------

// onSmartGroup is the client half of Parser._smart_group. The node joins a
// group an owned Totem hosts, and on finalize bonds only with the owned
// members.
func (n *Node) onSmartGroup(now time.Time, rx Received, g mesh.SmartGroup) {
	switch g.Instruction {
	case mesh.SmartGroupAbandon:
		if !n.inGroup || n.smartUID == g.UID {
			n.inGroup, n.smartUID = false, 0
			n.log.Info("smart group abandoned", "uid", g.UID)
		}
		return
	case mesh.SmartGroupAdvertise:
		if n.inGroup || !n.bonding {
			if n.inGroup && g.UID == n.smartUID {
				n.groupUntil = now.Add(time.Duration(g.TimeoutMs) * time.Millisecond)
			}
			return
		}
		if slices.ContainsFunc(g.Members, func(m mesh.SmartGroupMember) bool { return m.MAC == n.cfg.MAC }) {
			n.inGroup, n.smartUID = true, g.UID
			n.groupUntil = now.Add(time.Duration(g.TimeoutMs) * time.Millisecond)
			n.log.Info("joined smart group", "uid", g.UID, "host", rx.Src)
			return
		}
		if rx.RSSI < n.cfg.SmartGrpRSSI {
			return
		}
		join := mesh.SmartGroupReply{UID: g.UID, PosAccuracyM: -1}
		if f := n.fix(); f != nil {
			join.Lat, join.Lon, join.PosAccuracyM = f.Lat, f.Lon, f.AccuracyM
		}
		n.send(mesh.Broadcast, join)
		n.at(now.Add(joinGap), func(time.Time) { n.send(mesh.Broadcast, join) })
		n.log.Info("sent smart group join", "uid", g.UID, "rssi", rx.RSSI)
	case mesh.SmartGroupFinalize:
		if !n.inGroup || g.UID != n.smartUID {
			return
		}
		n.inGroup, n.smartUID = false, 0
		// peer_management.auto_bond_to_peers deletes every peer first.
		for _, mac := range slices.Clone(n.order) {
			n.deletePeer(mac)
		}
		for _, m := range g.Members {
			if m.MAC == n.cfg.MAC {
				// The group assigns this device a colour, and the crystal
				// is what shows it: setting only the config would leave
				// the two saying different things.
				n.cfg.ColorID = m.ColorID
				n.leds.SetDefaultColor(Color(m.ColorID))
				continue
			}
			if !slices.Contains(n.cfg.Owned, m.MAC) {
				n.log.Info("skipping smart group member outside the owned scope", "mac", m.MAC)
				continue
			}
			if _, known := n.peers[m.MAC]; !known && len(n.peers) >= maxBonds {
				// A group may list more members than a Totem can bond to,
				// and the ones past the limit would be lost at the next
				// reboot anyway: the saved list is read back through the
				// same limit.
				n.log.Warn("smart group member dropped: already at the bond limit",
					"mac", m.MAC, "bonds", len(n.peers), "max", maxBonds)
				continue
			}
			p := n.addPeer(m.MAC)
			if m.Lat != 0 || m.Lon != 0 {
				p.hasCoords, p.lat, p.lon, p.coordsAt = true, m.Lat, m.Lon, now
			}
		}
		n.log.Info("smart group completed", "peers", len(n.peers), "color", n.cfg.ColorID)
	}
}

// ---- state for the driver ------------------------------------------------------

// AddBond puts back a bond the device already had, the way a Totem reads
// config.peers at boot: the peer is bonded again without a pairing
// handshake, and nothing goes on the air. A MAC outside the owned scope is
// refused, so a saved peer list cannot widen what this node talks to, and
// so is a list longer than the firmware's bond limit.
func (n *Node) AddBond(mac mesh.MAC, name string, now time.Time) error {
	if !slices.Contains(n.cfg.Owned, mac) {
		return fmt.Errorf("%s is not one of this node's Totems", mac)
	}
	if _, ok := n.peers[mac]; ok {
		return nil
	}
	if len(n.peers) >= maxBonds {
		return fmt.Errorf("already bonded to %d peers", len(n.peers))
	}
	p := n.addPeer(mac)
	p.status.Name = name
	// The peer has not been heard since the reboot. Leaving lastHeard
	// zero says so, and keeps the mesh from treating silence as a peer
	// that has just gone quiet.
	p.heard, p.stale, p.firstStale = false, true, now
	n.log.Info("bond restored", "mac", mac, "name", name)
	return nil
}

// SOSMuted reports whether the alarm's blinking is muted. It survives a
// reboot on a device that saves its settings.
func (n *Node) SOSMuted() bool { return n.sosMuted }

// SetSOSMuted puts the mute back to a saved value, without the toggle a
// button press makes.
func (n *Node) SetSOSMuted(muted bool) { n.sosMuted = muted }

// BondCount is how many peers are bonded. A driver polls it to notice a
// bond gained or lost without building the whole list every pass.
func (n *Node) BondCount() int { return len(n.peers) }

// Peers lists the bonded peers in bond order.
func (n *Node) Peers() []PeerInfo {
	var out []PeerInfo
	for _, mac := range n.order {
		p := n.peers[mac]
		out = append(out, PeerInfo{MAC: mac, Status: p.status, RSSI: p.rssi, LastHeard: p.lastHeard, ViaMesh: p.viaMesh, DistanceM: n.peerDistance(p)})
	}
	return out
}

// Pairing reports whether the pairing window is open.
func (n *Node) Pairing() bool { return n.pairing }

// SetClock gives the node the wall time, as a GNSS lock does. It is how a
// board without a clock of its own gets one, from totemctl mesh clock.
func (n *Node) SetClock(wall, now time.Time) error {
	if !wall.After(minClock) {
		return fmt.Errorf("clock %s is before %s", wall.UTC().Format(time.RFC3339), minClock.Format("2006"))
	}
	if c := n.controls(); c != nil {
		c.SetClock(wall, now)
	}
	n.clockOffset = wall.Sub(now)
	n.clockSet, n.gnssClock = true, true
	n.log.Info("clock set", "wall", wall.UTC().Format(time.RFC3339Nano))
	n.startAligned(now)
	n.read(now)
	return nil
}

// SetSensors replaces the sensor source, such as a simulated Totem or a
// board's own drivers.
func (n *Node) SetSensors(src SensorSource, now time.Time) {
	n.source = src
	n.cfg.Sensors = src
	n.read(now)
}

// controls is the hand-driven half of the sensor source, which the pos,
// heading, batt, flat and clock commands go through. It is nil once a
// source that only reports real hardware is installed.
func (n *Node) controls() Controls {
	c, _ := n.source.(Controls)
	return c
}

// errNoControls says a setting cannot reach the sensors because they are a
// board's own. Reporting it beats accepting the command and changing
// nothing.
var errNoControls = errors.New("the sensors report the board's own hardware and take no settings")

// set applies one hand-driven setting and takes a fresh reading, so the
// next status frame carries it. Every setter goes through here: one place
// decides what a source can take, and none of them can forget a case.
func (n *Node) set(now time.Time, f func(c Controls)) error {
	c := n.controls()
	if c == nil {
		return errNoControls
	}
	f(c)
	n.read(now)
	return nil
}

// sim is the running simulation, or nil when the readings come from
// somewhere else.
func (n *Node) sim() *Sim {
	s, _ := n.source.(*Sim)
	return s
}

// StartSim runs a simulated Totem: it walks a track from wherever the node
// is now, keeping its battery and orientation.
func (n *Node) StartSim(m Motion, bearing int16, now time.Time) {
	if s := n.sim(); s != nil {
		s.SetMotion(m, bearing)
		n.read(now)
		return
	}
	cfg := SimConfig{
		Motion: m, Bearing: bearing, NoFix: n.sensors.Fix == nil,
		// The battery carries over as it reads, 0% included: a simulation
		// started on a flat device must not report a full one.
		Percent: n.sensors.Battery.Percent, Charging: n.sensors.Battery.Charging,
		Flat: n.sensors.Orientation == mesh.OrientationHorizontal, Rand: n.rng,
	}
	if f := n.sensors.Fix; f != nil {
		cfg.Lat, cfg.Lon = f.Lat, f.Lon
	}
	n.SetSensors(NewSim(cfg, now), now)
	n.log.Info("simulation started", "motion", m, "bearing", bearing)
	if cfg.NoFix {
		n.log.Warn("the simulation has no position to walk from: set one with pos <lat> <lon>")
	}
}

// StopSim freezes the readings where the simulation left them.
func (n *Node) StopSim(now time.Time) {
	if n.sim() == nil {
		return
	}
	held := n.sensors
	if f := held.Fix; f != nil {
		frozen := *f
		frozen.SpeedKPH, frozen.Time = 0, time.Time{}
		held.Fix = &frozen
	}
	n.SetSensors(NewStatic(held), now)
	n.log.Info("simulation stopped")
}

// SetFlat reports the Totem lying down or upright, which changes how often
// peers expect its status.
func (n *Node) SetFlat(flat bool, now time.Time) error {
	if err := n.set(now, func(c Controls) { c.SetFlat(flat) }); err != nil {
		return err
	}
	n.cfg.Orientation = n.sensors.Orientation
	return nil
}

// SetBattery sets the charge level and whether the Totem is charging.
func (n *Node) SetBattery(percent int8, charging bool, now time.Time) error {
	if err := n.set(now, func(c Controls) { c.SetBattery(percent, charging, now) }); err != nil {
		return err
	}
	n.cfg.BattPct, n.cfg.BattVolts = n.sensors.Battery.Percent, n.sensors.Battery.Volts
	return nil
}

// SetPosition changes the reported fix; nil means none.
func (n *Node) SetPosition(p *Position, now time.Time) error {
	if err := n.set(now, func(c Controls) { c.SetFix(p) }); err != nil {
		return err
	}
	n.cfg.Position = n.sensors.Fix
	return nil
}

// SetSOS switches SOS on or off. It is the node's own flag, not a
// reading, so it holds whatever the sensors are. The crystal blinks with
// it, unless the alarm has been muted.
func (n *Node) SetSOS(on bool) {
	n.cfg.SOS = on
	switch {
	case on && !n.sosMuted:
		n.leds.Play(AnimSOS, n.sensorsAt)
	case !on:
		// Turning SOS off clears the mute too: the next alarm blinks.
		n.sosMuted = false
		n.leds.Stop(AnimSOS, n.sensorsAt)
	}
}

// SetHeading changes the reported compass azimuth. While a simulation runs
// it steers the walk, which is where the azimuth comes from.
func (n *Node) SetHeading(deg int16, now time.Time) error {
	if err := n.set(now, func(c Controls) { c.SetAzimuth(deg) }); err != nil {
		return err
	}
	n.cfg.Heading = n.sensors.Azimuth
	return nil
}

// Sensors is the last sensor reading.
func (n *Node) Sensors() Sensors { return n.sensors }

// Config returns the node's current settings.
func (n *Node) Config() Config { return n.cfg }
