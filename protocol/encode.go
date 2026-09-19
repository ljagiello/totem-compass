package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Frame is one app→device message and the characteristic it is written to.
type Frame struct {
	Channel Channel
	Bytes   []byte
}

func connFrame(b ...byte) Frame { return Frame{ConnStatus, b} }

func dataFrame(cat, cmd byte, payload ...any) (Frame, error) {
	var buf bytes.Buffer
	buf.WriteByte(cat)
	buf.WriteByte(cmd)
	for _, p := range payload {
		var err error
		switch v := p.(type) {
		case []byte:
			_, err = buf.Write(v)
		case string:
			_, err = buf.WriteString(v)
		default:
			err = binary.Write(&buf, binary.LittleEndian, v)
		}
		if err != nil {
			return Frame{}, err
		}
	}
	if buf.Len() > DataBufferSize {
		return Frame{}, fmt.Errorf("(%d,%d) frame is %d bytes, device buffer is %d", cat, cmd, buf.Len(), DataBufferSize)
	}
	return Frame{Data, buf.Bytes()}, nil
}

func mustData(cat, cmd byte, payload ...any) Frame {
	f, err := dataFrame(cat, cmd, payload...)
	if err != nil {
		panic(err) // only reachable with fixed-size payloads, i.e. a programming error
	}
	return f
}

// ---- conn-status characteristic -------------------------------------------

// Ready is the ConnStatus-Ready frame [0x00, 0x01, conn_mode, schema].
// conn_mode 1 resets the device's pending static/peer requests and makes it
// the TX owner; schema picks the transmit loop (see SchemaLegacy). Since
// 5.0.3 the device drops a link that has not sent Ready within 15 s.
func Ready(schema byte) Frame { return connFrame(CatConn, 0x01, 0x01, schema) }

// ReadyWithUploads is Ready plus the 5.0.3 capability byte (bit0: the app
// accepts file uploads). Only send it from a client that reads the on-demand
// characteristic and answers the uploader's headers with (2,3) frames on
// conn-status; with plain Ready the device suspends uploads.
func ReadyWithUploads(schema byte) Frame { return connFrame(CatConn, 0x01, 0x01, schema, 0x01) }

// DisconnectRequest asks the device to treat the next disconnect as graceful.
func DisconnectRequest() Frame { return connFrame(CatConn, 0x03) }

// RuntimeState is the app's UI state as reported to the device.
type RuntimeState struct {
	Active   bool // app in foreground/active
	Focused  bool // app window focused
	Locked   bool // phone locked
	UIClosed bool // app UI closed (service only)
	Service  bool // running as background service
	// AllowDisconnect lets the device drop BLE on its schedule and reconnect
	// later (modes.is_ble_auto_reconn).
	AllowDisconnect bool
}

// AppRuntime reports the app runtime bitfield [0x03, 0x00, bits].
func AppRuntime(s RuntimeState) Frame {
	return connFrame(CatAppRuntime, 0x00,
		packFlags(s.Active, s.Focused, s.Locked, s.UIClosed, s.Service, s.AllowDisconnect))
}

// GrantTX hands the half-duplex transmit window back to the device
// ([0x04, 0x03, bit1]).
func GrantTX() Frame { return connFrame(CatHandoff, 0x03, 0x02) }

// RevokeTX takes the half-duplex transmit window away from the device
// ([0x04, 0x03, bit0]).
func RevokeTX() Frame { return connFrame(CatHandoff, 0x03, 0x01) }

// ---- data characteristic ---------------------------------------------------

// RequestStaticData asks the device to send its Static Data record (1,2).
func RequestStaticData() Frame { return mustData(CatStaticData, 0x01) }

// AckStaticData tells the device the Static Data record was received.
func AckStaticData() Frame { return mustData(CatStaticData, 0x00) }

// RequestPeerSync asks for the bonded-peer MAC list (6,7).
func RequestPeerSync() Frame { return mustData(CatPeer, 0x01) }

// RequestPeerDetails asks for a Peer Ping (6,2) for the given peers, or for
// every bonded peer when none are given.
func RequestPeerDetails(peers ...MAC) (Frame, error) {
	if len(peers) == 0 {
		return mustData(CatPeer, 0x08), nil
	}
	payload := []any{uint8(len(peers)), uint8(0)}
	for _, m := range peers {
		payload = append(payload, m[:])
	}
	return dataFrame(CatPeer, 0x08, payload...)
}

// PeerUpdate changes how a bonded peer is shown on the ring, or deletes it.
type PeerUpdate struct {
	MAC    MAC
	Color  RGB
	Hidden bool
	Delete bool
}

// UpdatePeer builds (6,3): [mac(6), r, g, b, flags(bit0 delete, bit2 hidden)].
func UpdatePeer(u PeerUpdate) Frame {
	return mustData(CatPeer, 0x03, u.MAC[:], u.Color.R, u.Color.G, u.Color.B,
		packFlags(u.Delete, false, u.Hidden))
}

// SelectPeer opens the on-device peer-management UI with the given peer
// selected (6,5).
func SelectPeer(m MAC) Frame { return mustData(CatPeer, 0x05, m.String()) }

// StopPeerManagement closes the on-device peer-management UI (6,0).
func StopPeerManagement() Frame { return mustData(CatPeer, 0x00) }

// POI is a navigation target pushed from the app (a "pin" on the compass).
type POI struct {
	ID       MAC // key in the device's peer table; must not collide with a real Totem
	Name     string
	Lat, Lon float32
	Color    RGB
	// Sticky keeps the heading to this point on the ring.
	Sticky bool
	Hidden bool
	Locked bool
}

// poiWire is '<ffbBBBhiiBBbbbhhiiib' as unpacked by ble_manager.add_new_bond
// and written by app 2.3.0's genNewBondPayload.
//
// Azimuth is what the app writes at index 6, the slot gen_peer_ping fills
// with peer_azimuth; add_new_bond reads it into a local it never uses. The
// Reserved fields, named by frame offset, are written as 0 by the app and
// never read by any published firmware (3.2.12 to 5.0.3).
type poiWire struct {
	Lat, Lon                           float32
	PAcc                               int8
	R, G, B                            uint8
	Azimuth                            int16
	Eat, Nbt                           int32
	AnimationID                        uint8
	Flags                              uint8 // sos, is_poi, is_sticky_heading, is_hidden, is_locked
	Reserved33, Reserved34, Reserved35 int8
	Reserved36, Reserved38             int16
	Reserved40, Reserved44, Reserved48 int32
	NameLen                            int8
}

// poiHeader is the POI frame up to its name: cat, cmd, length, id, poiWire.
const poiHeader = 53

// maxPOIName is the longest name add_new_bond can read: its length is a
// signed byte. (The device buffer would hold DataBufferSize-poiHeader = 132.)
const maxPOIName = 127

// AddPOI builds (6,6), which registers a point of interest the compass can
// point to. The device stores it in its peer table under p.ID. Byte 2 is the
// frame length, as the app writes it; the firmware does not check it.
func AddPOI(p POI) (Frame, error) {
	name := []byte(p.Name)
	if len(name) > maxPOIName {
		return Frame{}, fmt.Errorf("POI name is %d bytes, max %d", len(name), maxPOIName)
	}
	w := poiWire{
		Lat: p.Lat, Lon: p.Lon, PAcc: 0,
		R: p.Color.R, G: p.Color.G, B: p.Color.B,
		Flags:   packFlags(false, true, p.Sticky, p.Hidden, p.Locked),
		NameLen: int8(len(name)),
	}
	return dataFrame(CatPeer, 0x06, uint8(poiHeader+len(name)), p.ID[:], w, name)
}

// PeerLocation feeds a bonded peer's position obtained out-of-band (the app
// relays these from the cloud) into the device (6,9).
type PeerLocation struct {
	MAC      MAC
	Unix     int32 // fix time; ignored unless newer than what the device has
	Lat, Lon float32
	PAcc     int8
	SpeedKPH int8
	SOS      bool
}

// SetPeerLocation builds (6,9): [0, mac(6), '<iff3bB'].
func SetPeerLocation(l PeerLocation) Frame {
	return mustData(CatPeer, 0x09, uint8(0), l.MAC[:], l.Unix, l.Lat, l.Lon,
		l.PAcc, int8(0), l.SpeedKPH, packFlags(l.SOS))
}

// CompassPrefs is the settings block the app writes as (7,3).
type CompassPrefs struct {
	PersistentNorth bool
	CompassLock     bool
	PeerBlink       bool
	PowerMode       PowerMode // PowerUnchanged keeps the current mode
}

// SetCompassPrefs builds (7,3): [0, flags(north, lock), flags(_, _, blink), 0, power_mode].
// App 2.3.0 puts the crystal colour id in the first byte and more flags in
// the fourth; handle_compass_pref reads neither.
func SetCompassPrefs(p CompassPrefs) Frame {
	return mustData(CatCompassPref, 0x03, int8(0),
		packFlags(p.PersistentNorth, p.CompassLock),
		packFlags(false, false, p.PeerBlink),
		uint8(0),
		uint8(p.PowerMode)&0x07)
}

// MaxNameLen is the longest device name, in UTF-16 code units: the limit of
// the official app's rename field. That is at most 96 UTF-8 bytes, so the
// name fits the signed length byte Static Data reports it with.
const MaxNameLen = 32

// CleanName returns name as the device will store it (update_options saves
// name.strip()), or an error for a name the device could not report back
// in Static Data. The device takes any length, and a name longer than 127
// bytes would make its Static Data unreadable.
func CleanName(name string) (string, error) {
	name = strings.Trim(name, " \t\n\v\f\r") // MicroPython's str.strip()
	units := 0
	for _, r := range name {
		units += utf16.RuneLen(r)
	}
	switch {
	case name == "":
		return "", errors.New("name must not be empty")
	case !utf8.ValidString(name):
		return "", errors.New("name must be valid UTF-8")
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", errors.New("name must not contain control characters")
	case units > MaxNameLen:
		return "", fmt.Errorf("name is %d characters, max %d", units, MaxNameLen)
	}
	return name, nil
}

// SetName renames the Totem (5,3) to CleanName(name). The device stores it
// in user-config.json and reports it in Static Data.
func SetName(name string) (Frame, error) {
	name, err := CleanName(name)
	if err != nil {
		return Frame{}, err
	}
	b, err := jsonPayload(map[string]string{"name": name})
	if err != nil {
		return Frame{}, err
	}
	return dataFrame(CatOptions, 0x03, b)
}

// jsonPayload encodes v for the device's json.loads. HTML escaping is off:
// it would spend six of the few frame bytes on every &, < and >.
func jsonPayload(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ScanWiFi asks the device to scan for WiFi networks (2,1); the result
// arrives as a WiFiNetworks message.
func ScanWiFi() Frame { return mustData(CatWiFi, 0x01) }

// ClearWiFiScan makes the device forget its cached scan result (2,0), so the
// next ScanWiFi rescans.
func ClearWiFiScan() Frame { return mustData(CatWiFi, 0x00) }

// SaveWiFi stores the WiFi network used for firmware updates (2,3).
func SaveWiFi(ssid, key string) (Frame, error) {
	if ssid == "" || len(ssid) > 32 {
		return Frame{}, fmt.Errorf("ssid must be 1-32 bytes, not %d", len(ssid))
	}
	b, err := jsonPayload(map[string]string{"nw": ssid, "join": key})
	if err != nil {
		return Frame{}, err
	}
	return dataFrame(CatWiFi, 0x03, b)
}

// PhoneFix is the phone's GNSS fix and clock, used by the device when it has
// no fix of its own (gnss_data.phone_*).
type PhoneFix struct {
	Lat, Lon float32
	HAcc     float64 // horizontal accuracy in meters
	Unix     int32
	UnixMS   int16
	// Internet (5.0.3+) tells the device the phone is online, one of the
	// gates for pushing log uploads (modes.ble_ticks_app_internet).
	Internet bool
	UIClosed bool
	Focused  bool
}

// SendPhoneFix builds (12,3): '<ffbBih' = lat, lon, h_acc, flags, unix, unix_ms.
func SendPhoneFix(f PhoneFix) Frame {
	hacc := int8(math.Min(math.Max(math.Round(f.HAcc), -128), 127))
	return mustData(CatPhone, 0x03, f.Lat, f.Lon, hacc,
		packFlags(f.Internet, f.UIClosed, f.Focused), f.Unix, f.UnixMS)
}

// OTARequest asks the device to reboot and update itself over WiFi (4,cmd).
// It requires a saved WiFi network and a charged battery.
type OTARequest struct {
	Cmd        int8   // ota_cmd; the app daemon default is 1
	Branch     string // S3 branch dir, e.g. "totem"
	Version    string // release code or "latest"
	EndpointID int16
}

// StartOTA builds (4,0): '<bBbbbh' header then the branch and version strings.
func StartOTA(r OTARequest) (Frame, error) {
	if len(r.Branch) > 127 || len(r.Version) > 127 {
		return Frame{}, fmt.Errorf("branch/version too long")
	}
	return dataFrame(CatOTA, 0x00, r.Cmd, uint8(0), int8(len(r.Branch)), int8(len(r.Version)),
		int8(0), r.EndpointID, r.Branch, r.Version)
}
