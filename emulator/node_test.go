package emulator

import (
	"bytes"
	"context"
	"errors"
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

	// Ten minutes, so the throttled and unthrottled rates are far enough
	// apart to tell apart: at one ask every thirty seconds that is twenty,
	// and on the unthrottled path — every five-second group slot — it was
	// thirty. Two minutes could not distinguish them, because six was both
	// the ceiling this asserted and exactly what the bug produced.
	const run = 10 * time.Minute
	h.advance(run)
	var asks int
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Locate); ok && m.Origin == self && m.ReplyRequested {
			asks++
		}
	}
	if asks == 0 {
		t.Fatal("the node never asked the mesh about a peer it had lost")
	}
	if want := int(run/meshSendFreq) + 2; asks > want {
		t.Errorf("%d mesh requests in %s, want at most %d — one every %s",
			asks, run, want, meshSendFreq)
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

// TestConfigCannotWidenTheOwnedScope: Config() looks like a read, and the
// owned list is the one invariant this package promises to keep. Handing
// out the same backing array let a caller add a Totem its owner never
// listed.
func TestConfigCannotWidenTheOwnedScope(t *testing.T) {
	h := newHarness(t, nil)
	cfg := h.n.Config()
	if len(cfg.Owned) == 0 {
		t.Fatal("no owned Totems to test with")
	}
	cfg.Owned[0] = stranger
	if slices.Contains(h.n.Config().Owned, stranger) {
		t.Error("a caller widened the owned scope through Config()")
	}
	if err := h.n.AddBond(stranger, "nope", h.now); err == nil {
		t.Error("the node bonded with a Totem outside its owned scope")
	}
}

func manyOwned() []mesh.MAC {
	out := make([]mesh.MAC, 0, maxBonds)
	for i := range maxBonds {
		out = append(out, mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, byte(0x80 + i)})
	}
	return out
}

// TestNewTakesItsOwnCopies: Config() and Sensors() hand out copies, but
// the way in was the same hole — a caller that kept the slice or the
// pointer it passed to New could widen the owned scope, or write a
// position, afterwards.
func TestNewTakesItsOwnCopies(t *testing.T) {
	owned := []mesh.MAC{totem}
	pos := &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}
	h := newHarness(t, func(c *Config) { c.Owned, c.Position = owned, pos })

	owned[0] = stranger
	if got := h.n.Config().Owned; got[0] == stranger {
		t.Error("a caller widened the owned scope after New")
	}
	if err := h.n.AddBond(stranger, "nope", h.now); err == nil {
		t.Error("the node bonded with a Totem outside its owned scope")
	}

	// A poll, so the node takes a fresh reading: without one the fix it
	// answers with is a copy staticSensors made during New, and the
	// caller's write could not have reached it either way.
	pos.Lat = float32(math.NaN())
	h.collect(h.n.Poll(h.now))
	if f := h.n.fix(); f == nil || math.IsNaN(float64(f.Lat)) {
		t.Error("a caller wrote a position through the Config it passed to New")
	}
	if got := h.n.Config().Position; got == nil || math.IsNaN(float64(got.Lat)) {
		t.Error("the node's own configured position was written from outside")
	}
}

// TestEveryReadingIsActedOn: the mode is not an opinion about the
// battery, it is the battery — so it follows every reading, from the one
// place that takes them. Five of the ten callers of read() paired it
// with applyPowerMode and five did not, so `batt 2` on the console
// reported power mode normal until something else polled, and a
// simulation swapped in on a flat pack went on running.
func TestEveryReadingIsActedOn(t *testing.T) {
	t.Run("a console battery", func(t *testing.T) {
		h := newHarness(t, nil)
		h.advance(bootAnim + time.Second)
		if err := h.n.SetBattery(2, false, h.now); err != nil {
			t.Fatal(err)
		}
		if got := h.n.Power().Mode(); got == PowerNormal {
			t.Error("a pack at 2 percent still reads as mode normal")
		}
	})

	t.Run("a simulation swapped in", func(t *testing.T) {
		h := newHarness(t, nil)
		h.advance(bootAnim + time.Second)
		h.n.SetSensors(NewStatic(Sensors{Battery: Battery{Volts: 3.1, Percent: 40}}), h.now)
		if !h.n.Power().Off() {
			t.Error("a source reporting a pack under the cutoff left the device running")
		}
	})

	t.Run("a clock", func(t *testing.T) {
		pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
		h := newHarness(t, func(c *Config) { c.Sensors = pack })
		h.advance(bootAnim + time.Second)
		pack.b = Battery{Volts: 3.1, Percent: 40}
		if err := h.n.SetClock(t0, h.now); err != nil {
			t.Fatal(err)
		}
		if !h.n.Power().Off() {
			t.Error("the reading taken with the clock was not acted on")
		}
	})
}

// TestABatteryWarningIsSaidOnce: read() runs on every pass of a loop
// that polls every 5 ms, and the check moved into it says its piece
// there. A driver stuck out of range filled the only diagnostic channel
// the board has with two hundred copies a second of the same line,
// drowning everything else and burning serial time in the loop that has
// to keep feeding the watchdog.
func TestABatteryWarningIsSaidOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(*Config)
		want string
	}{
		{"a voltage no frame can carry", func(c *Config) { c.BattVolts = 131072 }, "battery voltage"},
		{"a percentage that is not one", func(c *Config) { c.BattPct = 120 }, "battery percentage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			h := newHarness(t, func(c *Config) {
				tc.cfg(c)
				c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
			})
			// A second of the board's own polling.
			for range 200 {
				h.now = h.now.Add(5 * time.Millisecond)
				h.collect(h.n.Poll(h.now))
			}
			if got := strings.Count(logged.String(), tc.want); got != 1 {
				t.Errorf("the warning was said %d times in a second of polling", got)
			}
		})
	}
}

// TestARealReadingIsNotZeroed: the check in read() is about what a peer
// frame can carry, not about what a cell is likely to read. A board
// whose divider reads a little high is still telling the truth, and a
// real Totem on a charger reads 4.48 — zeroing either would be this
// device lying about its own battery over a tenth of a volt.
func TestARealReadingIsNotZeroed(t *testing.T) {
	for _, v := range []float32{3.1, 4.2, 4.48, 4.62, 5.0} {
		h := newHarness(t, func(c *Config) { c.BattVolts = v })
		if got := h.n.Sensors().Battery.Volts; got != v {
			t.Errorf("a reading of %v V came back as %v", v, got)
		}
	}
	// And what the frame cannot hold is refused, whatever it claims.
	for _, v := range []float32{131072, 1e6, float32(math.NaN()), float32(math.Inf(1))} {
		h := newHarness(t, func(c *Config) { c.BattVolts = v })
		if got := h.n.Sensors().Battery.Volts; got != 0 {
			t.Errorf("a reading of %v was kept as %v", v, got)
		}
	}
}

// TestSetBatterySaysNo: a caller that asked for something impossible
// should be told, which is the rule SetPosition already states. Left to
// read(), the percentage was quietly zeroed and the caller went on
// believing the device was at 120%.
func TestSetBatterySaysNo(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	for _, pct := range []int8{-50, 101, 120} {
		if err := h.n.SetBattery(pct, false, h.now); err == nil {
			t.Errorf("SetBattery(%d) was accepted", pct)
		}
	}
	if err := h.n.SetBattery(55, false, h.now); err != nil {
		t.Errorf("SetBattery(55): %v", err)
	}
	if got := h.n.Sensors().Battery.Percent; got != 55 {
		t.Errorf("the battery reads %d%% after being set to 55", got)
	}
}

// TestFixIsWhatTheAirSees: a driver may report a position this node
// refuses, and a caller showing one should show the same one every frame
// carries. The board's console writes JSON, which cannot hold a NaN at
// all, so a receiver reporting one turned the whole self line into an
// error string.
func TestFixIsWhatTheAirSees(t *testing.T) {
	nan := float32(math.NaN())
	for _, tc := range []struct {
		name     string
		lat, lon float32
		want     bool
	}{
		{"a real position", 37.7749, -122.4194, true},
		{"null island", 0, 0, false},
		{"not a number", nan, nan, false},
		{"off the globe", 400, 900, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				c.Sensors = NewStatic(Sensors{
					Fix:     &Fix{Lat: tc.lat, Lon: tc.lon, AccuracyM: 3},
					Battery: Battery{Volts: 4.1, Percent: 90},
				})
			})
			if got := h.n.Fix() != nil; got != tc.want {
				t.Errorf("Fix() reports a position: %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAHeadingThatIsNotABearing: SetHeading refuses its own argument,
// but that is one door of several — a config, a driver or a simulation
// all reach the reading, and an azimuth of 900 went into every status
// frame through any of them.
func TestAHeadingThatIsNotABearing(t *testing.T) {
	// The exported setter says no.
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	for _, deg := range []int16{-1, 360, 900, -30} {
		if err := h.n.SetHeading(deg, h.now); err == nil {
			t.Errorf("SetHeading(%d) was accepted", deg)
		}
	}
	if err := h.n.SetHeading(187, h.now); err != nil {
		t.Errorf("SetHeading(187): %v", err)
	}

	// And a reading that arrives another way is not sent either.
	h = newHarness(t, func(c *Config) { c.Heading = 900 })
	if got := h.n.Sensors().Azimuth; got < 0 || got > 359 {
		t.Errorf("a configured heading of 900 came back as %d", got)
	}
	if got := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; got < 0 || got > 359 {
		t.Errorf("a status frame carried an azimuth of %d", got)
	}
}

// TestUptimeHasBothEnds: the field is a uint16 of minutes. A board up
// 45 days wrapped to zero and told every peer it had just started; a
// clock that runs backwards made it negative, and uint16 of that is 45
// days of uptime rather than none.
func TestUptimeHasBothEnds(t *testing.T) {
	h := newHarness(t, nil)
	for _, d := range []time.Duration{
		0, time.Minute, 24 * time.Hour,
		46 * 24 * time.Hour, // past what the field holds
		400 * 24 * time.Hour,
	} {
		got := h.n.status(t0.Add(d), mesh.PeerStatus, false).UptimeMin
		want := uint16(min(d/time.Minute, math.MaxUint16))
		if got != want {
			t.Errorf("after %v the frame says %d minutes, want %d", d, got, want)
		}
	}
	// And a stamp before the node was built says none, not 45 days.
	if got := h.n.status(t0.Add(-time.Hour), mesh.PeerStatus, false).UptimeMin; got != 0 {
		t.Errorf("a clock that ran backwards gave an uptime of %d minutes", got)
	}
}

// TestConfigReportsWhereTheDeviceIsNow: cfg.Position is what the node
// was built or last told, and the sources hand out a fresh reading every
// time. A walking simulation moved and Config() went on answering with
// where it set off from.
func TestConfigReportsWhereTheDeviceIsNow(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 52.2, Lon: 21.0, AccuracyM: 3}, h.now); err != nil {
		t.Fatal(err)
	}
	start := h.n.Config().Position
	if start == nil || start.Lat != 52.2 {
		t.Fatalf("the position did not take: %+v", start)
	}

	h.n.SetSensors(NewSim(SimConfig{
		Lat: 52.2, Lon: 21.0, Motion: Walk, Bearing: 90, Percent: -1,
	}, h.now), h.now)
	h.advance(10 * time.Minute)

	got := h.n.Config().Position
	if got == nil {
		t.Fatal("a walking device reports no position")
	}
	if got.Lon == start.Lon {
		t.Errorf("after ten minutes of walking east the position is still %v", got.Lon)
	}
}

// TestSetBatteryAndHeadingReportTheirRefusals: the setters say no rather
// than recording something else, which is the rule SetPosition states.
func TestSetBatteryAndHeadingReportTheirRefusals(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second)
	if err := h.n.SetBattery(120, false, h.now); err == nil {
		t.Error("SetBattery(120) was accepted")
	}
	if err := h.n.SetHeading(900, h.now); err == nil {
		t.Error("SetHeading(900) was accepted")
	}
	// Nothing is asserted about the reading here. Both setters return
	// before anything touches the sensors, so a check on them — or on
	// the frame, which status() builds from the same sensors — cannot
	// fail whatever the setters do. What matters is that a value that
	// gets past a setter does not reach the air, and that is
	// TestAHeadingThatIsNotABearing, which drives one in through a door
	// with no check on it at all.
	if err := h.n.SetPosition(&Position{Lat: 91, Lon: 0}, h.now); err == nil {
		t.Error("SetPosition(91) was accepted")
	}
}

// TestASentinelIsNotABearing: the compass field has no value meaning
// "unknown", and -1 is what this package writes when it means that —
// HeadingOfMotion, SpeedKPH and PosAccuracyM all use it. Folding an
// out-of-range reading modulo 360 turned that -1 into 359, one degree
// west of north, and put it on the air as a direction every peer's
// compass would point in. SetHeading refuses -1 outright, so the two
// doors disagreed about the same value.
func TestASentinelIsNotABearing(t *testing.T) {
	pack := Battery{Volts: 4.1, Percent: 90}
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Azimuth: 187, Battery: pack})
	})
	h.advance(time.Second)
	if got := h.n.Sensors().Azimuth; got != 187 {
		t.Fatalf("the compass reads %d before anything went wrong", got)
	}

	for _, bad := range []int16{-1, -30, 360, 900, -32768} {
		h.n.SetSensors(NewStatic(Sensors{Azimuth: bad, Battery: pack}), h.now)
		if got := h.n.Sensors().Azimuth; got != 187 {
			t.Errorf("a reading of %d became %d; the last bearing the compass saw was 187", bad, got)
		}
		// And it is not on the air either.
		if f := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; f != 187 {
			t.Errorf("a reading of %d reached the air as %d", bad, f)
		}
	}

	// A real bearing still takes.
	h.n.SetSensors(NewStatic(Sensors{Azimuth: 42, Battery: pack}), h.now)
	if got := h.n.Sensors().Azimuth; got != 42 {
		t.Errorf("a bearing of 42 came back as %d", got)
	}
}

// TestAnOrientationTheFrameHasAMeaningFor: New settles this for what it
// is given, and a source set later never passes through New — so a
// driver that leaves the field alone told every peer "unknown", and
// Config() handed the same out through the front door.
func TestAnOrientationTheFrameHasAMeaningFor(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 4.1, Percent: 90}})
	})
	// Vertical, not merely "not unknown": three values mean anything in
	// this field and the frame carries whatever it is handed, so a test
	// that accepts everything else would pass on a 9.
	if got := h.n.Sensors().Orientation; got != mesh.OrientationVertical {
		t.Errorf("a source that said nothing about orientation reads as %v", got)
	}
	if got := h.n.Config().Orientation; got != mesh.OrientationVertical {
		t.Errorf("Config reports an orientation of %v", got)
	}
	if got := h.n.status(h.now, mesh.PeerStatus, false).Orientation; got != mesh.OrientationVertical {
		t.Errorf("a status frame carried an orientation of %v", got)
	}

	// And a value the frame has no meaning for does not reach it.
	for _, bad := range []mesh.Orientation{9, 127, -1} {
		h.n.SetSensors(NewStatic(Sensors{
			Orientation: bad, Battery: Battery{Volts: 4.1, Percent: 90},
		}), h.now)
		if got := h.n.status(h.now, mesh.PeerStatus, false).Orientation; got != mesh.OrientationVertical {
			t.Errorf("an orientation of %v reached the air as %v", bad, got)
		}
	}
}

// TestABoardThatHasNeverHadABearing: the fallback for a reading that is
// not a bearing has to start somewhere, and a zero nobody chose is due
// north — the answer the last two rounds here were about not giving. It
// starts from the configured heading, which New has already made into a
// bearing, so a board built with an impossible one reports the default
// rather than something derived from the impossible value.
func TestABoardThatHasNeverHadABearing(t *testing.T) {
	for _, bad := range []int16{-1, 360, 900, -32768} {
		h := newHarness(t, func(c *Config) { c.Heading = bad })
		if got := h.n.Config().Heading; got != 0 {
			t.Errorf("a configured heading of %d came back as %d, want the default", bad, got)
		}
		if got := h.n.Sensors().Azimuth; got != 0 {
			t.Errorf("a configured heading of %d reads as %d", bad, got)
		}
		if got := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; got != 0 {
			t.Errorf("a configured heading of %d reached the air as %d", bad, got)
		}

		// And the case the seeded fallback is for: a driver that reports
		// no bearing at all, on a node whose configured one was refused.
		// What goes out is the default, not something derived from the
		// value that was turned away.
		h.n.SetSensors(NewStatic(Sensors{
			Azimuth: -1, Battery: Battery{Volts: 4.1, Percent: 90},
		}), h.now)
		if got := h.n.status(h.now, mesh.PeerStatus, false).Azimuth; got != 0 {
			t.Errorf("a driver reporting no bearing put %d on the air", got)
		}
	}
	// A heading that is a bearing is kept as given, north included.
	for _, good := range []int16{0, 1, 187, 359} {
		h := newHarness(t, func(c *Config) { c.Heading = good })
		if got := h.n.Config().Heading; got != good {
			t.Errorf("a configured heading of %d came back as %d", good, got)
		}
	}
}

// TestAnOdometerHasBothEnds: the field on the air is an int16 and the
// reading is an int32, so a negative odometer past -32768 wrapped into a
// large positive distance — a device reporting -100000 told its peers it
// had traveled 31 km. The uptime beside it was given both ends; this
// had only the top.
func TestAnOdometerHasBothEnds(t *testing.T) {
	for _, m := range []int32{0, 1, 32767, 100000, -1, -100000, math.MinInt32, math.MaxInt32} {
		h := newHarness(t, func(c *Config) {
			c.Sensors = NewStatic(Sensors{
				Fix:     &Fix{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3, OdometerM: m},
				Battery: Battery{Volts: 4.1, Percent: 90},
			})
		})
		got := h.n.status(h.now, mesh.PeerStatus, false).OdometerM
		if got < 0 {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
		if m >= 0 && m <= math.MaxInt16 && int32(got) != m {
			t.Errorf("an odometer of %d went out as %d", m, got)
		}
	}
}

// TestAVoltageTheFrameCannotCarry: the field is a half float and the
// encoder is a port with no range check of its own, so a finite voltage
// past its limit runs off the exponent into the sign bit. The emulator
// guards its own readings; mesh is an exported package, and its encoder
// refused an over-long name while taking any voltage at all.
//
// The first version of this check also refused NaN, the infinities and
// every negative — which CI caught, because those are patterns the half
// holds and decodeHalf hands back, so a frame we could read became one
// we could not write. Both halves of that are pinned below.
func TestAVoltageTheFrameCannotCarry(t *testing.T) {
	// Finite and past the half's largest: silently becomes something else.
	for _, v := range []float32{100000, 131072, 1e6, -100000} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err == nil {
			t.Errorf("a battery of %v V was encoded", v)
		}
	}
	// Every voltage a cell reads, and every other pattern the half holds:
	// a frame carrying one of these must re-encode.
	for _, v := range []float32{
		0, 3.1, 4.1, 4.48, 4.6, 65504, -5,
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)),
	} {
		p := mesh.Peer{Command: mesh.PeerStatus, Name: "x", BattVolts: v,
			TimeOfDayMs: -1, Unix: -1}
		if _, err := p.MarshalBinary(); err != nil {
			t.Errorf("a battery of %v V was refused: %v", v, err)
		}
	}
}

// TestUsablePositionIsOneRule: every path that takes a position from
// outside asks the same two things — that something was reported, and
// that it is somewhere a device could be. The pair was written out four
// times, and the fourth was added because the third had been missed.
func TestUsablePositionIsOneRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lat, lon float32
		want     bool
	}{
		{"a real place", 37.775, -122.42, true},
		{"null island is no fix", 0, 0, false},
		{"a latitude with a zero longitude", 37.775, 0, true},
		{"past the pole", 91, 0, false},
		{"past the meridian", 0, 181, false},
	} {
		if got := usablePosition(tc.lat, tc.lon); got != tc.want {
			t.Errorf("%s: usablePosition(%v, %v) = %v, want %v", tc.name, tc.lat, tc.lon, got, tc.want)
		}
	}
	// And a frame carrying one of the refused pairs leaves the peer
	// without coordinates, whichever path it came in on.
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	f := statusFrame(t0)
	f.Lat, f.Lon = 0, 0
	h.rx(totem, self, -40, f)
	if p := h.n.peers[totem]; p.hasCoords {
		t.Errorf("a frame reporting no fix gave the peer a position of %v, %v", p.lat, p.lon)
	}
}

// TestOurOwnFixIsCheckedToo: four ingress paths got the "could a device
// be there" guard and the fifth — this device's own receiver — did not,
// which is the one position it puts on the air. A NaN reached the
// compass, where converting a bearing to an int16 is implementation
// defined, so it pointed confidently north; it reached the distance to
// every peer; and it went out in every status frame.
func TestOurOwnFixIsCheckedToo(t *testing.T) {
	for _, bad := range []struct {
		name     string
		lat, lon float32
	}{
		{"nan", float32(math.NaN()), float32(math.NaN())},
		{"infinite", float32(math.Inf(1)), -122.42},
		{"past the pole", 900, -122.42},
	} {
		h := newHarness(t, func(c *Config) {
			c.Position = &Position{Lat: bad.lat, Lon: bad.lon, AccuracyM: 3}
		})
		h.bond()
		h.rx(totem, self, -40, statusFrame(t0))
		h.advance(bootAnim + time.Second)

		if h.n.fix() != nil {
			t.Errorf("%s: the node believes it is somewhere", bad.name)
		}
		if deg := h.n.LEDs().dial; deg >= 0 {
			t.Errorf("%s: the compass points at %d", bad.name, deg)
		}
		for _, p := range h.n.Peers() {
			if math.IsNaN(p.DistanceM) || math.IsInf(p.DistanceM, 0) {
				t.Errorf("%s: a peer is %v away", bad.name, p.DistanceM)
			}
		}
		// And nothing impossible goes out on the air.
		for _, s := range h.take() {
			// usablePosition, which is the rule fix() applies: Null Island
			// is the firmware's own way of saying it has no fix, so a
			// frame carrying it is a frame carrying no position. The
			// earlier spelling paired livePosition with a zero check that
			// could never change the answer.
			if m, ok := s.msg.(mesh.Peer); ok && !usablePosition(m.Lat, m.Lon) && (m.Lat != 0 || m.Lon != 0) {
				t.Errorf("%s: broadcast a position of %v, %v", bad.name, m.Lat, m.Lon)
			}
		}
	}

	// And the exported setter refuses one outright, rather than storing
	// something every reader then has to ignore.
	h := newHarness(t, nil)
	if err := h.n.SetPosition(&Position{Lat: 999, Lon: -999}, h.now); err == nil {
		t.Error("SetPosition accepted a place no device could be")
	}
}

// TestAConfiguredColourIsChecked: the board reads the crystal colour off
// a flash sector and hands it straight to New, so a corrupt id arrives
// before Restore ever runs — and Restore "keeping the default" would keep
// the corrupt one. An unlit crystal for good, from one bad byte, written
// back to flash on the next save.
func TestAConfiguredColourIsChecked(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.ColorID = 120
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d from the configuration", int(got))
	}
	if got := h.n.Config().ColorID; Color(got).InPalette() == false {
		t.Errorf("the configuration kept colour %d", got)
	}
	if !strings.Contains(logged.String(), "thirteen") {
		t.Errorf("nothing was said about it: %s", logged.String())
	}
	// And it is not written back to flash.
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved")
	}
}

// TestABatteryReadingKeepsTheBoardsShape: NoPowerChip describes the
// board, not the reading. Rebuilding the battery struct without it
// silently re-armed the OTA gate on a board that has nothing to gate.
func TestABatteryReadingKeepsTheBoardsShape(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NoPowerChip, c.BattVolts, c.BattPct = true, 0, 0 })
	h.collect(h.n.Poll(h.now))
	if err := h.n.Update(h.now); errors.Is(err, ErrBatteryLow) {
		t.Fatal("a board with no power chip was gated before any reading")
	}
	// A reading arrives — the console batt command, or a driver.
	if err := h.n.SetBattery(20, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Poll(h.now))
	if !h.n.Sensors().Battery.NoPowerChip {
		t.Error("a battery reading erased the fact that the board has no power chip")
	}

	// And a source of its own does not get to decide either.
	h = newHarness(t, func(c *Config) {
		c.NoPowerChip = true
		c.Sensors = NewStatic(Sensors{Battery: Battery{Volts: 3.6, Percent: 20}})
	})
	h.collect(h.n.Poll(h.now))
	if !h.n.Sensors().Battery.NoPowerChip {
		t.Error("a sensor source of its own overrode the board's own shape")
	}
}

// TestOnlyTheBoardSaysItHasNoPowerChip: the flag describes the board, so
// the configuration decides it both ways. Forcing it on but never off let
// a sensor source claim there was no battery monitor and turn the OTA
// gate off on a device with a flat cell — the opposite of the guarantee.
func TestOnlyTheBoardSaysItHasNoPowerChip(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		// No flag: this board has a power chip.
		c.Sensors = NewStatic(Sensors{Battery: Battery{
			Volts: 3.4, Percent: 2, Low: true, NoPowerChip: true,
		}})
	})
	h.collect(h.n.Poll(h.now))
	if h.n.Sensors().Battery.NoPowerChip {
		t.Error("a sensor source talked the node out of its power chip")
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Errorf("an update on a flat cell was refused with %v, want %v", err, ErrBatteryLow)
	}
}

// TestNullIslandIsNoFix: 0,0 is how the firmware says it has no solution.
// Treated as a place, it stops the search animation, points the compass
// on a bearing measured from the Gulf of Guinea, and puts distances of
// thousands of kilometers into the mesh timing.
func TestNullIslandIsNoFix(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.rx(totem, self, -40, statusFrame(t0))
	// Past New and SetPosition, both of which refuse this themselves: a
	// receiver driver reports through the sensor source, so that is where
	// a fix of 0,0 actually arrives.
	h.n.SetSensors(NewStatic(Sensors{
		Fix:         &Fix{Lat: 0, Lon: 0, AccuracyM: 3},
		Orientation: mesh.OrientationVertical,
	}), h.now)
	h.advance(bootAnim + time.Second)

	if h.n.fix() != nil {
		t.Error("the node believes Null Island is a place")
	}
	if got := h.n.LEDs().Animation(); got != AnimGNSSSearch {
		t.Errorf("the ring shows %s, want the search for a fix", got)
	}
	if deg := h.n.LEDs().dial; deg >= 0 {
		t.Errorf("the compass points at %d", deg)
	}
	for _, p := range h.n.Peers() {
		if p.DistanceM > 1_000_000 {
			t.Errorf("a peer is %.0f m away, measured from Null Island", p.DistanceM)
		}
	}
}

// TestAClockNeedsAPlace: a receiver whose position the node refuses has
// not earned its clock either — that clock is advertised to every peer
// and re-slots every radio window.
func TestAClockNeedsAPlace(t *testing.T) {
	h := newHarness(t, nil)
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{
			Lat: float32(math.NaN()), Lon: float32(math.NaN()),
			AccuracyM: 3, Time: t0,
		},
		Orientation: mesh.OrientationVertical,
	}), h.now)
	h.advance(time.Second)
	if h.n.gnssClock {
		t.Error("took a GNSS clock from a receiver that cannot say where it is")
	}
}

// TestAConfiguredPositionIsChecked: New validates the name and the
// colour; a position no device could be at was stored anyway, and then
// silently ignored by every reader — a device searching for a fix for
// ever with nothing in the log to say why.
func TestAConfiguredPositionIsChecked(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 999, Lon: -999}
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	if h.n.fix() != nil {
		t.Error("the node kept a position no device could be at")
	}
	if !bytes.Contains(logged.Bytes(), []byte("configured position")) {
		t.Errorf("nothing was said about it: %s", logged.String())
	}
}

// TestSetColorIsCheckedLikeTheRest: New, Restore and the Smart Group all
// check a colour id; SetColor is reachable from a driver or an embedding
// caller and was the one that did not. State writes it to flash.
func TestSetColorIsCheckedLikeTheRest(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ColorID = int8(ColorTeal) })
	h.n.SetColor(Color(120), h.now)
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d", int(got))
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorTeal {
		t.Errorf("the crystal is %s, want the colour it had", got)
	}
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved to flash")
	}
}

// TestTheClockWarningReachesTheConsole: the warning explains why a Totem
// never picks up a clock, and totemctl decodes "src" as a MAC — so a line
// putting a name there failed to parse and was dropped whole, which is
// the one place it had to arrive.
func TestTheClockWarningReachesTheConsole(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.bond()
	h.advance(rtcSyncDelay + time.Second)

	old := statusFrame(t0)
	old.Unix, old.TimeOfDayMs = 1, 1
	h.rx(totem, self, -40, old)

	var line string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "ignoring a peer's clock") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("the warning was not logged at all: %s", logged.String())
	}
	// totemctl decodes src as a MAC, so a line carrying anything else
	// there does not parse and never reaches the person watching. The
	// name has a key of its own.
	if strings.Contains(line, "src=") {
		t.Errorf("the warning uses src, which totemctl reads as a MAC: %s", line)
	}
	if !strings.Contains(line, "name=Lukasz") {
		t.Errorf("the warning does not name the peer: %s", line)
	}
	if !strings.Contains(line, "mac=") {
		t.Errorf("the warning does not say which peer: %s", line)
	}
}

// TestAClockThePeerFrameCannotCarry: the Unix field in a peer frame is an
// int32, as it is in the firmware. Fuzzing set a clock in 2055 and the
// node advertised 1919 — the cast wrapped — which is exactly the value a
// peer with no clock of its own would have taken as real.
func TestAClockThePeerFrameCannotCarry(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()

	// Past what the frame can hold: refused, like one from before 2020.
	tooLate := maxClock.Add(time.Hour)
	if err := h.n.SetClock(tooLate, h.now); err == nil {
		t.Errorf("accepted a clock of %s, which a peer frame cannot carry", tooLate)
	}
	if h.n.gnssClock {
		t.Error("a clock the frame cannot carry was taken anyway")
	}

	// One just inside the ceiling is fine, and what goes out is never a
	// second from the wrong side of the epoch — not even once the device
	// has been up long enough to cross it.
	ok := maxClock.Add(-time.Hour)
	if err := h.n.SetClock(ok, h.now); err != nil {
		t.Fatalf("a clock an hour inside the ceiling was refused: %v", err)
	}
	h.take()
	h.advance(2 * time.Hour)
	for _, s := range h.take() {
		m, isPeer := s.msg.(mesh.Peer)
		if !isPeer || (m.Unix == -1 && m.TimeOfDayMs == -1) {
			continue
		}
		if got := time.Unix(int64(m.Unix), 0); got.Before(minClock) {
			t.Errorf("advertised %s, which is before %s", got, minClock)
		}
	}
}
