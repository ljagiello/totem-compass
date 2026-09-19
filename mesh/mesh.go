// Package mesh encodes and decodes the ESP-NOW frames Totems exchange with
// each other (the "Unity Mesh"), as firmware 5.0.3 builds them in
// espnow_conn_v2.Messages and Parser.
//
// Every frame starts with the SyncWord A7 74 and the (category, command)
// pair. Offsets in this package count from the SyncWord, as the firmware's
// pack_into calls do. Integers are little-endian. There is no checksum,
// encryption or authentication.
//
// The radio settings a peer must match are in [Channel], [Protocol] and
// [PHYRate].
package mesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ljagiello/totem-compass/protocol"
)

// Radio settings from EspConn.power_on: WLAN.config(channel=6,
// protocol=MODE_LR) and ESPNow.config(rate=41).
const (
	Channel  = 6
	Protocol = 0x08 // WIFI_PROTOCOL_LR, Espressif Long Range only
	PHYRate  = 0x29 // WIFI_PHY_RATE_LORA_250K
)

// SyncWord opens every frame (project_data b'\xa7t').
var SyncWord = [2]byte{0xa7, 0x74}

// Broadcast is the ESP-NOW broadcast address.
var Broadcast = protocol.MAC{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// MaxFrame is the largest ESP-NOW payload (ESP_NOW_MAX_DATA_LEN).
const MaxFrame = 250

// Categories (frame offset 2).
const (
	CatPeer       = 0
	CatDemiGod    = 1
	CatMesh       = 2
	CatSmartGroup = 7
)

// MAC is a Totem's ESP-NOW (WiFi station) address.
type MAC = protocol.MAC

// ParseMAC accepts "a1b2c3d4e5f6", "A1:B2:C3:D4:E5:F6" or "a1-b2-...".
func ParseMAC(s string) (MAC, error) { return protocol.ParseMAC(s) }

// Message is a decoded frame. MarshalBinary returns the frame as a Totem
// puts it on the air, SyncWord included.
type Message interface {
	MarshalBinary() ([]byte, error)
	isMessage()
}

// ErrNotTotem means the frame does not start with the SyncWord, the only
// check a Totem makes before it looks at the category (Parser.aread:
// "Invalid ESP-NOW message SyncWord or length").
var ErrNotTotem = errors.New("mesh: not a Totem frame")

// Parse decodes one ESP-NOW payload. Frames with a known SyncWord but an
// unknown category come back as [Unknown].
func Parse(b []byte) (Message, error) {
	if len(b) < 4 || !bytes.HasPrefix(b, SyncWord[:]) {
		return nil, ErrNotTotem
	}
	switch cat, cmd := b[2], b[3]; {
	case cat == CatPeer:
		return parsePeer(b)
	case cat == CatMesh && cmd == 0:
		return parseLocate(b)
	case cat == CatSmartGroup && cmd == 0:
		return parseSmartGroup(b)
	case cat == CatSmartGroup && cmd == 1:
		return parseSmartGroupReply(b)
	case cat == CatDemiGod:
		return DemiGod{Command: cmd, Payload: bytes.Clone(b[4:])}, nil
	default:
		return Unknown{Category: cat, Command: cmd, Payload: bytes.Clone(b[4:])}, nil
	}
}

// DemiGod is a category 1 broadcast: an administrator command (OTA update,
// nav-log rule, find device, power control, venue code). This package only
// recognizes them; it has no encoder for their payloads.
type DemiGod struct {
	Command uint8
	Payload []byte
}

// Unknown is a frame with the SyncWord and an unrecognized category or
// command.
type Unknown struct {
	Category, Command uint8
	Payload           []byte
}

func (DemiGod) isMessage() {}
func (Unknown) isMessage() {}

// MarshalBinary returns the frame unchanged.
func (d DemiGod) MarshalBinary() ([]byte, error) {
	return frame(CatDemiGod, d.Command, d.Payload)
}

// MarshalBinary returns the frame unchanged.
func (u Unknown) MarshalBinary() ([]byte, error) {
	return frame(u.Category, u.Command, u.Payload)
}

func frame(cat, cmd uint8, payload []byte) ([]byte, error) {
	b := append([]byte{SyncWord[0], SyncWord[1], cat, cmd}, payload...)
	if len(b) > MaxFrame {
		return nil, fmt.Errorf("mesh: (%d,%d) frame is %d bytes, ESP-NOW carries %d", cat, cmd, len(b), MaxFrame)
	}
	return b, nil
}

// readAt decodes the fixed-size v from b at offset off.
func readAt(b []byte, off int, v any) error {
	n := binary.Size(v)
	if len(b) < off+n {
		return fmt.Errorf("mesh: (%d,%d) frame is %d bytes, need %d", b[2], b[3], len(b), off+n)
	}
	_, err := binary.Decode(b[off:off+n], binary.LittleEndian, v)
	return err
}

// appendWire appends the SyncWord and the fixed-size wire struct w, whose
// first two fields are the category and command.
func appendWire(b []byte, w any) ([]byte, error) {
	return binary.Append(append(b, SyncWord[:]...), binary.LittleEndian, w)
}
