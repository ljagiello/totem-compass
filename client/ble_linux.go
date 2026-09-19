//go:build linux

package client

import "time"

// watchLink polls the connection state until the link goes. BlueZ reports a
// disconnect only as a D-Bus property change, which tinygo's central role
// does not pass to the connect handler: a Totem dropping the link (e.g. to
// reboot into the updater) would otherwise never close Done.
func watchLink(l *bleLink) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-l.gone:
			return
		case <-t.C:
		}
		up, err := l.connected()
		switch {
		case err != nil:
			// BlueZ drops the device object some time after a disconnect;
			// one failed read is not proof, three in a row are. Disconnect
			// anyway, in case BlueZ still holds the link: a connected Totem
			// stops advertising.
			if failures++; failures < 3 {
				continue
			}
			_ = l.dev.Disconnect()
		case up:
			failures = 0
			continue
		}
		links.CompareAndDelete(l.addr, l)
		l.markGone()
		return
	}
}
