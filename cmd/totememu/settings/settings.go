// Package settings keeps what a reboot has to remember — the name, the
// crystal colour, the alarm's mute and the bonds — in the journal the
// store package writes to one flash sector, the way firmware 5.0.3 keeps
// config.json.
//
// It is its own package for the reason cmd/totememu/ring is: everything
// here is plain Go over a store.Sector and an emulator.Node, and the
// board's flash driver is the only part that cannot be compiled on a
// host. Passing the sector in rather than reaching for it means this can
// be tested, which is worth doing — the two worst faults found in it were
// a flag copied in one place and not another, and both would have cost
// someone every bond and setting on their device.
package settings

import (
	"bytes"
	"errors"
	"log/slog"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// Store is the saved state and the journal it came from.
type Store struct {
	log *slog.Logger
	j   *store.Journal
	// key is the last record written, with the free-running counter
	// zeroed: what a person could have changed, plus the boot count. A
	// save that would not change it costs no flash write. The total sleep
	// grows on its own, and comparing it would make every save a write.
	key []byte
	// state is what the device is running with.
	state store.State
	// found is whether the flash held a record this boot. The zero State
	// above is what a board with nothing saved runs with, and it is not
	// a set of settings: nobody chose it.
	found bool
}

// Unread is a board whose flash driver is being worked on: it boots
// without reading the sector and saves nothing, and `store open` reads
// the settings by hand later.
//
// A constructor rather than a nil sector, because a nil interface and an
// interface holding a nil pointer are not the same value in Go, and the
// second would sail past a nil check and panic inside the journal —
// which on a board is a boot loop rather than the missing driver it is
// meant to describe.
func Unread(log *slog.Logger) *Store {
	log.Info("settings not read at boot; run store open")
	return &Store{log: log}
}

// Open reads the settings sector. A board whose flash holds nothing, an
// older format or noise starts with defaults: the device has to boot
// either way.
func Open(log *slog.Logger, sec store.Sector) *Store {
	s := &Store{log: log}
	// A sector that is not there comes back as an error from store.Open,
	// which is where the check belongs, and lands in the same warning as
	// every other reason the settings cannot be read. Unread is how a
	// caller says so deliberately.
	j, err := store.Open(sec)
	if err != nil {
		log.Warn("settings unavailable, running without saving", "err", err)
		return s
	}
	s.j = j
	if n := j.Torn(); n > 0 {
		log.Warn("a save did not finish before a reset", "records", n)
	}
	if n := j.Blank(); n > 0 {
		// Not a torn write: these are whole records that say nothing,
		// which this build refuses to write and an earlier one did not.
		log.Warn("the sector holds records with nothing in them", "records", n)
	}
	b, err := j.Load()
	if errors.Is(err, store.ErrEmpty) {
		log.Info("no saved settings: first boot on this board")
		return s
	}
	if err != nil {
		log.Warn("settings could not be read", "err", err)
		return s
	}
	if err := s.state.UnmarshalBinary(b); err != nil {
		log.Warn("saved settings are not this version's, ignoring them", "err", err)
		s.state = store.State{}
		return s
	}
	s.key = emulator.SettingsKey(s.state)
	s.found = true
	s.state.BootCount++
	log.Info("settings restored", "name", s.state.Name, "peers", len(s.state.Peers),
		"boots", s.state.BootCount, "seq", j.Seq(), "free", j.Free())
	return s
}

// State is what the device is running with, for the caller that builds
// the node out of it.
//
// A copy, peers and all. The struct alone would be one: the slice header
// is copied but the array under it is not, so a caller holding the
// result could write through to the record this Store compares against
// to decide whether a save is needed — and then the save that was needed
// would not happen. The emulator package clones at the same boundary for
// the same reason.
func (s *Store) State() store.State {
	st := s.state
	st.Peers = append([]store.PeerState(nil), s.state.Peers...)
	return st
}

// Found is whether the flash holds a record: one was read at boot, or
// one has been written since. A board with nothing saved runs on the
// zero State, and that is not a set of settings — nobody chose it.
func (s *Store) Found() bool { return s.found }

// Restore puts the saved bonds and settings back into a node, so the
// device comes up as it went down.
func (s *Store) Restore(n *emulator.Node, now time.Time) {
	// A copy, for the reason State() hands one out: this goes into
	// another package, and the record it points at is the one this Store
	// compares against to decide whether a save is needed. Nothing over
	// there writes to it today, and the day something does — a name
	// normalised in place, peers sorted — the comparison would match, the
	// save that was needed would not happen, and the device would lose
	// what it had just been told.
	var rec *store.State
	if s.found {
		held := s.State()
		rec = &held
	}
	had := n.BondCount()
	for _, err := range n.Restore(rec, now) {
		// A saved peer that is no longer in the owned list is refused,
		// and that is worth saying: it means the board was reflashed for
		// a different Totem and the old bond is being dropped.
		s.log.Warn("saved bond not restored", "err", err)
	}
	// What this record brought back, not what the node holds: a board
	// reflashed for a different Totem refuses every saved bond, and
	// saying three were restored beside three refusals is the opposite
	// of a report. Counting the difference rather than the total is what
	// makes it the record's doing — Restore runs into a fresh node
	// today, and a count of everything bonded would stop being an answer
	// to "what was restored" the moment it does not.
	if got := n.BondCount() - had; got > 0 {
		s.log.Info("bonds restored", "peers", got, "saved", len(s.state.Peers))
	}
}

// Save writes the node's bonds and settings, and does nothing when they
// are what is already on the flash.
func (s *Store) Save(n *emulator.Node) {
	if s.j == nil {
		return
	}
	st := n.State(s.state.BootCount)
	b, err := st.MarshalBinary()
	if err != nil {
		s.log.Warn("settings could not be encoded", "err", err)
		return
	}
	// Compare what a person could have changed, and the boot count. The
	// total sleep is left out: it grows on its own, and letting it drive
	// a write would put a record on the flash every time anything so much
	// as looked at the settings, filling a sector — and an erase stalls
	// the radio — for no news at all.
	key := emulator.SettingsKey(st)
	if key == nil {
		s.log.Warn("settings could not be encoded")
		return
	}
	if bytes.Equal(key, s.key) {
		return // nothing a person changed, so nothing to write
	}
	// A flash write is the firmware's 'vfs write' blocker: while it runs,
	// nothing feeds the watchdog.
	n.Power().Block(emulator.BlockVFSWrite)
	start := time.Now()
	err = s.j.Save(b)
	n.Power().Unblock(emulator.BlockVFSWrite)
	if err != nil {
		s.log.Warn("settings could not be saved", "err", err)
		return
	}
	// found with them: a save means the sector holds a record from here
	// on, whatever it held at boot, so the field goes on saying what is
	// on the flash rather than what was found on it once.
	s.key, s.state, s.found = key, st, true
	s.log.Info("settings saved", "peers", len(st.Peers), "bytes", len(b),
		"took_ms", time.Since(start).Milliseconds(), "free", s.j.Free())
}

// Forget wipes the sector and the node's bonds, as a factory reset does.
// Wiping only the flash would not last a second: the save that follows
// the command writes the live bonds straight back, and the board would
// come up still bonded after being told to forget them.
func (s *Store) Forget(n *emulator.Node, now time.Time) error {
	if s.j == nil {
		return errors.New("no settings sector")
	}
	n.FactoryReset(now)
	empty := n.State(0)
	// The record a reset writes says nothing about the pack or the run
	// that is ending. Neither is read back at boot, so this is only about
	// what someone finds in the sector afterwards — but a record labeled
	// empty should be.
	empty.LearnedMaxVolts = 0
	empty.SleepMs = 0
	b, err := empty.MarshalBinary()
	if err != nil {
		return err
	}
	if err := s.j.Save(b); err != nil {
		return err
	}
	// found with them: the sector holds a record from here on, whatever
	// it held at boot, and the field says what is on the flash rather
	// than what was found on it once.
	s.key, s.state, s.found = emulator.SettingsKey(empty), empty, true
	return nil
}

// Reopen reads the settings sector now, for a board whose driver is
// being brought up.
func (s *Store) Reopen(n *emulator.Node, now time.Time, sec store.Sector) {
	if s.j != nil {
		s.Report()
		return
	}
	fresh := Open(s.log, sec)
	// found as well as the rest. Leaving it behind made this path restore
	// nothing — Restore reads no record where there was one — and then
	// save the running defaults over the record it had just read, which
	// on a board brought up without reading the sector at boot is every
	// bond and setting the device had.
	s.j, s.key, s.state, s.found = fresh.j, fresh.key, fresh.state, fresh.found
	if s.j == nil {
		return
	}
	s.Restore(n, now)
	s.Save(n)
	s.Report()
}

// Report prints what is saved.
func (s *Store) Report() {
	if s.j == nil {
		s.log.Warn("settings unavailable on this board")
		return
	}
	s.log.Info("settings", "name", s.state.Name, "color", s.state.ColorID,
		"boots", s.state.BootCount, "peers", len(s.state.Peers),
		// What the device last reported about itself. Neither comes back
		// at boot — the firmware starts both at zero — so this line is
		// the only place they can be seen.
		"last_run_slept_ms", s.state.SleepMs, "last_run_max_volts", s.state.LearnedMaxVolts,
		"seq", s.j.Seq(), "used", s.j.Used(), "free", s.j.Free(),
		// Both counts: `store` is where someone looks after flash
		// trouble, and a warning logged once at boot is not there any
		// more when they do.
		"torn", s.j.Torn(), "blank", s.j.Blank())
	for _, p := range s.state.Peers {
		s.log.Info("saved peer", "mac", mesh.MAC(p.MAC), "name", p.Name,
			"lat", p.Lat, "lon", p.Lon, "seen_unix", p.LastSeenUnix)
	}
}
