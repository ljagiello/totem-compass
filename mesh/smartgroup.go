package mesh

import (
	"encoding/binary"
	"fmt"
)

// SmartGroupInstruction is the host's instruction_id in a (7,0) beacon.
type SmartGroupInstruction int8

// Smart Group instructions.
const (
	SmartGroupAbandon   SmartGroupInstruction = -1 // the host gave up; members leave
	SmartGroupAdvertise SmartGroupInstruction = 1  // invite: nearby Totems in pairing may join
	SmartGroupFinalize  SmartGroupInstruction = 3  // commit the group and its colors
)

// SmartGroupRSSI is SMART_GROUP_RSSI: a Totem joins a group only when the
// host's beacon arrives at this RSSI or stronger.
const SmartGroupRSSI = -45

const (
	smartGroupHeaderLen = 21
	smartGroupMemberLen = 16
	// smartGroupSlots is the constant gen_auto_bond_grp packs at offset 18.
	smartGroupSlots = 8
)

// MaxSmartGroupMembers is the most members a beacon can list and still fit
// an ESP-NOW frame.
const MaxSmartGroupMembers = (MaxFrame - smartGroupHeaderLen) / smartGroupMemberLen

// SmartGroup is the (7,0) beacon a Smart Group host broadcasts about three
// times a second while it gathers members (Messages.gen_auto_bond_grp).
type SmartGroup struct {
	Lat, Lon     float32 // host position, degrees
	PosAccuracyM int8
	// ColorID is 0 while advertising and the host's color on finalize.
	ColorID     int8
	Instruction SmartGroupInstruction
	UID         uint16 // modes.smart_grp_uid, randint(1, 65534) per session
	TimeoutMs   uint16 // time left in the host's window; 0 on finalize or abandon
	Members     []SmartGroupMember
}

// SmartGroupMember is one 16-byte record after the header: the MAC, then
// '<ffbb'. It is also the wire layout.
type SmartGroupMember struct {
	MAC          MAC
	Lat, Lon     float32
	PosAccuracyM int8
	ColorID      int8
}

// smartGroupWire is '<BB' plus '<ffbbbHbbH' packed at offset 4.
//
// Reserved18 always holds 8, a constant gen_auto_bond_grp stores in a local
// before the pack. Parser._smart_group unpacks it and never reads it. It
// equals max_bonds, the group size limit, but no code ties the two.
type smartGroupWire struct {
	Category, Command    uint8
	Lat, Lon             float32
	PAcc, ColorID, Instr int8
	UID                  uint16
	MemberCount          int8
	Reserved18           int8
	TimeoutMs            uint16
}

func (SmartGroup) isMessage() {}

// MarshalBinary encodes the beacon: 21 bytes plus 16 per member.
func (g SmartGroup) MarshalBinary() ([]byte, error) {
	if len(g.Members) > MaxSmartGroupMembers {
		return nil, fmt.Errorf("mesh: %d smart group members, a frame holds %d", len(g.Members), MaxSmartGroupMembers)
	}
	b, err := appendWire(make([]byte, 0, smartGroupHeaderLen+smartGroupMemberLen*len(g.Members)), smartGroupWire{
		Category: CatSmartGroup, Command: 0,
		Lat: g.Lat, Lon: g.Lon, PAcc: g.PosAccuracyM, ColorID: g.ColorID, Instr: int8(g.Instruction),
		UID: g.UID, MemberCount: int8(len(g.Members)), Reserved18: smartGroupSlots, TimeoutMs: g.TimeoutMs,
	})
	if err != nil {
		return nil, err
	}
	for _, m := range g.Members {
		if b, err = binary.Append(b, binary.LittleEndian, m); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func parseSmartGroup(b []byte) (Message, error) {
	var w smartGroupWire
	if err := readAt(b, 2, &w); err != nil {
		return nil, err
	}
	if w.MemberCount < 0 {
		return nil, fmt.Errorf("mesh: smart group member count %d", w.MemberCount)
	}
	g := SmartGroup{
		Lat: w.Lat, Lon: w.Lon, PosAccuracyM: w.PAcc, ColorID: w.ColorID,
		Instruction: SmartGroupInstruction(w.Instr), UID: w.UID, TimeoutMs: w.TimeoutMs,
	}
	for i := range int(w.MemberCount) {
		var m SmartGroupMember
		if err := readAt(b, smartGroupHeaderLen+smartGroupMemberLen*i, &m); err != nil {
			return nil, err
		}
		g.Members = append(g.Members, m)
	}
	return g, nil
}

// SmartGroupReply is the (7,1) frame a Totem in pairing mode broadcasts to
// join a host's group (twice, 200 ms apart, per beacon heard) or to leave
// it (Parser._smart_group).
type SmartGroupReply struct {
	UID          uint16 // the host's group UID
	Leave        bool   // join_flag -1; +1 joins
	Lat, Lon     float32
	PosAccuracyM int8
}

// SmartGroupReplyLen is the size of a (7,1) frame.
const SmartGroupReplyLen = 16

// smartGroupReplyWire is '<BB' plus '<Hbffb' packed at offset 4.
type smartGroupReplyWire struct {
	Category, Command uint8
	UID               uint16
	JoinFlag          int8
	Lat, Lon          float32
	PAcc              int8
}

func (SmartGroupReply) isMessage() {}

// MarshalBinary encodes the 16-byte frame.
func (r SmartGroupReply) MarshalBinary() ([]byte, error) {
	join := int8(1)
	if r.Leave {
		join = -1
	}
	return appendWire(make([]byte, 0, SmartGroupReplyLen), smartGroupReplyWire{
		Category: CatSmartGroup, Command: 1, UID: r.UID, JoinFlag: join, Lat: r.Lat, Lon: r.Lon, PAcc: r.PosAccuracyM,
	})
}

func parseSmartGroupReply(b []byte) (Message, error) {
	var w smartGroupReplyWire
	if err := readAt(b, 2, &w); err != nil {
		return nil, err
	}
	return SmartGroupReply{UID: w.UID, Leave: w.JoinFlag < 0, Lat: w.Lat, Lon: w.Lon, PosAccuracyM: w.PAcc}, nil
}
