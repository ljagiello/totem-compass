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
		connected, err := l.dev.Connected()
		switch {
		case err != nil:
			// BlueZ drops the device object some time after a disconnect;
			// one failed read is not proof, three in a row are.
			if failures++; failures < 3 {
				continue
			}
		case connected:
			failures = 0
			continue
		}
		links.CompareAndDelete(l.addr, l)
		l.markGone()
		return
	}
}
