package client

import (
	"errors"
	"testing"
)

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func testLink(addr string, up *bool) *bleLink {
	return &bleLink{addr: addr, gone: make(chan struct{}), connected: func() (bool, error) { return *up, nil }}
}

// The connect handler reports only an address, so a disconnect callback can
// belong to an older connection to the same Totem: after a Close that
// stopped waiting for it, or a canceled connect that completed and was
// dropped. It must not mark the current link gone while the OS still
// reports that link connected.
func TestLateDisconnectDoesNotReachNewLink(t *testing.T) {
	up := true
	l := testLink("8C:94:DF:7B:04:78", &up)
	links.Store(l.addr, l)
	defer links.Delete(l.addr)

	onDisconnect(l.addr) // the old connection's late callback
	if isClosed(l.gone) {
		t.Fatal("a callback for an older connection closed the current link")
	}

	up = false
	onDisconnect(l.addr) // the current link's own disconnect
	if !isClosed(l.gone) {
		t.Fatal("the link's disconnect did not close it")
	}
	if _, ok := links.Load(l.addr); ok {
		t.Error("a gone link is still routed")
	}
	onDisconnect(l.addr) // and nothing breaks on a repeat
}

// A Totem first heard by its service UUID alone, with the name "totem" in a
// later scan response, used to be reported only nameless, so -d totem never
// matched it.
func TestScanReportsANameThatArrivesLate(t *testing.T) {
	var seen seenDevices
	for _, tc := range []struct {
		addr, name string
		want       bool
	}{
		{"a", "", true},       // first heard, no name yet
		{"a", "", false},      // nothing new
		{"a", "totem", true},  // the name arrives
		{"a", "totem", false}, // reported with its name already
		{"a", "", false},      // a later nameless advertisement
		{"b", "totem", true},  // named from the start
		{"b", "", false},
	} {
		if got := seen.first(tc.addr, tc.name); got != tc.want {
			t.Errorf("first(%q, %q) = %v, want %v", tc.addr, tc.name, got, tc.want)
		}
	}
}

// A connect that completes after its dial was canceled is dropped, unless a
// newer link to the Totem exists: the OS shares one connection per device,
// so dropping it would end the newer session.
func TestLateConnectSparesANewerLink(t *testing.T) {
	dropped := 0
	disconnect := func() error { dropped++; return nil }

	dropLateConnect("x", disconnect)
	if dropped != 1 {
		t.Fatalf("an orphaned connection was not dropped")
	}
	up := true
	links.Store("x", testLink("x", &up))
	defer links.Delete("x")
	dropLateConnect("x", disconnect)
	if dropped != 1 {
		t.Fatal("dropping the late connection would have ended the newer link")
	}
}

// If the OS cannot say, the callback is believed.
func TestDisconnectWithUnknownState(t *testing.T) {
	l := &bleLink{addr: "a", gone: make(chan struct{}), connected: func() (bool, error) { return false, errors.New("no such object") }}
	links.Store(l.addr, l)
	onDisconnect(l.addr)
	if !isClosed(l.gone) {
		t.Fatal("link not marked gone")
	}
	l.markGone() // Close after the callback is harmless
}
