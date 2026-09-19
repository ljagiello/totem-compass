package client_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/client/clienttest"
	"github.com/ljagiello/totem-compass/protocol"
)

// fakeLink records writes and lets a test inject device frames directly.
type fakeLink struct {
	mu     sync.Mutex
	subs   map[protocol.Channel]func([]byte)
	gate   chan struct{} // if set, writes wait until it is closed
	fail   error         // if set, writes fail with it
	writes chan protocol.Frame
	done   chan struct{}
	once   sync.Once
	closes int // Close calls
}

func newFakeLink() *fakeLink {
	return &fakeLink{
		subs:   map[protocol.Channel]func([]byte){},
		writes: make(chan protocol.Frame, 64),
		done:   make(chan struct{}),
	}
}

func (l *fakeLink) Subscribe(ch protocol.Channel, fn func([]byte)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subs[ch] = fn
	return nil
}

func (l *fakeLink) Write(ch protocol.Channel, b []byte) error {
	l.mu.Lock()
	gate, fail := l.gate, l.fail
	l.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if fail != nil {
		return fail
	}
	l.writes <- protocol.Frame{Channel: ch, Bytes: append([]byte(nil), b...)}
	return nil
}

func (l *fakeLink) holdWrites() (release func()) {
	gate := make(chan struct{})
	l.mu.Lock()
	l.gate = gate
	l.mu.Unlock()
	return func() { close(gate) }
}

func (l *fakeLink) Done() <-chan struct{} { return l.done }

func (l *fakeLink) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *fakeLink) inject(ch protocol.Channel, b []byte) {
	l.mu.Lock()
	fn := l.subs[ch]
	l.mu.Unlock()
	fn(b)
}

func (l *fakeLink) injectMsg(t *testing.T, m interface{ MarshalBinary() ([]byte, error) }) {
	t.Helper()
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	l.inject(protocol.Data, b)
}

// expectWrite fails unless the next write is want.
func (l *fakeLink) expectWrite(t *testing.T, want protocol.Frame) {
	t.Helper()
	select {
	case got := <-l.writes:
		if got.Channel != want.Channel || !bytes.Equal(got.Bytes, want.Bytes) {
			t.Fatalf("wrote %s % x, want %s % x", got.Channel, got.Bytes, want.Channel, want.Bytes)
		}
	case <-time.After(time.Second):
		t.Fatalf("no write, want %s % x", want.Channel, want.Bytes)
	}
}

// expectNoWrite fails if the client writes anything within a short grace
// period.
func (l *fakeLink) expectNoWrite(t *testing.T) {
	t.Helper()
	select {
	case got := <-l.writes:
		t.Fatalf("unexpected write %s % x", got.Channel, got.Bytes)
	case <-time.After(50 * time.Millisecond):
	}
}

func newClient(t *testing.T, opts client.Options) (*client.Client, *fakeLink) {
	t.Helper()
	l := newFakeLink()
	c, err := client.New(l, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return c, l
}

func nextEvent(t *testing.T, c *client.Client) client.Event {
	t.Helper()
	select {
	case ev := <-c.Events():
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event")
		return client.Event{}
	}
}

var static = protocol.StaticData{Version: "5.0.3", ReleaseID: 339, Name: "t", Branch: "totem"}

func TestStartLegacy(t *testing.T) {
	c, l := newClient(t, client.Options{})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.AppRuntime(protocol.RuntimeState{Active: true, Focused: true}))
	l.expectWrite(t, protocol.Ready(protocol.SchemaLegacy))
	// Legacy mode never waits for a TX window.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Send(ctx, protocol.RequestStaticData()); err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.RequestStaticData())
}

// The legacy loop repeats Static Data, the WiFi list and Peer Sync until the
// app acks them. A record requested again later must be acked again: a
// time-based dedupe once swallowed the second ack when a command re-read
// Static Data within a second, and the device then repeated it forever.
func TestLegacyAcksEveryRequest(t *testing.T) {
	c, l := newClient(t, client.Options{})
	peerAck, _ := protocol.RequestPeerDetails()
	for _, tc := range []struct {
		name string
		rec  interface{ MarshalBinary() ([]byte, error) }
		ack  protocol.Frame
	}{
		{"static", static, protocol.AckStaticData()},
		{"wifi", protocol.WiFiNetworks{SSIDs: []string{"a"}}, protocol.ClearWiFiScan()},
		{"peer sync", protocol.PeerSync{}, peerAck},
	} {
		for range 2 {
			l.injectMsg(t, tc.rec)
			l.expectWrite(t, tc.ack)
			if ev := nextEvent(t, c); ev.Err != nil {
				t.Fatalf("%s: %v", tc.name, ev.Err)
			}
		}
	}
	// Live Data needs no ack.
	l.injectMsg(t, protocol.LiveData{})
	l.expectNoWrite(t)
}

// A WiFi list cut off in transit is still the list, and still acked: the
// device repeats it until acked, ahead of Live Data.
func TestTruncatedWiFiListIsAcked(t *testing.T) {
	c, l := newClient(t, client.Options{})
	l.inject(protocol.Data, append([]byte{protocol.CatWiFi, 0x02}, `["home", "caf`...))
	l.expectWrite(t, protocol.ClearWiFiScan())
	if ev := nextEvent(t, c); ev.Msg == nil || !ev.Msg.(protocol.WiFiNetworks).Truncated {
		t.Errorf("event = %+v", ev)
	}
}

// Repeats that arrive while the ack is still being written don't queue more.
func TestLegacyAckNotDuplicatedWhileInFlight(t *testing.T) {
	_, l := newClient(t, client.Options{})
	release := l.holdWrites()
	for range 3 {
		l.injectMsg(t, static)
	}
	l.expectNoWrite(t)
	release()
	l.expectWrite(t, protocol.AckStaticData())
	l.expectNoWrite(t)
}

func TestEventsCarryParsedFramesAndErrors(t *testing.T) {
	c, l := newClient(t, client.Options{})
	l.injectMsg(t, protocol.LiveData{BattPct: 42})
	ev := nextEvent(t, c)
	if live, ok := ev.Msg.(protocol.LiveData); !ok || live.BattPct != 42 || ev.Channel != protocol.Data {
		t.Fatalf("event = %+v", ev)
	}
	l.inject(protocol.Data, []byte{protocol.CatLiveData, 0x01, 0x00}) // truncated
	if ev := nextEvent(t, c); ev.Err == nil || ev.Msg != nil {
		t.Fatalf("truncated frame: event = %+v", ev)
	}
}

func TestHalfDuplexWaitsForHandoff(t *testing.T) {
	c, l := newClient(t, client.Options{HalfDuplex: true, AutoGrant: true, GrantDelay: 10 * time.Millisecond})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.AppRuntime(protocol.RuntimeState{Active: true, Focused: true}))
	l.expectWrite(t, protocol.Ready(protocol.SchemaExtended))

	sent := make(chan error, 1)
	go func() { sent <- c.Send(context.Background(), protocol.RequestStaticData(), protocol.RequestPeerSync()) }()
	l.expectNoWrite(t) // the device owns TX

	l.inject(protocol.ConnStatus, []byte{protocol.CatHandoff, 0x02, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	l.expectWrite(t, protocol.RequestStaticData())
	l.expectWrite(t, protocol.RequestPeerSync())
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.GrantTX()) // AutoGrant, after the batch
	if n := c.Handoffs(); n != 1 {
		t.Errorf("Handoffs() = %d, want 1", n)
	}
	if ev := nextEvent(t, c); ev.Msg != (protocol.Handoff{ToApp: true}) {
		t.Errorf("event = %+v", ev)
	}
	// Half duplex does not send legacy acks.
	l.injectMsg(t, static)
	l.expectNoWrite(t)
}

// A GrantTX the device never got must leave the TX window with the app:
// the device will not hand back a window it does not have, so every later
// Send used to wait out its full timeout.
func TestFailedGrantKeepsTX(t *testing.T) {
	c, l := newClient(t, client.Options{HalfDuplex: true})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.AppRuntime(protocol.RuntimeState{Active: true, Focused: true}))
	l.expectWrite(t, protocol.Ready(protocol.SchemaExtended))
	l.inject(protocol.ConnStatus, []byte{protocol.CatHandoff, 0x02, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	nextEvent(t, c)

	l.mu.Lock()
	l.fail = errors.New("ATT error")
	l.mu.Unlock()
	if err := c.Grant(); err == nil {
		t.Fatal("Grant reported success for a failed write")
	}
	l.mu.Lock()
	l.fail = nil
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Send(ctx, protocol.RequestStaticData()); err != nil {
		t.Fatalf("Send after a failed grant: %v", err)
	}
	l.expectWrite(t, protocol.RequestStaticData())
}

// AwaitTX calls that give up must not leave their channels behind.
func TestAwaitTXForgetsWaitersThatGiveUp(t *testing.T) {
	c, _ := newClient(t, client.Options{HalfDuplex: true})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		if err := c.AwaitTX(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("AwaitTX = %v", err)
		}
		cancel()
	}
	if n := c.Waiters(); n != 0 {
		t.Errorf("%d waiters left behind", n)
	}
}

// When the consumer falls behind, the oldest events go: a Handoff that a
// command is waiting for used to be dropped as the newest.
func TestFullEventBufferKeepsNewest(t *testing.T) {
	c, l := newClient(t, client.Options{})
	for range 300 {
		l.injectMsg(t, protocol.LiveData{})
	}
	l.inject(protocol.ConnStatus, []byte{protocol.CatHandoff, 0x02, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	var last client.Event
	for n := 0; ; n++ {
		select {
		case ev := <-c.Events():
			last = ev
			continue
		default:
		}
		if n == 0 {
			t.Fatal("no events")
		}
		break
	}
	if last.Msg != (protocol.Handoff{ToApp: true}) {
		t.Errorf("last event = %+v, want the Handoff", last)
	}
}

// Close closes the link even after it reports Done, which can mean the
// transport lost track of a link the OS still holds.
func TestCloseAlwaysClosesTheLink(t *testing.T) {
	c, l := newClient(t, client.Options{})
	_ = l.Close() // the link reports Done
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closes != 2 {
		t.Errorf("link closed %d times, want 2", l.closes)
	}
}

func TestSendGivesUpWhenLinkDrops(t *testing.T) {
	c, l := newClient(t, client.Options{HalfDuplex: true})
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = l.Close()
	}()
	if err := c.Send(context.Background(), protocol.RequestStaticData()); !errors.Is(err, client.ErrDisconnected) {
		t.Fatalf("Send after link loss = %v, want ErrDisconnected", err)
	}
}

func TestCloseRequestsGracefulDisconnect(t *testing.T) {
	c, l := newClient(t, client.Options{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	l.expectWrite(t, protocol.DisconnectRequest())
	select {
	case <-c.Done():
	default:
		t.Fatal("link still up after Close")
	}
	if err := c.SendNow(protocol.RequestStaticData()); !errors.Is(err, client.ErrDisconnected) {
		t.Errorf("write after Close = %v, want ErrDisconnected", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

// logBuffer collects slog text output written from several goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogger(b *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func TestLoggerSeesFramesBothWays(t *testing.T) {
	var logs logBuffer
	c, l := newClient(t, client.Options{Logger: newLogger(&logs)})
	if err := c.SendNow(protocol.RequestStaticData()); err != nil {
		t.Fatal(err)
	}
	<-l.writes
	l.inject(protocol.ConnStatus, []byte{protocol.CatHandoff, 0x02, 0x02})
	nextEvent(t, c)
	want := "level=DEBUG msg=frame dir=tx channel=data data=0101\n" +
		"level=DEBUG msg=frame dir=rx channel=conn_status data=040202\n"
	if got := logs.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

// Writes the client makes on its own have no caller to report to, so their
// failures are logged.
func TestFailedLegacyAckIsLogged(t *testing.T) {
	var logs logBuffer
	_, l := newClient(t, client.Options{Logger: newLogger(&logs)})
	l.mu.Lock()
	l.fail = errors.New("gatt write failed")
	l.mu.Unlock()
	l.injectMsg(t, static)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), `level=WARN msg="legacy ack failed" err="write data 01 00: gatt write failed"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no warning in log:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FuzzReceive feeds arbitrary notifications to the client, as a transport
// would deliver them from the radio.
func FuzzReceive(f *testing.F) {
	for _, rec := range []interface{ MarshalBinary() ([]byte, error) }{
		static, protocol.LiveData{}, protocol.PeerSync{Peers: []protocol.MAC{{1}}},
		protocol.WiFiNetworks{SSIDs: []string{"a"}}, protocol.PeerPing{Name: "p"},
	} {
		b, err := rec.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(byte(protocol.Data), b, false)
	}
	f.Add(byte(protocol.ConnStatus), []byte{protocol.CatHandoff, 0x02, 0x02}, true)
	f.Add(byte(protocol.ConnStatus), []byte{protocol.CatConn, 0x05, 1, 0, 0, 0, 2, 0, 0, 0}, false)
	f.Add(byte(protocol.Data), []byte{0x03}, false)
	f.Fuzz(func(t *testing.T, chb byte, b []byte, halfDuplex bool) {
		ch := protocol.Channel(chb & 1)
		c, l := newClient(t, client.Options{HalfDuplex: halfDuplex})
		orig := bytes.Clone(b)
		l.inject(ch, b)
		for i := range b {
			b[i] ^= 0xff // transports reuse their buffers
		}
		ev := nextEvent(t, c)
		if ev.Channel != ch || !bytes.Equal(ev.Raw, orig) {
			t.Fatalf("event %s % x, want %s % x", ev.Channel, ev.Raw, ch, orig)
		}
		want, perr := protocol.Parse(ch, orig)
		if (ev.Err == nil) != (perr == nil) || (ev.Msg == nil) != (want == nil) {
			t.Fatalf("event msg %v err %v, Parse gives %v, %v", ev.Msg, ev.Err, want, perr)
		}
		if h, ok := ev.Msg.(protocol.Handoff); ok && h.ToApp && c.Handoffs() != 1 {
			t.Fatalf("handoff to the app not counted: %d", c.Handoffs())
		}
		// Legacy mode acks exactly the records the device repeats until acked.
		var ack protocol.Frame
		switch ev.Msg.(type) {
		case protocol.StaticData:
			ack = protocol.AckStaticData()
		case protocol.WiFiNetworks:
			ack = protocol.ClearWiFiScan()
		case protocol.PeerSync:
			ack, _ = protocol.RequestPeerDetails()
		}
		if ack.Bytes != nil && !halfDuplex {
			l.expectWrite(t, ack)
		}
		select {
		case w := <-l.writes:
			t.Fatalf("unexpected write %s % x", w.Channel, w.Bytes)
		default:
		}
	})
}

// A full legacy session against the simulated firmware.
func TestSessionWithSimulatedTotem(t *testing.T) {
	totem := clienttest.New()
	c, err := totem.Connect(client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c.SendNow(protocol.RequestStaticData()); err != nil {
		t.Fatal(err)
	}
	var gotStatic, gotLive bool
	deadline := time.After(2 * time.Second)
	for !gotStatic || !gotLive {
		select {
		case ev := <-c.Events():
			switch m := ev.Msg.(type) {
			case protocol.StaticData:
				gotStatic = m.Name == "Test Totem" && m.Version == "5.0.3"
			case protocol.LiveData:
				gotLive = m.BattPct == 97
			}
		case <-deadline:
			t.Fatalf("static %v live %v", gotStatic, gotLive)
		}
	}
	// The client acked Static Data, so the device stopped repeating it.
	acked := false
	for _, f := range totem.Received() {
		acked = acked || bytes.Equal(f.Bytes, protocol.AckStaticData().Bytes)
	}
	if !acked {
		t.Error("the simulated Totem never received the Static Data ack")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !totem.Graceful() {
		t.Error("Close did not request a graceful disconnect")
	}
}
