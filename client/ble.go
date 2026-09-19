package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ljagiello/totem-compass/protocol"
	"tinygo.org/x/bluetooth"
)

// This file is the Bluetooth transport: scanning, connecting, and a Link
// over the two GATT characteristics.

var (
	serviceUUID    = mustUUID(protocol.ServiceUUID)
	connStatusUUID = mustUUID(protocol.ConnStatusUUID)
	dataUUID       = mustUUID(protocol.DataTransferUUID)
)

func mustUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

var adapter = bluetooth.DefaultAdapter

var (
	enableOnce sync.Once
	enableErr  error

	// links routes the adapter-wide connect handler to the current link for
	// each address. The handler says only which address disconnected, so a
	// late callback for an older connection can land on a newer link;
	// onDisconnect asks the link itself before believing it.
	links sync.Map // address string -> *bleLink
)

func enable() error {
	enableOnce.Do(func() {
		adapter.SetConnectHandler(func(d bluetooth.Device, connected bool) {
			if !connected {
				onDisconnect(d.Address.String())
			}
		})
		if err := adapter.Enable(); err != nil {
			enableErr = fmt.Errorf("enable bluetooth: %w (on macOS, allow Bluetooth for your terminal in System Settings → Privacy & Security → Bluetooth)", err)
		}
	})
	return enableErr
}

// Device is a Totem seen while scanning.
type Device struct {
	Address  bluetooth.Address
	Name     string
	RSSI     int16
	Services []string // advertised service UUIDs
	MfgData  map[uint16][]byte
}

// isTotem matches the advertisement ble_manager builds: name "totem",
// appearance 1361 and the Totem service UUID (which may land in the scan
// response).
func isTotem(r bluetooth.ScanResult) bool {
	return r.HasServiceUUID(serviceUUID) || isTotemName(r.LocalName())
}

func isTotemName(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "totem")
}

// Scan reports every Totem it hears until ctx is done: each device once,
// and once more if its name arrives only after its first advertisement (in
// a scan response). With all set it reports every BLE device instead. No
// report comes after Scan returns.
func Scan(ctx context.Context, all bool, found func(Device)) error {
	if err := enable(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	var (
		mu      sync.Mutex
		seen    seenDevices
		stopped bool
	)
	defer func() {
		// CoreBluetooth can still deliver an advertisement after StopScan.
		mu.Lock()
		stopped = true
		mu.Unlock()
	}()
	scanDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// StopScan fails until adapter.Scan has registered the scan, which
		// takes several D-Bus round trips on Linux; a failed stop would leave
		// the scan running for good. Retry until it takes or the scan ends.
		for adapter.StopScan() != nil {
			select {
			case <-scanDone:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	defer stop()
	defer close(scanDone)
	return adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
		if !all && !isTotem(r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if stopped || !seen.first(r.Address.String(), r.LocalName()) {
			return
		}
		d := Device{Address: r.Address, Name: r.LocalName(), RSSI: r.RSSI}
		for _, u := range r.ServiceUUIDs() {
			d.Services = append(d.Services, u.String())
		}
		for _, m := range r.ManufacturerData() {
			if d.MfgData == nil {
				d.MfgData = map[uint16][]byte{}
			}
			d.MfgData[m.CompanyID] = m.Data
		}
		found(d)
	})
}

// seenDevices remembers which devices Scan reported, and whether with a
// name.
type seenDevices map[string]bool // address -> reported with a name

// first reports whether a device heard with this name is news: never
// reported, or reported before its name was known.
func (s *seenDevices) first(addr, name string) bool {
	if *s == nil {
		*s = seenDevices{}
	}
	named, ok := (*s)[addr]
	if ok && (named || name == "") {
		return false
	}
	(*s)[addr] = name != ""
	return true
}

// ErrNotFound is returned by Find when no matching Totem advertised in time.
var ErrNotFound = errors.New("no Totem found")

// Find scans for the first Totem whose name or address contains match
// (case-insensitive); an empty match takes the first Totem heard.
func Find(parent context.Context, match string) (Device, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var dev *Device // written only by Scan's callback, which ends before Scan returns
	err := Scan(ctx, false, func(d Device) {
		if dev == nil && matches(d, match) {
			dev = &d
			cancel()
		}
	})
	if err != nil {
		return Device{}, err
	}
	if dev == nil {
		// Scan ends quietly when its context does; only a deadline means
		// nothing matched. A cancellation (Ctrl-C) is not "not found".
		if err := parent.Err(); errors.Is(err, context.Canceled) {
			return Device{}, err
		}
		if match != "" {
			return Device{}, fmt.Errorf("%w matching %q", ErrNotFound, match)
		}
		return Device{}, ErrNotFound
	}
	return *dev, nil
}

func matches(d Device, match string) bool {
	match = strings.ToLower(match)
	return match == "" || strings.Contains(strings.ToLower(d.Name), match) ||
		strings.Contains(strings.ToLower(d.Address.String()), match)
}

// Connect opens a GATT connection to d, discovers the Totem service and
// returns a Client subscribed to both characteristics. The app owns the TX
// window until Start is called.
func Connect(ctx context.Context, d Device, opts Options) (*Client, error) {
	l, err := dial(ctx, d)
	if err != nil {
		return nil, err
	}
	c, err := New(l, opts)
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	return c, nil
}

// bleLink is a Link over the Totem's conn-status and data characteristics.
type bleLink struct {
	addr      string
	dev       bluetooth.Device
	connected func() (bool, error) // the OS's view of this connection
	chars     map[protocol.Channel]*bluetooth.DeviceCharacteristic
	gone      chan struct{}
	goneOnce  sync.Once
}

func (l *bleLink) markGone() { l.goneOnce.Do(func() { close(l.gone) }) }

// onDisconnect handles a disconnect callback for addr. The callback may be a
// late one for an older connection to the same Totem (after a Close that
// stopped waiting, or a canceled connect that completed): the current link
// is marked gone only if the OS no longer reports it connected.
func onDisconnect(addr string) {
	v, ok := links.Load(addr)
	if !ok {
		return
	}
	l := v.(*bleLink)
	if up, err := l.connected(); err == nil && up {
		return
	}
	links.CompareAndDelete(addr, l)
	l.markGone()
}

func dial(ctx context.Context, d Device) (*bleLink, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	l := &bleLink{addr: d.Address.String(), gone: make(chan struct{})}

	type result struct {
		dev bluetooth.Device
		err error
	}
	done := make(chan result, 1)
	go func() {
		dev, err := adapter.Connect(d.Address, bluetooth.ConnectionParams{})
		done <- result{dev, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		// Connect cannot be canceled. If it still succeeds, drop the link:
		// a connected Totem stops advertising and could not be found again.
		go func() {
			if r := <-done; r.err == nil {
				dropLateConnect(l.addr, r.dev.Disconnect)
			}
		}()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, fmt.Errorf("connect %s: %w", d.Address, r.err)
	}
	l.dev = r.dev
	l.connected = r.dev.Connected
	links.Store(l.addr, l)
	go watchLink(l)
	if err := l.discover(); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// dropLateConnect disconnects a connection that completed after its dial
// was canceled, unless a newer link to the same Totem exists: the OS keeps
// one connection per device, so the disconnect would end that one.
func dropLateConnect(addr string, disconnect func() error) {
	if _, ok := links.Load(addr); ok {
		return
	}
	_ = disconnect()
}

func (l *bleLink) discover() error {
	svcs, err := l.dev.DiscoverServices([]bluetooth.UUID{serviceUUID})
	if err != nil {
		return fmt.Errorf("discover services: %w", err)
	}
	if len(svcs) == 0 {
		return errors.New("device does not expose the Totem service")
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{connStatusUUID, dataUUID})
	if err != nil {
		return fmt.Errorf("discover characteristics: %w", err)
	}
	l.chars = map[protocol.Channel]*bluetooth.DeviceCharacteristic{}
	for i, ch := range chars {
		switch ch.UUID() {
		case connStatusUUID:
			l.chars[protocol.ConnStatus] = &chars[i]
		case dataUUID:
			l.chars[protocol.Data] = &chars[i]
		}
	}
	if len(l.chars) != 2 {
		return errors.New("the Totem service is missing its conn-status or data characteristic")
	}
	return nil
}

// Subscribe enables notifications. The device indicates on both
// characteristics; the OS acknowledges indications on its own, so fn only
// sees the payload.
func (l *bleLink) Subscribe(ch protocol.Channel, fn func([]byte)) error {
	return l.chars[ch].EnableNotifications(fn)
}

// Write is a GATT write with response.
func (l *bleLink) Write(ch protocol.Channel, b []byte) error {
	_, err := l.chars[ch].Write(b)
	return err
}

func (l *bleLink) Done() <-chan struct{} { return l.gone }

// Close drops the connection. The OS tears it down even if the process
// exits before the disconnect callback arrives, so it waits only briefly;
// Done is closed either way.
func (l *bleLink) Close() error {
	err := l.dev.Disconnect()
	select {
	case <-l.gone:
	case <-time.After(closeWait):
	}
	links.CompareAndDelete(l.addr, l)
	l.markGone()
	return err
}
