package client

import (
	"context"
	"testing"
	"time"
)

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A disconnect callback that arrives after Close gave up waiting belongs to
// the old link; it must not mark a newer link to the same Totem gone.
func TestLateDisconnectDoesNotReachNewLink(t *testing.T) {
	ctx := context.Background()
	old := &bleLink{addr: "8C:94:DF:7B:04:78", gone: make(chan struct{})}
	if err := claim(ctx, old, time.Second); err != nil {
		t.Fatal(err)
	}
	old.markGone() // Close timed out: Done closes, the callback is still due
	old.markGone() // and closing twice is harmless

	claimed := make(chan error, 1)
	fresh := &bleLink{addr: old.addr, gone: make(chan struct{})}
	go func() { claimed <- claim(ctx, fresh, 5*time.Second) }()
	select {
	case err := <-claimed:
		t.Fatalf("new link claimed the address while the old callback was due: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	onDisconnect(old.addr) // the old link's late callback
	if err := <-claimed; err != nil {
		t.Fatal(err)
	}
	if isClosed(fresh.gone) {
		t.Fatal("the old link's callback closed the new link")
	}
	onDisconnect(fresh.addr) // the new link's own disconnect
	if !isClosed(fresh.gone) {
		t.Fatal("the new link's disconnect did not close it")
	}
}

// If the old callback never comes, a new link waits only so long.
func TestClaimGivesUpOnAMissingCallback(t *testing.T) {
	ctx := context.Background()
	old := &bleLink{addr: "a", gone: make(chan struct{})}
	if err := claim(ctx, old, time.Second); err != nil {
		t.Fatal(err)
	}
	fresh := &bleLink{addr: "a", gone: make(chan struct{})}
	start := time.Now()
	if err := claim(ctx, fresh, 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > time.Second {
		t.Errorf("claim waited %v, want about 150ms", d)
	}
	onDisconnect("a")
	if !isClosed(fresh.gone) {
		t.Fatal("the address does not route to the new link")
	}
}
