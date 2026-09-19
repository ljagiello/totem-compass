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

	// links routes the adapter-wide connect handler to the link for each
	// address. A closed link stays until its disconnect callback arrives, so
	// a late callback cannot reach a newer link to the same device.
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

// Scan reports every Totem it hears until ctx is done, each device once.
// With all set it reports every BLE device instead.
func Scan(ctx context.Context, all bool, found func(Device)) error {
	if err := enable(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	seen := map[string]bool{}
	var mu sync.Mutex
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
		key := r.Address.String()
		if seen[key] {
			return
		}
		seen[key] = true
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

// ErrNotFound is returned by Find when no matching Totem advertised in time.
var ErrNotFound = errors.New("no Totem found")

// Find scans for the first Totem whose name or address contains match
// (case-insensitive); an empty match takes the first Totem heard.
func Find(ctx context.Context, match string) (Device, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var dev *Device
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
	addr     string
	dev      bluetooth.Device
	chars    map[protocol.Channel]*bluetooth.DeviceCharacteristic
	gone     chan struct{}
	goneOnce sync.Once
}

func (l *bleLink) markGone() { l.goneOnce.Do(func() { close(l.gone) }) }

// onDisconnect marks the link for addr gone and forgets it.
func onDisconnect(addr string) {
	if l, ok := links.LoadAndDelete(addr); ok {
		l.(*bleLink).markGone()
	}
}

// claim registers l for its address's disconnect callback. A closed link to
// the same device may still be waiting for its own callback; claim waits
// for it (up to patience), so that callback cannot mark the new link gone.
func claim(ctx context.Context, l *bleLink, patience time.Duration) error {
	deadline := time.Now().Add(patience)
	for {
		if _, loaded := links.LoadOrStore(l.addr, l); !loaded {
			return nil
		}
		if !time.Now().Before(deadline) {
			links.Store(l.addr, l) // the old link is already marked gone
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func dial(ctx context.Context, d Device) (*bleLink, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	l := &bleLink{addr: d.Address.String(), gone: make(chan struct{})}
	if err := claim(ctx, l, 3*time.Second); err != nil {
		return nil, err
	}

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
		links.CompareAndDelete(l.addr, l)
		// Connect cannot be canceled. If it still succeeds, drop the link:
		// a connected Totem stops advertising and could not be found again.
		go func() {
			if r := <-done; r.err == nil {
				_ = r.dev.Disconnect()
			}
		}()
		return nil, ctx.Err()
	}
	if r.err != nil {
		links.CompareAndDelete(l.addr, l)
		return nil, fmt.Errorf("connect %s: %w", d.Address, r.err)
	}
	l.dev = r.dev
	go watchLink(l)
	if err := l.discover(); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
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
	case <-time.After(250 * time.Millisecond):
	}
	l.markGone()
	return err
}
