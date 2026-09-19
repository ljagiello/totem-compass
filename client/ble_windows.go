//go:build windows

package client

import "time"

// closeWait is how long Close waits for the disconnect callback.
const closeWait = 250 * time.Millisecond

// askConnected is false here: tinygo's WinRT backend releases the device
// (Disconnect) before it calls the connect handler, so asking a link whose
// callback arrived whether it is connected would use a released object.
// Its callbacks are reliable, so they are believed.
const askConnected = false

// watchLink does nothing here: WinRT reports every connection status change,
// including a disconnect the Totem initiates, to the connect handler.
func watchLink(*bleLink) {}
