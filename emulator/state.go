package emulator

// What a node keeps across a reboot, and what it takes back.
//
// The firmware writes this to a flash sector and reads it at boot; the
// shaping lives here so it can be tested on a host, which is where two
// bugs in it were found — a counter that forced a write every time
// anything looked at the settings, and a mute that never survived.

import (
	"time"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// State is what this node would have saved: its name and colour, the
// brightness and mute someone chose, the bonds, and the counters it
// carries across boots. Names are made storable on the way out, because
// a peer's name is bytes off the air and one that cannot be encoded
// would stop every save that followed.
func (n *Node) State(boots uint32) store.State {
	st := store.State{
		Name:       store.SanitizeName(n.cfg.Name),
		ColorID:    n.cfg.ColorID,
		Brightness: uint8(n.leds.Brightness() * 255),
		SOSMuted:   n.sosMuted,
		BootCount:  boots,
		SleepMs:    uint64(n.power.SleptMs()),
		// LearnedMaxVolts belongs to the battery rather than to the node,
		// so whoever holds the saved state carries it over.
	}
	for _, mac := range n.order {
		if len(st.Peers) == store.MaxPeers {
			break
		}
		p := n.peers[mac]
		ps := store.PeerState{
			MAC:     [6]byte(mac),
			Name:    store.SanitizeName(p.status.Name),
			ColorID: int8(p.color),
		}
		if p.hasCoords {
			ps.Lat, ps.Lon = p.lat, p.lon
			// When the position was reported, not when the peer was last
			// heard from: Restore reads this back into coordsAt, which is
			// what decides whether a peer is stale. The last time anything
			// arrived from a peer would make a position from an hour ago
			// look as fresh as the frame that carried nothing new.
			//
			// The zero time is the year 1, and its Unix value is a large
			// negative number that would come back as a timestamp and
			// saturate every duration measured from it.
			if !p.coordsAt.IsZero() {
				ps.LastSeenUnix = p.coordsAt.Unix()
			}
		}
		st.Peers = append(st.Peers, ps)
	}
	return st
}

// Restore puts a saved state back: the bonds, the brightness and the
// mute. A bond whose Totem is no longer owned is refused and reported,
// which is what happens when a board is reflashed for a different one.
func (n *Node) Restore(st store.State, now time.Time) []error {
	var errs []error
	for _, p := range st.Peers {
		if err := n.AddBond(p.MAC, p.Name, now); err != nil {
			errs = append(errs, err)
			continue
		}
		// Where the peer was last seen comes back with it, so the compass
		// can point at it before it has been heard from again — which is
		// what it was saved for.
		peer := n.peers[mesh.MAC(p.MAC)]
		if p.Lat != 0 || p.Lon != 0 {
			peer.hasCoords, peer.lat, peer.lon = true, p.Lat, p.Lon
			if p.LastSeenUnix != 0 {
				peer.coordsAt = time.Unix(p.LastSeenUnix, 0)
			}
		}
		if p.ColorID != 0 {
			peer.color = Color(p.ColorID)
		}
	}
	if st.Brightness > 0 {
		n.leds.SetBrightness(float64(st.Brightness) / 255)
	}
	// A muted alarm stays muted: someone silenced it, and a power cut is
	// not them changing their mind.
	n.sosMuted = st.SOSMuted
	// The colour is applied whatever it is: 0 is red, which is also the
	// default, so there is nothing to tell apart.
	n.cfg.ColorID = st.ColorID
	n.leds.SetDefaultColor(Color(st.ColorID))
	return errs
}

// SettingsKey is the part of a state a person chose, as bytes: the same
// encoding with the free-running counter zeroed. Two states that differ
// only in how long the device has been awake compare equal, so a save
// that would say nothing new costs no flash write — and an erase stalls
// the radio. The boot count is kept, because a boot that writes nothing
// is a boot that is never recorded.
func SettingsKey(st store.State) []byte {
	st.SleepMs = 0
	// The same goes for where and when each peer was last heard: a bonded
	// Totem sends its position every few seconds, and letting that drive
	// a write would put a record on the flash every minute for the life
	// of the board. What counts is which peers are bonded, and as what.
	peers := make([]store.PeerState, len(st.Peers))
	for i, p := range st.Peers {
		p.Lat, p.Lon, p.LastSeenUnix = 0, 0, 0
		peers[i] = p
	}
	st.Peers = peers
	b, err := st.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}
