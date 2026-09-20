// Package client runs a Totem Compass BLE session the way the official app
// does: subscribe, send ConnStatus-Ready, then receive Live/Static/Peer
// records and acknowledge them (legacy mode), or pass the half-duplex TX
// window back and forth (Options.HalfDuplex).
//
// Connect opens a session over Bluetooth. New runs one over any Link, which
// is how package clienttest attaches a simulated Totem.
package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ljagiello/totem-compass/protocol"
)

// Link is the GATT connection a Client drives.
type Link interface {
	// Subscribe delivers every notification or indication received on ch to
	// fn. fn may run on the transport's own goroutine and must not block.
	Subscribe(ch protocol.Channel, fn func([]byte)) error
	// Write writes b to ch and returns once the device has it.
	Write(ch protocol.Channel, b []byte) error
	// Done is closed when the link drops.
	Done() <-chan struct{}
	// Close drops the link.
	Close() error
}

// ErrDisconnected is returned for writes after the link dropped.
var ErrDisconnected = errors.New("disconnected")

// Event is one frame received from the device.
type Event struct {
	Channel protocol.Channel
	Raw     []byte
	Msg     protocol.Message // nil when Err is set
	Err     error
	At      time.Time // when the frame arrived
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
	AutoGrant bool
	// GrantDelay is how long AutoGrant waits after a handoff so frames
	// queued with Send go out first; zero means 300 ms.
	GrantDelay time.Duration
	// Logger gets every frame in both directions at debug level, and
	// failures of the writes the client makes on its own (legacy acks,
	// AutoGrant) at warn level. Nil discards.
	Logger *slog.Logger
}

// Client is a session with one Totem.
type Client struct {
	link   Link
	opts   Options
	log    *slog.Logger
	events chan Event

	batchMu sync.Mutex // held while a Send batch uses the app's TX window

	mu        sync.Mutex
	appOwnsTX bool
	txWaiters []chan struct{}
	handoffs  uint64

	// Every GATT write goes through one FIFO queue served by writer.
	qmu     sync.Mutex
	queue   []writeReq
	pending map[[2]byte]bool // acks waiting in queue
	wake    chan struct{}    // the queue has work
}

// writeReq is one queued GATT write. result receives its outcome; acks,
// which nobody waits for, have none.
type writeReq struct {
	f      protocol.Frame
	result chan error
}

// New starts a session over l and subscribes to both characteristics.
func New(l Link, opts Options) (*Client, error) {
	if opts.HalfDuplex && opts.Schema == 0 {
		opts.Schema = protocol.SchemaExtended
	}
	if opts.GrantDelay == 0 {
		opts.GrantDelay = 300 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		link:      l,
		opts:      opts,
		log:       opts.Logger,
		events:    make(chan Event, 256),
		appOwnsTX: true,
		pending:   map[[2]byte]bool{},
		wake:      make(chan struct{}, 1),
	}
	go c.writer()
	for _, ch := range []protocol.Channel{protocol.Data, protocol.ConnStatus} {
		if err := l.Subscribe(ch, c.receiver(ch)); err != nil {
			return nil, fmt.Errorf("subscribe %s: %w", ch, err)
		}
	}
	return c, nil
}

// receiver runs on the transport's goroutine, so it must not block or write.
func (c *Client) receiver(ch protocol.Channel) func([]byte) {
	return func(buf []byte) {
		at := time.Now()
		c.logFrame("rx", ch, buf)
		// Parse keeps nothing of buf, so it can read the transport's buffer;
		// the event gets one copy (an Unknown message already holds it).
		msg, err := protocol.Parse(ch, buf)
		var raw []byte
		if u, ok := msg.(protocol.Unknown); ok {
			raw = u.Raw
		} else {
			raw = bytes.Clone(buf)
		}
		if h, ok := msg.(protocol.Handoff); ok && h.ToApp {
			c.onHandoff()
		}
		if !c.opts.HalfDuplex && ch == protocol.Data {
			c.legacyAck(buf)
		}
		c.deliver(Event{Channel: ch, Raw: raw, Msg: msg, Err: err, At: at})
	}
}

// deliver queues ev without blocking the transport. When the consumer falls
// behind, the oldest event goes, not this one: the newest state (and a
// Handoff, which a waiting command needs) matters more than stale records.
func (c *Client) deliver(ev Event) {
	for {
		select {
		case c.events <- ev:
			return
		default:
		}
		select {
		case old := <-c.events:
			c.log.Debug("event dropped: consumer not keeping up", "channel", old.Channel)
		default: // the consumer just made room
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
// Every repeat is acked, except while the same ack is still waiting in the
// queue, where one write will answer them all. Once the ack is being
// written the next repeat queues a fresh one: the device may have sent it
// before ours arrived, and a request that goes unacked is repeated forever,
// which also stops Live Data. A duplicate ack is harmless, a missing one is
// not.
//
// The record is recognized by its header, not by decoding it: one the
// client cannot parse (Static Data for a name over 127 bytes, say) must
// still be acked, or the device would repeat it forever. (The half-duplex
// loop needs none of this: it counts a confirmed indication as the ack and
// does not gate Peer Sync on Static Data.)
func (c *Client) legacyAck(frame []byte) {
	if len(frame) < 2 {
		return
	}
	var ack protocol.Frame
	switch cat, cmd := frame[0], frame[1]; {
	case cat == protocol.CatStaticData && cmd == 0x02:
		ack = protocol.AckStaticData()
	case protocol.IsWiFiList(frame):
		ack = protocol.ClearWiFiScan()
	case cat == protocol.CatPeer && cmd == 0x07:
		ack, _ = protocol.RequestPeerDetails()
	default:
		return
	}
	key := [2]byte{ack.Bytes[0], ack.Bytes[1]}
	c.qmu.Lock()
	if c.pending[key] {
		c.qmu.Unlock()
		return
	}
	c.pending[key] = true
	c.queue = append(c.queue, writeReq{f: ack})
	c.qmu.Unlock()
	c.signal()
}

func (c *Client) onHandoff() {
	c.mu.Lock()
	c.handoffs++
	c.mu.Unlock()
	c.takeTX()
	if c.opts.AutoGrant {
		go func() {
			time.Sleep(c.opts.GrantDelay)
			c.backgroundErr("TX grant", c.Grant())
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
func (c *Client) Done() <-chan struct{} { return c.link.Done() }

// HalfDuplex reports whether the session uses the TX-handoff loop.
func (c *Client) HalfDuplex() bool { return c.opts.HalfDuplex }

// Start sends the app runtime state and the ConnStatus-Ready frame. The
// device then streams Live Data (every ~3 s in legacy mode). In half-duplex
// mode Ready also hands it the TX window; it answers with Live Data, pending
// peer records and then a Handoff back to the app. Firmware 5.0.3 drops a
// link that has not sent Ready within 15 s.
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
	if err := c.write(protocol.GrantTX()); err != nil {
		// The device did not take the window, so it will not hand it back:
		// keep it, or every later Send would wait for a Handoff that never
		// comes.
		c.takeTX()
		return err
	}
	return nil
}

// takeTX gives the app the TX window and wakes everything waiting for it.
func (c *Client) takeTX() {
	c.mu.Lock()
	c.appOwnsTX = true
	waiters := c.txWaiters
	c.txWaiters = nil
	c.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

// AwaitTX blocks until the app holds the TX window: always in legacy mode,
// and in half-duplex mode before Start or after the device's next Handoff.
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
	case <-c.link.Done():
		c.dropWaiter(w)
		return ErrDisconnected
	case <-ctx.Done():
		c.dropWaiter(w)
		return ctx.Err()
	}
}

// dropWaiter forgets a waiter that gave up, so repeated timeouts do not
// pile up channels until the next handoff.
func (c *Client) dropWaiter(w chan struct{}) {
	c.mu.Lock()
	c.txWaiters = slices.DeleteFunc(c.txWaiters, func(x chan struct{}) bool { return x == w })
	c.mu.Unlock()
}

// Send writes frames, in order, within one app TX window, as the official
// app does. In legacy mode that is immediately.
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
// device processes writes regardless of who owns TX.
func (c *Client) SendNow(f protocol.Frame) error { return c.write(f) }

// write queues f and waits for the writer to write it.
func (c *Client) write(f protocol.Frame) error {
	select {
	case <-c.link.Done():
		return ErrDisconnected
	default:
	}
	r := writeReq{f: f, result: make(chan error, 1)}
	c.qmu.Lock()
	c.queue = append(c.queue, r)
	c.qmu.Unlock()
	c.signal()
	select {
	case err := <-r.result:
		return err
	case <-c.link.Done():
		return ErrDisconnected
	}
}

func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// writer performs every GATT write, one at a time, in the order they were
// queued. An ack the receiver queues therefore reaches the device before a
// request queued after it: a late Static Data ack (1,0) written after the
// next request (1,1) would cancel that request.
func (c *Client) writer() {
	for {
		select {
		case <-c.link.Done():
			c.qmu.Lock()
			q := c.queue
			c.queue = nil
			// The pending marks go with the queue they describe. The
			// invariant elsewhere is that a key in pending means an
			// unwritten ack for it is waiting, and dropping the queue
			// without the marks makes that false — so an ack for one of
			// those records would never be queued again.
			clear(c.pending)
			c.qmu.Unlock()
			for _, r := range q {
				if r.result != nil {
					r.result <- ErrDisconnected
				}
			}
			return
		case <-c.wake:
		}
		for {
			c.qmu.Lock()
			if len(c.queue) == 0 {
				c.qmu.Unlock()
				break
			}
			r := c.queue[0]
			c.queue = c.queue[1:]
			if r.result == nil {
				// The record can be repeated while this ack is still on the
				// wire, and that repeat must queue a fresh one: clear the
				// flag when the ack leaves the queue, not when its write
				// finishes. A duplicate ack is harmless, a missing one
				// leaves the device repeating the record forever.
				delete(c.pending, [2]byte{r.f.Bytes[0], r.f.Bytes[1]})
			}
			c.qmu.Unlock()
			err := c.writeNow(r.f)
			if r.result != nil {
				r.result <- err
				continue
			}
			c.backgroundErr("legacy ack", err)
		}
	}
}

func (c *Client) writeNow(f protocol.Frame) error {
	c.logFrame("tx", f.Channel, f.Bytes)
	if err := c.link.Write(f.Channel, f.Bytes); err != nil {
		return fmt.Errorf("write %s % x: %w", f.Channel, f.Bytes, err)
	}
	return nil
}

// backgroundErr logs a failed write nobody waits for, unless the link has
// dropped, which fails every write.
func (c *Client) backgroundErr(what string, err error) {
	if err == nil {
		return
	}
	select {
	case <-c.link.Done():
	default:
		c.log.Warn(what+" failed", "err", err)
	}
}

func (c *Client) logFrame(dir string, ch protocol.Channel, b []byte) {
	if c.log.Enabled(context.Background(), slog.LevelDebug) {
		c.log.Debug("frame", "dir", dir, "channel", ch, "data", hex.EncodeToString(b))
	}
}

// Close asks the device for a graceful disconnect, then drops the link. The
// request is written with response, so the device has it before the link
// goes.
func (c *Client) Close() error {
	select {
	case <-c.link.Done():
		// Done may mean the transport lost track of the link rather than
		// that the OS dropped it: close it anyway, or the Totem could stay
		// connected (and stop advertising).
		_ = c.link.Close()
		return nil
	default:
	}
	_ = c.write(protocol.DisconnectRequest())
	return c.link.Close()
}
