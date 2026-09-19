// Package clienttest provides a simulated Totem for testing code built on
// package client, in the spirit of net/http/httptest.
package clienttest

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

// Totem is an in-memory Totem on firmware 5.0.3 running the legacy
// (schema 0) BLE loop. It implements client.Link: writes go to the same
// command handlers the firmware has (recv_status_msgs / recv_data_msgs),
// and a send loop repeats Static Data, the WiFi list and Peer Sync until the
// app acknowledges them, pushes queued Peer Pings, and emits Live Data every
// third pass, like send_data.
//
// The exported fields are the device state. Set them before connecting; while
// a client is connected, read or change them inside Do.
type Totem struct {
	Static    protocol.StaticData
	Live      protocol.LiveData
	Peers     []protocol.PeerPing // bonded peers and POIs, in peer-table order
	Networks  []string            // what a WiFi scan finds
	PeerBlink bool
	WiFiKey   string
	// Fix is the last phone GNSS fix received, if any.
	Fix *protocol.PhoneFix
	// HidePowerMode models firmware 4.x, whose Live Data leaves power_mode
	// at 0 even though (7,3) still changes it.
	HidePowerMode bool
	// NoLiveData stops Live Data, as if the send loop never got to it.
	NoLiveData bool
	// IgnoreDeletes drops peer delete requests, as a device that refused
	// them would.
	IgnoreDeletes bool
	// Tick is the send-loop period; zero means 2 ms.
	Tick time.Duration

	mu        sync.Mutex
	subs      map[protocol.Channel]func([]byte)
	done      chan struct{}
	closeOnce sync.Once
	received  []protocol.Frame
	running   bool
	pass      int

	// firmware state (see send_data / recv_data_msgs)
	appConn    bool
	staticCmd  byte
	wifiCmd    byte
	peerCmd    byte
	staticSent bool
	scanned    bool
	outbox     []protocol.MAC
	graceful   bool
}

// New returns a Totem with plausible state: firmware 5.0.3, a GNSS fix,
// normal power, no peers.
func New() *Totem {
	acc := float32(3.5)
	alt := int32(18)
	return &Totem{
		Static: protocol.StaticData{
			MAC:       protocol.MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78},
			ReleaseID: 339, Version: "5.0.3", Age: 192, ColorID: 8, HalfDuplex: true,
			Name: "Test Totem", Branch: "totem",
		},
		Live: protocol.LiveData{
			SatCount: 18, PosAccuracyM: &acc, AltitudeM: &alt, Lat: 37.5867, Lon: -122.0073,
			BattVolts: 4.1, BattPct: 97, Channel: 6, PowerMode: protocol.PowerNormal,
			ColorID: 8, Azimuth: 90, HeadingMot: -1, Uptime: time.Minute, Age: 192,
			FullBright: true, HasLocation: true,
		},
		Networks: []string{"home", "cafe"},
		subs:     map[protocol.Channel]func([]byte){},
		done:     make(chan struct{}),
	}
}

// Connect starts a client session with t.
func (t *Totem) Connect(opts client.Options) (*client.Client, error) {
	return client.New(t, opts)
}

// Do runs f with the device state locked.
func (t *Totem) Do(f func(*Totem)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(t)
}

// Received returns every frame the client wrote, in order.
func (t *Totem) Received() []protocol.Frame {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.received)
}

// Graceful reports whether the client asked for a graceful disconnect.
func (t *Totem) Graceful() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.graceful
}

// Drop simulates the device dropping the link, as it does when it reboots.
func (t *Totem) Drop() { t.closeOnce.Do(func() { close(t.done) }) }

// Subscribe implements client.Link.
func (t *Totem) Subscribe(ch protocol.Channel, fn func([]byte)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subs[ch] = fn
	return nil
}

// Done implements client.Link.
func (t *Totem) Done() <-chan struct{} { return t.done }

// Close implements client.Link.
func (t *Totem) Close() error {
	t.Drop()
	return nil
}

// Write implements client.Link by running the firmware's command handlers.
func (t *Totem) Write(ch protocol.Channel, b []byte) error {
	select {
	case <-t.done:
		return client.ErrDisconnected
	default:
	}
	if len(b) < 2 {
		return fmt.Errorf("clienttest: %s frame too short: % x", ch, b)
	}
	t.mu.Lock()
	t.received = append(t.received, protocol.Frame{Channel: ch, Bytes: slices.Clone(b)})
	var err error
	start := false
	if ch == protocol.ConnStatus {
		start, err = t.statusCommand(b)
	} else {
		err = t.dataCommand(b)
	}
	if start && !t.running {
		t.running = true
		go t.sendLoop()
	}
	t.mu.Unlock()
	return err
}

// statusCommand mirrors recv_status_msgs. It reports whether the Ready
// frame arrived.
func (t *Totem) statusCommand(b []byte) (bool, error) {
	switch cat, cmd := b[0], b[1]; {
	case cat == protocol.CatConn && cmd == 0x01:
		if len(b) >= 4 && b[3] > 0 && t.Static.HalfDuplex {
			return false, errors.New("clienttest: only the legacy loop (schema 0) is simulated")
		}
		// Firmware without the half-duplex loop (Static.HalfDuplex false, as
		// on 4.x) ignores the schema byte and runs the legacy loop.
		t.appConn = true
		if len(b) >= 3 && b[2] == 1 {
			t.staticCmd, t.peerCmd = 0, 0
		}
		return true, nil
	case cat == protocol.CatConn && cmd == 0x03:
		t.graceful = true
	}
	return false, nil
}

// dataCommand mirrors recv_data_msgs and the handlers it launches.
func (t *Totem) dataCommand(b []byte) error {
	cat, cmd := b[0], b[1]
	switch cat {
	case protocol.CatStaticData:
		t.staticCmd = cmd
		if cmd == 0 {
			t.staticSent = true
		}
	case protocol.CatWiFi:
		t.wifiCmd = cmd
		switch cmd {
		case 0:
			t.scanned = false
		case 1:
			t.scanned = true
		case 3:
			var v struct{ NW, Join string }
			if err := json.Unmarshal(b[2:], &v); err != nil {
				return fmt.Errorf("clienttest: save wifi: %w", err)
			}
			if len(v.NW) > maxStaticString {
				return fmt.Errorf("clienttest: a %d-byte SSID would corrupt Static Data", len(v.NW))
			}
			t.Static.WiFiSSID, t.WiFiKey = v.NW, v.Join
		}
	case protocol.CatOptions:
		if cmd == 3 {
			// update_options: if 'name' in parsed, name = parsed['name'].strip()
			var v map[string]any
			if err := json.Unmarshal(b[2:], &v); err != nil {
				return fmt.Errorf("clienttest: save options: %w", err)
			}
			raw, ok := v["name"]
			if !ok {
				return nil
			}
			name, ok := raw.(string)
			if !ok {
				return fmt.Errorf("clienttest: save options: name is %T", raw)
			}
			name = strings.Trim(name, " \t\n\v\f\r")
			if len(name) > maxStaticString {
				return fmt.Errorf("clienttest: a %d-byte name would corrupt Static Data", len(name))
			}
			t.Static.Name = name
		}
	case protocol.CatPeer:
		t.peerCmd = cmd
		return t.peerCommand(cmd, b)
	case protocol.CatCompassPref:
		if cmd == 3 && len(b) >= 5 {
			t.Static.PersistentNorth = b[3]&1 != 0
			t.Static.CompassLock = b[3]&2 != 0
			t.PeerBlink = b[4]&4 != 0
			if len(b) > 6 {
				if pm := protocol.PowerMode(b[6] & 0x07); pm != protocol.PowerUnchanged {
					t.Live.PowerMode = pm
					t.Live.Eco = pm == protocol.PowerEco
				}
			}
		}
	case protocol.CatPhone:
		if cmd == 3 {
			var v struct {
				Lat, Lon float32
				HAcc     int8
				Flags    uint8
				Unix     int32
				UnixMS   int16
			}
			if err := binary.Read(bytes.NewReader(b[2:]), binary.LittleEndian, &v); err != nil {
				return fmt.Errorf("clienttest: phone fix: %w", err)
			}
			t.Fix = &protocol.PhoneFix{
				Lat: v.Lat, Lon: v.Lon, HAcc: float64(v.HAcc), Unix: v.Unix, UnixMS: v.UnixMS,
				Internet: v.Flags&1 != 0, UIClosed: v.Flags&2 != 0, Focused: v.Flags&4 != 0,
			}
		}
	case protocol.CatOTA:
		// cb__start_ota saves perform.ota and soft-reboots into the updater.
		d := t.tick()
		go func() {
			time.Sleep(d)
			t.Drop()
		}()
	}
	return nil
}

func (t *Totem) peerCommand(cmd byte, b []byte) error {
	switch cmd {
	case 3: // edit or delete: mac(6), rgb, flags(bit0 delete, bit2 hidden)
		if len(b) < 12 {
			return fmt.Errorf("clienttest: peer update too short: % x", b)
		}
		i := t.peerIndex(protocol.MAC(b[2:8]))
		if i < 0 {
			return nil
		}
		if b[11]&1 != 0 {
			if !t.IgnoreDeletes {
				t.Peers = slices.Delete(t.Peers, i, i+1)
			}
			return nil
		}
		t.Peers[i].Color = protocol.RGB{R: b[8], G: b[9], B: b[10]}
		t.Peers[i].Hidden = b[11]&4 != 0
	case 6: // add_new_bond: length, mac(6), '<ffbBBBhiiBBbbbhhiiib', name
		var v struct {
			Lat, Lon                           float32
			PAcc                               int8
			R, G, B                            uint8
			Azimuth                            int16
			Eat, Nbt                           int32
			AnimationID                        uint8
			Flags                              uint8
			Reserved33, Reserved34, Reserved35 int8
			Reserved36, Reserved38             int16
			Reserved40, Reserved44, Reserved48 int32
			NameLen                            int8
		}
		if len(b) < 53 {
			return fmt.Errorf("clienttest: add bond too short: % x", b)
		}
		if err := binary.Read(bytes.NewReader(b[9:53]), binary.LittleEndian, &v); err != nil {
			return err
		}
		p := protocol.PeerPing{
			MAC: protocol.MAC(b[3:9]), Name: string(pySlice(b, 53, 53+int(v.NameLen))),
			Lat: v.Lat, Lon: v.Lon, PosAccuracyM: v.PAcc, SpeedKPH: -1, Bearing: -1, RSSI: 100,
			Color: protocol.RGB{R: v.R, G: v.G, B: v.B},
			SOS:   v.Flags&1 != 0, POI: v.Flags&2 != 0, Hidden: v.Flags&8 != 0, Locked: v.Flags&16 != 0,
		}
		if i := t.peerIndex(p.MAC); i >= 0 {
			t.Peers[i] = p
		} else {
			t.Peers = append(t.Peers, p)
		}
	case 8: // queue Peer Pings for the listed peers, or all
		if len(b) > 3 {
			for off := 4; off+6 <= len(b); off += 6 {
				if m := protocol.MAC(b[off : off+6]); t.peerIndex(m) >= 0 {
					t.outbox = append(t.outbox, m)
				}
			}
		} else {
			for _, p := range t.Peers {
				t.outbox = append(t.outbox, p.MAC)
			}
		}
	}
	return nil
}

// maxStaticString is the longest string Static Data can report: its
// length fields are signed bytes.
const maxStaticString = 127

// pySlice is Python's b[i:j] for i >= 0, as unpack_utf8_str slices: j is
// clamped to the buffer and a negative j counts from its end.
func pySlice(b []byte, i, j int) []byte {
	if j < 0 {
		j += len(b)
	}
	j = min(j, len(b))
	if i >= j {
		return nil
	}
	return b[i:j]
}

func (t *Totem) peerIndex(m protocol.MAC) int {
	return slices.IndexFunc(t.Peers, func(p protocol.PeerPing) bool { return p.MAC == m })
}

func (t *Totem) tick() time.Duration {
	if t.Tick > 0 {
		return t.Tick
	}
	return 2 * time.Millisecond
}

// sendLoop mirrors send_data: one record per pass, in its priority order.
func (t *Totem) sendLoop() {
	for {
		t.mu.Lock()
		d := t.tick()
		t.mu.Unlock()
		select {
		case <-t.done:
			return
		case <-time.After(d):
		}
		t.mu.Lock()
		rec := t.nextRecord()
		fn := t.subs[protocol.Data]
		t.mu.Unlock()
		if rec == nil || fn == nil {
			continue
		}
		b, err := rec.MarshalBinary()
		if err != nil {
			panic(fmt.Sprintf("clienttest: encode %T: %v", rec, err))
		}
		fn(b)
	}
}

func (t *Totem) nextRecord() encoding.BinaryMarshaler {
	if !t.appConn {
		return nil
	}
	t.pass++
	switch {
	case t.staticCmd == 1:
		return t.Static
	case t.wifiCmd == 1 && t.scanned:
		return protocol.WiFiNetworks{SSIDs: slices.Clone(t.Networks)}
	case t.peerCmd == 1 && t.staticSent:
		s := protocol.PeerSync{}
		for _, p := range t.Peers {
			s.Peers = append(s.Peers, p.MAC)
		}
		return s
	case len(t.outbox) > 0:
		m := t.outbox[0]
		t.outbox = t.outbox[1:]
		if i := t.peerIndex(m); i >= 0 {
			return t.Peers[i]
		}
		return nil
	case t.pass%3 == 0 && !t.NoLiveData:
		live := t.Live
		if t.HidePowerMode {
			live.PowerMode = protocol.PowerUnchanged
		}
		return live
	}
	return nil
}
