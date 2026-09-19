//go:build !linux

package client

// watchLink does nothing here: CoreBluetooth reports every disconnect,
// including one the Totem initiates, to the adapter's connect handler.
func watchLink(*bleLink) {}
