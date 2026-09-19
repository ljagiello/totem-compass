// Package protocol implements the Totem Compass BLE application protocol
// (firmware v5.0.3) as spoken by the official phone app.
//
// The Totem is a GATT peripheral exposing one service with three
// characteristics (the third, on-demand, is new in 5.0.3 and only carries
// file uploads). Every message starts with a (cat_id, cmd_id) byte pair
// followed by a little-endian, struct-packed payload. The layouts were
// recovered from the frozen MicroPython bytecode (ble_manager.py,
// ble_core.py) and are unchanged from 5.0.2; firmware 4.1.3 uses the same
// Live/Static layouts. See docs/reference/building-a-client.
package protocol

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// GATT identifiers.
const (
	ServiceUUID      = "7913b588-0000-4635-b066-baa2cfc197cf"
	ConnStatusUUID   = "7913b588-0001-4635-b066-baa2cfc197cf"
	DataTransferUUID = "7913b588-0002-4635-b066-baa2cfc197cf"
	// OnDemandUUID (5.0.3+) streams file-upload chunks to apps that connect
	// with ReadyWithUploads. Clients that don't upload can ignore it.
	OnDemandUUID = "7913b588-0003-4635-b066-baa2cfc197cf"
)

// DataBufferSize is the GATT value buffer of the data characteristic; a
// single write must not exceed it.
const DataBufferSize = 185

// Channel identifies which characteristic a frame travels on.
type Channel int

const (
	// ConnStatus is characteristic ...-0001: handshake, runtime state and
	// half-duplex TX handoff (recv_status_msgs on the device).
	ConnStatus Channel = iota
	// Data is characteristic ...-0002: application data (recv_data_msgs).
	Data
)

func (c Channel) String() string {
	if c == ConnStatus {
		return "conn_status"
	}
	return "data"
}

// Category ids used on the data characteristic.
const (
	CatStaticData  = 0x01
	CatWiFi        = 0x02
	CatLiveData    = 0x03
	CatOTA         = 0x04
	CatOptions     = 0x05
	CatPeer        = 0x06
	CatCompassPref = 0x07
	CatDevOptions  = 0x0a
	CatPhone       = 0x0c
	CatExecute     = 0x0e
)

// Category ids used on the conn-status characteristic.
const (
	CatConn       = 0x00
	CatAppRuntime = 0x03
	CatHandoff    = 0x04
)

// Frame schema ids announced in the Ready frame. They select one of two
// device transmit loops in ble_manager.py:
//
//   - SchemaLegacy (0): send_data, full duplex. The device pushes records
//     with gatts_write(send_update=True), i.e. plain notifications, and
//     repeats Static Data / WiFi / Peer Sync until the app acknowledges them
//     with a data frame. Works with every BLE central.
//   - any non-zero id: send_data_v2, half duplex with an explicit TX
//     handoff. The device uses gatts_indicate and treats a record as lost
//     if the ATT confirmation does not arrive within 500 ms. CoreBluetooth
//     (macOS/iOS) subscribes these characteristics for notifications only
//     and does not confirm the indications in time, so this mode stalls on
//     Apple platforms. 72 is the newest EXTENDED schema in espnow_conn_v2.
const (
	SchemaLegacy   = 0
	SchemaExtended = 72
)

// MAC is a 6-byte Totem hardware address.
type MAC [6]byte

// String formats the MAC the way the firmware keys its peer table:
// lowercase hex without separators (binascii.hexlify).
func (m MAC) String() string { return hex.EncodeToString(m[:]) }

// Pretty formats the MAC as colon-separated uppercase hex.
func (m MAC) Pretty() string {
	var b strings.Builder
	for i, x := range m {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", x)
	}
	return b.String()
}

// MarshalText encodes the MAC as in String, for JSON output.
func (m MAC) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// ParseMAC accepts "a1b2c3d4e5f6", "A1:B2:C3:D4:E5:F6" or "a1-b2-...".
func ParseMAC(s string) (MAC, error) {
	var m MAC
	clean := strings.NewReplacer(":", "", "-", "", " ", "").Replace(s)
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != 6 {
		return m, fmt.Errorf("invalid MAC %q: want 6 hex bytes", s)
	}
	copy(m[:], b)
	return m, nil
}

// RGB is a colour as used by the halo/crystal LEDs.
type RGB struct{ R, G, B uint8 }

func (c RGB) String() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// MarshalText encodes the colour as in String, for JSON output.
func (c RGB) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// ParseRGB accepts "#rrggbb", "rrggbb", "r,g,b" or a palette colour name.
func ParseRGB(s string) (RGB, error) {
	if c, ok := paletteByName[strings.ToLower(s)]; ok {
		return c, nil
	}
	if strings.Contains(s, ",") {
		var c RGB
		if _, err := fmt.Sscanf(s, "%d,%d,%d", &c.R, &c.G, &c.B); err != nil {
			return c, fmt.Errorf("invalid colour %q: %w", s, err)
		}
		return c, nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "#"))
	if err != nil || len(b) != 3 {
		return RGB{}, fmt.Errorf("invalid colour %q: want #rrggbb, r,g,b or a palette name", s)
	}
	return RGB{b[0], b[1], b[2]}, nil
}

// Palette is the firmware's Colors table (project_data.py), indexed by
// color_id.
var Palette = []struct {
	Name string
	RGB  RGB
}{
	{"red", RGB{255, 0, 0}},
	{"orange", RGB{255, 128, 0}},
	{"yellow", RGB{255, 255, 0}},
	{"yl_green", RGB{128, 255, 0}},
	{"green", RGB{0, 255, 0}},
	{"bl_green", RGB{0, 255, 128}},
	{"teal", RGB{0, 255, 255}},
	{"white", RGB{255, 255, 255}},
	{"aqua", RGB{0, 128, 255}},
	{"blue", RGB{0, 0, 255}},
	{"indigo", RGB{128, 0, 255}},
	{"magenta", RGB{255, 0, 255}},
	{"hot_pink", RGB{255, 0, 128}},
}

var paletteByName = func() map[string]RGB {
	m := make(map[string]RGB, len(Palette))
	for _, p := range Palette {
		m[p.Name] = p.RGB
	}
	return m
}()

// ColorName returns the palette name for a color_id, or "color#N".
func ColorName(id int8) string {
	if id >= 0 && int(id) < len(Palette) {
		return Palette[id].Name
	}
	return fmt.Sprintf("color#%d", id)
}

// PowerMode is config.power_mode (compass.change_power_mode).
type PowerMode uint8

// Power modes. PowerUnchanged is also what 4.x firmware reports, as it does
// not fill in the Live Data field.
const (
	PowerUnchanged PowerMode = 0 // leave the current mode alone
	PowerEco       PowerMode = 1 // dim LEDs (brightness 0.1), eco profile
	PowerNormal    PowerMode = 2 // full brightness (0.6)
)

func (p PowerMode) String() string {
	switch p {
	case PowerUnchanged:
		return "unchanged"
	case PowerEco:
		return "eco"
	case PowerNormal:
		return "normal"
	}
	return fmt.Sprintf("mode#%d", uint8(p))
}

// MarshalText encodes the mode as in String, for JSON output.
func (p PowerMode) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// packFlags mirrors f_lib.bitwise.pack_flags: element i sets bit i.
func packFlags(flags ...bool) byte {
	var b byte
	for i, f := range flags {
		if f {
			b |= 1 << i
		}
	}
	return b
}

func bit(b byte, i uint) bool { return b&(1<<i) != 0 }
