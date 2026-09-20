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
	Sensors   SensorSource
	Position  *Position
	Heading   int16 // compass azimuth, degrees
	BattVolts float32
	BattPct   int8
	// NoPowerChip says the board has no battery monitor, so BattVolts and
	// BattPct are the absence of a reading rather than a reading of zero.
	// An update is not barred on such a board. Leaving it false is the
	// safe answer: a board that does have a chip, and reads zero because
	// the cell is flat or the ADC failed, is exactly what the gate is
	// for.
	NoPowerChip bool
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
	// saidBondLimit records that the full bond list has been reported, so
	// a Totem pairing next to a full device does not fill the log. Only a
	// deletion clears it, because only a deletion can make room.
	saidBondLimit bool
	// saidRestoreLimit is the same for the other reason a full list gets
	// reported: a Totem that still holds us and unicasts its status. Two
	// latches, because one would let whichever fired first silence the
	// other for the rest of the run.
	saidRestoreLimit bool
	// warnedClock is the peers already reported for broadcasting a clock
	// from before 2020, so each is said once rather than every few
	// seconds for as long as it is in range. Keyed by address rather than
	// by the name in the frame: the name is bytes off the air and a peer
	// that varied it would grow this without limit, while the addresses
	// it can hold are the owned ones.
	warnedClock map[mesh.MAC]bool
	// lowOwed is a low-battery reminder that has not been shown yet. The
	// mode changes when the pack says so, which may be while the ring is
	// busy with a boot animation, an alarm or a download; the reminder
	// waits for the strip rather than being lost or drawn over them.
	lowOwed bool
	// named is whether this device was built with a name — `-X
	// main.name=` on the board, Config.Name in a test — rather than
	// given the default one made from its MAC. A saved name does not
	// overwrite a name someone chose.
	named bool
}

// New starts a node at time now.
func New(cfg Config, now time.Time) *Node {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	// Every status frame carries the name, so one the frame cannot hold
	// would fail at the point of sending it, and one that is not text
	// would travel on into whatever reads it.
	gave, short := cfg.Name, store.SanitizeName(cfg.Name)
	cfg.Name = short
	// Whether anyone chose this name, recorded here because here is
	// where it is known: Restore used to ask whether the name still
	// equalled DefaultName, which is a guess at this fact, and one that
	// says yes to a board deliberately flashed with the name it would
	// have been given anyway.
	//
	// After sanitizing, not before. A name that is not text at all comes
	// out of it empty, and asking first left such a device both nameless
	// and marked as named — so it ran with "", and refused the saved
	// name at every boot afterwards.
	named := cfg.Name != ""
	if cfg.Name == "" {
		cfg.Name = DefaultName(cfg.MAC)
	}
	if short != gave {
		// After the default has been filled in, so the line says what the
		// device is actually called: it said `using=""` for a name that
		// is not text at all, while the device came up as emu_totem_xxxx.
		//
		// Two messages, because there are two outcomes and calling the
		// second one "what is left" would tell a reader that the name
		// made from the MAC is a remnant of the one they chose. What
		// SanitizeName did is not only shortening: a name that is not
		// text loses the bytes that are not, so saying it did not fit
		// would be wrong as often as right.
		msg := "name is not one a peer frame and the settings can both hold, using what is left"
		if short == "" {
			msg = "name is not text a peer frame and the settings can hold, falling back to the default"
		}
		cfg.Logger.Warn(msg, "name", gave, "bytes", len(gave), "using", cfg.Name)
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
	if p := cfg.Position; p != nil && !usablePosition(p.Lat, p.Lon) {
		// The same rule SetPosition applies. A board hands this straight
		// in, and a position every reader silently ignores would leave the
		// device searching for a fix for ever with nothing to say why.
		cfg.Logger.Warn("configured position is not one a device could be at, starting with none",
			"lat", p.Lat, "lon", p.Lon)
		cfg.Position = nil
	}
	// The board reads this off a flash sector and hands it straight here,
	// so an id outside the thirteen arrives before Restore ever runs —
	// and Restore keeping "the default" would be keeping the corrupt one.
	// An unlit crystal for good, from one bad byte.
	cfg.ColorID = int8(paletteColor(cfg.Logger, "configuration", cfg.ColorID, ColorRed))
	// The node's own copies. Owned is what allowedTX gates every
	// transmission against, and Position is what fix() answers with: a
	// caller that kept the slice or the pointer it passed in could change
	// either afterwards, which is the same hole Config() and Sensors()
	// close on the way out.
	cfg.Owned = slices.Clone(cfg.Owned)
	cfg.Position = clonePosition(cfg.Position)
	n := &Node{
		cfg: cfg, log: cfg.Logger, rng: cfg.Rand, boot: now,
		peers: map[mesh.MAC]*peer{}, outbox: map[mesh.MAC][]byte{}, recent: map[uint16]time.Time{},
		warnedClock: map[mesh.MAC]bool{},
		meshGrp:     meshGroup(cfg.MAC),
	}
	n.named = named
	n.source = cfg.Sensors
	if n.source == nil {
		n.source = &staticSensors{s: Sensors{
			Fix: cfg.Position, Orientation: cfg.Orientation, Azimuth: cfg.Heading,
			Battery: Battery{
				Volts: cfg.BattVolts, Percent: cfg.BattPct,
				NoPowerChip: cfg.NoPowerChip,
			},
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
	// Not on a device the reading above has already switched off: read()
	// acts on what it reads, so a node built on a pack under the cutoff
	// is down by now, and arming a window would leave it transmitting
	// from a device that is supposed to be silent.
	if !n.power.Off() && !n.hasJob(jobWindow) {
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
	// A device that has powered down has no radio. Receive already
	// refuses to answer while it is off, and this is the other half: the
	// console can still reach Pair and Unbond, and without this a device
	// someone had switched off broadcast bond requests every 50-99 ms for
	// six seconds while being deaf to the replies. The guard belongs here
	// rather than at each entry point for the same reason allowedTX does
	// — this is the one place frames leave the node.
	if n.power.Off() {
		n.log.Debug("not sending: the device is powered down", "dst", dst)
		return
	}
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
	// Again, because the reading may have switched the device off: a
	// pack that collapsed since the last poll is found here, and a
	// device that is down does not take a frame into its peer table,
	// adopt a clock from it or answer it. sendRaw refuses while off, so
	// nothing would go on the air either way — this is the state that
	// would be left behind.
	if n.power.Off() {
		// flush, not nil: anything the reading queued on the way down
		// belongs to the run that is ending. PowerOff clears the outbox
		// for that reason, and this is the one way out of Receive that
		// would otherwise skip the drain and leave a frame for the next
		// Poll to send after the device is down.
		return n.flush()
	}
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

// maxClock is the last instant a peer frame can carry. The Unix field is
// an int32, as it is in the firmware, so a clock past this wraps: fuzzing
// set one in 2055 and the node advertised 1919, a timestamp a peer with
// no clock of its own would have taken. A clock that cannot be said is
// not a clock this node accepts.
var maxClock = time.Unix(math.MaxInt32, 0).UTC()

// usableClock reports whether a wall time is one this node may hold and
// put on the air: after the floor every path applies, and inside what the
// frame can carry.
func usableClock(w time.Time) bool { return w.After(minClock) && !w.After(maxClock) }

func (n *Node) read(now time.Time) {
	n.sensors, n.sensorsAt = n.source.Read(now), now
	// Whether the board has a battery monitor is a fact about the board,
	// so the configuration has the last word on it, both ways: a source
	// of its own — a driver, or a simulation swapped in at runtime — has
	// no way to know, and one that claimed there was no chip would turn
	// the OTA battery gate off on a board with a flat cell. An
	// assignment, not a set: clearing is the half that keeps the gate on.
	n.sensors.Battery.NoPowerChip = n.cfg.NoPowerChip
	// A real power chip reports a cell voltage; the percentage is what
	// get_batt_pct makes of it. A source that gives the voltage and no
	// percentage gets one here, so every reader — the status frame, the
	// power mode, the OTA gate — sees the same battery. There is no
	// inverse: a fuel gauge that reports only a percentage keeps a
	// voltage of zero, because the curve cannot be run backwards without
	// inventing a number.
	// Learned before the percentage is worked out, so a pack that has
	// just been seen higher than the curve's top is read against its own
	// maximum rather than the previous one. Not while the device is off:
	// the driver keeps polling so the power button still works, and a
	// device left switched off on a charger would otherwise go on
	// stretching its curve.
	if !n.power.Off() {
		n.power.learn(n.sensors.Battery)
	}
	if b := &n.sensors.Battery; !b.NoPowerChip && b.Volts > 0 && b.Percent == 0 {
		b.Percent = battPctFor(b.Volts, n.power.LearnedMaxVolts())
	}
	// n.fix(), not the raw reading: a receiver whose position this node
	// refuses has not earned its clock either, and that clock would be
	// advertised to every peer and would re-slot every radio window.
	if f := n.fix(); f != nil && usableClock(f.Time) {
		n.clockOffset = f.Time.Sub(now)
		// Before startAligned, not after: scheduleWindow asks clockSet
		// which slot to use, so setting it afterwards put the first
		// window on the no-clock period — an arbitrary phase, when
		// aligning that window is the only reason startAligned is called.
		// adoptClock and SetClock both order it this way.
		n.clockSet = true
		if !n.gnssClock {
			n.gnssClock = true
			n.log.Info("clock from GNSS", "wall", f.Time.UTC().Format(time.RFC3339Nano))
			n.startAligned(now)
		}
	}
	// And the mode that follows from what was just read. Here rather
	// than at each caller: every path that takes a reading has the same
	// decision to make, and of the ten that call this, five had it and
	// five did not — so `batt 2` on the console reported power mode
	// normal until something else polled, and a simulation swapped in on
	// a flat pack went on running. A mode is not an opinion about the
	// battery, it is the battery.
	n.applyPowerMode(now)
}

// fix is this device's own GNSS solution, or nil when it has none.
//
// A position that no device could be at is no position: nil is what a
// Totem indoors reports, and that is the honest answer for a garbled one
// too. Null Island counts as garbled — it is the firmware's own way of
// saying it has no fix, and treating it as a place puts the compass
// confidently on a bearing measured from the Gulf of Guinea. That is
// usablePosition, the same guard every position off the air gets, and it
// belongs here as much as there — a board's own receiver driver is code
// like any other, the console can be told anything, and this is the one
// position this device puts on the air. Left unchecked, a NaN reached the
// compass (where the conversion to a bearing is implementation-defined,
// so it pointed confidently north), the distance to every peer, and every
// status frame this node broadcast.
func (n *Node) fix() *Fix {
	f := n.sensors.Fix
	if f == nil || !usablePosition(f.Lat, f.Lon) {
		return nil
	}
	return f
}

func (n *Node) wall(now time.Time) time.Time { return now.Add(n.clockOffset) }

// startAligned moves the radio windows and the mesh tick onto wall-clock
// slots, once the node has a clock.
func (n *Node) startAligned(now time.Time) {
	// Not on a device that is off: PowerOff empties the job list, and
	// arming it again here — a clock arriving while the device is down —
	// had it running radio windows and building a status frame for each
	// one for sendRaw to discard. PowerOn schedules its own.
	if n.power.Off() {
		return
	}
	n.jobs = slices.DeleteFunc(n.jobs, func(j job) bool { return j.kind != jobOnce })
	n.scheduleWindow(now)
	n.scheduleMeshTick(now)
}

// adoptClock is the RTC block of Parser._peer: a Totem without a clock
// takes the time from a peer whose clock came from GNSS.
func (n *Node) adoptClock(now time.Time, src mesh.MAC, p mesh.Peer) {
	if n.clockSet || n.gnssClock || p.TimeOfDayMs <= 0 || p.Unix <= 0 || now.Sub(n.boot) < rtcSyncDelay {
		return
	}
	wall := time.UnixMilli(int64(p.Unix)*1000 + int64(p.TimeOfDayMs%1000))
	if !usableClock(wall) {
		// The same floor the GNSS path and the console apply. A peer that
		// says 1970 — a garbled frame, or a device whose own clock never
		// started — would otherwise pin this one there for good: nothing
		// clears the clock once it is set, and every locate frame this
		// node sent would carry an expiry 55 years in the past, which
		// every peer with a real clock drops.
		//
		// The ceiling as well: a clock past what the int32 can hold is no
		// more usable than one before 2020, and the message has to say
		// which it was rather than name the floor for both.
		//
		// Once per name: a peer whose own RTC never started broadcasts
		// status every one to four seconds, and warning on each would
		// fill the log for as long as it is in range — on the board the
		// rotate that follows holds a watchdog blocker. But it is worth
		// saying at all, because someone wondering why a Totem in the
		// field never picks up a clock has nothing else to go on.
		if !n.warnedClock[src] {
			n.warnedClock[src] = true
			// "name", not "src": totemctl decodes src as a MAC, and a
			// line whose src is a name fails to parse and is dropped
			// whole — so the one warning that explains why a Totem never
			// picks up a clock would never reach the person watching.
			n.log.Warn("ignoring a peer's clock: outside what a peer frame can carry",
				"mac", src, "name", p.Name, "wall", wall.UTC().Format(time.RFC3339),
				"earliest", minClock.Format("2006"), "latest", maxClock.Format("2006"))
		}
		return
	}
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
	var start time.Time
	if n.clockSet {
		// period() walks every bonded peer, and the other branch does not
		// want it.
		w := n.wall(now).UnixMilli()
		p := n.period().Milliseconds()
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
		// Encoded once and sent to each peer: the same bytes go to all of
		// them, and with eight bonds and a far peer this ran sixteen
		// marshals and sixteen 108-byte allocations every window, on a
		// heap of a few hundred kilobytes.
		st := mustMarshal(n.status(now, mesh.PeerStatus, false))
		for _, mac := range n.order {
			n.sendRaw(mac, st)
		}
		if n.furthestPeer() > farPeerM {
			for _, mac := range n.order {
				n.sendRaw(mac, st)
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
	// fix(), not sense.Fix: this is the frame that puts our position on
	// the air, so it is the last place a position no device could be at
	// should get through. A peer receiving one would refuse it anyway —
	// there is no reason to make it.
	if f := n.fix(); f != nil {
		p.Lat, p.Lon, p.PosAccuracyM = f.Lat, f.Lon, f.AccuracyM
		p.SpeedKPH, p.AltitudeM, p.SolutionID = f.SpeedKPH, f.AltitudeM, f.SolutionID
		p.HeadingOfMotion = f.HeadingOfMotion
		p.OdometerM = int16(min(f.OdometerM, math.MaxInt16))
	}
	// usableClock again rather than gnssClock alone: the clock is a base
	// plus however long the device has been running, so one set near the
	// ceiling crosses it while the device is up. Past it the int32 wraps
	// and the frame would carry a time from the 1900s, which a peer with
	// no clock of its own would take as real.
	if n.gnssClock && usableClock(n.wall(now)) {
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

// startPairing opens the window, and says why when it cannot. Every
// caller is answered: the throttling belongs to the caller that repeats,
// not here — a boolean saying which caller this is would have to be got
// right by every caller added later, and getting it wrong either fills
// the log or silences a button.
func (n *Node) startPairing(now time.Time) {
	switch {
	case n.power.Off():
		// Refused here rather than dropped in sendRaw: a window opened on
		// a device that is off still arms its timers and runs pair_nearby
		// every 50-99 ms for six seconds, building a status frame each
		// time for the radio to throw away. The bytes never leaving is
		// not the same as the work not happening.
		n.log.Warn("cannot pair: the device is powered down")
		return
	case n.pairing:
		return
	case len(n.peers) >= maxBonds:
		n.log.Warn("cannot have more than 8 bonds")
		return
	}
	n.pairing = true // modes.bonding_start
	n.bondMAC, n.tempBond = nil, nil
	n.pairEnd = now.Add(pairingWindow)
	n.leds.Play(AnimPairing, now)
	n.log.Info("pairing started", "for", dur(pairingWindow))
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
	n.pairing, n.bondMAC = false, nil
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
	// There is room again, so the next time there is not is worth saying.
	n.saidBondLimit, n.saidRestoreLimit = false, false
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
	if n.power.Off() {
		// The notice would sit in the outbox until the device came back
		// and then go out, announcing an unbond from a power cycle the
		// peer never saw. A device that is off does nothing.
		n.log.Warn("cannot delete a peer: the device is powered down", "mac", mac)
		return nil
	}
	n.outbox[mac] = mustMarshal(n.status(now, mesh.PeerUnbond, false))
	n.deletePeer(mac)
	n.leds.Play(AnimPeerDeleteCountdown, now)
	n.log.Info("peer deleted", "mac", mac)
	return n.flush()
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
		// Once per full bond list: this runs for every bond broadcast a
		// Totem next to us sends, and pair_nearby repeats every 50-99 ms
		// for six seconds — eighty identical lines on a console that has
		// frames to carry. A person pressing the button is answered every
		// time, because they went through Pair rather than through here.
		if len(n.peers) >= maxBonds {
			if !n.saidBondLimit {
				n.saidBondLimit = true
				n.log.Warn("cannot bond with a Totem pairing next to us: already at the limit",
					"mac", rx.Src, "bonds", len(n.peers), "max", maxBonds)
			}
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

	n.adoptClock(now, rx.Src, m)
	p, ok := n.peers[rx.Src]
	if !ok && m.Command == mesh.PeerStatus && rx.Dst == n.cfg.MAC {
		// Status unicast to us means the sender kept us in its
		// config.json while this device rebooted, or was flashed.
		if len(n.peers) >= maxBonds {
			// The same limit every other path applies. Past it the bond
			// would be lost at the next boot anyway, because the saved
			// list is read back through it.
			if n.saidRestoreLimit {
				return
			}
			n.saidRestoreLimit = true
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
	if usablePosition(m.Lat, m.Lon) {
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

// locateExpiry is when a locate frame stops being worth relaying, as an
// int32 of Unix seconds — the field the frame carries, so the lifetime
// has to be added inside it. Near the ceiling the sum wraps, and a
// negative expiry is one every receiver reads as long past, so the flood
// would die at the first hop rather than travel. Capped instead: an
// expiry at the ceiling says "as long as this format can mean".
func locateExpiry(wall time.Time) int32 {
	sec := wall.Unix()
	switch {
	case sec > math.MaxInt32-mesh.LocateLifetimeSec:
		return math.MaxInt32
	case sec < 0:
		// A board whose clock has not started, or a caller handing over a
		// zero time: truncating that into the field gives an arbitrary
		// value, and a large positive one is a frame every relay keeps
		// alive for decades.
		//
		// 1, not 0. relay() here reads any expiry at or below zero as
		// expired, so either would do for this node — but the frame goes
		// to other firmwares too, and zero is the value a reader is most
		// likely to treat as "no expiry set". One second after the epoch
		// says the same thing and cannot be mistaken for absence.
		return 1
	}
	return int32(sec) + mesh.LocateLifetimeSec
}

// locate is Messages.gen_mesh_msg.
func (n *Node) locate(now time.Time, request bool, uid uint16) mesh.Locate {
	if uid == 0 {
		uid = uint16(1 + n.rng.IntN(65534))
	}
	l := mesh.Locate{
		Origin: n.cfg.MAC, PosAccuracyM: -1, SOS: n.cfg.SOS, UID: uid, ReplyRequested: request,
		MinRSSI: mesh.DefaultMinRSSI, MinDistM: -1, MaxDistM: -1, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(n.wall(now)), RelayMinDistM: mesh.DefaultRelayMinDist,
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
	// Either half being set is a position: a Totem on the meridian or the
	// equator sends one coordinate as a true zero. The status and reply
	// paths already read it this way.
	if usablePosition(m.Lat, m.Lon) {
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
	// An expiry at or below zero is expired, not absent. Reading zero as
	// "none set" put the rule in every producer of the field instead of
	// here: a frame off the air stamped zero — a peer whose clock never
	// started, another firmware, a garbled field — was relayed until the
	// hop count ran out, and every new path that stamps one had to know.
	if int(m.Hops)+1 >= int(m.MaxHops) || m.Expiry <= 0 || n.wall(now).Unix() > int64(m.Expiry) {
		return false
	}
	pos := n.fix()
	// The last hop's position is four bytes off the air, so it gets what
	// every other coordinate from outside gets. A frame carrying NaN
	// there — another firmware, a garbled field, a peer with no fix that
	// wrote one anyway — compares false against any distance, which
	// turned the minimum-distance rule off for exactly the frames least
	// worth trusting. An unusable one is no last hop at all, and a hop
	// whose distance cannot be known cannot be too close.
	if m.RelayMinDistM > 0 && usablePosition(m.LastHopLat, m.LastHopLon) &&
		distance(pos.Lat, pos.Lon, m.LastHopLat, m.LastHopLon) < float64(m.RelayMinDistM) {
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
	// asked collects the peers this tick is asking about, so the throttle
	// below covers every reason for asking, not only staleness.
	asked := map[mesh.MAC]bool{}
	for _, mac := range n.order {
		p := n.peers[mac]
		n.updateStale(now, p)
		if p.viaMesh && now.Sub(p.lastHeard) >= meshStaleHeard &&
			!now.Before(p.meshNext) && p.meshCount <= meshPeerLimit {
			// A peer heard only through the mesh has gone quiet. Asking
			// for it counts against the same throttle as any other ask,
			// which means reading meshNext here and not only writing it
			// below: without that this path fired on every tick its group
			// slot came round, which is every five seconds rather than
			// the thirty MESH_SEND_FREQ_MS allows.
			asked[mac] = true
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
		// The lower MAC asks first. Comparing the bytes orders them the
		// same way as the firmware's integer compare, and allocates
		// nothing on a tick that runs every second.
		if p.meshGrp != n.meshGrp || now.Sub(p.firstStale) >= delay || bytes.Compare(p.mac[:], n.cfg.MAC[:]) <= 0 {
			send = true
		} else {
			// Only the wait is shortened, and only then: the firmware
			// takes its second off after it has compared the elapsed
			// time against the whole delivery time, not before.
			//
			// The modulus really is on milliseconds, odd as that reads.
			// compass.dis, in the stale-peer loop, is:
			//
			//	bc LOAD_FAST 12      # the delivery time, in ms
			//	86 LOAD_CONST_SMALL_INT 6
			//	f8 BINARY_OP 33 __mod__
			//	80 LOAD_CONST_SMALL_INT 0
			//	d9 BINARY_OP 2 __eq__
			//	...  22:87:68 LOAD_CONST_SMALL_INT 1000
			//	     e6 BINARY_OP 15 __isub__
			//
			// so 8200 ms is left alone and 8250 ms loses a second, which
			// is arbitrary — but it is what the device does, and the
			// point of this package is to behave like the device.
			if delay.Milliseconds()%6 == 0 {
				delay -= time.Second
			}
			p.meshNext = now.Add(delay)
		}
	}
	if !send {
		return
	}
	for mac, p := range n.peers {
		if p.stale || asked[mac] {
			p.meshNext = now.Add(meshSendFreq)
			p.meshCount++
		}
	}
	req := n.locate(now, true, 0)
	n.originUID = req.UID
	n.outbox[mesh.Broadcast] = mustMarshal(req)
	n.log.Info("asking the mesh for lost peers", "uid", req.UID)
}

// earthHalfCircumferenceM is as far apart as two devices on the planet
// can be, which is the widest a distance passed below can honestly be.
// A ceiling on the delay itself would be dead: the firmware's own
// arithmetic tops out at about six and a half days for a peer on the
// other side of the world, and this package's job is to behave like the
// device, so any limit that did not change that answer would be a
// branch that never runs.
const earthHalfCircumferenceM = 20_100_000

// meshDelivery is Parser.cal_mesh_delivery(dist, range=75).
func meshDelivery(distM float64) time.Duration {
	// A distance that is not one goes to the floor rather than into the
	// arithmetic. int(NaN) is implementation-defined — 0 on arm64, the
	// most negative int64 on amd64 — and from there the multiplications
	// below overflow and come out as a peer next asked about in two
	// hundred years, which is a peer never asked about again. The
	// callers all check their coordinates; this is the floor under them.
	if !(distM > 0) || math.IsInf(distM, 0) {
		distM = 0
	}
	// And a ceiling, because int() below is what overflows and a floor
	// alone does not stop a number that is merely enormous. Go leaves
	// that conversion implementation-defined when the value does not
	// fit, and this host saturates in a way that happens to land back
	// inside the answer's range — so no test here can show the
	// difference, the same as the NaN guard in scale(). The board is a
	// different compiler and a different architecture.
	distM = min(distM, earthHalfCircumferenceM)
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

// clonePosition takes a copy of a fix, so neither side can write through
// the other. Every place a position crosses this package's edge — in
// through New, out through Config and Sensors, and across a simulation
// stopping — needs the same thing, and had its own copy of it.
func clonePosition(p *Position) *Position {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// livePosition reports whether a position off the air is one a device
// could be at. A peer frame is bytes from a radio: nothing in the format
// stops a NaN, an infinity or a latitude of 900, and a poisoned
// coordinate would spread into the distance, the compass dial and the
// relay decision. What arrives is used only if it could be real.
func livePosition(lat, lon float32) bool {
	return !math.IsNaN(float64(lat)) && !math.IsNaN(float64(lon)) &&
		!math.IsInf(float64(lat), 0) && !math.IsInf(float64(lon), 0) &&
		lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// usablePosition is what every path that takes a position from outside
// has to ask: that something was reported at all, and that it is
// somewhere a device could be. Null Island is how the firmware says "no
// fix", so a pair of zeroes is an absence rather than a place.
//
// One helper rather than the same two-part test written out at each
// ingress: it was written out four times, and the fourth was added
// because the third had been missed.
func usablePosition(lat, lon float32) bool {
	return (lat != 0 || lon != 0) && livePosition(lat, lon)
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
	// Clamped: for two points near opposite sides of the world, rounding
	// can leave a just above 1, and Asin of that is NaN — which spreads
	// into the mesh delay (where int(NaN) is implementation-defined) and
	// into furthestPeer, where the builtin max carries it to every peer.
	//
	// Go's min returns NaN if either argument is one, so the clamp does
	// not stand in for this: a coordinate that arrives as NaN goes
	// straight through it. Every caller checks its coordinates first —
	// peerDistance through fix() and hasCoords, relay() through
	// usablePosition — and -1 is what the rest of this file already
	// means by "no distance", so a caller that forgets is answered with
	// that rather than with a number that poisons the arithmetic.
	//
	// The clamp is both ways round. Above 1 is the rounding at the far
	// side of the world; below 0 is the same rounding with a latitude
	// outside ±90, where the two terms cancel and the sign is left to
	// the last bit — and Sqrt of a negative is NaN just as surely.
	if math.IsNaN(a) {
		return -1
	}
	return 2 * r * math.Asin(math.Sqrt(min(max(a, 0), 1)))
}

// ---- Smart Group client ------------------------------------------------------

// onSmartGroup is the client half of Parser._smart_group. The node joins a
// group an owned Totem hosts, and on finalize bonds only with the owned
// members.
func (n *Node) onSmartGroup(now time.Time, rx Received, g mesh.SmartGroup) {
	switch g.Instruction {
	case mesh.SmartGroupAbandon:
		if n.inGroup && n.smartUID == g.UID {
			n.inGroup, n.smartUID = false, 0
			n.log.Info("smart group abandoned", "uid", g.UID)
		}
		return
	case mesh.SmartGroupAdvertise:
		if n.inGroup || !n.pairing {
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
		// The group is the bonding: a pairing window still open would end
		// by deleting whatever it had half-made — and tempBond may well
		// name a Totem the group has just bonded us to, so the timer
		// would undo the group's own work a few seconds later. Ended
		// here, before the members are added, so nothing it holds
		// survives into the list below.
		n.stopPairing(now)
		// peer_management.auto_bond_to_peers deletes every peer first.
		for _, mac := range slices.Clone(n.order) {
			n.deletePeer(mac)
		}
		for _, m := range g.Members {
			if m.MAC == n.cfg.MAC {
				// The group assigns this device a colour, and the crystal
				// is what shows it: setting only the config would leave
				// the two saying different things.
				//
				// It is one byte off the air from the host, and an id
				// outside the thirteen renders unlit — so one garbled
				// frame would blank the crystal, and State would write it
				// to flash for the next boot to find.
				c := paletteColor(n.log, "smart group", m.ColorID, n.leds.DefaultColor())
				n.cfg.ColorID = int8(c)
				n.leds.SetDefaultColor(c)
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
			if usablePosition(m.Lat, m.Lon) {
				p.hasCoords, p.lat, p.lon, p.coordsAt = true, m.Lat, m.Lon, now
			}
		}
		n.log.Info("smart group completed", "peers", len(n.peers), "color", n.cfg.ColorID)
	}
}

// ---- state for the driver ------------------------------------------------------

// FactoryReset is the device forgetting everything it worked out about
// where it is and what it is attached to: the bonds, and what the power
// model learned about this pack. Kept apart from ForgetPeers, whose name
// promises only the bonds — a caller that wants to drop bonds should not
// have to know it also throws away the battery calibration.
func (n *Node) FactoryReset(now time.Time) {
	// The window first: a reset during one would otherwise leave the
	// sibling still broadcasting bond requests, and the bond just dropped
	// would be back inside the same six seconds — which is the thing the
	// board's `store forget` exists to prevent.
	n.stopPairing(now)
	n.ForgetPeers(now)
	// Cleared, then read with the curve the reset leaves behind: a
	// reading taken before the clear is the percentage worked out with
	// the stretch being forgotten, which would stand until the next poll
	// and then jump — the thing the read is here to prevent.
	//
	// What this run worked out about the pack goes with the bonds. The
	// reading that follows measures it again while the device is on; a
	// reset on a device that is switched off leaves nothing, and the
	// next power-up starts the counters from zero anyway.
	n.power.ClearLearnedMaxVolts()
	n.read(now)
}

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
	if !usableClock(wall) {
		return fmt.Errorf("clock %s is outside %s to %s, which is what a peer frame can carry",
			wall.UTC().Format(time.RFC3339), minClock.Format("2006"), maxClock.Format("2006"))
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
		Motion: m, Bearing: bearing, NoFix: n.fix() == nil,
		// The battery carries over as it reads, 0% included: a simulation
		// started on a flat device must not report a full one.
		Percent: n.sensors.Battery.Percent, Charging: n.sensors.Battery.Charging,
		Flat: n.sensors.Orientation == mesh.OrientationHorizontal, Rand: n.rng,
	}
	if f := n.fix(); f != nil {
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
	if held.Fix = clonePosition(held.Fix); held.Fix != nil {
		held.Fix.SpeedKPH, held.Fix.Time = 0, time.Time{}
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
	// Refused here as well as ignored by fix(): a caller that asked for
	// something impossible should be told, not left thinking the device
	// is standing somewhere it is not. The console range-checks its own
	// arguments; this is the same rule for everyone else.
	if p != nil && !usablePosition(p.Lat, p.Lon) {
		return fmt.Errorf("no device could be at %v, %v", p.Lat, p.Lon)
	}
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
func (n *Node) Sensors() Sensors {
	// The Fix is a pointer into the node's own reading, so handing it out
	// would let a caller write a position through a method that looks
	// like a read — past usablePosition, which is the whole point of it.
	s := n.sensors
	s.Fix = clonePosition(n.sensors.Fix)
	return s
}

// Config returns the node's current settings.
func (n *Node) Config() Config {
	// A copy whose Owned list and Position are the caller's own. Owned is
	// the one invariant this package promises to keep — allowedTX reads
	// the same backing array — so handing it out would let a caller add a
	// Totem its owner never listed, through a method that looks like a
	// read. The position is the same story for fix().
	//
	// The rest is shared on purpose and by necessity: Rand, Sensors,
	// OTATransport and Logger are the node's actual collaborators, and a
	// copy of any of them would be a different thing. Drawing from Rand
	// in particular shifts the pairing jitter and the locate UIDs this
	// node will use, so a caller that wants randomness should bring its
	// own.
	c := n.cfg
	c.Owned = slices.Clone(n.cfg.Owned)
	c.Position = clonePosition(n.cfg.Position)
	return c
}
