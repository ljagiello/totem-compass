//go:build tinygo && esp32

package main

// The settings a reboot keeps: the name, the crystal colour and the bonds,
// as firmware 5.0.3 keeps them in config.json. They live in the journal in
// the store package, on one flash sector.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// settings is the saved state and the journal it came from.
type settings struct {
	log *slog.Logger
	j   *store.Journal
	// held is what was written last, so a save that would change nothing
	// costs no flash write. An erase runs with the radio stalled.
	held []byte
	// key is held with the counters zeroed: what a person could have
	// changed. The boot count and the total sleep grow on their own, and
	// comparing them would make every save a write.
	key []byte
	// state is what the device is running with.
	state store.State
}

// storeAtBoot says whether the settings are read while the device starts.
// Turning it off leaves the board bootable while the flash driver is
// being worked on: the store open command then reads them by hand.
const storeAtBoot = true

// openSettings reads the settings sector. A board whose flash holds
// nothing, an older format or noise starts with defaults: the device has
// to boot either way.
func openSettings(log *slog.Logger, atBoot bool) *settings {
	s := &settings{log: log}
	if !atBoot {
		log.Info("settings not read at boot; run store open")
		return s
	}
	j, err := store.Open(flashSector{addr: storeSector})
	if err != nil {
		log.Warn("settings unavailable, running without saving", "err", err)
		return s
	}
	s.j = j
	if n := j.Torn(); n > 0 {
		log.Warn("a save did not finish before a reset", "records", n)
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
	s.held = b
	s.key = settingsKey(s.state)
	s.state.BootCount++
	log.Info("settings restored", "name", s.state.Name, "peers", len(s.state.Peers),
		"boots", s.state.BootCount, "seq", j.Seq(), "free", j.Free())
	return s
}

// restore puts the saved bonds back into a node, so the device comes up
// still bonded, the way a Totem does.
func (s *settings) restore(n *emulator.Node, now time.Time) {
	for _, p := range s.state.Peers {
		// A saved peer that is no longer in the owned list is refused, and
		// that is worth saying: it means the board was reflashed for a
		// different Totem and the old bond is being dropped.
		if err := n.AddBond(mesh.MAC(p.MAC), p.Name, now); err != nil {
			s.log.Warn("saved bond not restored", "mac", mesh.MAC(p.MAC), "err", err)
		}
	}
	if len(s.state.Peers) > 0 {
		s.log.Info("bonds restored", "peers", len(s.state.Peers))
	}
	if b := s.state.Brightness; b > 0 {
		n.LEDs().SetBrightness(float64(b) / 255)
	}
	// A muted alarm stays muted: someone silenced it, and a power cut is
	// not them changing their mind.
	n.SetSOSMuted(s.state.SOSMuted)
}

// save writes the node's bonds and settings, and does nothing when they
// are what is already on the flash.
func (s *settings) save(n *emulator.Node) {
	if s.j == nil {
		return
	}
	cfg := n.Config()
	st := store.State{
		Name: cfg.Name, ColorID: cfg.ColorID,
		// The brightness a tap of the power button chose, and the sleep
		// the device has taken, both belong to the device rather than to
		// this boot.
		Brightness:      uint8(n.LEDs().Brightness() * 255),
		SOSMuted:        n.SOSMuted(),
		BootCount:       s.state.BootCount,
		SleepMs:         uint64(n.Power().SleptMs()),
		LearnedMaxVolts: s.state.LearnedMaxVolts,
	}
	for _, p := range n.Peers() {
		if len(st.Peers) == store.MaxPeers {
			break
		}
		ps := store.PeerState{MAC: [6]byte(p.MAC), Name: p.Status.Name}
		if p.Status.Lat != 0 || p.Status.Lon != 0 {
			ps.Lat, ps.Lon = p.Status.Lat, p.Status.Lon
		}
		if !p.LastHeard.IsZero() {
			ps.LastSeenUnix = p.LastHeard.Unix()
		}
		st.Peers = append(st.Peers, ps)
	}
	b, err := st.MarshalBinary()
	if err != nil {
		s.log.Warn("settings could not be encoded", "err", err)
		return
	}
	// Compare only what a person could have changed. The counters ride
	// along with a real change; letting them drive one would put a record
	// on the flash every time anything looked at the settings, filling a
	// sector — and an erase stalls the radio — for no news at all.
	key := settingsKey(st)
	if key == nil {
		s.log.Warn("settings could not be encoded")
		return
	}
	if bytes.Equal(key, s.key) {
		return // nothing a person changed, so nothing to write
	}
	start := time.Now()
	if err := s.j.Save(b); err != nil {
		s.log.Warn("settings could not be saved", "err", err)
		return
	}
	s.held, s.key, s.state = b, key, st
	s.log.Info("settings saved", "peers", len(st.Peers), "bytes", len(b),
		"took_ms", time.Since(start).Milliseconds(), "free", s.j.Free())
}

// settingsKey is the part of a state a person chose: the same bytes with
// the counters zeroed, so two states that differ only in how long the
// device has been awake compare equal. It returns nil when the state
// cannot be encoded, which the caller treats as a reason not to write.
func settingsKey(st store.State) []byte {
	st.BootCount, st.SleepMs = 0, 0
	b, err := st.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}

// forget wipes the sector, as a factory reset does.
func (s *settings) forget() error {
	if s.j == nil {
		return errors.New("no settings sector")
	}
	if err := s.j.Save(nil); err != nil {
		return err
	}
	s.held, s.key, s.state = nil, nil, store.State{}
	return nil
}

// open reads the settings sector now, for a board whose driver is being
// brought up.
func (s *settings) open(n *emulator.Node, now time.Time) {
	if s.j != nil {
		s.report()
		return
	}
	fresh := openSettings(s.log, true)
	s.j, s.held, s.key, s.state = fresh.j, fresh.held, fresh.key, fresh.state
	if s.j == nil {
		return
	}
	s.restore(n, now)
	s.save(n)
	s.report()
}

// report prints what is saved.
func (s *settings) report() {
	if s.j == nil {
		s.log.Warn("settings unavailable on this board")
		return
	}
	s.log.Info("settings", "name", s.state.Name, "color", s.state.ColorID,
		"boots", s.state.BootCount, "peers", len(s.state.Peers),
		"seq", s.j.Seq(), "used", s.j.Used(), "free", s.j.Free(), "torn", s.j.Torn())
	for _, p := range s.state.Peers {
		s.log.Info("saved peer", "mac", mesh.MAC(p.MAC), "name", p.Name,
			"lat", p.Lat, "lon", p.Lon, "seen_unix", p.LastSeenUnix)
	}
}

// flashSelfTest checks the flash driver on the scratch sector: erase, read
// back, write a pattern, read it again. It never touches the settings
// sector, so a board that fails it still boots with its bonds.
func flashSelfTest(log *slog.Logger) {
	sec := flashSector{addr: scratchSector}
	step := func(what string, err error) bool {
		if err != nil {
			log.Warn("flash test failed", "step", what, "err", err)
			return false
		}
		return true
	}
	before, err := flashStatus()
	if !step("status", err) {
		return
	}
	log.Info("flash chip", "status", fmt.Sprintf("%#06x", before), "rom_descriptor", flashChipDescriptor())
	start := time.Now()
	if !step("erase", sec.Erase()) {
		return
	}
	erase := time.Since(start)
	buf := make([]byte, 64)
	if !step("read after erase", sec.ReadAt(buf, 0)) {
		return
	}
	for i, b := range buf {
		if b != 0xff {
			log.Warn("flash test failed", "step", "erase left data", "offset", i, "byte", b)
			return
		}
	}
	want := make([]byte, 64)
	for i := range want {
		want[i] = byte(i*7 + 1)
	}
	start = time.Now()
	if !step("write", sec.WriteAt(want, 0)) {
		return
	}
	write := time.Since(start)
	got := make([]byte, 64)
	if !step("read back", sec.ReadAt(got, 0)) {
		return
	}
	if !bytes.Equal(got, want) {
		log.Warn("flash test failed", "step", "read back",
			"want", fmt.Sprintf("%x", want), "got", fmt.Sprintf("%x", got))
		return
	}
	// An unaligned read has to come back right too: the journal reads
	// headers wherever they land.
	if !step("read at 3", sec.ReadAt(got[:16], 3)) {
		return
	}
	if !bytes.Equal(got[:16], want[3:19]) {
		log.Warn("flash test failed", "step", "unaligned read", "got", fmt.Sprintf("%x", got[:16]))
		return
	}
	log.Info("flash test passed", "sector", fmt.Sprintf("%#x", scratchSector),
		"erase_ms", erase.Milliseconds(), "write_us", write.Microseconds())
}
