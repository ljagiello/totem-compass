package emulator

// Bonds, pairing, Smart Groups, locate relaying and the radio windows
// — the parts of node.go that are about somebody else.

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// TestALocateExpiryNeverWraps: the expiry a locate carries is an int32 of
// Unix seconds, and the lifetime is added inside it. Near the ceiling the
// sum wrapped, and a negative expiry is one every receiver reads as long
// past — so the flood would have died at the first hop.
func TestALocateExpiryNeverWraps(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	// A clock inside the ceiling, but within the locate lifetime of it.
	near := maxClock.Add(-30 * time.Second)
	if err := h.n.SetClock(near, h.now); err != nil {
		t.Fatal(err)
	}
	if got := locateExpiry(near); got <= 0 {
		t.Errorf("a locate near the ceiling expires at %d, which is before the epoch", got)
	}
	h.take()
	h.advance(30 * time.Second)
	seen := 0
	for _, s := range h.take() {
		m, ok := s.msg.(mesh.Locate)
		if !ok {
			continue
		}
		seen++
		if m.Expiry <= 0 {
			t.Errorf("sent a locate expiring at %d", m.Expiry)
		}
	}
	if seen == 0 {
		t.Error("no locate went out, so the on-air half of this test proved nothing")
	}
}

// TestPressingPairAlwaysAnswers: the bond-limit warning is said once so a
// Totem pairing next to a full device does not fill the log — but the
// person who pressed the button has to be told why nothing happened, or
// the button looks dead.
func TestPressingPairAlwaysAnswers(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.AutoPair = true
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for i, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatalf("bond %d: %v", i, err)
		}
	}
	h.advance(bootDebounce)

	// The automatic path — an owned Totem pairing next to us, which
	// repeats every 50-99 ms — says it once.
	for range 5 {
		h.rx(h.n.Config().Owned[0], mesh.Broadcast, -20, bondFrame(false))
	}
	if n := strings.Count(logged.String(), "already at the limit"); n != 1 {
		t.Errorf("the automatic path said it %d times, want once", n)
	}
	// A press says it every time, however often the radio already did.
	logged.Reset()
	h.collect(h.n.Pair(h.now))
	if !strings.Contains(logged.String(), "more than 8 bonds") {
		t.Errorf("pressing pair on a full device said nothing: %s", logged.String())
	}
	logged.Reset()
	h.collect(h.n.Pair(h.now))
	if !strings.Contains(logged.String(), "more than 8 bonds") {
		t.Errorf("pressing pair a second time said nothing: %s", logged.String())
	}
}

// TestAnExpiredLocateIsNotImmortal: relay() reads an expiry of zero as
// "none set" and skips the age test, so a frame stamped zero is relayed
// for ever — the opposite of what stamping it was for. A board whose
// clock has not started produces exactly that.
func TestAnExpiredLocateIsNotImmortal(t *testing.T) {
	if got := locateExpiry(time.Time{}); got == 0 {
		t.Error("a pre-epoch clock stamped the frame with no expiry at all")
	}
	if got := locateExpiry(time.Time{}); got < 0 {
		t.Errorf("a pre-epoch clock stamped the frame %d", got)
	}

	// And such a frame is refused by the relay, not carried. It has to
	// ask for a reply: onLocate returns before relay() for one that does
	// not, so a frame without the flag proves nothing about the relay.
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.take()
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4242, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(time.Time{}),
	})
	if got := relayCount(h.take()); got != 0 {
		t.Errorf("relayed %d copies of a frame that expired in 1970", got)
	}

	// And one stamped exactly zero, which is what relay() used to read as
	// "no expiry set" — a frame off the air can carry it however
	// carefully this node stamps its own.
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4244, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: 0,
	})
	if got := relayCount(h.take()); got != 0 {
		t.Errorf("relayed %d copies of a frame carrying no expiry at all", got)
	}

	// The same frame with a live expiry is relayed, which is what shows
	// the two above were refused for their expiry and not ignored for
	// some other reason.
	h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
		Origin: totem, UID: 4243, ReplyRequested: true,
		MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
		Expiry: locateExpiry(t0),
	})
	if got := relayCount(h.take()); got == 0 {
		t.Error("a frame with a live expiry was not relayed either, so the test proves nothing")
	}
}

// relayCount is how many of these frames carry another Totem's origin,
// which is what a relay is.
func relayCount(sent []sent) int {
	n := 0
	for _, s := range sent {
		if m, ok := s.msg.(mesh.Locate); ok && m.Origin == totem {
			n++
		}
	}
	return n
}

// TestAResetDuringPairingStays: a reset while a window is open used to
// leave the sibling still broadcasting bond requests, so the bond came
// straight back inside the same six seconds — and the board saved a
// record still holding the bond it was told to wipe.
func TestAResetDuringPairingStays(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.bond()
	h.collect(h.n.Pair(h.now))
	if !h.n.Pairing() {
		t.Fatal("the pairing window did not open")
	}

	h.n.FactoryReset(h.now)
	if h.n.Pairing() {
		t.Error("a factory reset left the pairing window open")
	}
	if got := h.n.BondCount(); got != 0 {
		t.Fatalf("the reset left %d bonds", got)
	}
	// The sibling keeps asking for the rest of the window it thinks is
	// open. None of it may take.
	for range 5 {
		h.rx(totem, self, -20, bondFrame(false))
		h.advance(200 * time.Millisecond)
	}
	h.advance(pairingWindow + time.Second)
	if got := h.n.BondCount(); got != 0 {
		t.Errorf("the bond came back after the reset: %d bonds", got)
	}
}

// TestTheTwoBondLimitWarningsAreIndependent: one latch for two
// diagnostics meant whichever fired first silenced the other for the
// rest of the run.
func TestTheTwoBondLimitWarningsAreIndependent(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.AutoPair = true
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for _, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(bootDebounce)

	// One Totem pairing beside us.
	h.rx(h.n.Config().Owned[0], mesh.Broadcast, -20, bondFrame(false))
	if !strings.Contains(logged.String(), "pairing next to us") {
		t.Fatalf("the pairing warning did not fire: %s", logged.String())
	}
	// And another that still holds us, which is a different thing to say.
	ninth := mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x99}
	h.n.cfg.Owned = append(h.n.cfg.Owned, ninth)
	h.rx(ninth, self, -40, statusFrame(t0))
	if !strings.Contains(logged.String(), "bond not restored") {
		t.Errorf("the first warning silenced the second: %s", logged.String())
	}
}

// TestTheFirstWindowAfterAFixIsAligned: startAligned exists to put the
// radio windows on wall-clock slots. Setting clockSet after calling it
// meant the first window — the one being re-slotted — was scheduled on
// the no-clock period, at an arbitrary phase.
func TestTheFirstWindowAfterAFixIsAligned(t *testing.T) {
	h := newHarness(t, nil)
	// Off the slot boundary, or an unaligned window would land on one by
	// accident and the test would pass either way.
	h.advance(1300 * time.Millisecond)
	// A receiver that arrives with a fix and a clock, as a lock does.
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{Lat: 37.775, Lon: -122.42, AccuracyM: 3, Time: t0},
	}), h.now)
	if !h.n.clockSet {
		t.Fatal("the node did not take the clock")
	}
	// The radio window itself, not whatever the strip wants next.
	var next time.Time
	for _, j := range h.n.jobs {
		if j.kind == jobWindow {
			next = j.at
		}
	}
	if next.IsZero() {
		t.Fatal("no radio window was scheduled")
	}
	// An aligned window lands on a whole radio period of the wall clock,
	// plus the fixed transmit delay. Unaligned it lands on whatever phase
	// the last transmission left, which the no-clock period gives.
	period := h.n.period().Milliseconds()
	off := (h.n.wall(next).UnixMilli() - windowTXDelay.Milliseconds()) % period
	if off != 0 {
		t.Errorf("the first window after a fix is %d ms off its %d ms slot", off, period)
	}
}

// TestAPowerCycleLeavesOneWindowChain: a clock arriving while the device
// was off re-armed the radio windows that PowerOff had just cleared, and
// PowerOn then started a second chain beside it — so the device sent its
// status to every peer twice a period for the rest of the run.
func TestAPowerCycleLeavesOneWindowChain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.n.PowerOff(h.now)

	// A receiver that locks on while the device is down.
	h.n.SetSensors(NewStatic(Sensors{
		Fix: &Fix{Lat: 37.775, Lon: -122.42, AccuracyM: 3, Time: t0},
	}), h.now)
	if got := countJobs(h.n, jobWindow); got != 0 {
		t.Errorf("a powered-down device armed %d radio windows", got)
	}

	h.n.PowerOn(h.now)
	if got := countJobs(h.n, jobWindow); got != 1 {
		t.Fatalf("after a power cycle there are %d window chains, want 1", got)
	}

	// And that is what it sends: one status per peer per period.
	h.advance(bootDebounce)
	h.take()
	h.advance(9 * time.Second)
	perPeer := map[mesh.MAC]int{}
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Peer); ok && m.Command == mesh.PeerStatus {
			perPeer[s.Dst]++
		}
	}
	for dst, n := range perPeer {
		// Two periods in nine seconds, and the firmware sends a second
		// copy to a far peer — four is the ceiling, eight is two chains.
		if n > 4 {
			t.Errorf("sent %d status frames to %s in 9 s", n, dst)
		}
	}
}

func countJobs(n *Node, kind jobKind) int {
	c := 0
	for _, j := range n.jobs {
		if j.kind == kind {
			c++
		}
	}
	return c
}

// TestAPowerCycleOwesNoMeshReply: sendOutbox repeats the last locate
// reply for ten seconds. One owed at the moment the power went would go
// out on the next power-up, answering — with a fresh position and the
// old UID — a request from the far side of a power cycle the peers never
// saw.
func TestAPowerCycleOwesNoMeshReply(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.replyUID, h.n.replyAt, h.n.lastReply = 4242, h.now, h.now

	h.n.PowerOff(h.now)
	if !h.n.replyAt.IsZero() {
		t.Error("powering down left a mesh reply owed")
	}
	h.n.PowerOn(h.now)
	h.take()
	h.advance(12 * time.Second)
	for _, s := range h.take() {
		if m, ok := s.msg.(mesh.Locate); ok && m.UID == 4242 {
			t.Error("a reply owed before the power cycle went out after it")
		}
	}
}

// TestTheDistanceToTheOtherSideOfTheWorld: haversine's intermediate can
// come out a hair above 1 for two points near opposite sides of the
// globe, and Asin of that is NaN — which reaches the mesh delay, where
// int(NaN) is implementation-defined, and furthestPeer, where the
// builtin max carries it to every peer.
func TestTheDistanceToTheOtherSideOfTheWorld(t *testing.T) {
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{48.8575, -31.900307, -48.8575, 148.09969},
		{0, 0, 0, 180},
		{90, 0, -90, 0},
		{37.775, -122.42, -37.775, 57.58},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Errorf("distance(%v,%v -> %v,%v) = %v", tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
		if got < 0 || got > 21_000_000 {
			t.Errorf("distance(%v,%v -> %v,%v) = %.0f m, which is further than round the world",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
	}

	// And such a peer does not poison the node.
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 48.8575, Lon: -31.900307, AccuracyM: 3}
	})
	h.bond()
	f := statusFrame(t0)
	f.Lat, f.Lon = -48.8575, 148.09969
	h.rx(totem, self, -40, f)
	for _, p := range h.n.Peers() {
		if math.IsNaN(p.DistanceM) {
			t.Error("a peer on the other side of the world is NaN away")
		}
	}
	// And the fleet-wide figure the same distance feeds: furthestPeer
	// takes the builtin max over every peer, which returns NaN if any
	// argument is one, so a single unplaceable peer turned off the
	// far-peer status copy for all of them.
	if got := h.n.furthestPeer(); math.IsNaN(got) {
		t.Error("one peer on the other side of the world made every distance NaN")
	}
}

// TestALastHopThatCannotBePlaced: the minimum-distance rule compares a
// distance against RelayMinDistM, and every comparison with NaN is
// false. The frames whose last-hop position is unreadable are the least
// worth trusting, and they were the ones the rule was deciding about on
// a number that meant nothing. An unplaceable hop is no hop at all now,
// which is what the firmware's own Null Island says, and the rule still
// bites on a hop that can be placed.
func TestALastHopThatCannotBePlaced(t *testing.T) {
	// Where this node is, and a rule that suppresses a relay within 30 km.
	const lat, lon = 37.7749, -122.4194
	relayed := func(t *testing.T, hopLat, hopLon float32) bool {
		t.Helper()
		h := newHarness(t, func(c *Config) {
			c.Position = &Position{Lat: lat, Lon: lon, AccuracyM: 3}
		})
		h.bond()
		// onLocate leaves the mesh alone until the node has a clock.
		if err := h.n.SetClock(t0, h.now); err != nil {
			t.Fatal(err)
		}
		h.take()
		h.rx(totem, mesh.Broadcast, -40, mesh.Locate{
			Origin: totem, UID: 4242, ReplyRequested: true,
			MinRSSI: mesh.DefaultMinRSSI, MaxHops: mesh.DefaultMaxHops,
			Expiry:        locateExpiry(h.n.wall(h.now)),
			RelayMinDistM: 30_000,
			LastHopLat:    hopLat, LastHopLon: hopLon,
		})
		return relayCount(h.take()) > 0
	}

	// The rule itself: a hop a few streets away, well inside 30 km.
	if relayed(t, lat+0.01, lon+0.01) {
		t.Error("relayed a frame whose last hop was next door, past a rule that says not to")
	}
	// And one on the far side of the state is relayed, or the rule is a
	// refusal rather than a rule.
	if !relayed(t, lat+5, lon+5) {
		t.Error("refused to relay a frame whose last hop was hundreds of kilometers away")
	}

	// A hop that cannot be placed is not measured against the rule at
	// all: nothing that is not a number reaches the comparison, and the
	// frame travels as it would from a Totem with no fix.
	for _, tc := range []struct {
		name     string
		lat, lon float32
	}{
		{"not a number", float32(math.NaN()), float32(math.NaN())},
		{"one half of one", lat, float32(math.NaN())},
		{"off the globe", 400, 900},
		{"null island", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !relayed(t, tc.lat, tc.lon) {
				t.Error("a last hop that cannot be placed was read as one that is too close")
			}
		})
	}
}

// TestADistanceThatCannotBeMeasured: distance() is a helper anything in
// this file can reach, and Go's min returns NaN if either argument is
// one — so the clamp on the haversine intermediate never removed one.
// -1 is what the rest of the file means by "no distance".
func TestADistanceThatCannotBeMeasured(t *testing.T) {
	nan := float32(math.NaN())
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{nan, 0, 0, 0},
		{0, nan, 0, 0},
		{0, 0, nan, 0},
		{0, 0, 0, nan},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) {
			t.Errorf("distance(%v,%v -> %v,%v) is not a number",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		} else if got >= 0 {
			t.Errorf("distance(%v,%v -> %v,%v) = %v, which reads as a real distance",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon, got)
		}
	}
}

// TestADistanceWhoseIntermediateGoesNegative: the clamp was one-sided.
// With a latitude outside ±90 the two haversine terms cancel and the
// sign is left to the last bit; Sqrt of a negative is NaN, which Asin
// carries straight out past a guard that only looked at the inputs.
func TestADistanceWhoseIntermediateGoesNegative(t *testing.T) {
	// Found by sweeping: at these the two terms cancel to
	// -2.7755575615628914e-17, and Sqrt of that is NaN.
	for _, tc := range []struct{ aLat, aLon, bLat, bLon float32 }{
		{113.5, 0, 66.5, 180},
		{117.5, 0, 62.5, 180},
		{118.5, 0, 61.5, 180},
	} {
		got := distance(tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		if math.IsNaN(got) {
			t.Errorf("distance(%v,%v -> %v,%v) is not a number",
				tc.aLat, tc.aLon, tc.bLat, tc.bLon)
		}
	}
}

// TestTheMeshScheduleIsATimeThisDeviceReaches: peerDistance feeds
// meshDelivery and comes out as meshNext, the instant this peer is next
// asked about. distance() no longer hands it a NaN, but the arithmetic
// after it is still unguarded, and a number that overflows or that lands
// centuries away is a peer the mesh never asks about again.
//
// The far end is the firmware's own answer and is kept: a peer on the
// other side of the world really is put off for days by
// cal_mesh_delivery, and this package's job is to behave like the
// device. What is pinned is that the answer is a real, positive,
// climbing span for every distance a device can be from another one.
func TestTheMeshScheduleIsATimeThisDeviceReaches(t *testing.T) {
	const halfWayRound = 20_015_000 // meters, pole to pole the long way
	var last time.Duration
	for d := 0.0; d <= halfWayRound; d += 5000 {
		got := meshDelivery(d)
		switch {
		case got <= 0:
			t.Fatalf("meshDelivery(%.0f m) = %v", d, got)
		case got < 6*time.Second:
			t.Fatalf("meshDelivery(%.0f m) = %v, under the floor", d, got)
		case got < last:
			t.Fatalf("meshDelivery(%.0f m) = %v, less than the %v before it", d, got, last)
		case got > 155*time.Hour:
			// The firmware's own answer for the far side of the world is
			// about six and a half days. Anything beyond it is the
			// arithmetic having gone wrong rather than a distance.
			t.Fatalf("meshDelivery(%.0f m) = %v, longer than the width of the world", d, got)
		}
		last = got
	}

	// And a distance that could not be measured goes to the floor, not
	// into the arithmetic. The same band as the loop above, because
	// "positive" is not an assertion here: int(NaN) is the most negative
	// int64 on amd64, and the multiplications that follow overflow into
	// a large positive number — a peer next asked about in two hundred
	// years, which reads as positive and is a peer never asked about
	// again.
	for _, d := range []float64{-1, -20_000_000, math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := meshDelivery(d)
		if got < 6*time.Second || got > time.Minute {
			t.Errorf("meshDelivery(%v) = %v, want the floor", d, got)
		}
	}
}

// TestAFrameThatArrivesAsThePackDies: the reading Receive takes can
// switch the device off, and the check that a powered-down device does
// not answer runs before it. The frame was then taken into the peer
// table, and could set the clock, on a device that was down.
func TestAFrameThatArrivesAsThePackDies(t *testing.T) {
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	h.bond()
	h.advance(time.Second)
	h.take()

	was := h.n.Peers()[0]

	// The pack collapses, and the next thing to happen is a frame.
	pack.b = Battery{Volts: 3.1, Percent: 40}
	f := statusFrame(h.now)
	f.Name, f.BattPct = "SomeoneElse", 42
	h.rx(totem, self, -40, f)

	if !h.n.Power().Off() {
		t.Fatal("the reading taken on the frame did not switch the device off")
	}
	got := h.n.Peers()[0]
	if got.Status.Name != was.Status.Name || got.Status.BattPct != was.Status.BattPct {
		t.Errorf("a device that had just powered down took the frame in: the peer is now %q at %d%%",
			got.Status.Name, got.Status.BattPct)
	}
	if !got.LastHeard.Equal(was.LastHeard) {
		t.Errorf("a device that had just powered down recorded hearing from a peer at %v", got.LastHeard)
	}
	if h.n.clockSet {
		t.Error("a device that had just powered down took a clock off a frame")
	}
	if s := h.take(); len(s) != 0 {
		t.Errorf("a device that had just powered down sent %d frames", len(s))
	}
}

// TestAClockOnAFrameThatArrivesAsThePackDies: the clock half of the
// above, which needs its own run. adoptClock ignores a peer's clock for
// the first 30 seconds of uptime — "Block sleep for GNSS RTC Sync" —
// so a test that sends its frame seven seconds in proves nothing about
// the clock either way.
func TestAClockOnAFrameThatArrivesAsThePackDies(t *testing.T) {
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	h.bond()
	h.advance(rtcSyncDelay + time.Second)
	h.take()
	if h.n.clockSet {
		t.Fatal("the node had a clock before any frame carried one")
	}

	pack.b = Battery{Volts: 3.1, Percent: 40}
	h.rx(totem, self, -40, statusFrame(h.now))

	if !h.n.Power().Off() {
		t.Fatal("the reading taken on the frame did not switch the device off")
	}
	if h.n.clockSet {
		t.Error("a device that had just powered down took a peer's clock, and would re-slot every window off it")
	}
}

// TestAbandonLeavesOtherGroupsAlone: SmartGroupAbandon ends the group it
// names. A device in no group, or in a different one, has nothing to
// abandon and must not report that it did — the flags look the same
// either way, so what gives it away is the line in the log.
func TestAbandonLeavesOtherGroupsAlone(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if h.n.inGroup {
		t.Fatal("an abandon put the device into a group")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("a device in no group reported abandoning one: %s", logged.String())
	}
	logged.Reset()

	// In a group, only its own UID ends it.
	h.n.inGroup, h.n.smartUID = true, 7
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 99})
	if !h.n.inGroup {
		t.Error("another group's abandon ended this one")
	}
	if strings.Contains(logged.String(), "abandoned") {
		t.Errorf("another group's abandon was reported as this one's: %s", logged.String())
	}
	h.rx(totem, mesh.Broadcast, -40, mesh.SmartGroup{Instruction: mesh.SmartGroupAbandon, UID: 7})
	if h.n.inGroup {
		t.Error("the group's own abandon did not end it")
	}
}

// TestUnbondNoticeReachesTheAir: deleting a peer tells it so. The notice
// waits for the next window rather than leaving with the call, so this
// covers the whole path — Unbond also hands back whatever the node
// queued now, as every other command does, instead of returning nil.
func TestUnbondNoticeReachesTheAir(t *testing.T) {
	h := newHarness(t, nil)
	h.bond()
	h.take()
	h.collect(h.n.Unbond(h.now, totem))
	if h.n.BondCount() != 0 {
		t.Fatalf("the peer is still bonded: %d", h.n.BondCount())
	}
	// The unbond notice goes out at the next window, not from the call,
	// so what matters is that nothing is stranded: drive a window and the
	// frame has to appear.
	h.advance(10 * time.Second)
	var found bool
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerUnbond {
			found = true
		}
	}
	if !found {
		t.Error("the unbond notice never reached the air")
	}
}

// TestPeerTimeIsAWallSecond: State writes each peer's position time as a
// Unix second, but coordsAt is in the device's own base — on a board with
// no RTC that base starts near the epoch at every boot. Saving it raw put
// a number like 126 in a field everything else reads as a wall time. It
// is saved only when there is a clock to convert it with, and the zero
// time, whose Unix value is a large negative number, is not saved at all.
func TestPeerTimeIsAWallSecond(t *testing.T) {
	// No clock: nothing is claimed.
	h := newHarness(t, nil)
	h.bond()
	p := h.n.peers[totem]
	p.hasCoords, p.lat, p.lon, p.coordsAt = true, 37.775, -122.42, h.now
	if got := h.n.State(1).Peers[0].LastSeenUnix; got != 0 {
		t.Errorf("a node with no clock saved a last-seen time of %d", got)
	}

	// The zero time is not a time either.
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	p.coordsAt = time.Time{}
	if got := h.n.State(1).Peers[0].LastSeenUnix; got != 0 {
		t.Errorf("the zero time was saved as %d", got)
	}

	// With a clock, it is the wall second the position was reported.
	p.coordsAt = h.now
	got := h.n.State(1).Peers[0].LastSeenUnix
	if want := t0.Unix(); got < want-5 || got > want+5 {
		t.Errorf("saved %d (%s), want about %d (%s)",
			got, time.Unix(got, 0).UTC(), want, t0.UTC())
	}

	// And it comes back where it went in. A node restoring without a
	// clock cannot place it, so the position ages from the restore rather
	// than from a second it cannot read.
	st := h.n.State(1)
	fresh := newHarness(t, nil)
	if errs := fresh.n.Restore(&st, fresh.now); len(errs) != 0 {
		t.Fatalf("restore: %v", errs)
	}
	rp := fresh.n.peers[totem]
	if !rp.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := fresh.now.Sub(rp.coordsAt); age < 0 || age > time.Second {
		t.Errorf("a restored position without a clock is %v old, want about 0", age)
	}
}

// TestABrokenPeerClockIsReportedOnce: a peer whose RTC never started
// broadcasts status every few seconds. Warning on each would fill the log
// — on the board the rotate that follows holds a watchdog blocker — but
// saying nothing at all leaves someone wondering why a Totem never picks
// up a clock with nothing to go on.
func TestABrokenPeerClockIsReportedOnce(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.bond()
	h.advance(rtcSyncDelay + time.Second)

	old := statusFrame(t0)
	old.Unix, old.TimeOfDayMs = 1, 1
	for range 10 {
		h.rx(totem, self, -40, old)
		h.advance(2 * time.Second)
	}
	if n := strings.Count(logged.String(), "ignoring a peer's clock"); n != 1 {
		t.Errorf("a peer with a broken clock was reported %d times, want once", n)
	}
	if h.n.clockSet {
		t.Error("the node took a clock from before 2020")
	}
}

// TestASmartGroupCannotBlankTheCrystal: the colour a group assigns is one
// byte off the air from the host. An id outside the thirteen renders
// unlit, and State writes it to flash, so one garbled frame would leave
// the crystal dark past the next reboot.
func TestASmartGroupCannotBlankTheCrystal(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.collect(h.n.Pair(h.now))
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 20000,
		Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: self}},
	})
	if !h.n.inGroup {
		t.Fatal("the node did not join the group")
	}
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupFinalize, UID: 7,
		Members: []mesh.SmartGroupMember{{MAC: self, ColorID: 120}, {MAC: totem, ColorID: 4}},
	})
	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the group set the crystal to colour %d", int(got))
	}
	if got := h.n.Config().ColorID; !Color(got).InPalette() {
		t.Errorf("the group set the configuration to colour %d", got)
	}
}

// TestAPeerColourOfRedIsLeftAlone: the peer guard and the crystal guard
// read the same flash sector, so they follow the same rule. 0 is red and
// also the zero value, so a saved 0 leaves the colour the bond drew.
func TestAPeerColourOfRedIsLeftAlone(t *testing.T) {
	h := newHarness(t, nil)
	h.n.Restore(&store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", ColorID: 0,
	}}}, h.now)
	got := h.n.peers[totem].color
	if !got.InPalette() {
		t.Errorf("restored colour %d", int(got))
	}
	mac := h.n.Config().MAC
	if want := BondColor(0, uint32(mac[2])<<24|uint32(mac[3])<<16|uint32(mac[4])<<8|uint32(mac[5])); got != want {
		t.Errorf("a saved colour of 0 gave %s, want the colour the bond drew, %s", got, want)
	}
}

// TestASmartGroupBondOutlivesThePairingWindow: a group can finalize while
// a pairing window is still open. The window ends by deleting whatever
// bond it had half-made — and that is often a Totem the group has just
// bonded us to, so the timer undid the group's own work seconds later.
func TestASmartGroupBondOutlivesThePairingWindow(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.collect(h.n.Pair(h.now))

	// A bond request from the owned Totem: half-made, held as tempBond.
	h.rx(totem, self, -20, bondFrame(false))
	if h.n.tempBond == nil {
		t.Fatal("the bond request did not start a bond")
	}

	// The group forms and names both of us.
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 20000,
		Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: self}},
	})
	h.rx(totem, mesh.Broadcast, -30, mesh.SmartGroup{
		Instruction: mesh.SmartGroupFinalize, UID: 7,
		Members: []mesh.SmartGroupMember{{MAC: self}, {MAC: totem}},
	})
	if h.n.BondCount() != 1 {
		t.Fatalf("the group left %d bonds", h.n.BondCount())
	}

	// Well past where the pairing window would have closed.
	h.advance(pairingWindow + 2*time.Second)
	if got := h.n.BondCount(); got != 1 {
		t.Errorf("the pairing window deleted the group's bond: %d bonds left", got)
	}
	if h.n.Pairing() {
		t.Error("the group finalized and the device is still pairing")
	}
}
