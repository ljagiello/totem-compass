package mesh

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PeerCommand is the command byte of a category 0 frame.
type PeerCommand uint8

const (
	// PeerStatus is the periodic status a Totem unicasts to every bonded
	// peer in each radio window.
	PeerStatus PeerCommand = 0
	// PeerBond asks for a bond (broadcast, Ack false) or confirms one
	// (unicast, Ack true).
	PeerBond PeerCommand = 1
	// PeerUnbond tells a deleted peer. Receivers ignore it in 5.0.3.
	PeerUnbond PeerCommand = 2
)

// Orientation is fusion.orientation.
type Orientation int8

// Orientations. A Totem transmits more often while it or a peer lies flat.
const (
	OrientationUnknown    Orientation = 0
	OrientationVertical   Orientation = 1
	OrientationHorizontal Orientation = 2
)

// GNSSSource says where the position came from.
type GNSSSource int8

// GNSS sources.
const (
	GNSSDevice GNSSSource = 1 // the Totem's own receiver
	GNSSPhone  GNSSSource = 2 // gnss_data.is_phone_gnss_used
)

// Frame sizes.
const (
	// PeerFrameLen is len(ENOW_PEER_BUFF): a peer frame always goes on the
	// air as the whole buffer.
	PeerFrameLen = 108
	// MaxPeerName is the longest name whose trailing fields still fit the
	// buffer; Messages.gen_peer_msg fails on a longer one.
	MaxPeerName = PeerFrameLen - peerNameOffset - peerTailLen
)

const (
	peerNameOffset = 71
	peerTailLen    = 5
)

// Peer is a category 0 frame: the status, bond request or unbond notice
// built by Messages.gen_peer_msg.
type Peer struct {
	Command      PeerCommand
	Lat, Lon     float32 // degrees; 0 without a fix
	PosAccuracyM int8    // -1 without a fix, else clamped to 127
	SpeedKPH     int8
	Azimuth      int16 // compass heading, degrees
	SOS          bool
	Orientation  Orientation
	// Ack marks a bond confirmation (PeerBond sent back to the chosen
	// partner) as opposed to the broadcast request.
	Ack bool
	// TargetMAC is always zero in 5.0.3: no caller passes target_mac.
	TargetMAC MAC
	// Unscheduled is always 0 from a Totem. A receiver that sees a
	// non-zero value does not predict the sender's next radio window.
	Unscheduled int16
	// TimeOfDayMs (rtc.itod) and Unix are -1 unless the sender's clock
	// came from GNSS; a peer without a clock adopts them.
	TimeOfDayMs int32
	Unix        int32
	// PhoneConnected is flags bit 0, modes.evt_ble_active.
	PhoneConnected      bool
	Major, Minor, Patch uint8
	AltitudeM           int16  // meters above sea level; -500 unknown
	UptimeMin           uint16 // minutes since boot
	BattVolts           float32
	HeadingOfMotion     int16 // degrees; -1 until 10 m traveled
	OdometerM           int16
	Name                string // at most MaxPeerName bytes of UTF-8
	SolutionID          int8   // 0 no or poor fix, 1 within 3.5 m, 2 within 15 m
	GNSSSource          GNSSSource
	ReleaseID           uint16
	BattPct             int8
}

// peerWire is EXTENDED[(0,0)][72], '<BBffbbhbbb6Bhii4BhHehhffbBBiiBBb'
// (69 bytes) at frame offset 2, filled by gen_peer_msg, update_location,
// update_sos and update_device_data.
//
// The Reserved fields are named by frame offset. Every 5.x and 4.1.3 build
// packs constants into them (gen_peer_msg: 0.0, 0.0, -1, 0 at 49-58;
// update_device_data: '<BiiBBb' zeros at 59-69), and Parser._peer never
// reads them.
type peerWire struct {
	Category, Command      uint8
	Lat, Lon               float32
	PAcc, Speed            int8
	Azimuth                int16
	SOS, Orientation, Ack  int8
	TargetMAC              MAC
	Unscheduled            int16
	TimeOfDay, Unix        int32
	Flags                  uint8
	Major, Minor, Patch    uint8
	Altitude               int16
	UptimeMin              uint16
	BattVolts              uint16 // struct 'e'
	HeadingMot, Odometer   int16
	Reserved49, Reserved53 float32
	Reserved57             int8
	Reserved58, Reserved59 uint8
	Reserved60, Reserved64 int32
	Reserved68, Reserved69 uint8
	NameLen                int8
}

// peerTail is '<bbHb', packed right after the name.
type peerTail struct {
	SolutionID, GNSSSource int8
	ReleaseID              uint16
	BattPct                int8
}

func (Peer) isMessage() {}

// MarshalBinary encodes the frame as a 108-byte ENOW_PEER_BUFF.
func (p Peer) MarshalBinary() ([]byte, error) {
	if len(p.Name) > MaxPeerName {
		return nil, fmt.Errorf("mesh: peer name is %d bytes, the frame holds %d", len(p.Name), MaxPeerName)
	}
	w := peerWire{
		Category: CatPeer, Command: uint8(p.Command),
		Lat: p.Lat, Lon: p.Lon, PAcc: p.PosAccuracyM, Speed: p.SpeedKPH, Azimuth: p.Azimuth,
		SOS: flag(p.SOS), Orientation: int8(p.Orientation), Ack: flag(p.Ack),
		TargetMAC: p.TargetMAC, Unscheduled: p.Unscheduled,
		TimeOfDay: p.TimeOfDayMs, Unix: p.Unix, Flags: uint8(flag(p.PhoneConnected)),
		Major: p.Major, Minor: p.Minor, Patch: p.Patch,
		Altitude: p.AltitudeM, UptimeMin: p.UptimeMin, BattVolts: encodeHalf(p.BattVolts),
		HeadingMot: p.HeadingOfMotion, Odometer: p.OdometerM,
		Reserved57: -1,
		NameLen:    int8(len(p.Name)),
	}
	b, err := appendWire(make([]byte, 0, PeerFrameLen), w)
	if err != nil {
		return nil, err
	}
	b = append(b, p.Name...)
	t := peerTail{SolutionID: p.SolutionID, GNSSSource: int8(p.GNSSSource), ReleaseID: p.ReleaseID, BattPct: p.BattPct}
	if b, err = binary.Append(b, binary.LittleEndian, t); err != nil {
		return nil, err
	}
	return append(b, make([]byte, PeerFrameLen-len(b))...), nil
}

func parsePeer(b []byte) (Message, error) {
	var w peerWire
	if err := readAt(b, 2, &w); err != nil {
		return nil, err
	}
	if w.NameLen < 0 {
		return nil, fmt.Errorf("mesh: peer name length %d", w.NameLen)
	}
	end := peerNameOffset + int(w.NameLen)
	var t peerTail
	if err := readAt(b, end, &t); err != nil {
		return nil, errors.Join(errors.New("mesh: peer frame ends inside the name or the fields after it"), err)
	}
	return Peer{
		Command: PeerCommand(w.Command),
		Lat:     w.Lat, Lon: w.Lon, PosAccuracyM: w.PAcc, SpeedKPH: w.Speed, Azimuth: w.Azimuth,
		SOS: w.SOS != 0, Orientation: Orientation(w.Orientation), Ack: w.Ack != 0,
		TargetMAC: w.TargetMAC, Unscheduled: w.Unscheduled,
		TimeOfDayMs: w.TimeOfDay, Unix: w.Unix, PhoneConnected: w.Flags&1 != 0,
		Major: w.Major, Minor: w.Minor, Patch: w.Patch,
		AltitudeM: w.Altitude, UptimeMin: w.UptimeMin, BattVolts: decodeHalf(w.BattVolts),
		HeadingOfMotion: w.HeadingMot, OdometerM: w.Odometer,
		Name:       string(b[peerNameOffset:end]),
		SolutionID: t.SolutionID, GNSSSource: GNSSSource(t.GNSSSource), ReleaseID: t.ReleaseID, BattPct: t.BattPct,
	}, nil
}

func flag(v bool) int8 {
	if v {
		return 1
	}
	return 0
}
