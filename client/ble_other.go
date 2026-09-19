//go:build !linux

package client

import "time"

// closeWait is how long Close waits for CoreBluetooth to report the
// disconnect; the OS finishes it even if the process exits first.
const closeWait = 250 * time.Millisecond

// watchLink does nothing here: CoreBluetooth reports every disconnect,
// including one the Totem initiates, to the adapter's connect handler.
func watchLink(*bleLink) {}
