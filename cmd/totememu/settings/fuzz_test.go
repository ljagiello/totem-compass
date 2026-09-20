package settings

import (
	"log/slog"
	"testing"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func nodeFor(name string, bond bool) *emulator.Node {
	n := emulator.New(emulator.Config{
		MAC: self, Owned: []mesh.MAC{totem}, Name: name,
		BattVolts: 4.1, BattPct: 90,
	}, t0)
	if bond {
		_ = n.AddBond([6]byte(totem), "Lukasz", t0)
	}
	return n
}

// FuzzSettings drives the whole path a reboot takes over a sector holding
// whatever the fuzzer supplies: read it, put it into a node, save what the
// node then holds, and read it back. The bytes are the untrusted part — a
// torn write, a bad block, a previous firmware's layout or plain noise —
// and every step here has had a fault in it that cost a device its
// settings.
//
// The properties hold whatever the bytes mean: the device comes up as
// itself, a sector cannot widen what it will talk to, and what a save
// reports as written is what comes back.
func FuzzSettings(f *testing.F) {
	f.Add([]byte{}, false)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, true)
	f.Add([]byte("TTM1"), false)
	f.Add(append([]byte("TTM1"), make([]byte, 60)...), true)

	// A real record, so the corpus starts from something that parses.
	seedSec := newMemSector()
	Open(discard(), seedSec).Save(nodeFor("lcfs_spare", true))
	f.Add(append([]byte(nil), seedSec.b[:256]...), true)

	f.Fuzz(func(t *testing.T, raw []byte, bonded bool) {
		sec := newMemSector()
		copy(sec.b, raw)

		s := Open(discard(), sec)
		n := nodeFor("lcfs_spare", bonded)
		s.Restore(n, t0)

		// The device is up and is still itself.
		if n.Config().Name == "" {
			t.Fatal("a sector left the device with no name at all")
		}
		if got := n.Config().Owned; len(got) != 1 || got[0] != totem {
			t.Fatalf("a sector changed what the device will talk to: %v", got)
		}
		if got := n.BondCount(); got > 1 {
			t.Fatalf("a sector bonded the device to %d Totems", got)
		}

		// What it saves comes back. A save that reports success and then
		// reads back as something else is the fault this package has had
		// twice.
		want := n.State(0)
		s.Save(n)
		if !s.Found() {
			return // nothing was written: the sector is as it was
		}
		again := Open(discard(), sec)
		if !again.Found() {
			t.Fatal("a save reported as written did not come back")
		}
		if got := again.State().Name; got != want.Name {
			t.Fatalf("saved name %q came back as %q", want.Name, got)
		}
		if got := len(again.State().Peers); got != len(want.Peers) {
			t.Fatalf("saved %d peers, %d came back", len(want.Peers), got)
		}
	})
}
