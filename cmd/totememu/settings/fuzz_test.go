package settings

import (
	"testing"

	"github.com/ljagiello/totem-compass/mesh"
	"github.com/ljagiello/totem-compass/store"
)

// FuzzSettings drives the whole path a reboot takes over a sector holding
// whatever the fuzzer supplies: read it, put it into a node, save what the
// node then holds, and read it back. The bytes are the untrusted part — a
// torn write, a bad block, a previous firmware's layout or plain noise —
// and every step here has had a fault in it that cost a device its
// settings.
//
// The device's own name comes from the fuzzer too, and empty is the case
// that matters: a board built without `-X main.name=` has not been named
// by anyone, so the saved name is put back, and that is the one path
// where bytes off the flash reach the name in every status frame on the
// air. Naming the node in every seed left that path untested.
//
// The properties hold whatever the bytes mean: the device comes up as
// itself, no sector can widen what it will talk to, and what a save
// reports as written is what comes back.
func FuzzSettings(f *testing.F) {
	f.Add([]byte{}, false, "")
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, true, "lcfs_spare")
	f.Add([]byte("TTM1"), false, "")
	f.Add(append([]byte("TTM1"), make([]byte, 60)...), true, "")

	// A real record, so the corpus starts from something that parses.
	seedSec := newMemSector()
	Open(discard(), seedSec).Save(node(nil, "lcfs_spare", true))
	f.Add(append([]byte(nil), seedSec.b[:256]...), true, "")

	f.Fuzz(func(t *testing.T, raw []byte, bonded bool, built string) {
		sec := newMemSector()
		copy(sec.b, raw)

		s := Open(discard(), sec)
		n := node(nil, built, bonded)
		was := n.Config().Name
		s.Restore(n, t0)

		// The device is up and is still itself. A name off the flash may
		// replace the one it booted with — that is what the sector is
		// for — but it has to be one a status frame can carry, and a
		// device is never nameless.
		got := n.Config().Name
		if got == "" {
			t.Fatalf("a sector left the device with no name at all (it booted as %q)", was)
		}
		// The frame's own limit, not a number picked here: a name a
		// status frame cannot hold fails at the point of sending it, and
		// this is the path that puts one off the flash into every frame.
		if len(got) > mesh.MaxPeerName {
			t.Fatalf("a sector named the device %q, which is %d bytes and the frame holds %d",
				got, len(got), mesh.MaxPeerName)
		}
		// A name someone chose is not overwritten by one off the flash.
		// "Chose" means one that survived being made into a name: a
		// build flag that is not text at all leaves the device with the
		// default, which nobody chose, and then the record's name is
		// exactly what should come back.
		if store.SanitizeName(built) != "" && got != was {
			t.Fatalf("a sector renamed a device that was built as %q to %q", was, got)
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
