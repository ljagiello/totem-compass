package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// Device-side encoders. They build the records a Totem sends, byte for byte
// as the v5.0.3 gen_* builders do, for simulators and tests; a client only
// needs Parse.

// MarshalBinary encodes the (0x03,0x01) Live Data record (71 bytes).
func (d LiveData) MarshalBinary() ([]byte, error) {
	w := liveDataWire{
		SatCount: d.SatCount, PAcc: -1, Altitude: -500,
		Lat: d.Lat, Lon: d.Lon, BattVolts: d.BattVolts, Channel: d.Channel,
		// pack_bits(v, 3, 5, 31) fills the upper five bits in 5.0.x.
		PowerBits: 0xf8 | uint8(d.PowerMode)&0x07,
		MaxHopCnt: d.MaxHopCnt, MeshRx: d.MeshRx, MeshRelayed: d.MeshRelayed,
		ColorID: d.ColorID, Orientation: d.Orientation, Solution: d.SolutionID,
		HeadingMot: d.HeadingMot, Azimuth: d.Azimuth, Speed: -1, Odometer: d.Odometer,
		Reserved: -1, Uptime: int32(d.Uptime.Seconds()), Age: d.Age, PowerLevel: d.PowerLevel,
		Flags:   packFlags(d.SOS, d.Eco, d.FullBright, d.HasLocation, d.HighPower, d.Charging, false, d.MagCalNeeded),
		BattPct: d.BattPct,
	}
	if d.PosAccuracyM != nil {
		w.PAcc = *d.PosAccuracyM
	}
	if d.AltitudeM != nil {
		w.Altitude = *d.AltitudeM
	}
	if d.SpeedKPH != nil {
		w.Speed = *d.SpeedKPH
	}
	if !d.Time.IsZero() {
		w.Unix = int32(d.Time.Unix())
	}
	return record(CatLiveData, 0x01, w)
}

// MarshalBinary encodes the (0x01,0x02) Static Data record.
func (d StaticData) MarshalBinary() ([]byte, error) {
	var major, minor, patch uint8
	if _, err := fmt.Sscanf(d.Version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return nil, fmt.Errorf("static data version %q: %w", d.Version, err)
	}
	strs := []string{d.Name, d.Branch, d.WiFiSSID}
	for _, s := range strs {
		if len(s) > 127 {
			return nil, fmt.Errorf("static data string %q too long", s)
		}
	}
	w := staticDataWire{
		Age: d.Age, ReleaseID: d.ReleaseID, Major: major, Minor: minor, Patch: patch,
		ColorID: d.ColorID, Settings: packFlags(d.PersistentNorth, d.CompassLock), Const: 1,
		ServiceID: d.ServiceID,
		NameLen:   int8(len(d.Name)), BranchLen: int8(len(d.Branch)), SSL: int8(len(d.WiFiSSID)),
	}
	var buf bytes.Buffer
	buf.Write([]byte{CatStaticData, 0x02, 0})
	buf.Write(d.MAC[:])
	if err := binary.Write(&buf, binary.LittleEndian, w); err != nil {
		return nil, err
	}
	buf.WriteString(strings.Join(strs, ""))
	b := buf.Bytes()
	b[2] = byte(len(b))
	return b, nil
}

// MarshalBinary encodes the (0x06,0x02) Peer Ping record.
func (p PeerPing) MarshalBinary() ([]byte, error) {
	if len(p.Name) > 127 {
		return nil, fmt.Errorf("peer name %q too long", p.Name)
	}
	w := peerPingWire{
		Lat: p.Lat, Lon: p.Lon, PAcc: p.PosAccuracyM, Speed: p.SpeedKPH, Azimuth: p.Bearing,
		FlagsA: packFlags(p.SOS, p.POI, p.ViaMesh, p.Stale, p.Collected, false, p.Unknown),
		R:      p.Color.R, G: p.Color.G, B: p.Color.B,
		NameLen: int8(len(p.Name)), RSSI: p.RSSI,
		MsgRx: p.MsgRx, MsgTx: p.MsgTx, MeshRx: p.MeshRx, MeshSendCount: p.MeshSendCount,
		LastUpdate: p.LastUpdate, DistanceDiff: p.DistanceDiff,
		FlagsB: packFlags(p.Hidden, p.Locked), Orientation: p.Orientation, Volts: p.Volts,
	}
	if !p.LastCoords.IsZero() {
		w.LastCoordsUnix = int32(p.LastCoords.Unix())
	}
	var buf bytes.Buffer
	buf.Write([]byte{CatPeer, 0x02, 0})
	buf.Write(p.MAC[:])
	buf.WriteByte(p.MeshHops)
	if err := binary.Write(&buf, binary.LittleEndian, w); err != nil {
		return nil, err
	}
	buf.WriteString(p.Name)
	if err := binary.Write(&buf, binary.LittleEndian, struct {
		BattPct   int8
		ReleaseID uint16
	}{p.BattPct, p.ReleaseID}); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	b[2] = byte(len(b))
	return b, nil
}

// MarshalBinary encodes the (0x06,0x07) Peer Sync record.
func (s PeerSync) MarshalBinary() ([]byte, error) {
	b := []byte{CatPeer, 0x07, 0, byte(len(s.Peers))}
	for _, m := range s.Peers {
		b = append(b, m[:]...)
	}
	b[2] = byte(len(b))
	return b, nil
}

// MarshalBinary encodes the (0x02,0x02) WiFi scan result as the firmware
// does: json.dumps of the SSID list, with Python's ", " separator.
func (w WiFiNetworks) MarshalBinary() ([]byte, error) {
	quoted := make([]string, len(w.SSIDs))
	for i, s := range w.SSIDs {
		q, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		quoted[i] = string(q)
	}
	return append([]byte{CatWiFi, 0x02}, "["+strings.Join(quoted, ", ")+"]"...), nil
}

func record(cat, cmd byte, wire any) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write([]byte{cat, cmd})
	if err := binary.Write(&buf, binary.LittleEndian, wire); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
