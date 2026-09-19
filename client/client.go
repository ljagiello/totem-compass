// Package client connects to a Totem Compass over BLE and runs the same
// session the official app does: subscribe, send ConnStatus-Ready, receive
// Live/Static/Peer records and acknowledge them (legacy mode), or pass the
// half-duplex TX window back and forth (HalfDuplex).
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

	// disconnects routes the adapter-wide connect handler to live clients.
	disconnects sync.Map // address string -> chan struct{}
)

func enable() error {
	enableOnce.Do(func() {
		adapter.SetConnectHandler(func(d bluetooth.Device, connected bool) {
			if connected {
				return
			}
			if ch, ok := disconnects.LoadAndDelete(d.Address.String()); ok {
				close(ch.(chan struct{}))
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
	if r.HasServiceUUID(serviceUUID) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(r.LocalName()), "totem")
}

// Scan reports every Totem it hears until ctx is done. With all set it
// reports every BLE device instead.
func Scan(ctx context.Context, all bool, found func(Device)) error {
	if err := enable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var mu sync.Mutex
	stop := context.AfterFunc(ctx, func() { _ = adapter.StopScan() })
	defer stop()
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

// Find scans for the first Totem whose name or address contains match
// (case-insensitive); an empty match takes the first Totem heard.
func Find(ctx context.Context, match string) (Device, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	match = strings.ToLower(match)
	var dev *Device
	err := Scan(ctx, false, func(d Device) {
		if dev != nil {
			return
		}
		if match == "" || strings.Contains(strings.ToLower(d.Name), match) ||
			strings.Contains(strings.ToLower(d.Address.String()), match) {
			dev = &d
			cancel()
		}
	})
	if err != nil {
		return Device{}, err
	}
	if dev == nil {
		what := "a Totem"
		if match != "" {
			what = fmt.Sprintf("a Totem matching %q", match)
		}
		return Device{}, fmt.Errorf("did not find %s. A Totem is only visible while its crystal breathes blue: "+
			"double-press the power button (it toggles Bluetooth, so press again if it doesn't breathe blue). "+
			"If the Totem phone app is open nearby, close it: a connected Totem stops advertising", what)
	}
	return *dev, nil
}

// Event is one frame received from the device.
type Event struct {
	Channel protocol.Channel
	Raw     []byte
	Msg     protocol.Message // nil when Err is set
	Err     error
}

// Options tune a session.
type Options struct {
	// HalfDuplex selects the device's send_data_v2 loop (explicit TX
	// handoff, indications) instead of the legacy full-duplex loop
	// (notifications, app-level acks). Half duplex stalls on macOS/iOS; see
	// protocol.SchemaLegacy.
	HalfDuplex bool
	// Schema is the frame_schema_id announced in half-duplex mode; zero
	// means protocol.SchemaExtended.
	Schema byte
	// AutoGrant (half duplex only) hands the TX window back to the device
	// after each handoff, so it keeps streaming Live Data (~every 8 s).
	// Commands queued with Send while the device owns TX are written first.
	AutoGrant bool
	// Trace, if set, receives every frame in both directions.
	Trace func(dir string, ch protocol.Channel, b []byte)
}

// Client is a connected Totem.
type Client struct {
	Device Device
	opts   Options

	dev        bluetooth.Device
	conn, data bluetooth.DeviceCharacteristic

	events chan Event
	gone   chan struct{}

	writeMu sync.Mutex // one GATT write in flight at a time
	batchMu sync.Mutex // held while a Send batch uses the app's TX window

	mu        sync.Mutex
	appOwnsTX bool
	txWaiters []chan struct{}
	handoffs  uint64
	lastAck   map[protocol.Channel]map[[2]byte]time.Time
}

// Connect opens a GATT connection, discovers the Totem service and
// subscribes to both characteristics. The app owns the TX window until
// Start is called.
func Connect(ctx context.Context, d Device, opts Options) (*Client, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	if opts.HalfDuplex && opts.Schema == 0 {
		opts.Schema = protocol.SchemaExtended
	}
	c := &Client{
		Device:    d,
		opts:      opts,
		events:    make(chan Event, 256),
		gone:      make(chan struct{}),
		appOwnsTX: true,
	}
	disconnects.Store(d.Address.String(), c.gone)

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
		return nil, ctx.Err()
	}
	if r.err != nil {
		disconnects.Delete(d.Address.String())
		return nil, fmt.Errorf("connect %s: %w", d.Address, r.err)
	}
	c.dev = r.dev

	if err := c.discover(); err != nil {
		_ = c.dev.Disconnect()
		return nil, err
	}
	return c, nil
}

func (c *Client) discover() error {
	svcs, err := c.dev.DiscoverServices([]bluetooth.UUID{serviceUUID})
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
	var haveConn, haveData bool
	for _, ch := range chars {
		switch ch.UUID() {
		case connStatusUUID:
			c.conn, haveConn = ch, true
		case dataUUID:
			c.data, haveData = ch, true
		}
	}
	if !haveConn || !haveData {
		return errors.New("Totem service is missing its conn-status or data characteristic")
	}
	// The device indicates on both characteristics; CoreBluetooth
	// acknowledges indications for us.
	if err := c.data.EnableNotifications(c.receiver(protocol.Data)); err != nil {
		return fmt.Errorf("subscribe data: %w", err)
	}
	if err := c.conn.EnableNotifications(c.receiver(protocol.ConnStatus)); err != nil {
		return fmt.Errorf("subscribe conn-status: %w", err)
	}
	return nil
}

// receiver runs on the CoreBluetooth dispatch queue, so it must not block
// or write.
func (c *Client) receiver(ch protocol.Channel) func([]byte) {
	return func(buf []byte) {
		b := append([]byte(nil), buf...)
		if c.opts.Trace != nil {
			c.opts.Trace("<-", ch, b)
		}
		msg, err := protocol.Parse(ch, b)
		if h, ok := msg.(protocol.Handoff); ok && h.ToApp {
			c.onHandoff()
		}
		if !c.opts.HalfDuplex {
			c.legacyAck(msg)
		}
		select {
		case c.events <- Event{Channel: ch, Raw: b, Msg: msg, Err: err}:
		default: // consumer is not keeping up; drop rather than stall BLE
		}
	}
}

// legacyAck answers the records the legacy send_data loop repeats until the
// app confirms them, as the official app does:
//
//	Static Data  -> (1,0)  clears static_data_cmd_id, sets is_static_data_sent
//	WiFi list    -> (2,0)  clears wifi_cmd_id and the cached scan
//	Peer Sync    -> (6,8)  moves peer_cmd_id on and asks for every Peer Ping
//
// The device can send a record again before our ack lands, so each ack is
// sent at most once per second.
func (c *Client) legacyAck(m protocol.Message) {
	var ack protocol.Frame
	switch m.(type) {
	case protocol.StaticData:
		ack = protocol.AckStaticData()
	case protocol.WiFiNetworks:
		ack = protocol.ClearWiFiScan()
	case protocol.PeerSync:
		ack, _ = protocol.RequestPeerDetails()
	default:
		return
	}
	key := [2]byte{ack.Bytes[0], ack.Bytes[1]}
	c.mu.Lock()
	if c.lastAck == nil {
		c.lastAck = map[protocol.Channel]map[[2]byte]time.Time{}
	}
	if c.lastAck[ack.Channel] == nil {
		c.lastAck[ack.Channel] = map[[2]byte]time.Time{}
	}
	if time.Since(c.lastAck[ack.Channel][key]) < time.Second {
		c.mu.Unlock()
		return
	}
	c.lastAck[ack.Channel][key] = time.Now()
	c.mu.Unlock()
	go func() { _ = c.write(ack) }() // never write from the CoreBluetooth queue
}

func (c *Client) onHandoff() {
	c.mu.Lock()
	c.appOwnsTX = true
	c.handoffs++
	waiters := c.txWaiters
	c.txWaiters = nil
	c.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
	if c.opts.AutoGrant {
		go func() {
			// Give queued commands (woken above) a moment to go out first.
			time.Sleep(300 * time.Millisecond)
			_ = c.Grant()
		}()
	}
}

// Handoffs counts the TX handoffs received so far. It is incremented before
// the corresponding Handoff event is delivered.
func (c *Client) Handoffs() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handoffs
}

// Events delivers every frame received from the device.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the link drops.
func (c *Client) Done() <-chan struct{} { return c.gone }

// HalfDuplex reports whether the session uses the TX-handoff loop.
func (c *Client) HalfDuplex() bool { return c.opts.HalfDuplex }

// Start sends the app runtime state and the ConnStatus-Ready frame. The
// device then streams Live Data (every ~3 s in legacy mode). In half-duplex
// mode Ready also hands it the TX window; it answers with Live Data, pending
// peer records and then a Handoff back to the app.
func (c *Client) Start() error {
	if err := c.write(protocol.AppRuntime(protocol.RuntimeState{Active: true, Focused: true})); err != nil {
		return err
	}
	if !c.opts.HalfDuplex {
		return c.write(protocol.Ready(protocol.SchemaLegacy))
	}
	c.mu.Lock()
	c.appOwnsTX = false
	c.mu.Unlock()
	return c.write(protocol.Ready(c.opts.Schema))
}

// Grant hands the TX window to the device (half duplex only; a no-op in
// legacy mode). It waits for an in-progress Send batch to finish first.
func (c *Client) Grant() error {
	if !c.opts.HalfDuplex {
		return nil
	}
	c.batchMu.Lock()
	defer c.batchMu.Unlock()
	c.mu.Lock()
	c.appOwnsTX = false
	c.mu.Unlock()
	return c.write(protocol.GrantTX())
}

// AwaitTX blocks until the app holds the TX window: before Start, or after
// the device's next Handoff.
func (c *Client) AwaitTX(ctx context.Context) error {
	c.mu.Lock()
	if c.appOwnsTX {
		c.mu.Unlock()
		return nil
	}
	w := make(chan struct{})
	c.txWaiters = append(c.txWaiters, w)
	c.mu.Unlock()
	select {
	case <-w:
		return nil
	case <-c.gone:
		return errors.New("disconnected")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send writes frames, in order, in the app's next TX window, like the
// official app does. Requests that ask the device to transmit (static data,
// peer details, WiFi scan) are answered in the device's following window.
func (c *Client) Send(ctx context.Context, frames ...protocol.Frame) error {
	for {
		if err := c.AwaitTX(ctx); err != nil {
			return fmt.Errorf("waiting for TX window: %w", err)
		}
		c.batchMu.Lock()
		c.mu.Lock()
		owns := c.appOwnsTX
		c.mu.Unlock()
		if owns {
			break
		}
		c.batchMu.Unlock() // a Grant won the race; wait for the next window
	}
	defer c.batchMu.Unlock()
	for _, f := range frames {
		if err := c.write(f); err != nil {
			return err
		}
	}
	return nil
}

// SendNow writes a frame immediately, ignoring the half-duplex window. The
// device processes writes regardless; use it for requests that must reach it
// while it is transmitting.
func (c *Client) SendNow(f protocol.Frame) error { return c.write(f) }

func (c *Client) write(f protocol.Frame) error {
	select {
	case <-c.gone:
		return errors.New("disconnected")
	default:
	}
	ch := c.data
	if f.Channel == protocol.ConnStatus {
		ch = c.conn
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.opts.Trace != nil {
		c.opts.Trace("->", f.Channel, f.Bytes)
	}
	if _, err := ch.Write(f.Bytes); err != nil {
		return fmt.Errorf("write %s % x: %w", f.Channel, f.Bytes, err)
	}
	return nil
}

// Close asks the device for a graceful disconnect, then drops the link.
func (c *Client) Close() error {
	select {
	case <-c.gone:
		return nil
	default:
	}
	// The request is written with response, so the device has it before we
	// drop the link; the OS tears the connection down even if we exit before
	// its disconnect callback, so don't wait long for that.
	_ = c.write(protocol.DisconnectRequest())
	err := c.dev.Disconnect()
	select {
	case <-c.gone:
	case <-time.After(250 * time.Millisecond):
	}
	disconnects.Delete(c.Device.Address.String())
	return err
}
