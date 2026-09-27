package emulator

// Carrying the mesh for Totems this node does not own: off by default,
// and under handle_mesh_msg's budgets when it is on.

import (
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
)

// strangerLocate builds a locate frame originated by a Totem we do not
// own, arriving from a last hop far enough away to clear relay_min_dist.
func strangerLocate(t *testing.T, request bool, uid uint16) []byte {
	t.Helper()
	l := mesh.Locate{
		Origin: stranger, Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 3,
		UID: uid, ReplyRequested: request,
		MinRSSI: mesh.DefaultMinRSSI, MinDistM: -1, MaxDistM: -1,
		MaxHops: mesh.DefaultMaxHops, RelayMinDistM: mesh.DefaultRelayMinDist,
		// Far from the harness position, so the 10 m rule does not bite.
		LastHopLat: 37.8000, LastHopLon: -122.4194,
		Expiry: int32(t0.Add(2 * time.Minute).Unix()),
	}
	b, err := l.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// relayHarness is a node with a fix and a clock, which the mesh needs.
func relayHarness(t *testing.T, relayUnowned bool) *harness {
	t.Helper()
	h := newHarness(t, func(c *Config) {
		c.Position = &Position{Lat: 37.7749, Lon: -122.4194, AccuracyM: 3}
		c.RelayUnowned = relayUnowned
	})
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(bootAnim + time.Second)
	h.take()
	return h
}

// relayed counts the locate frames the node put on the air.
func relayed(sent []sent) int {
	n := 0
	for _, s := range sent {
		if mesh.IsLocate(s.Data) {
			n++
		}
	}
	return n
}

// TestAStrangersFrameIsNotCarriedByDefault: the node does not transmit on
// behalf of a Totem its owner does not own unless asked to. A real Totem
// does carry them, and the difference is the point of the option.
func TestAStrangersFrameIsNotCarriedByDefault(t *testing.T) {
	h := relayHarness(t, false)
	h.collect(h.n.Receive(h.now, Received{
		Src: stranger, Dst: mesh.Broadcast, RSSI: -40,
		Data: strangerLocate(t, true, 4242),
	}))
	if got := relayed(h.take()); got != 0 {
		t.Errorf("the node relayed %d frame(s) for a Totem it does not own", got)
	}
}

// TestAStrangersFrameIsCarriedWhenAsked: with the option on, the frame is
// rebroadcast — the same bytes with one more hop and our own position as
// the last hop, which is what Parser._relay_frame does.
func TestAStrangersFrameIsCarriedWhenAsked(t *testing.T) {
	h := relayHarness(t, true)
	in := strangerLocate(t, true, 4243)
	h.collect(h.n.Receive(h.now, Received{
		Src: stranger, Dst: mesh.Broadcast, RSSI: -40, Data: in,
	}))
	out := h.take()
	if got := relayed(out); got != 1 {
		t.Fatalf("relayed %d frames, want 1", got)
	}
	var got mesh.Locate
	for _, s := range out {
		if mesh.IsLocate(s.Data) {
			m, err := mesh.Parse(s.Data)
			if err != nil {
				t.Fatal(err)
			}
			got = m.(mesh.Locate)
		}
	}
	if got.Origin != stranger {
		t.Errorf("the relay rewrote the origin to %s", got.Origin)
	}
	if got.UID != 4243 {
		t.Errorf("the relay changed the uid to %d", got.UID)
	}
	if got.Hops != 1 {
		t.Errorf("hops = %d, want 1", got.Hops)
	}
	if got.LastHopLat != 37.7749 {
		t.Errorf("last hop latitude = %v, want our own position", got.LastHopLat)
	}
}

// TestTheTwoRelayBudgetsAreSeparate: handle_mesh_msg sheds a request at
// mesh_ack_cnt >= 20 (-3) and a reply at mesh_no_ack_cnt >= 10 (-4), so a
// request travels twice as far through a crowd as the answer to it. One
// budget running out must not stop the other.
func TestTheTwoRelayBudgetsAreSeparate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request bool
		want    int
	}{
		{"requests", true, meshAckLimit},
		{"replies", false, meshNoAckLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := relayHarness(t, true)
			// Well past either budget, each with its own uid so the
			// de-duplication does not account for the drop.
			for i := range 40 {
				h.collect(h.n.Receive(h.now, Received{
					Src: stranger, Dst: mesh.Broadcast, RSSI: -40,
					Data: strangerLocate(t, tt.request, uint16(1000+i)),
				}))
			}
			if got := relayed(h.take()); got != tt.want {
				t.Errorf("relayed %d %s, want the budget of %d", got, tt.name, tt.want)
			}
		})
	}
}

// TestCarryingTheMeshOpensNothingElse: the option is about the mesh. A
// peer frame from a Totem we do not own is still dropped unread, so the
// node never takes a stranger into its peer table or answers one.
func TestCarryingTheMeshOpensNothingElse(t *testing.T) {
	h := relayHarness(t, true)
	p := mesh.Peer{
		Command: mesh.PeerStatus, Name: "not ours", TimeOfDayMs: -1, Unix: -1,
		Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 3,
	}
	b, err := p.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	h.collect(h.n.Receive(h.now, Received{
		Src: stranger, Dst: mesh.Broadcast, RSSI: -40, Data: b,
	}))
	if _, known := h.n.peers[stranger]; known {
		t.Error("a Totem we do not own reached the peer table")
	}
	if got := h.take(); len(got) != 0 {
		t.Errorf("the node answered a Totem it does not own with %d frame(s)", len(got))
	}
}
