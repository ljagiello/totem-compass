package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"
)

// Message is a decoded device→app frame.
type Message interface{ isMessage() }

// LiveData is (0x03,0x01), sent every ~4 s while the app holds a session.
type LiveData struct {
	SatCount     int8
	PosAccuracyM *float32 // nil when the receiver reports none
	AltitudeM    *int32
	Lat, Lon     float32
	BattVolts    float32
	BattPct      int8
	Channel      int8 // ESP-NOW home channel
	PowerMode    PowerMode
	// PowerLevel is modes.power_level, the battery health: 0, or 2 below
	// about 3.45 V (compass.battery_volt). 4.1.3 packs modes.power_mode here.
	PowerLevel  int8
	MaxHopCnt   uint8
	MeshRx      uint8
	MeshRelayed uint8
	Time        time.Time // zero when the device RTC is not set
	ColorID     int8
	Orientation int8
	SolutionID  int8 // GNSS fix type
	HeadingMot  int16
	Azimuth     int16
	SpeedKPH    *int8
	Odometer    int32
	Uptime      time.Duration
	Age         int32
	SOS         bool
	Eco         bool
	// FullBright is config.led_brt >= GLOBAL_BRT (0.6), i.e. not dimmed by
	// eco mode. App 2.3.0 labels this bit isDimLeds.
	FullBright  bool
	HasLocation bool
	// LowBattery is modes.power_level == 2 (see PowerLevel).
	LowBattery   bool
	Charging     bool
	MagCalNeeded bool
}

// liveDataWire is '<bfi3fb4Bi3b2hbiffb3ibHBBb' (69 bytes) at offset 2, as
// ble_manager.gen_live_data packs it.
//
// The Reserved fields are named by their frame offset. They carry fixed
// values in every published firmware (3.2.12, 4.1.3, 5.0.2, 5.0.3): the
// struct.pack_into call passes constants for them. App 2.3.0
// (useLiveDataParser) reads each one and discards it, and runs Reserved69
// through unpackFlags without using a bit. Neither side names them.
// gen_live_data also computes len(config.peers), config.closest_peer,
// config.furthest_peer, the ms since config.last_peer_msg and
// gc.mem_free(), then never packs them, which suggests these slots once
// carried such statistics.
type liveDataWire struct {
	SatCount                       int8
	PAcc                           float32
	Altitude                       int32
	Lat, Lon, BattVolts            float32
	Channel                        int8
	PowerBits                      uint8 // bits 0-2 config.power_mode; 5.x sets bits 3-7
	MaxHopCnt, MeshRx, MeshRelayed uint8
	Unix                           int32
	ColorID, Orientation, Solution int8
	HeadingMot, Azimuth            int16
	Speed                          int8
	Odometer                       int32
	Reserved44, Reserved48         float32 // always 0
	Reserved52                     int8    // always -1
	Uptime, Age                    int32
	Reserved61                     int32 // always 0
	PowerLevel                     int8
	Reserved66                     uint16 // always 0
	Flags                          uint8
	Reserved69                     uint8 // always 0
	BattPct                        int8
}

// StaticData is (0x01,0x02): identity and settings that rarely change.
type StaticData struct {
	MAC             MAC
	ReleaseID       uint16
	Version         string
	Age             int32
	ColorID         int8
	PersistentNorth bool
	CompassLock     bool
	// BondChat is settings bit 3, which makes app 2.3.0 enable bond chat.
	// Firmware up to 5.0.3 always sends 0.
	BondChat bool
	// HalfDuplex means the device has the half-duplex (TX handoff) loop,
	// send_data_v2. 5.x sets it; 4.1.3, which has no such loop, does not.
	// App 2.3.0 switches to TX handoff when it is set.
	HalfDuplex bool
	ServiceID  uint8
	Name       string
	Branch     string
	WiFiSSID   string
}

// staticDataWire is '<biHBBBbBBBbhhiiibbb' (34 bytes) at offset 9, as
// ble_core.gen_static_data packs it. The Reserved fields, named by frame
// offset, are 0 in every published firmware: the first is a literal 0 and
// the rest are unpacked from the literal tuple (0, 0, 0, 0, 0, 0). App 2.3.0
// (useStaticDataParser) reads each one and discards it.
type staticDataWire struct {
	Reserved9                          int8
	Age                                int32
	ReleaseID                          uint16
	Major, Minor, Patch                uint8
	ColorID                            int8
	Settings                           uint8 // bit0 persistent north, bit1 compass lock, bit3 bond chat
	Caps                               uint8 // bit0 half duplex
	ServiceID                          uint8
	Reserved23                         int8
	Reserved24, Reserved26             int16
	Reserved28, Reserved32, Reserved36 int32
	NameLen, BranchLen, SSL            int8
}

// PeerPing is (0x06,0x02): one bonded peer (or POI) and its last known state.
type PeerPing struct {
	MAC          MAC
	Name         string
	MeshHops     uint8
	Lat, Lon     float32
	PosAccuracyM int8 // -1 = unknown
	SpeedKPH     int8 // -1 = unknown
	Bearing      int16
	Color        RGB
	// DTIM is what app 2.3.0 reads as the peer's dtim. Firmware up to 5.0.3
	// always sends 0; the firmware's DTIM is an ESP-NOW message parameter
	// (espnow_conn_v2.gen_peer_msg), not stored per peer.
	DTIM          uint16
	RSSI          int8 // 100 = unknown
	MsgRx, MsgTx  uint8
	MeshRx        uint8
	MeshSendCount uint8
	LastUpdate    int32
	LastCoords    time.Time
	DistanceDiff  int16
	Orientation   int8
	Volts         float32
	BattPct       int8
	ReleaseID     uint16
	SOS           bool
	POI           bool
	ViaMesh       bool
	Stale         bool
	Collected     bool
	// Idle is flags bit 5, which app 2.3.0 reads as isIdle. Firmware up to
	// 5.0.3 always sends 0.
	Idle    bool
	Unknown bool
	Hidden  bool
	Locked  bool
}

// peerPingWire is '<ffbbh4BHbb4BiihBbf' (40 bytes) at offset 10, as
// ble_manager.gen_peer_ping packs it.
type peerPingWire struct {
	Lat, Lon                            float32
	PAcc, Speed                         int8
	Azimuth                             int16
	FlagsA, R, G, B                     uint8
	DTIM                                uint16
	NameLen, RSSI                       int8
	MsgRx, MsgTx, MeshRx, MeshSendCount uint8
	LastUpdate, LastCoordsUnix          int32
	DistanceDiff                        int16
	FlagsB                              uint8
	Orientation                         int8
	Volts                               float32
}

// PeerSync is (0x06,0x07): the MACs of every bonded peer.
type PeerSync struct{ Peers []MAC }

// WiFiNetworks is (0x02,0x02)+JSON: SSIDs found by a ScanWiFi, strongest first.
type WiFiNetworks struct {
	SSIDs []string
	// Truncated means the list was cut off in transit (a long list can
	// outgrow the frame): SSIDs holds the complete entries before the cut.
	Truncated bool
}

// FileChunk is (0x02,0x02)+'<HBBBiHiB': the device announcing or closing a
// file (log) upload to the app. In 5.0.3 the chunk data itself streams on the
// on-demand characteristic.
type FileChunk struct {
	FileID   uint16
	Status   uint8 // 1 in progress, 4 error
	Action   uint8 // 0 announce, 1 last chunk
	FileType uint8
	BytePos  int32
	ChunkNo  uint16
	FileSize int32
	// Flags byte, 0 in 5.0.2; 5.0.3 packs (is_last_chunk, is_origin_compass).
	LastChunk   bool
	FromCompass bool
	SHA256      []byte
	Name        string
	ErrNo       uint8
}

// Handoff is (0x04,0x02) on conn-status: the device has sent everything it
// had queued and hands the half-duplex TX window to the app.
type Handoff struct{ ToApp bool }

// DisconnectIntent is (0x00,0x02|0x05) on conn-status: the device is about
// to drop the link (e.g. to save power).
type DisconnectIntent struct {
	Cmd    uint8
	Params [2]int32 // (0x00,0x05) only
}

// Unknown is any frame this package does not decode.
type Unknown struct {
	Channel  Channel
	Cat, Cmd uint8
	Raw      []byte
}

func (LiveData) isMessage()         {}
func (StaticData) isMessage()       {}
func (PeerPing) isMessage()         {}
func (PeerSync) isMessage()         {}
func (WiFiNetworks) isMessage()     {}
func (FileChunk) isMessage()        {}
func (Handoff) isMessage()          {}
func (DisconnectIntent) isMessage() {}
func (Unknown) isMessage()          {}

// Parse decodes one frame received (notify/indicate) on ch.
func Parse(ch Channel, b []byte) (Message, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("%s frame too short: % x", ch, b)
	}
	cat, cmd := b[0], b[1]
	unknown := func() (Message, error) { return Unknown{ch, cat, cmd, bytes.Clone(b)}, nil }
	if ch == ConnStatus {
		switch {
		case cat == CatHandoff && cmd == 0x02 && len(b) >= 3:
			return Handoff{ToApp: bit(b[2], 1)}, nil
		case cat == CatConn && cmd == 0x05 && len(b) >= 10:
			var d DisconnectIntent
			d.Cmd = cmd
			err := binary.Read(bytes.NewReader(b[2:10]), binary.LittleEndian, &d.Params)
			return d, err
		case cat == CatConn && cmd == 0x02:
			return DisconnectIntent{Cmd: cmd}, nil
		}
		return unknown()
	}
	switch {
	case cat == CatLiveData && cmd == 0x01:
		return parseLiveData(b)
	case cat == CatStaticData && cmd == 0x02:
		return parseStaticData(b)
	case cat == CatPeer && cmd == 0x02:
		return parsePeerPing(b)
	case cat == CatPeer && cmd == 0x07:
		return parsePeerSync(b)
	case cat == CatWiFi && cmd == 0x02:
		// Both the WiFi list and file chunks use (2,2); the app tells them
		// apart by whether it asked for a scan. Without that context: a
		// frame starting ["  or [] is the list, anything else a chunk. (A
		// chunk whose FileID is 0x225b or 0x5d5b would be misread.)
		if len(b) > 3 && b[2] == '[' && (b[3] == '"' || b[3] == ']') {
			return parseWiFiNetworks(b[2:])
		}
		return parseFileChunk(b)
	}
	return unknown()
}

// parseWiFiNetworks decodes the JSON list of SSIDs, keeping the complete
// entries of a list cut off in transit. The device repeats the list until
// it is acknowledged, so it must decode even then.
func parseWiFiNetworks(b []byte) (Message, error) {
	var w WiFiNetworks
	if json.Unmarshal(b, &w.SSIDs) == nil {
		return w, nil
	}
	w.SSIDs = nil
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil { // the '['
		return nil, fmt.Errorf("wifi networks: %w", err)
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			w.Truncated = true
			return w, nil
		}
		ssid, ok := tok.(string)
		if !ok {
			if d, ok := tok.(json.Delim); ok && d == ']' {
				// A complete list that failed to unmarshal holds non-strings.
				return nil, fmt.Errorf("wifi networks: not a list of strings: %q", b)
			}
			return nil, fmt.Errorf("wifi networks: unexpected %v in %q", tok, b)
		}
		w.SSIDs = append(w.SSIDs, ssid)
	}
}

func readAt(b []byte, off int, v any) error {
	n := binary.Size(v)
	if len(b) < off+n {
		return fmt.Errorf("(%d,%d) frame is %d bytes, need %d", b[0], b[1], len(b), off+n)
	}
	return binary.Read(bytes.NewReader(b[off:off+n]), binary.LittleEndian, v)
}

func str(b []byte, off, n int) (string, int, error) {
	if n < 0 || len(b) < off+n {
		return "", off, fmt.Errorf("string at %d+%d overruns %d-byte frame", off, n, len(b))
	}
	return string(b[off : off+n]), off + n, nil
}

func unixTime(s int32) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(s), 0)
}

func parseLiveData(b []byte) (Message, error) {
	var w liveDataWire
	if err := readAt(b, 2, &w); err != nil {
		return nil, err
	}
	d := LiveData{
		SatCount: w.SatCount, Lat: w.Lat, Lon: w.Lon, BattVolts: w.BattVolts, BattPct: w.BattPct,
		Channel: w.Channel, PowerMode: PowerMode(w.PowerBits & 0x07), PowerLevel: w.PowerLevel,
		MaxHopCnt: w.MaxHopCnt, MeshRx: w.MeshRx, MeshRelayed: w.MeshRelayed,
		Time: unixTime(w.Unix), ColorID: w.ColorID, Orientation: w.Orientation, SolutionID: w.Solution,
		HeadingMot: w.HeadingMot, Azimuth: w.Azimuth, Odometer: w.Odometer,
		Uptime: time.Duration(w.Uptime) * time.Second, Age: w.Age,
		SOS: bit(w.Flags, 0), Eco: bit(w.Flags, 1), FullBright: bit(w.Flags, 2), HasLocation: bit(w.Flags, 3),
		LowBattery: bit(w.Flags, 4), Charging: bit(w.Flags, 5), MagCalNeeded: bit(w.Flags, 7),
	}
	if w.PAcc != -1 {
		d.PosAccuracyM = &w.PAcc
	}
	if w.Altitude != -500 {
		d.AltitudeM = &w.Altitude
	}
	if w.Speed != -1 {
		d.SpeedKPH = &w.Speed
	}
	return d, nil
}

func parseStaticData(b []byte) (Message, error) {
	var w staticDataWire
	if err := readAt(b, 9, &w); err != nil {
		return nil, err
	}
	d := StaticData{
		ReleaseID: w.ReleaseID, Age: w.Age, ColorID: w.ColorID, ServiceID: w.ServiceID,
		Version:         fmt.Sprintf("%d.%d.%d", w.Major, w.Minor, w.Patch),
		PersistentNorth: bit(w.Settings, 0), CompassLock: bit(w.Settings, 1), BondChat: bit(w.Settings, 3),
		HalfDuplex: bit(w.Caps, 0),
	}
	copy(d.MAC[:], b[3:9])
	off := 9 + binary.Size(w)
	var err error
	if d.Name, off, err = str(b, off, int(w.NameLen)); err != nil {
		return nil, err
	}
	if d.Branch, off, err = str(b, off, int(w.BranchLen)); err != nil {
		return nil, err
	}
	if d.WiFiSSID, _, err = str(b, off, int(w.SSL)); err != nil {
		return nil, err
	}
	return d, nil
}

func parsePeerPing(b []byte) (Message, error) {
	var w peerPingWire
	if err := readAt(b, 10, &w); err != nil {
		return nil, err
	}
	p := PeerPing{
		MeshHops: b[9], Lat: w.Lat, Lon: w.Lon, PosAccuracyM: w.PAcc, SpeedKPH: w.Speed,
		Bearing: w.Azimuth, Color: RGB{w.R, w.G, w.B}, DTIM: w.DTIM, RSSI: w.RSSI,
		MsgRx: w.MsgRx, MsgTx: w.MsgTx, MeshRx: w.MeshRx, MeshSendCount: w.MeshSendCount,
		LastUpdate: w.LastUpdate, LastCoords: unixTime(w.LastCoordsUnix), DistanceDiff: w.DistanceDiff,
		Orientation: w.Orientation, Volts: w.Volts,
		SOS: bit(w.FlagsA, 0), POI: bit(w.FlagsA, 1), ViaMesh: bit(w.FlagsA, 2), Stale: bit(w.FlagsA, 3),
		Collected: bit(w.FlagsA, 4), Idle: bit(w.FlagsA, 5), Unknown: bit(w.FlagsA, 6),
		Hidden: bit(w.FlagsB, 0), Locked: bit(w.FlagsB, 1),
	}
	copy(p.MAC[:], b[3:9])
	off := 10 + binary.Size(w)
	var err error
	if p.Name, off, err = str(b, off, int(w.NameLen)); err != nil {
		return nil, err
	}
	var tail struct {
		BattPct   int8
		ReleaseID uint16
	}
	if err := readAt(b, off, &tail); err != nil {
		return nil, err
	}
	p.BattPct, p.ReleaseID = tail.BattPct, tail.ReleaseID
	return p, nil
}

func parsePeerSync(b []byte) (Message, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("peer sync frame too short: % x", b)
	}
	n := int(b[3])
	if len(b) < 4+6*n {
		return nil, fmt.Errorf("peer sync lists %d peers but is %d bytes", n, len(b))
	}
	s := PeerSync{Peers: make([]MAC, n)}
	for i := range s.Peers {
		copy(s.Peers[i][:], b[4+6*i:])
	}
	return s, nil
}

func parseFileChunk(b []byte) (Message, error) {
	var h struct {
		FileID                   uint16
		Status, Action, FileType uint8
		BytePos                  int32
		ChunkNo                  uint16
		FileSize                 int32
		Flags                    uint8
	}
	if err := readAt(b, 2, &h); err != nil {
		return nil, err
	}
	c := FileChunk{
		FileID: h.FileID, Status: h.Status, Action: h.Action, FileType: h.FileType,
		BytePos: h.BytePos, ChunkNo: h.ChunkNo, FileSize: h.FileSize,
		LastChunk: bit(h.Flags, 0), FromCompass: bit(h.Flags, 1),
	}
	if len(b) >= 51 {
		c.SHA256 = append([]byte(nil), b[18:50]...)
		n := int(b[50])
		if len(b) >= 51+n {
			c.Name = string(b[51 : 51+n])
			if len(b) > 51+n {
				c.ErrNo = b[51+n]
			}
		}
	}
	return c, nil
}
