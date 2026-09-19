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
