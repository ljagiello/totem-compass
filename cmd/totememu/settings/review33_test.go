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
	}{
		{"already open", func(l *slog.Logger) *Store { return Open(l, filled) }, filled},
		{"opened now", Unread, filled},
		{"nothing to open", Unread, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := tc.open(slog.New(slog.NewTextHandler(&logged, nil)))
			logged.Reset() // only what Reopen itself says
			s.Reopen(node(t, "", false), t0, tc.sec)
			got := logged.String()
			if !strings.Contains(got, "msg=settings") && !strings.Contains(got, "settings unavailable") {
				t.Errorf("store open said nothing about the settings:\n%s", got)
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

			// Watched from inside the write, which is the only place the
			// question means anything: the blocker is taken and given
			// back around it, so nothing outside can see it held.
			var sawBlocked, writes int
			sec.onWrite = func() {
				writes++
				if !n.Power().WatchdogFeeding() {
					sawBlocked++
				}
			}
			if err := tc.run(s, n); err != nil {
				t.Fatal(err)
			}
			if writes == 0 {
				t.Fatal("nothing was written")
			}
			if sawBlocked != writes {
				t.Errorf("%d of %d writes ran with the watchdog still being fed", writes-sawBlocked, writes)
			}
			if !n.Power().WatchdogFeeding() {
				t.Error("the blocker was not given back")
			}
		})
	}
}
