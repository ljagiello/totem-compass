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
		Name:    store.SanitizeName(n.cfg.Name),
		ColorID: n.cfg.ColorID,
		// Rounded, not truncated: 0.25 stored as 63 comes back as
		// 0.2470…, which is a change, so SetBrightness redraws and the
		// next save writes a different byte again — a flash write for a
		// level nobody touched.
		Brightness: uint8(n.leds.Brightness()*255 + 0.5),
		SOSMuted:   n.sosMuted,
		BootCount:  boots,
		SleepMs:    uint64(n.power.SleptMs()),
		// What this run learned about the pack and how long it slept.
		// Both are written and never read back — the firmware starts them
		// at zero on every boot — so they are here for whoever looks at
		// the sector, which is what `store` prints.
		LearnedMaxVolts: n.power.LearnedMaxVolts(),
	}
	// No cap here: n.order cannot hold more than maxBonds, which is half
	// of store.MaxPeers, so a limit at this end could never bind and
	// would only send a reader looking for a truncation that never
	// happens.
	for _, mac := range n.order {
		p := n.peers[mac]
		ps := store.PeerState{
			MAC:     [6]byte(mac),
			Name:    store.SanitizeName(p.status.Name),
			ColorID: int8(p.color),
		}
		if p.hasCoords {
			ps.Lat, ps.Lon = p.lat, p.lon
			// When the position was reported, not when the peer was last
			// heard from: this is what decides whether a peer is stale,
			// and the last time anything arrived from a peer would make a
			// position from an hour ago look as fresh as the frame that
			// carried nothing new.
			//
			// A Unix second, so it has to be a wall time. coordsAt is in
			// the device's own base, which on a board with no RTC starts
			// near the epoch at every boot, so it is a second anyone can
			// read only once the node has a clock to convert it with. The
			// zero time is the year 1, whose Unix value is a large
			// negative number, and is no use to anyone either.
			if n.clockSet && !p.coordsAt.IsZero() {
				ps.LastSeenUnix = n.wall(p.coordsAt).Unix()
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
	// A record that carries nothing at all is the absence of settings,
	// not a set of them: the bonds, the brightness and the reading below
	// are all worth applying from a real record, and none of them is
	// worth applying from a blank one.
	empty := st.Name == "" && st.ColorID == 0 && st.Brightness == 0 &&
		!st.SOSMuted && len(st.Peers) == 0
	for _, p := range st.Peers {
		if err := n.AddBond(p.MAC, p.Name, now); err != nil {
			errs = append(errs, err)
			continue
		}
		// Where the peer was last seen comes back with it, so the compass
		// can point at it before it has been heard from again — which is
		// what it was saved for.
		peer := n.peers[mesh.MAC(p.MAC)]
		// The same guard every frame off the air gets. These four bytes
		// came off a flash sector, which a torn write, a bad block or a
		// different firmware's layout can leave saying anything at all —
		// and NaN != 0, so the zero check alone lets it through, into the
		// distance, the compass dial and the relay decision.
		if usablePosition(p.Lat, p.Lon) {
			peer.hasCoords, peer.lat, peer.lon = true, p.Lat, p.Lon
			// The saved second is a wall time and coordsAt is in the
			// device's base, so it can only be put back once this node has
			// a clock of its own — which at boot, before any frame or fix
			// has arrived, it has not. Then the position comes back
			// without an age and starts aging from here, which is what it
			// was saved for: something for the compass to point at until
			// the peer is heard from again.
			peer.coordsAt = now
			// The second is four bytes off the same sector as the rest,
			// and store decodes it without a range check: one that is not
			// a time a device could have seen would come back as a
			// coordsAt centuries away, and a peer whose position is
			// always fresh is one the mesh never asks about again.
			// Not later than the clock this node is on, whatever the
			// sector says: a position from the future is one updateStale
			// measures a negative age for, so the peer is never stale and
			// the mesh is never asked where it went. That happens without
			// any corruption at all — a board that saved with a GNSS
			// clock and came back up on a peer's slower one.
			if saved := time.Unix(p.LastSeenUnix, 0); p.LastSeenUnix != 0 && n.clockSet &&
				usableClock(saved) && !saved.After(n.wall(now)) {
				peer.coordsAt = now.Add(saved.Sub(n.wall(now)))
			}
		}
		// 0 is red and also the zero value, so a saved 0 is left as the
		// colour the bond drew; anything else goes through the same check
		// every colour from outside gets.
		if p.ColorID != 0 {
			peer.color = paletteColor(n.log, "saved peer", p.ColorID, peer.color)
		}
	}
	if st.Brightness > 0 {
		n.leds.SetBrightness(float64(st.Brightness) / 255)
	}
	// The name comes back, unless this device was built with one. The
	// board takes `-X main.name=` over the saved name on purpose, and
	// restore() runs straight after New — so assigning unconditionally
	// put the old name back and then saved it again, and a reflash with
	// a new name never took effect. DefaultName is not a choice anyone
	// made, so a device still carrying it yields to the record.
	if name := store.SanitizeName(st.Name); name != "" && n.cfg.Name == DefaultName(n.cfg.MAC) {
		n.cfg.Name = name
	}
	// Neither the sleep total nor the learned maximum is put back. Both
	// are counters the firmware keeps in modes and starts at 0 on every
	// boot, so a device that restored them would be reporting something
	// its own firmware never does. They are saved so that whoever reads
	// the sector can see what the device last said, and the pack is
	// measured again on the first poll.
	// The alarm's mute and the crystal's colour come from a record that
	// says something. `store open` on a sector that is empty or
	// unreadable restores a zero State, and taking that at face value
	// un-muted an alarm and turned the crystal red — settings the
	// operator had chosen on a device that had not saved them yet.
	//
	// A muted alarm otherwise stays muted: someone silenced it, and a
	// power cut is not them changing their mind.
	if empty {
		n.log.Info("nothing saved to restore; keeping what the device is running with")
	} else {
		n.sosMuted = st.SOSMuted
		// The crystal's own colour comes off the same sector as the
		// peers', so it gets the same check. 0 is red, which is also the
		// default, so there is nothing to tell apart there.
		c := paletteColor(n.log, "saved settings", st.ColorID, n.leds.DefaultColor())
		n.cfg.ColorID = int8(c)
		n.leds.SetDefaultColor(c)
	}
	// A reading, and the mode that follows from it, whatever the record
	// said — this is the node coming into step with its own sensors, not
	// a setting. A device restoring
	// its settings at boot has not polled yet, so without this it reports
	// power mode normal until the first poll — and a device coming up on
	// a pack below the cutoff has to power down rather than report that
	// it has.
	n.read(now)
	n.applyPowerMode(now)
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
	// And what the power model learned about the pack, for the same
	// reason: nobody chose it, it creeps on its own as the battery is
	// read, and letting it drive the comparison would put a record on the
	// flash for a hundredth of a volt. It is still written whenever
	// something a person did causes a save.
	st.LearnedMaxVolts = 0
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
