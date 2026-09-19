//go:build !linux && !windows

package client

import "time"

// closeWait is how long Close waits for CoreBluetooth to report the
// disconnect; the OS finishes it even if the process exits first.
const closeWait = 250 * time.Millisecond

// askConnected: a disconnect callback may be a late one for an older
// connection, and the peripheral's state tells.
const askConnected = true

// watchLink does nothing here: CoreBluetooth reports every disconnect,
// including one the Totem initiates, to the adapter's connect handler.
func watchLink(*bleLink) {}
