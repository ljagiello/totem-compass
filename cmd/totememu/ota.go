//go:build tinygo && esp32

package main

// The update transport.
//
// A Totem's update runs over station WiFi: it joins a network, asks
// api.totemportal.com what release it should be on, and downloads a
// package over plain HTTP. This board can do the radio but has no
// credentials and no TCP stack wired up, so the transport here answers
// the same exchange from memory: the update client, its checks, its
// progress ring and its battery gate all run, and nothing leaves the
// board.
//
// A board that does have a network fills in OTATransport instead. The
// interface is the exchange, not the plumbing, so nothing above it
// changes.

import (
	"fmt"
	"time"

	"github.com/ljagiello/totem-compass/emulator"
)

// localOTA answers the update exchange without a network.
type localOTA struct {
	// size is how big the package it serves claims to be, which is about
	// what a Totem's own firmware image weighs.
	size int64
}

func newLocalOTA() *localOTA { return &localOTA{size: 1 << 20} }

func (o *localOTA) Post(url string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("ota: empty body for %s", url)
	}
	// The release a server would answer with: this device's own version,
	// so an update run on the board is a no-op rather than a downgrade
	// invitation.
	return []byte(`{"ota_url":"http://api.totemportal.com/repo","version":"5.0.3",` +
		`"product":"totem_compass","branch":"totem","release_code":"5.0.3","release_id":339}`), nil
}

func (o *localOTA) Get(string) ([]byte, error) {
	return []byte(`["firmware_v5.0.3.bin"]`), nil
}

func (o *localOTA) Download(_ string, progress func(done, total int64)) (int64, string, error) {
	// Paced, but only just. The update runs on the main loop, so every
	// millisecond spent here is a millisecond in which no frame is drawn
	// and the receive ring is not drained — a peer transmitting every
	// second would start losing frames.
	const steps = 10
	for i := 0; i <= steps; i++ {
		progress(o.size*int64(i)/steps, o.size)
		time.Sleep(10 * time.Millisecond)
	}
	return o.size, "0000000000000000000000000000000000000000000000000000000000000000", nil
}

// compile-time check that it answers what the client asks.
var _ emulator.OTATransport = (*localOTA)(nil)
