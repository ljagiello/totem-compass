package emulator

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ljagiello/totem-compass/mesh"
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

// TestASavedPositionTimeIsChecked: the second a peer's position was seen
// is four bytes off the same flash sector as everything else, and store
// decodes it without a range check. One that is not a time a device could
// have seen gave a coordsAt centuries away — and a peer whose position is
// always fresh is one the mesh never asks about again.
func TestASavedPositionTimeIsChecked(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Restore(&store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", Lat: 37.775, Lon: -122.42,
		LastSeenUnix: 1 << 62,
	}}}, h.now)

	p := h.n.peers[totem]
	if p == nil || !p.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := h.now.Sub(p.coordsAt); age < 0 {
		t.Errorf("the position is %v old, which is in the future", age)
	}
}

// TestBrightnessComesBackWhereItWas: the level is stored as one byte.
// Truncating meant 0.25 came back as 0.2470…, which counts as a change —
// so the strip redrew and the next save wrote a different byte again, a
// flash write for a level nobody touched.
func TestBrightnessComesBackWhereItWas(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.n.ToggleBrightness(h.now) // to the dim level
	was := h.n.LEDs().Brightness()

	st := h.n.State(1)
	fresh := newHarness(t, nil)
	fresh.n.Restore(&st, fresh.now)

	// Within half a step of the byte it was stored as, which is the best
	// one byte can do. Truncating is a whole step out.
	if got := fresh.n.LEDs().Brightness(); math.Abs(got-was)*255 > 0.5 {
		t.Errorf("brightness came back as %v, want %v — %.2f of a step out",
			got, was, math.Abs(got-was)*255)
	}
	// And saving it again produces the same byte, so an idle device
	// writes nothing.
	if a, b := st.Brightness, fresh.n.State(1).Brightness; a != b {
		t.Errorf("the saved level drifted from %d to %d across one restore", a, b)
	}
}

// TestASavedPositionIsNeverFromTheFuture: a board that saved with a GNSS
// clock and came back up on a peer's slower one has a saved second later
// than its own wall time. Used as it stands, the peer is never stale and
// the mesh is never asked where it went — no corruption required.
func TestASavedPositionIsNeverFromTheFuture(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetClock(t0, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.Restore(&store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", Lat: 37.775, Lon: -122.42,
		LastSeenUnix: t0.Add(5 * 365 * 24 * time.Hour).Unix(),
	}}}, h.now)

	p := h.n.peers[totem]
	if p == nil || !p.hasCoords {
		t.Fatal("the position did not come back")
	}
	if age := h.now.Sub(p.coordsAt); age < 0 {
		t.Errorf("the position is dated %v into the future", -age)
	}
}

// TestTheRestoreBondWarningIsThrottled: a Totem that still holds us in
// its config.json unicasts a status every one to four seconds. On a
// device already at eight bonds that warning fired on every one of them
// — the same flood the pairing warning was quietened for, in the branch
// beside it.
func TestTheRestoreBondWarningIsThrottled(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Owned = manyOwned()
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for _, mac := range h.n.Config().Owned[:maxBonds] {
		if err := h.n.AddBond(mac, "peer", h.now); err != nil {
			t.Fatal(err)
		}
	}
	// A ninth Totem that kept us, unicasting status as the firmware does.
	ninth := mesh.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x99}
	h.n.cfg.Owned = append(h.n.cfg.Owned, ninth)
	for range 6 {
		h.rx(ninth, self, -40, statusFrame(t0))
		h.advance(2 * time.Second)
	}
	if n := strings.Count(logged.String(), "bond not restored"); n != 1 {
		t.Errorf("the bond-limit warning fired %d times, want once", n)
	}
}

// TestRestorePutsThePowerModeInStep: Restore takes a fresh reading, and
// the mode has to follow it. Left to the next poll, `power` and `status`
// report the mode from before the restore in between — and on a device
// coming up on a flat pack that is the interval in which nothing says so.
func TestRestorePutsThePowerModeInStep(t *testing.T) {
	// A pack the node comes up happy with, which then reads low. New
	// puts the mode in step with what it read at the time, so the
	// reading has to change after it for this to be about Restore.
	pack := &collapsingPack{b: Battery{Volts: 4.0, Percent: 90}}
	h := newHarness(t, func(c *Config) { c.Sensors = pack })
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Fatalf("a node on a full pack starts in mode %s, want normal", got)
	}

	// No poll in between: this is the driver's reading at boot, with the
	// settings about to be put back.
	pack.b = Battery{Volts: 3.4, Percent: 5}
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Fatalf("the node moved to %s before anything read the pack", got)
	}
	h.n.Restore(&store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerLow {
		t.Errorf("after restoring on a 3.4 V pack the mode is %s, want low", got)
	}
}

// TestARestoreOnAFlatPackPowersDown: moving the power mode is not acting
// on it. Restore called update and dropped what it reported, so the mode
// went to off while the device stayed on — and because update only
// reports a change once, every poll afterwards saw nothing to do. The
// radio windows kept running on a pack below the cutoff.
func TestARestoreOnAFlatPackPowersDown(t *testing.T) {
	// Under the cutoff, which is 3.15 rather than the 3.30 this used to
	// assume — 3.2 V is a flat pack that is still running.
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 3.1, 0 })
	h.n.Restore(&store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerOff {
		t.Fatalf("a 3.1 V pack reads mode %s, want off", got)
	}
	if !h.n.Power().Off() {
		t.Error("the device reports power mode off while still running")
	}
	// And it stays down: a poll must not find it half-off.
	h.advance(30 * time.Second)
	if !h.n.Power().Off() {
		t.Error("the device came back up by itself")
	}
}

// TestARestoreOnALowPackSaysSo: the same for the low-battery ring, which
// is the only thing that tells someone holding the device.
func TestARestoreOnALowPackSaysSo(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.BattVolts, c.BattPct = 3.4, 5
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.n.Restore(&store.State{}, h.now)
	if got := h.n.Power().Mode(); got != PowerLow {
		t.Fatalf("a 3.4 V pack reads mode %s, want low", got)
	}
	if !strings.Contains(logged.String(), "power mode") {
		t.Errorf("the mode changed without a word: %s", logged.String())
	}
	// The reminder is owed, not lost and not drawn over the power-up
	// ring: a device coming up on a low pack shows its boot animation
	// and then says the pack is low, in that order, which is what the
	// firmware does with every passing event that wants the strip.
	if got := h.n.LEDs().Animation(); got != AnimBoot {
		t.Errorf("the ring shows %s during the power-up animation", got)
	}
	h.advance(bootAnim + time.Second)
	if got := h.n.LEDs().Animation(); got != AnimLowBattery {
		t.Errorf("once the strip was free the ring showed %s, want the low-battery flash", got)
	}
}

// TestAResetLeavesNothingToSave: the board snapshots the state right
// after the reset, so anything the reset re-learns lands in the record
// meant to be empty. Clearing before the fresh reading put the pack
// straight back — and marked it measured, so a later restore could no
// longer correct it.
func TestAResetLeavesNothingToSave(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BattVolts, c.BattPct = 4.25, 100 })
	h.collect(h.n.Poll(h.now))
	h.n.FactoryReset(h.now)
	// The device has measured the pack in front of it again, which is a
	// reading rather than the memory it was told to drop — but nothing
	// older than that survives.
	if got := h.n.Power().LearnedMaxVolts(); got > 4.26 {
		t.Errorf("a reset kept %v, which is more than the pack reads", got)
	}
}

// TestABlankRecordIsNotSettings: `store open` on a sector that is empty
// or unreadable has no record to hand back. Taking that for a set of
// settings un-muted an alarm and turned the crystal red — choices
// someone had made on a device that had not saved them yet.
func TestABlankRecordIsNotSettings(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ColorID = int8(ColorBlue) })
	h.advance(bootDebounce)
	h.n.SetSOS(true)
	h.n.MuteSOS(h.now)
	if !h.n.SOSMuted() {
		t.Fatal("the alarm was not muted to begin with")
	}

	h.n.Restore(nil, h.now)
	if !h.n.SOSMuted() {
		t.Error("restoring nothing un-muted the alarm")
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorBlue {
		t.Errorf("restoring nothing turned the crystal %s", got)
	}
}

// TestABuiltInNameBeatsTheSavedOne: the board takes `-X main.name=` over
// the saved name on purpose, and restore runs straight after New — so
// assigning unconditionally put the old name back, saved it again, and a
// reflash with a new name never took effect.
func TestABuiltInNameBeatsTheSavedOne(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Name = "fieldunit7" })
	h.n.Restore(&store.State{Name: "emu_totem_abb0"}, h.now)
	if got := h.n.Config().Name; got != "fieldunit7" {
		t.Errorf("the saved name overwrote the built-in one: %q", got)
	}

	// A device still carrying the default has not been named, so the
	// record wins.
	h = newHarness(t, nil)
	h.n.Restore(&store.State{Name: "lcfs_spare"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_spare" {
		t.Errorf("the saved name did not come back: %q", got)
	}
}

// TestABrightnessThatIsNotANumber: NaN survives min and max — every
// comparison with it is false — and would reach the uint8 conversion in
// every pixel, which Go leaves implementation-defined.
func TestABrightnessThatIsNotANumber(t *testing.T) {
	l := newLEDs(t0, ColorTeal)
	was := l.Brightness()
	l.SetBrightness(math.NaN())
	if got := l.Brightness(); math.IsNaN(got) {
		t.Error("the strip took a brightness that is not a number")
	} else if got != was {
		t.Errorf("brightness moved to %v", got)
	}
	// And the same for the download's share, which reaches the ring.
	l.Play(AnimOTA, t0)
	l.SetProgress(0.5)
	l.Tick(t0.Add(time.Second))
	half := litPixels(l)
	if half == 0 {
		t.Fatal("half a download lit no pixels at all")
	}
	// The share that is not a number leaves the ring where it was: it is
	// not a smaller download, it is no answer, and the last real one is
	// the truest thing on the strip.
	l.SetProgress(math.NaN())
	l.Tick(t0.Add(2 * time.Second))
	if got := litPixels(l); got != half {
		t.Errorf("a share that is not a number moved the ring from %d pixels to %d", half, got)
	}
}

// TestARecordThatSaysNothingIsStillARecord: Restore used to work out
// whether the flash held anything by looking at the State's fields, and
// a device saved with no name, no bonds, the default colour and the
// brightness left alone reads as blank that way — which is exactly what
// a device someone has only ever switched on and muted writes. Its mute
// was then dropped at every boot.
func TestARecordThatSaysNothingIsStillARecord(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)

	// The record a device that has only been muted writes: everything
	// else is still the default.
	rec := store.State{SOSMuted: true}
	h.n.Restore(&rec, h.now)
	if !h.n.SOSMuted() {
		t.Error("a saved mute was read as an empty record and dropped")
	}
	// No record at all is TestABlankRecordIsNotSettings, in review19.
}

// TestABuiltInNameThatLooksLikeTheDefault: the guard was `n.cfg.Name ==
// DefaultName(MAC)`, which asks the answer rather than the question. A
// board deliberately flashed with the name it would have been given
// anyway had chosen that name, and the saved one overwrote it.
func TestABuiltInNameThatLooksLikeTheDefault(t *testing.T) {
	built := DefaultName(self)
	h := newHarness(t, func(c *Config) { c.Name = built })
	h.n.Restore(&store.State{Name: "lcfs_old_spare"}, h.now)
	if got := h.n.Config().Name; got != built {
		t.Errorf("the saved name overwrote the one the board was flashed with: %q", got)
	}
}

// TestTheRecordHoldsEveryBondTheNodeCan: State's truncation cannot bind
// while a node holds fewer bonds than a record holds peers, and the only
// thing between a breach of that and a device that silently stops saving
// is MarshalBinary's error. This pins the relationship rather than the
// branch: it is what has to stay true, and what someone raising the bond
// limit would otherwise not be told.
func TestTheRecordHoldsEveryBondTheNodeCan(t *testing.T) {
	if maxBonds > store.MaxPeers {
		t.Errorf("a node holds %d bonds and a saved record holds %d peers: "+
			"every bond past the record's limit would stop the device saving at all",
			maxBonds, store.MaxPeers)
	}
}

// TestANameThatIsNotTextAtAll: New decides whether anyone chose the name
// before SanitizeName has had its say, and a name that is not text comes
// out of that empty. Such a device ran nameless and, because it counted
// as named, refused the saved name at every boot afterwards.
func TestANameThatIsNotTextAtAll(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Name = "\xff\xfe\xff" })
	// Nameless is not a name: the default stands in.
	if got := h.n.Config().Name; got != DefaultName(self) {
		t.Errorf("a name that is not text left the device called %q", got)
	}
	// And nobody chose it, so the record's name comes back.
	h.n.Restore(&store.State{Name: "lcfs_spare"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_spare" {
		t.Errorf("the saved name did not come back: %q", got)
	}
}

// countingPack answers like a driver and counts how often it is asked.
type countingPack struct {
	b     Battery
	reads int
}

func (c *countingPack) Read(time.Time) Sensors {
	c.reads++
	return Sensors{Battery: c.b}
}

// TestEveryRestorePathReadsTheSensorsOnce: the reading and the power
// mode that follows it are the node coming into step with its own
// sensors rather than anything a record said, so they happen on every
// path through Restore — including the one where there is no record,
// which is where they were missing.
//
// Once, too, which is the narrower half: a reading taken in both
// restoreRecord and the tail would poll a driver twice per boot and
// book two spans of time against a device that lived through one.
//
// What no test here can see is that the pairing appears once in the
// source; the refactor that put it there is for the reader, and this is
// for the behavior.
func TestEveryRestorePathReadsTheSensorsOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *store.State
	}{
		{"no record", nil},
		{"a record", &store.State{Name: "lcfs_spare"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A pack under the cutoff: the device has to come up and go
			// straight back down, rather than report mode normal until
			// something polls it.
			pack := &countingPack{b: Battery{Volts: 3.1, Percent: 40}}
			h := newHarness(t, func(c *Config) { c.Sensors = pack })
			pack.reads = 0

			h.n.Restore(tc.rec, h.now)
			if !h.n.Power().Off() {
				t.Error("a device restored on a flat pack kept running")
			}
			if pack.reads != 1 {
				t.Errorf("Restore read the sensors %d times, want once", pack.reads)
			}
		})
	}
}

// TestTheNameWarningSaysWhatTheDeviceRunsAs: this line is what someone
// reads when a name did not take, and it reported the name from before
// the default was filled in — `using=""` while the device came up as
// emu_totem_xxxx.
func TestTheNameWarningSaysWhatTheDeviceRunsAs(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.Name = "\xff\xfe\xff"
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	got := h.n.Config().Name
	if !strings.Contains(logged.String(), "name is not text") {
		t.Fatalf("a name that is not text was taken without a word about it: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "using="+got) {
		t.Errorf("the device runs as %q and the line says %s", got, logged.String())
	}
	// And the line does not call the MAC-derived default a remnant of
	// the name someone chose.
	if strings.Contains(logged.String(), "using what is left") {
		t.Error("the default name was described as what is left of the one that was given")
	}
}

// TestTheRecordHoldsEveryNameAFrameCan: the two name limits are set in
// different packages — store.maxName bounds what SanitizeName leaves,
// mesh.MaxPeerName bounds what a status frame carries — and a name that
// passes the first and fails the second reaches mustMarshal, which
// panics in the middle of a transmit rather than returning an error.
// The sibling pair, maxBonds against store.MaxPeers, is pinned the same
// way; this one had only prose saying they agree.
func TestTheRecordHoldsEveryNameAFrameCan(t *testing.T) {
	long := strings.Repeat("n", 200)
	kept := store.SanitizeName(long)
	if len(kept) > mesh.MaxPeerName {
		t.Errorf("a name the settings keep is %d bytes and a peer frame holds %d",
			len(kept), mesh.MaxPeerName)
	}
	// And the frame really takes what survives, rather than the two
	// limits merely agreeing on paper.
	h := newHarness(t, func(c *Config) { c.Name = long })
	if _, err := h.n.status(h.now, mesh.PeerStatus, false).MarshalBinary(); err != nil {
		t.Errorf("the status frame refused this device's own name: %v", err)
	}
}

// TestRestoreRefusesImpossiblePositions: Restore is an ingress path like
// any other, and its bytes come off a flash sector that a torn write, a
// bad block or another firmware's layout can leave saying anything. NaN
// is not equal to zero, so the "did we save a position" check passed it
// through into the distance, the compass and the relay decision.
func TestRestoreRefusesImpossiblePositions(t *testing.T) {
	for _, bad := range []struct {
		name     string
		lat, lon float32
	}{
		{"nan latitude", float32(math.NaN()), -122.42},
		{"infinite longitude", 37.775, float32(math.Inf(-1))},
		{"latitude past the pole", 900, -122.42},
	} {
		h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
		st := store.State{Peers: []store.PeerState{{
			MAC: [6]byte(totem), Name: "totem", Lat: bad.lat, Lon: bad.lon,
		}}}
		h.n.Restore(&st, h.now)

		// The position is refused outright, not stored and then worked
		// around: a peer that kept it would still feed it to anything
		// added later that reads lat/lon directly.
		p := h.n.peers[totem]
		if p == nil {
			t.Fatalf("%s: the bond itself was refused", bad.name)
		}
		if p.hasCoords {
			t.Errorf("%s: kept the position as %v, %v", bad.name, p.lat, p.lon)
		}
		for _, info := range h.n.Peers() {
			if math.IsNaN(info.DistanceM) || math.IsInf(info.DistanceM, 0) {
				t.Errorf("%s: the peer is %v away", bad.name, info.DistanceM)
			}
		}
		h.advance(bootAnim + time.Second)
		if deg := h.n.LEDs().dial; deg >= 0 {
			t.Errorf("%s: the compass points at %d", bad.name, deg)
		}
	}
}

// TestRestoreRefusesAColourOutsideThePalette: a colour id off the flash
// is a number, not a promise. One outside the thirteen renders as an
// unlit pixel, so the peer would get a dial point that cannot be seen.
func TestRestoreRefusesAColourOutsideThePalette(t *testing.T) {
	h := newHarness(t, nil)
	st := store.State{Peers: []store.PeerState{{
		MAC: [6]byte(totem), Name: "totem", ColorID: 120,
	}}}
	h.n.Restore(&st, h.now)
	got := h.n.peers[totem].color
	if !got.InPalette() {
		t.Errorf("restored colour %d, which is not in the palette", int(got))
	}
	// Refused, not clamped: the peer keeps the colour AddBond drew for
	// it. Clamping to zero would also be "in the palette", and would
	// quietly turn every peer with a corrupt id red.
	mac := h.n.Config().MAC
	if want := BondColor(0, binary.BigEndian.Uint32(mac[2:6])); got != want {
		t.Errorf("restored colour %s, want the one the bond drew, %s", got, want)
	}
}

// TestRestoreRefusesACrystalColourOutsideThePalette: the device's own
// colour comes off the same flash sector as its peers'. An id outside the
// thirteen renders unlit, and State writes it straight back, so one bad
// byte would leave the crystal dark for good.
func TestRestoreRefusesACrystalColourOutsideThePalette(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) {
		c.ColorID = int8(ColorTeal)
		c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.n.Restore(&store.State{ColorID: 120}, h.now)

	if got := h.n.LEDs().DefaultColor(); !got.InPalette() {
		t.Errorf("the crystal took colour %d, which is not in the palette", int(got))
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorTeal {
		t.Errorf("the crystal is %s, want the default it started with", got)
	}
	if !strings.Contains(logged.String(), "thirteen") {
		t.Errorf("nothing was said about the bad colour: %s", logged.String())
	}
	// And it is not written back out, so it cannot survive the next boot.
	if got := h.n.State(1).ColorID; got == 120 {
		t.Error("the bad colour was saved again")
	}
}

// TestTheSavedNameComesBack: State saves the device name, so Restore has
// to put it back. It did not, and the `store open` path — the one that
// exists to recover settings — read the saved name off the flash and then
// wrote the default straight back over it.
func TestTheSavedNameComesBack(t *testing.T) {
	h := newHarness(t, nil)
	was := h.n.Config().Name
	h.n.Restore(&store.State{Name: "lcfs_totem"}, h.now)
	if got := h.n.Config().Name; got != "lcfs_totem" {
		t.Errorf("the name is %q after a restore, want the saved one (was %q)", got, was)
	}
	// And what it saves next carries the restored name, so it survives.
	if got := h.n.State(1).Name; got != "lcfs_totem" {
		t.Errorf("the state saves %q", got)
	}
}
