package emulator

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ljagiello/totem-compass/mesh"
)

var (
	totem    = mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78} // the owned Totem
	self     = mesh.MAC{0x20, 0x9b, 0xa9, 0x70, 0xab, 0xb0} // the emulator
	stranger = mesh.MAC{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	t0       = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
)

type sent struct {
	at time.Time
	Packet
	msg mesh.Message
}

// harness drives a Node on a fake clock and plays the owned Totem.
type harness struct {
	t    *testing.T
	now  time.Time
	n    *Node
	sent []sent
}

func newHarness(t *testing.T, mut func(*Config)) *harness {
	t.Helper()
	cfg := Config{
		MAC: self, Owned: []mesh.MAC{totem}, BattVolts: 4.1, BattPct: 90,
		Rand:   rand.New(rand.NewPCG(1, 2)),
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mut != nil {
		mut(&cfg)
	}
	return &harness{t: t, now: t0, n: New(cfg, t0)}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(bytes.TrimRight(b, "\n")))
	return len(b), nil
}

func (h *harness) collect(ps []Packet) {
	h.t.Helper()
	for _, p := range ps {
		if !allowedTX(p.Dst, p.Data, h.n.cfg.Owned) {
			h.t.Fatalf("sent outside the owned scope: %s %x", p.Dst, p.Data)
		}
		m, err := mesh.Parse(p.Data)
		if err != nil {
			h.t.Fatalf("sent an unparseable frame %x: %v", p.Data, err)
		}
		h.sent = append(h.sent, sent{h.now, p, m})
	}
}

// advance runs the node's timers up to now+d.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	end := h.now.Add(d)
	for {
		next := h.n.Next()
		if next.IsZero() || next.After(end) {
			break
		}
		if next.After(h.now) {
			h.now = next
		}
		h.collect(h.n.Poll(h.now))
	}
	h.now = end
}

// rx delivers a frame from src.
func (h *harness) rx(src, dst mesh.MAC, rssi int8, m mesh.Message) {
	h.t.Helper()
	b, err := m.MarshalBinary()
	if err != nil {
		h.t.Fatal(err)
	}
	h.collect(h.n.Receive(h.now, Received{Src: src, Dst: dst, RSSI: rssi, Data: b}))
}

// take returns and forgets what was sent so far.
func (h *harness) take() []sent {
	s := h.sent
	h.sent = nil
	return s
}

func bondFrame(ack bool) mesh.Peer {
	return mesh.Peer{Command: mesh.PeerBond, Ack: ack, Name: "Lukasz", Orientation: mesh.OrientationVertical, TimeOfDayMs: -1, Unix: -1}
}

func statusFrame(at time.Time) mesh.Peer {
	return mesh.Peer{
		Command: mesh.PeerStatus, Name: "Lukasz", Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 4,
		Orientation: mesh.OrientationVertical, Unix: int32(at.Unix()),
		TimeOfDayMs: int32(at.Hour()*3600000 + at.Minute()*60000 + at.Second()*1000 + at.Nanosecond()/1e6),
	}
}

// bond runs the full P2P handshake and clears what was sent.
func (h *harness) bond() {
	h.t.Helper()
	h.collect(h.n.Pair(h.now))
	h.advance(30 * time.Millisecond)
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	h.advance(100 * time.Millisecond)
	h.rx(totem, self, -20, bondFrame(true))
	h.advance(pairingWindow)
	if len(h.n.Peers()) != 1 {
		h.t.Fatalf("peers after bonding = %v", h.n.Peers())
	}
	h.take()
}

func TestPairingHandshake(t *testing.T) {
	h := newHarness(t, nil)
	h.collect(h.n.Pair(h.now))
	h.advance(500 * time.Millisecond)
	for _, s := range h.take() {
		p := s.msg.(mesh.Peer)
		if s.Dst != mesh.Broadcast || p.Command != mesh.PeerBond || p.Ack {
			t.Fatalf("before a partner: %s %+v, want broadcast bond requests", s.Dst, p)
		}
	}

	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	if !slices.ContainsFunc(h.n.Peers(), func(p PeerInfo) bool { return p.MAC == totem }) {
		t.Fatal("the requester was not registered")
	}
	h.advance(300 * time.Millisecond)
	got := h.take()
	if len(got) < 3 {
		t.Fatalf("%d frames in 300 ms, want one every 50-99 ms", len(got))
	}
	for i, s := range got {
		p := s.msg.(mesh.Peer)
		if s.Dst != totem || !p.Ack || p.Command != mesh.PeerBond {
			t.Fatalf("after registering: %s %+v, want unicast bond confirmations", s.Dst, p)
		}
		if i > 0 {
			if gap := s.at.Sub(got[i-1].at); gap < 50*time.Millisecond || gap >= 100*time.Millisecond {
				t.Errorf("gap %v, want randrange(50, 100) ms", gap)
			}
		}
	}

	h.rx(totem, self, -20, bondFrame(true))
	h.advance(pairingWindow)
	if h.n.Pairing() {
		t.Error("still pairing after the window")
	}
	if ps := h.n.Peers(); len(ps) != 1 || ps[0].MAC != totem {
		t.Fatalf("peers = %v, want the Totem", ps)
	}
}

func TestPairingRejectsWeakSignal(t *testing.T) {
	h := newHarness(t, nil)
	h.collect(h.n.Pair(h.now))
	h.rx(totem, mesh.Broadcast, -26, bondFrame(false))
	if len(h.n.Peers()) != 0 {
		t.Fatal("bonded to a request below BONDING_RSSI")
	}
}

func TestPairingRollsBackHalfBond(t *testing.T) {
	h := newHarness(t, nil)
	h.collect(h.n.Pair(h.now))
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	h.advance(pairingWindow)
	if len(h.n.Peers()) != 0 {
		t.Fatalf("a bond without the confirmation survived: %v", h.n.Peers())
	}
}

func TestBondRequestIgnoredWhenNotPairing(t *testing.T) {
	h := newHarness(t, nil)
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	h.advance(time.Second)
	if len(h.n.Peers()) != 0 || h.n.Pairing() {
		t.Fatal("a bond request started pairing without AutoPair")
	}
}

func TestAutoPair(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AutoPair = true })
	h.rx(totem, mesh.Broadcast, -60, bondFrame(false))
	if h.n.Pairing() {
		t.Fatal("auto-paired with a distant Totem")
	}
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	if !h.n.Pairing() || len(h.n.Peers()) != 1 {
		t.Fatalf("pairing=%v peers=%v, want the Totem registered", h.n.Pairing(), h.n.Peers())
	}
	if s := h.take(); len(s) == 0 || s[len(s)-1].Dst != mesh.Broadcast {
		t.Fatalf("auto-pair did not start the request loop: %v", s)
	}
	h.advance(100 * time.Millisecond)
	if s := h.take(); len(s) == 0 || s[len(s)-1].Dst != totem {
		t.Fatalf("no confirmation unicast after registering: %v", s)
	}
}

func TestAlreadyBondedAcks(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.rx(totem, self, -50, statusFrame(h.now))
	h.advance(time.Second)
	h.take()

	h.collect(h.n.Pair(h.now))
	h.take()
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	h.advance(time.Second)
	var acks []sent
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerBond && p.Ack && s.Dst == totem {
			acks = append(acks, s)
		}
	}
	if len(acks) != ackBurstCount {
		t.Fatalf("%d acks, want %d", len(acks), ackBurstCount)
	}
	for i := 1; i < len(acks); i++ {
		if gap := acks[i].at.Sub(acks[i-1].at); gap != ackBurstGap {
			t.Errorf("ack gap %v, want %v", gap, ackBurstGap)
		}
	}
	if h.n.Pairing() {
		t.Error("still pairing after answering an already-bonded peer")
	}
}

func TestStatusWithoutClock(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.advance(20 * time.Second)
	var at []time.Time
	for _, s := range h.take() {
		if p := s.msg.(mesh.Peer); p.Command != mesh.PeerStatus || s.Dst != totem {
			t.Fatalf("unexpected %s %+v", s.Dst, p)
		}
		at = append(at, s.at)
	}
	// Speed 6: a window every 5 s with up to three bursts in its first 2.5 s.
	if len(at) < 4 || len(at) > 12 {
		t.Fatalf("%d status frames in 20 s", len(at))
	}
}

func TestClockAdoptionAlignsWindows(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.advance(rtcSyncDelay)
	peerClock := t0.Add(3*time.Hour + 123*time.Millisecond) // the Totem's GNSS time
	h.rx(totem, self, -50, statusFrame(peerClock.Add(h.now.Sub(t0))))
	h.take()
	h.advance(20 * time.Second)
	got := h.take()
	if len(got) < 4 {
		t.Fatalf("%d frames in 20 s with a clock", len(got))
	}
	for i, s := range got {
		wall := h.n.wall(s.at)
		if ms := wall.UnixMilli() % 4000; ms != windowTXDelay.Milliseconds() {
			t.Errorf("status %d at wall %s: %d ms into the 4 s slot, want %d", i, wall.Format("15:04:05.000"), ms, windowTXDelay.Milliseconds())
		}
		if p := s.msg.(mesh.Peer); p.Unix != -1 || p.TimeOfDayMs != -1 {
			t.Errorf("a borrowed clock is advertised: %+v", p)
		}
	}
}

func TestClockNotAdoptedEarly(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.rx(totem, self, -50, statusFrame(t0.Add(time.Hour)))
	if h.n.clockSet {
		t.Fatal("adopted a peer clock inside rtc_no_peer_sync_until")
	}
}

func TestStrangersIgnored(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AutoPair = true })
	h.collect(h.n.Pair(h.now))
	h.take()
	h.rx(stranger, mesh.Broadcast, -10, bondFrame(false))
	h.rx(stranger, self, -10, statusFrame(t0))
	h.rx(stranger, mesh.Broadcast, -10, mesh.SmartGroup{Instruction: mesh.SmartGroupAdvertise, UID: 5})
	if len(h.n.Peers()) != 0 {
		t.Fatalf("a stranger became a peer: %v", h.n.Peers())
	}
	for _, s := range h.take() {
		if s.Dst != mesh.Broadcast {
			t.Fatalf("answered a stranger: %v", s)
		}
	}
}

func TestRestoreBondFromUnicastStatus(t *testing.T) {
	h := newHarness(t, nil)
	h.rx(totem, mesh.Broadcast, -50, statusFrame(t0))
	if len(h.n.Peers()) != 0 {
		t.Fatal("a broadcast status restored a bond")
	}
	h.rx(totem, self, -50, statusFrame(t0))
	if ps := h.n.Peers(); len(ps) != 1 || ps[0].Status.Name != "Lukasz" {
		t.Fatalf("peers = %v, want the Totem restored", ps)
	}
}

func TestLocateReplyAndRelay(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.advance(rtcSyncDelay)
	h.rx(totem, self, -50, statusFrame(t0.Add(h.now.Sub(t0))))
	h.advance(61 * time.Second) // not heard directly for over a minute
	h.take()

	req := mesh.Locate{
		Origin: totem, Lat: 37.7749, Lon: -122.4194, UID: 4242, ReplyRequested: true,
		MinRSSI: -127, MaxHops: 99, Expiry: int32(h.n.wall(h.now).Unix()) + 120, RelayMinDistM: 10,
	}
	h.rx(totem, mesh.Broadcast, -70, req)
	got := h.take()
	if len(got) != 2 {
		t.Fatalf("sent %d frames, want a reply and a relay: %v", len(got), got)
	}
	reply, relay := got[0].msg.(mesh.Locate), got[1].msg.(mesh.Locate)
	if reply.Origin != self || reply.ReplyRequested || reply.UID == req.UID {
		t.Errorf("reply = %+v", reply)
	}
	if relay.UID != req.UID || relay.Hops != 1 || relay.Origin != totem || relay.LastHopLat != 37.775 {
		t.Errorf("relay = %+v", relay)
	}

	h.rx(totem, mesh.Broadcast, -70, req)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("answered a duplicate UID: %v", s)
	}

	// The one repeat of the reply, same UID, in a window at a second
	// divisible by 4.
	h.advance(10 * time.Second)
	var repeats int
	for _, s := range h.take() {
		if l, ok := s.msg.(mesh.Locate); ok && l.UID == reply.UID {
			repeats++
			if sec := h.n.wall(s.at).Second(); sec%4 != 0 {
				t.Errorf("repeat at second %d", sec)
			}
		}
	}
	if repeats != 1 {
		t.Errorf("%d reply repeats, want 1", repeats)
	}
}

// TestMeshAsksForLostPeer: once a bonded Totem goes quiet long enough to be
// stale, the node broadcasts locate requests in its own mesh slot, at most
// one per MESH_SEND_FREQ_MS. meshTick used to run only once.
func TestMeshAsksForLostPeer(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.advance(rtcSyncDelay)
	h.rx(totem, self, -50, statusFrame(t0.Add(h.now.Sub(t0))))
	heard := h.now
	h.take()
	h.advance(10 * time.Minute)
	var requests []time.Time
	for _, s := range h.take() {
		if l, ok := s.msg.(mesh.Locate); ok && l.Origin == self && l.ReplyRequested {
			if s.Dst != mesh.Broadcast {
				t.Errorf("locate request unicast to %s", s.Dst)
			}
			requests = append(requests, s.at)
		}
	}
	if len(requests) < 5 {
		t.Fatalf("%d locate requests in 10 min for a lost peer", len(requests))
	}
	if stale := requests[0].Sub(heard); stale < 2*time.Minute {
		t.Errorf("first request %v after the peer was heard, before it went stale", stale)
	}
	for i := 1; i < len(requests); i++ {
		if gap := requests[i].Sub(requests[i-1]); gap < meshSendFreq {
			t.Errorf("requests %v apart, want at least %v", gap, meshSendFreq)
		}
	}
}

func TestLocateIgnoredWithoutFix(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.advance(rtcSyncDelay)
	h.rx(totem, self, -50, statusFrame(t0.Add(h.now.Sub(t0))))
	h.take()
	h.rx(totem, mesh.Broadcast, -70, mesh.Locate{Origin: totem, UID: 1, ReplyRequested: true, MinRSSI: -127, MaxHops: 99})
	if s := h.take(); len(s) != 0 {
		t.Fatalf("a node without a fix used the mesh: %v", s)
	}
}

func TestSmartGroupClient(t *testing.T) {
	h := newHarness(t, nil)
	beacon := mesh.SmartGroup{Instruction: mesh.SmartGroupAdvertise, UID: 777, TimeoutMs: 30000}
	h.rx(totem, mesh.Broadcast, -30, beacon)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("joined without pairing: %v", s)
	}

	h.collect(h.n.Pair(h.now))
	h.take()
	h.rx(totem, mesh.Broadcast, -50, beacon)
	if s := h.take(); slices.ContainsFunc(s, func(s sent) bool { _, ok := s.msg.(mesh.SmartGroupReply); return ok }) {
		t.Fatal("joined a host below SMART_GROUP_RSSI")
	}
	h.rx(totem, mesh.Broadcast, -30, beacon)
	h.advance(250 * time.Millisecond)
	var joins []sent
	for _, s := range h.take() {
		if r, ok := s.msg.(mesh.SmartGroupReply); ok {
			if r.UID != 777 || r.Leave || s.Dst != mesh.Broadcast {
				t.Fatalf("join = %+v to %s", r, s.Dst)
			}
			joins = append(joins, s)
		}
	}
	if len(joins) != 2 || joins[1].at.Sub(joins[0].at) != joinGap {
		t.Fatalf("joins = %v, want two 200 ms apart", joins)
	}

	beacon.Members = []mesh.SmartGroupMember{{MAC: totem}, {MAC: self}, {MAC: stranger}}
	h.rx(totem, mesh.Broadcast, -30, beacon)
	final := beacon
	final.Instruction = mesh.SmartGroupFinalize
	final.Members = []mesh.SmartGroupMember{{MAC: totem, ColorID: 3}, {MAC: self, ColorID: 5}, {MAC: stranger, ColorID: 7}}
	h.rx(totem, mesh.Broadcast, -60, final)
	if ps := h.n.Peers(); len(ps) != 1 || ps[0].MAC != totem {
		t.Fatalf("peers = %v, want only the owned Totem", ps)
	}
	if h.n.Config().ColorID != 5 {
		t.Errorf("color = %d, want the host's pick 5", h.n.Config().ColorID)
	}
}

func TestUnbondNotice(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.collect(h.n.Unbond(h.now, totem))
	h.advance(6 * time.Second)
	var notices int
	for _, s := range h.take() {
		p := s.msg.(mesh.Peer)
		if p.Command == mesh.PeerStatus {
			t.Fatal("status sent to a deleted peer")
		}
		if p.Command == mesh.PeerUnbond && s.Dst == totem {
			notices++
		}
	}
	if notices != 1 {
		t.Errorf("%d unbond notices, want 1", notices)
	}
}

func TestAllowedTX(t *testing.T) {
	owned := []mesh.MAC{totem}
	for _, tt := range []struct {
		dst  mesh.MAC
		b    []byte
		want bool
	}{
		{totem, []byte{0xa7, 0x74, 0, 0}, true},
		{mesh.Broadcast, []byte{0xa7, 0x74, 2, 0}, true},
		{mesh.Broadcast, []byte{0xa7, 0x74, 7, 1}, true},
		{stranger, []byte{0xa7, 0x74, 0, 0}, false},
		{mesh.Broadcast, []byte{0xa7, 0x74, 1, 6}, false}, // demi-god power control
		{mesh.Broadcast, []byte{0xa7, 0x74, 7, 0}, false}, // Smart Group host beacon
		{totem, []byte{0xa7, 0x74, 9, 0}, false},
		{totem, []byte{0xa7}, false},
	} {
		if got := allowedTX(tt.dst, tt.b, owned); got != tt.want {
			t.Errorf("allowedTX(%s, %x) = %v, want %v", tt.dst, tt.b, got, tt.want)
		}
	}
}

func TestDefaultName(t *testing.T) {
	if got := New(Config{MAC: self}, t0).Config().Name; got != "emu_totem_abb0" {
		t.Errorf("default name = %q, want emu_totem_abb0", got)
	}
	if got := New(Config{MAC: self, Name: "Base Camp"}, t0).Config().Name; got != "Base Camp" {
		t.Errorf("configured name = %q, want Base Camp", got)
	}
}

func TestMeshGroup(t *testing.T) {
	// int.from_bytes(hashlib.sha256(bytes.fromhex(mac)).digest()[:4], 'big') % 5
	for mac, want := range map[mesh.MAC]int{totem: 0, self: 2} {
		if got := meshGroup(mac); got != want {
			t.Errorf("meshGroup(%s) = %d, want %d", mac, got, want)
		}
	}
}

// records is a slog handler that keeps every record.
type records struct{ rs *[]slog.Record }

func (records) Enabled(context.Context, slog.Level) bool { return true }
func (h records) Handle(_ context.Context, r slog.Record) error {
	*h.rs = append(*h.rs, r)
	return nil
}
func (h records) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h records) WithGroup(string) slog.Handler      { return h }

func recorded(rs []slog.Record, level slog.Level, msg string) int {
	n := 0
	for _, r := range rs {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// TestBondLoggedOnce: a Totem keeps sending confirmations until its
// pairing window closes; only the first one is news.
func TestBondLoggedOnce(t *testing.T) {
	var rs []slog.Record
	h := newHarness(t, func(c *Config) { c.Logger = slog.New(records{&rs}) })
	h.collect(h.n.Pair(h.now))
	h.rx(totem, mesh.Broadcast, -20, bondFrame(false))
	for range 5 {
		h.advance(90 * time.Millisecond)
		h.rx(totem, self, -20, bondFrame(true))
	}
	if n := recorded(rs, slog.LevelInfo, "bonded"); n != 1 {
		t.Errorf("logged bonded %d times, want 1", n)
	}
}

// TestLogKeysAvoidBuiltins: the firmware's slog handler rewrites the
// built-in keys (it logs uptime as the time), so the node must not use
// them for its own attributes.
func TestLogKeysAvoidBuiltins(t *testing.T) {
	var rs []slog.Record
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(records{&rs})
		c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}
	})
	h.bond()
	h.advance(rtcSyncDelay)
	h.rx(totem, self, -50, statusFrame(t0.Add(h.now.Sub(t0))))
	h.advance(time.Minute)
	if recorded(rs, slog.LevelInfo, "RTC set via peer") != 1 {
		t.Fatal("the clock was not adopted")
	}
	for _, r := range rs {
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey, slog.MessageKey, slog.SourceKey:
				t.Errorf("%q logs the built-in key %q", r.Message, a.Key)
			}
			return true
		})
	}
}

// TestLocateOnTheMeridian: a Totem standing on the prime meridian sends a
// true zero for longitude. Requiring both coordinates to be non-zero
// dropped its position, and the distance to it with it.
func TestLocateOnTheMeridian(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 51.4779, Lon: 0.0015, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil { // the mesh needs a clock
		t.Fatal(err)
	}
	h.advance(61 * time.Second)
	h.take()
	h.rx(totem, mesh.Broadcast, -70, mesh.Locate{
		Origin: totem, Lat: 51.4779, Lon: 0, UID: 77, MinRSSI: -127, MaxHops: 99,
		Expiry: int32(h.n.wall(h.now).Unix()) + 120, RelayMinDistM: 10,
	})
	p, ok := h.n.peers[totem]
	if !ok {
		t.Fatal("the peer is gone")
	}
	if !p.hasCoords || p.lat != 51.4779 || p.lon != 0 {
		t.Errorf("coords after a locate from the meridian: has=%v lat=%v lon=%v", p.hasCoords, p.lat, p.lon)
	}
}

// TestNameLongerThanAFrame: every status frame carries the name, so a name
// the frame cannot hold has to be cut when the node starts. It used to
// reach MarshalBinary and panic on the first unbond.
func TestNameLongerThanAFrame(t *testing.T) {
	long := strings.Repeat("é", mesh.MaxPeerName) // 2 bytes a rune, so twice over
	h := newHarness(t, func(c *Config) { c.Name = long })
	name := h.n.Config().Name
	if len(name) > mesh.MaxPeerName || !utf8.ValidString(name) {
		t.Fatalf("name = %q (%d bytes), want at most %d bytes of valid UTF-8", name, len(name), mesh.MaxPeerName)
	}
	h.bond()
	h.collect(h.n.Unbond(h.now, totem)) // panicked here
	h.advance(time.Second)
	if len(h.take()) == 0 {
		t.Error("no unbond frame")
	}
}

// TestMeshAskIsThrottledOnEveryPath: a peer heard only through the mesh
// that has gone quiet is a reason to ask the mesh where it is — but it
// counts against the same MESH_SEND_FREQ_MS throttle as any other
// reason. It used to bypass it and fire every time the group slot came
// round, which is every five seconds rather than every thirty.
func TestMeshAskIsThrottledOnEveryPath(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	// A peer this node has only ever heard relayed, and not lately.
	p := h.n.peers[totem]
	p.viaMesh, p.heard, p.lastHeard = true, true, h.now
	h.advance(meshStaleHeard + time.Second)
	h.take()

	h.advance(2 * time.Minute)
	var asks int
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Locate); ok && m.Origin == self && m.ReplyRequested {
			asks++
		}
	}
	if asks == 0 {
		t.Fatal("the node never asked the mesh about a peer it had lost")
	}
	// Two minutes at one ask per thirty seconds is four, with a little
	// room for where the window falls.
	if asks > 6 {
		t.Errorf("%d mesh requests in two minutes, want about one every %s", asks, meshSendFreq)
	}
}

// TestImpossiblePositionsAreIgnored: a peer frame is bytes off a radio,
// and nothing in the format stops a NaN, an infinity or a latitude of
// 900. A poisoned coordinate would spread into the distance, the compass
// dial and the relay decision, so what arrives is used only if a device
// could be at it.
func TestImpossiblePositionsAreIgnored(t *testing.T) {
	good := float32(37.775)
	for _, bad := range []struct {
		name     string
		lat, lon float32
	}{
		{"nan latitude", float32(math.NaN()), -122.42},
		{"nan longitude", good, float32(math.NaN())},
		{"infinite latitude", float32(math.Inf(1)), -122.42},
		{"latitude past the pole", 900, -122.42},
		{"longitude past the meridian", good, 1000},
	} {
		h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: good, Lon: -122.42, AccuracyM: 3} })
		h.bond()
		// A good position first, so there is something to poison.
		ok := statusFrame(t0)
		ok.Lat, ok.Lon = 37.785, -122.42
		h.rx(totem, self, -40, ok)
		was := h.n.Peers()[0].DistanceM

		poison := statusFrame(t0)
		poison.Lat, poison.Lon = bad.lat, bad.lon
		h.rx(totem, self, -40, poison)

		got := h.n.Peers()[0].DistanceM
		if math.IsNaN(got) {
			t.Errorf("%s made the distance NaN", bad.name)
		}
		if got != was {
			t.Errorf("%s moved the peer from %.0f m to %.0f m", bad.name, was, got)
		}
		// And the compass still points somewhere real.
		h.advance(bootAnim + time.Second)
		if desc := h.n.LEDs().Describe(); strings.Contains(desc, "NaN") {
			t.Errorf("%s reached the ring: %s", bad.name, desc)
		}
	}
}
