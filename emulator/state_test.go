package emulator

import (
	"bytes"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ljagiello/totem-compass/store"
)

// TestStateRoundTripsThroughANode: what a device saves is what it comes
// back as. This is where two bugs lived — a mute that never survived,
// and a counter that made every look at the settings a flash write — so
// it is tested here rather than only on the board.
func TestStateRoundTripsThroughANode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	// A clock first: the peer's position is saved as a Unix second, which
	// a device with no clock of its own cannot work out.
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.rx(totem, self, -40, statusFrame(t0))
	h.advance(bootDebounce)
	h.n.SetColor(ColorIndigo, h.now)
	h.n.ToggleBrightness(h.now)
	h.n.SetSOS(true)
	h.n.MuteSOS(h.now)

	st := h.n.State(7)
	if st.ColorID != int8(ColorIndigo) || !st.SOSMuted || st.BootCount != 7 {
		t.Fatalf("saved state = %+v", st)
	}
	if st.Brightness == 0 || st.Brightness == 255 {
		t.Errorf("brightness saved as %d, want the dimmed level", st.Brightness)
	}
	if len(st.Peers) != 1 || st.Peers[0].MAC != [6]byte(totem) {
		t.Fatalf("peers = %+v", st.Peers)
	}
	if st.Peers[0].Lat == 0 || st.Peers[0].LastSeenUnix == 0 {
		t.Errorf("the peer was saved without where or when it was heard: %+v", st.Peers[0])
	}

	// A fresh node given that state comes up the same.
	fresh := newHarness(t, nil)
	if errs := fresh.n.Restore(&st, fresh.now); len(errs) != 0 {
		t.Fatalf("restore: %v", errs)
	}
	if fresh.n.BondCount() != 1 {
		t.Errorf("bonds after restore = %d", fresh.n.BondCount())
	}
	if !fresh.n.SOSMuted() {
		t.Error("the mute did not survive")
	}
	if got := fresh.n.LEDs().DefaultColor(); got != ColorIndigo {
		t.Errorf("crystal colour after restore = %s", got)
	}
	if got := fresh.n.LEDs().Brightness(); got >= 1 {
		t.Errorf("brightness after restore = %v, want the dimmed level", got)
	}
}

// TestSettingsKeyIgnoresTheFreeRunningCounter: the sleep counter grows on
// its own. If it drove the comparison, every look at the settings would
// write a record and a sector's worth would cost an erase with the radio
// stalled. The boot count is the other way round: it has to be written,
// or a boot is never recorded.
func TestSettingsKeyIgnoresTheFreeRunningCounter(t *testing.T) {
	h := newHarness(t, nil)
	first := SettingsKey(h.n.State(1))

	// Time passes; the device sleeps; nothing a person chose has changed.
	h.advance(time.Minute)
	if got := SettingsKey(h.n.State(1)); !bytes.Equal(got, first) {
		t.Error("a minute of sleep changed the key, so an idle device would write to flash")
	}
	// A new boot does change it.
	if got := SettingsKey(h.n.State(2)); bytes.Equal(got, first) {
		t.Error("a new boot did not change the key, so the boot count would never be written")
	}
	// And so does something a person chose.
	h.n.SetColor(ColorBlue, h.now)
	if got := SettingsKey(h.n.State(1)); bytes.Equal(got, first) {
		t.Error("a colour change did not change the key")
	}
}

// TestNamesOffTheAirCannotWedgeSaving: a peer's name is bytes from a
// frame, and nothing on that path checks them. One that is not UTF-8
// used to make the state refuse to encode, and from then on nothing was
// ever saved again.
func TestNamesOffTheAirCannotWedgeSaving(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Name = "emu\xff\xfe" })
	h.bond()
	bad := statusFrame(t0)
	bad.Name = "bad\xff\xfename"
	h.rx(totem, self, -40, bad)

	st := h.n.State(1)
	if _, err := st.MarshalBinary(); err != nil {
		t.Fatalf("a peer name off the air stopped the state encoding: %v", err)
	}
	if SettingsKey(st) == nil {
		t.Fatal("the change key could not be built")
	}
	for _, p := range st.Peers {
		if !utf8.ValidString(p.Name) {
			t.Errorf("peer name %q was saved as it arrived", p.Name)
		}
	}
	if !utf8.ValidString(st.Name) {
		t.Errorf("the device's own name was saved as %q", st.Name)
	}
}

// TestRestoreRefusesAPeerItDoesNotOwn: a saved list from a board that
// was flashed for a different Totem must not widen what this one talks
// to.
func TestRestoreRefusesAPeerItDoesNotOwn(t *testing.T) {
	h := newHarness(t, nil)
	errs := h.n.Restore(&store.State{Peers: []store.PeerState{
		{MAC: [6]byte(totem), Name: "mine"},
		{MAC: [6]byte(stranger), Name: "someone else's"},
	}}, h.now)
	if len(errs) != 1 {
		t.Fatalf("restore reported %v, want one refusal", errs)
	}
	if h.n.BondCount() != 1 {
		t.Errorf("bonds = %d, want only the owned one", h.n.BondCount())
	}
}
