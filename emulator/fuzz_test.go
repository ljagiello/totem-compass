package emulator

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

var totem2 = mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x79}

// fuzzCommand encodes a console command step for FuzzNode.
func fuzzCommand(line string, step byte) []byte {
	return append([]byte{8, 0, step, byte(len(line))}, line...)
}

// fuzzStep encodes one FuzzNode step: op, sender/RSSI byte, time step in
// 100 ms units, then a length-prefixed frame.
func fuzzStep(op, who, step byte, m mesh.Message) []byte {
	b := []byte{op, who, step, 0}
	if m != nil {
		frame, err := m.MarshalBinary()
		if err != nil {
			panic(err)
		}
		b[3] = byte(len(frame))
		b = append(b, frame...)
	}
	return b
}

// FuzzNode plays arbitrary frames at the node from two owned Totems and a
// stranger, with arbitrary RSSI, timing, configuration and operator
// actions. Whatever arrives:
//
//   - everything sent stays inside the owned scope and parses
//     (harness.collect);
//   - no stranger becomes a peer;
//   - no timer is left overdue;
//   - a clock, once adopted, stays set;
//   - the node's own peer frames carry its name, and its own locate
//     frames its MAC, zero hops and the default limits;
//   - relays carry an owned origin and at least one hop, and one UID is
//     not relayed twice within the minimum de-duplication window;
//   - console commands (the clock, the simulation, the fix, the battery)
//     run against all of that without breaking any of it, and the node
//     never advertises a clock it does not have.
func FuzzNode(f *testing.F) {
	// Bond, take the clock, then hear a locate request from the Totem.
	var bondThenLocate []byte
	// who: bit 6 unicast to the node, low 5 bits × 3 = −RSSI (0x07 is −21).
	bondThenLocate = append(bondThenLocate, fuzzStep(3, 0, 1, nil)...)
	bondThenLocate = append(bondThenLocate, fuzzStep(0, 0x07, 1, bondFrame(false))...)
	bondThenLocate = append(bondThenLocate, fuzzStep(0, 0x47, 70, bondFrame(true))...)
	for range 2 {
		bondThenLocate = append(bondThenLocate, fuzzStep(0, 0x47, 250, statusFrame(t0.Add(time.Hour)))...)
	}
	bondThenLocate = append(bondThenLocate, fuzzStep(0, 0x0a, 1, mesh.Locate{
		Origin: totem, UID: 9, ReplyRequested: true, MinRSSI: -127, MaxHops: 99, Expiry: int32(t0.Add(2 * time.Hour).Unix()),
	})...)
	f.Add(byte(3), bondThenLocate)
	// Bond and take the clock, then lose the Totem until it goes stale and
	// the node asks the mesh for it.
	bondThenStale := slices.Clone(bondThenLocate[:len(bondThenLocate)-49])
	for range 7 {
		bondThenStale = append(bondThenStale, fuzzStep(5, 0, 250, nil)...)
	}
	f.Add(byte(3), bondThenStale)
	// Console commands interleaved with frames.
	var console []byte
	for _, line := range []string{"sim walk 90", "clock 1789855567000", "pos 37.775 -122.42 3", "batt 20", "flat on", "sim off", "clock 7"} {
		console = append(console, fuzzCommand(line, 40)...)
	}
	f.Add(byte(3), append(console, bondThenLocate...))
	for _, g := range []mesh.Message{
		bondFrame(false), bondFrame(true), statusFrame(t0.Add(time.Hour)),
		mesh.Locate{Origin: totem, UID: 9, ReplyRequested: true, MinRSSI: -127, MaxHops: 99, Expiry: int32(t0.Unix()) + 4000},
		mesh.SmartGroup{Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 9000, Members: []mesh.SmartGroupMember{{MAC: self}}},
		mesh.SmartGroup{Instruction: mesh.SmartGroupFinalize, UID: 7, Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: stranger}}},
	} {
		f.Add(byte(0), fuzzStep(0, 0x07, 50, g))
		f.Add(byte(3), fuzzStep(1, 0x87, 200, g))
	}
	f.Fuzz(func(t *testing.T, setup byte, script []byte) {
		owned := []mesh.MAC{totem, totem2}
		h := newHarness(t, func(c *Config) {
			c.Owned = owned
			c.AutoPair = setup&1 != 0
			if setup&2 != 0 {
				c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3, AltitudeM: -500}
			}
			c.Rand = rand.New(rand.NewPCG(uint64(setup), 4))
			c.Logger = nil
		})
		name := h.n.Config().Name
		relayed := map[uint16]time.Time{}
		clock := false
		for len(script) >= 4 {
			op, who, step, n := script[0], script[1], script[2], int(script[3])
			script = script[4:]
			n = min(n, len(script))
			frame := script[:n]
			script = script[n:]

			src := owned[who>>7&1]
			if who&0x20 != 0 {
				src = stranger
			}
			dst := mesh.Broadcast
			if who&0x40 != 0 {
				dst = self
			}
			rssi := -int8(who & 0x1f * 3)
			switch op % 10 {
			case 0, 1, 2:
				h.collect(h.n.Receive(h.now, Received{Src: src, Dst: dst, RSSI: rssi, Data: frame}))
			case 3:
				h.collect(h.n.Pair(h.now))
			case 4:
				h.collect(h.n.Unbond(h.now, src))
			case 5:
				h.n.SetSOS(step&1 != 0)
			case 6:
				if step&1 == 0 {
					_ = h.n.SetPosition(nil, h.now)
				} else {
					_ = h.n.SetPosition(&Position{Lat: float32(step) / 3, Lon: -float32(step) / 2, AccuracyM: int8(step & 0x7f)}, h.now)
				}
			case 7:
				_ = h.n.SetHeading(int16(step)%360, h.now)
			case 8, 9:
				// A console line, as an operator or totemctl mesh sends it.
				if c, err := ParseCommand(string(frame)); err == nil {
					out, handled, err := h.n.Apply(c, h.now)
					if handled && err == nil {
						h.collect(out)
					}
					if c.Op == OpClock && err == nil && !h.n.gnssClock {
						t.Fatalf("%q was accepted but left no clock", c)
					}
				}
			}
			h.advance(time.Duration(step) * 100 * time.Millisecond)

			for _, p := range h.n.Peers() {
				if !slices.Contains(owned, p.MAC) {
					t.Fatalf("peer %s outside the owned scope", p.MAC)
				}
			}
			if next := h.n.Next(); !next.IsZero() && !next.After(h.now) {
				t.Fatalf("a timer at %v is overdue at %v", next, h.now)
			}
			if clock && !h.n.clockSet {
				t.Fatal("the adopted clock was lost")
			}
			clock = h.n.clockSet
			if w := h.n.wall(h.now); h.n.gnssClock && !w.After(minClock) {
				t.Fatalf("the node holds a clock of %s", w)
			}
			for _, s := range h.take() {
				switch m := s.msg.(type) {
				case mesh.Peer:
					if m.Name != name {
						t.Fatalf("sent a peer frame named %q, want %q", m.Name, name)
					}
					// A clock is advertised only when the node has one of
					// its own, and never one from before 2020: a peer whose
					// clock is unset would take it.
					switch {
					case m.Unix == -1 && m.TimeOfDayMs == -1:
					case !h.n.gnssClock:
						t.Fatalf("advertised unix %d without a clock of our own", m.Unix)
					case time.Unix(int64(m.Unix), 0).Before(minClock):
						t.Fatalf("advertised %s, before %s", time.Unix(int64(m.Unix), 0), minClock)
					case m.TimeOfDayMs < 0 || m.TimeOfDayMs >= 86_400_000:
						t.Fatalf("advertised time of day %d ms", m.TimeOfDayMs)
					}
				case mesh.Locate:
					if m.Origin == self {
						if m.Hops != 0 || m.MaxHops != mesh.DefaultMaxHops || m.MinRSSI != mesh.DefaultMinRSSI || m.UID == 0 {
							t.Fatalf("originated %+v", m)
						}
						continue
					}
					if !slices.Contains(owned, m.Origin) || m.Hops == 0 {
						t.Fatalf("relayed %+v", m)
					}
					if last, ok := relayed[m.UID]; ok && s.at.Sub(last) < dedupeMin {
						t.Fatalf("relayed UID %d again after %v", m.UID, s.at.Sub(last))
					}
					relayed[m.UID] = s.at
				}
			}
		}
		h.advance(time.Minute)
	})
}

// FuzzOTAServer feeds the update client whatever an update server might
// answer. The exchange is plain HTTP with no key and no signature, so
// anything at all can arrive: it must be rejected or understood, never
// panic, and never yield a package name that is a path.
func FuzzOTAServer(f *testing.F) {
	f.Add(`{"ota_url":"http://ota.example/repo","release_id":340}`, `["firmware_v5.0.4.bin"]`)
	f.Add(`{}`, `[]`)
	f.Add(`{"ota_url":"http://x/r","update_method":"preview"}`, `["preview_v9.tgz","firmware_v9.bin"]`)
	f.Add("", "")
	f.Fuzz(func(t *testing.T, release, contents string) {
		rel, err := parseRelease([]byte(release))
		if err == nil {
			if rel.OTAURL == "" {
				t.Fatal("a release with no url was accepted")
			}
			if !strings.HasPrefix(rel.OTAURL, "http") {
				t.Fatalf("accepted a url that is not http: %q", rel.OTAURL)
			}
		}
		names, err := parseContents([]byte(contents))
		if err != nil {
			return
		}
		for _, n := range names {
			if strings.ContainsAny(n, "/\\") || strings.Contains(n, "..") {
				t.Fatalf("accepted %q as a file name", n)
			}
		}
		pkg, err := pickPackage(names, extFor(rel.UpdateMethod))
		if err != nil {
			return
		}
		if !strings.HasSuffix(pkg, ".bin") && !strings.HasSuffix(pkg, ".tgz") {
			t.Fatalf("picked %q, which is not a package", pkg)
		}
		if v, err := versionFromName(pkg); err == nil && v == "" {
			t.Fatalf("%q gave an empty version", pkg)
		}
	})
}
