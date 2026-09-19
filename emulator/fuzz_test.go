package emulator

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// FuzzNode plays arbitrary frames at the node from the owned Totem and a
// stranger, with arbitrary RSSI, timing and operator actions. Whatever
// arrives, everything the node sends must stay inside the owned scope
// (harness.collect checks every frame) and no stranger becomes a peer.
//
// Each step reads: op, sender/rssi byte, time step, frame length, frame.
func FuzzNode(f *testing.F) {
	for _, g := range []mesh.Message{
		bondFrame(false), bondFrame(true), statusFrame(t0.Add(time.Hour)),
		mesh.Locate{Origin: totem, UID: 9, ReplyRequested: true, MinRSSI: -127, MaxHops: 99, Expiry: int32(t0.Unix()) + 4000},
		mesh.SmartGroup{Instruction: mesh.SmartGroupAdvertise, UID: 7, TimeoutMs: 9000, Members: []mesh.SmartGroupMember{{MAC: self}}},
		mesh.SmartGroup{Instruction: mesh.SmartGroupFinalize, UID: 7, Members: []mesh.SmartGroupMember{{MAC: totem}, {MAC: stranger}}},
	} {
		b, err := g.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(append([]byte{0, 0x14, 50, byte(len(b))}, b...))
		f.Add(append([]byte{1, 0x94, 200, byte(len(b))}, b...))
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		h := newHarness(t, func(c *Config) {
			c.AutoPair = true
			c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}
			c.Rand = rand.New(rand.NewPCG(3, 4))
			c.Logger = nil
		})
		for len(script) >= 4 {
			op, who, step, n := script[0], script[1], script[2], int(script[3])
			script = script[4:]
			n = min(n, len(script))
			frame := script[:n]
			script = script[n:]

			src := totem
			if who&0x80 != 0 {
				src = stranger
			}
			dst := mesh.Broadcast
			if who&0x40 != 0 {
				dst = self
			}
			rssi := -int8(who & 0x3f)
			switch op % 6 {
			case 0, 1, 2:
				h.collect(h.n.Receive(h.now, Received{Src: src, Dst: dst, RSSI: rssi, Data: frame}))
			case 3:
				h.collect(h.n.Pair(h.now))
			case 4:
				h.collect(h.n.Unbond(h.now, src))
			case 5:
				h.n.SetSOS(step&1 != 0)
			}
			h.advance(time.Duration(step) * 100 * time.Millisecond)
			for _, p := range h.n.Peers() {
				if p.MAC != totem {
					t.Fatalf("peer %s outside the owned scope", p.MAC)
				}
			}
		}
		h.advance(time.Minute)
	})
}
