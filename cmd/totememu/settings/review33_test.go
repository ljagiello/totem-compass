package settings

// Regressions for the thirty-third review round, and the tests the two
// rounds before it should have had: both faults found here were in code
// that shipped without one.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/store"
)

// TestReopenAlwaysReports: `store open` exists to say what is in the
// sector, and it has three ways out — already open, opened now, and
// nothing to open. A deferred report placed after the first of them
// left the path a working board takes printing nothing at all.
func TestReopenAlwaysReports(t *testing.T) {
	// A sector with something in it, so there is something to report.
	filled := newMemSector()
	Open(discard(), filled).Save(node(t, "lcfs_spare", true))

	for _, tc := range []struct {
		name string
		open func(*slog.Logger) *Store
		sec  store.Sector
		want string
	}{
		// Each case has one right answer, not either of two: a store
		// that holds a record has to report the record, and one that
		// holds nothing has to say so. Accepting both would pass a
		// Reopen that read the sector and then reported it empty.
		{"already open", func(l *slog.Logger) *Store { return Open(l, filled) }, filled, `msg=settings `},
		{"opened now", Unread, filled, `msg=settings `},
		{"nothing to open", Unread, nil, "settings unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := tc.open(slog.New(slog.NewTextHandler(&logged, nil)))
			logged.Reset() // only what Reopen itself says
			s.Reopen(node(t, "", false), t0, tc.sec)
			got := logged.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("store open did not say %q:\n%s", tc.want, got)
			}
			// And the record it reports is the one that is there.
			if tc.sec != nil && !strings.Contains(got, `name=lcfs_spare`) {
				t.Errorf("store open reported settings that are not the saved ones:\n%s", got)
			}
		})
	}
}

// TestBothWritersHoldTheBlocker: a flash write is the firmware's 'vfs
// write' blocker, and while it runs nothing feeds the watchdog. A reset
// is the write that needs it most, being the one that erases, and it
// went without for as long as it had its own copy of the save path.
func TestBothWritersHoldTheBlocker(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Store, *emulator.Node) error
	}{
		{"a save", func(s *Store, n *emulator.Node) error { s.Save(n); return nil }},
		{"a reset", func(s *Store, n *emulator.Node) error { return s.Forget(n, t0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := newMemSector()
			s := Open(testLog(t), sec)
			n := node(t, "lcfs_spare", true)

			// Filled to the end, so the write under test is the one that
			// has to erase first — the longest the driver holds the
			// cache off and the interrupts down, and the case the
			// blocker exists for. On an empty sector a save just
			// appends and no erase happens at all.
			for i := range len(sec.b) {
				sec.b[i] = byte(i)
			}

			// Watched from inside the flash, which is the only place the
			// question means anything: the blocker is taken and given
			// back around the write, so nothing outside can see it held.
			var sawBlocked, ops, erases int
			watch := func() {
				ops++
				if !n.Power().WatchdogFeeding() {
					sawBlocked++
				}
			}
			sec.onWrite = watch
			sec.onErase = func() { erases++; watch() }
			if err := tc.run(s, n); err != nil {
				t.Fatal(err)
			}
			if ops == 0 {
				t.Fatal("the flash was not touched")
			}
			if erases == 0 {
				t.Error("the sector was full and nothing erased it")
			}
			if sawBlocked != ops {
				t.Errorf("%d of %d flash operations ran with the watchdog still being fed", ops-sawBlocked, ops)
			}
			if !n.Power().WatchdogFeeding() {
				t.Error("the blocker was not given back")
			}
		})
	}
}
