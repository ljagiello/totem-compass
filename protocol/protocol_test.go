package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Golden vectors were produced with CPython's struct module using the exact
// format strings recovered from the bytecode (see gen_* in ble_manager.py /
// ble_core.py and the recv_data_msgs handlers); the layouts are identical in
// v5.0.2 and v5.0.3.
var golden = map[string]string{
	"live":          "030109000020407b000000000016420080f4c2000078400601630c038015966a0401030f012d0011393000000000000000000000ff100e00002a00000000000000020000aa0057",
	"static":        "01023aa1b2c3d4e5f64f2a0000004f010500020c01010300000000000000000000000000000000000605044c756b61737a746f74656d686f6d65",
	"peer_ping":     "060238a1b2c3d4e5f602000051420000584105ffb40005ff0080000003c40708090a2b0200008015966affff010200007040457761404f01",
	"peer_sync":     "06071002a1b2c3d4e5f60102030405ff",
	"handoff":       "040202000000000000000000",
	"wifi":          "02025b22686f6d65222c202263616665225d",
	"chunk":         "020200010100020000000000000010000000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f0c6576656e74732d312e62696e00",
	"update_peer":   "0603a1b2c3d4e5f60a141e04",
	"add_poi":       "06063fa1b2c3d4e5f600004a420000884000ff8000000000000000000000000006000000000000000000000000000000000000000a4d61696e205374616765",
	"peer_location": "060900a1b2c3d4e5f68015966a008040420000384107001e01",
	"compass_prefs": "07030003040002",
	"phone_fix":     "0c0300002342000093c20c048015966afa00",
	"ota":           "040001000506000100746f74656d6c6174657374",
}

// realFrames were captured from a real Totem over BLE (totemctl -trace).
var realFrames = map[string]string{
	"static 5.0.3": "0102428c94df7b047800c000000053010500030800010000000000000000000000000000000000000a05084c43467320746f74656d746f74656d446f6d656b5f3547",
	"live 5.0.3":   "030114e17a8440ffffffffde581642b903f4c202f1844006fa000000e4d3ad6a0802025701850001bf2a00000000000000000000ffde000000c0000000000000000000000c0064",
	"static 4.1.3": "01023c8c94df7b047800b000000008010401030500000000000000000000000000000000000000000c05006d79746f74656d5f30343738746f74656d",
	"live 4.1.3":   "030114b072c83f0f000000c8581642c403f4c2f1638840060000000083cead6a060201ffff570000760b00000000000000000000fff7280000b000000000000000000000080064",
}

var testMAC = MAC{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6}

// mustHex returns a golden vector or a captured real frame by name.
func mustHex(t *testing.T, name string) []byte {
	t.Helper()
	h, ok := golden[name]
	if !ok {
		h, ok = realFrames[name]
	}
	if !ok {
		t.Fatalf("no test frame %q", name)
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWireSizesMatchFirmwareFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want int // struct.calcsize of the firmware format string
	}{
		{"<bfi3fb4Bi3b2hbiffb3ibHBBb", liveDataWire{}, 69},
		{"<biHBBBbBBBbhhiiibbb", staticDataWire{}, 34},
		{"<ffbbh4BHbb4BiihBbf", peerPingWire{}, 40},
		{"<ffbBBBhiiBBbbbhhiiib", poiWire{}, 44},
	} {
		if got := binary.Size(tc.v); got != tc.want {
			t.Errorf("%s: Go wire struct is %d bytes, firmware format is %d", tc.name, got, tc.want)
		}
	}
}

// TestRealDeviceFrames checks captured frames against the values the device
// reported at the time.
func TestRealDeviceFrames(t *testing.T) {
	t.Run("static 5.0.3", func(t *testing.T) {
		m, err := Parse(Data, mustHex(t, "static 5.0.3"))
		if err != nil {
			t.Fatal(err)
		}
		want := StaticData{MAC: MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78}, ReleaseID: 339, Version: "5.0.3",
			Age: 192, ColorID: 8, HalfDuplex: true, Name: "LCFs totem", Branch: "totem", WiFiSSID: "Domek_5G"}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("got  %+v\nwant %+v", m, want)
		}
	})
	t.Run("live 5.0.3", func(t *testing.T) {
		m, err := Parse(Data, mustHex(t, "live 5.0.3"))
		if err != nil {
			t.Fatal(err)
		}
		d := m.(LiveData)
		if d.SatCount != 20 || d.Azimuth != 133 || d.BattPct != 100 || d.PowerMode != PowerNormal || d.Channel != 6 {
			t.Errorf("fields: %+v", d)
		}
		if d.Uptime != 222*time.Second || !d.HasLocation || !d.FullBright || d.Charging {
			t.Errorf("uptime/flags: %+v", d)
		}
		if d.Lat < 37.5867 || d.Lat > 37.5868 || d.Lon > -122.0072 || d.Lon < -122.0073 {
			t.Errorf("position %v,%v", d.Lat, d.Lon)
		}
	})
	t.Run("static 4.1.3", func(t *testing.T) {
		m, err := Parse(Data, mustHex(t, "static 4.1.3"))
		if err != nil {
			t.Fatal(err)
		}
		// 4.1.3 has no half-duplex loop and leaves the capability bit clear.
		if s := m.(StaticData); s.Version != "4.1.3" || s.ReleaseID != 264 || s.Name != "mytotem_0478" || s.ColorID != 5 || s.HalfDuplex {
			t.Errorf("got %+v", s)
		}
	})
	t.Run("live 4.1.3 leaves power mode unset", func(t *testing.T) {
		m, err := Parse(Data, mustHex(t, "live 4.1.3"))
		if err != nil {
			t.Fatal(err)
		}
		if d := m.(LiveData); d.PowerMode != PowerUnchanged || d.Uptime != 10487*time.Second || d.Odometer != 2934 {
			t.Errorf("got %+v", d)
		}
	})
}

func TestParseLiveData(t *testing.T) {
	m, err := Parse(Data, mustHex(t, "live"))
	if err != nil {
		t.Fatal(err)
	}
	d := m.(LiveData)
	if d.SatCount != 9 || *d.PosAccuracyM != 2.5 || *d.AltitudeM != 123 {
		t.Errorf("gnss fields: %+v", d)
	}
	if d.Lat != 37.5 || d.Lon != -122.25 || d.BattVolts != 3.875 || d.BattPct != 87 {
		t.Errorf("position/battery: %+v", d)
	}
	if d.Channel != 6 || d.PowerMode != PowerEco || d.PowerLevel != 2 || d.MaxHopCnt != 99 {
		t.Errorf("radio/power: %+v", d)
	}
	if !d.Time.Equal(time.Unix(1788220800, 0)) || d.Uptime != time.Hour || d.Age != 42 {
		t.Errorf("time fields: %+v", d)
	}
	if d.ColorID != 4 || d.HeadingMot != 271 || d.Azimuth != 45 || *d.SpeedKPH != 17 || d.Odometer != 12345 {
		t.Errorf("nav fields: %+v", d)
	}
	wantFlags := [7]bool{false, true, false, true, false, true, true}
	gotFlags := [7]bool{d.SOS, d.Eco, d.FullBright, d.HasLocation, d.LowBattery, d.Charging, d.MagCalNeeded}
	if gotFlags != wantFlags {
		t.Errorf("flags = %v, want %v", gotFlags, wantFlags)
	}
}

func TestParseStaticData(t *testing.T) {
	m, err := Parse(Data, mustHex(t, "static"))
	if err != nil {
		t.Fatal(err)
	}
	want := StaticData{
		MAC: testMAC, ReleaseID: 335, Version: "5.0.2", Age: 42, ColorID: 12,
		PersistentNorth: true, HalfDuplex: true, ServiceID: 3, Name: "Lukasz", Branch: "totem", WiFiSSID: "home",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("got  %+v\nwant %+v", m, want)
	}
}

func TestParsePeerPing(t *testing.T) {
	m, err := Parse(Data, mustHex(t, "peer_ping"))
	if err != nil {
		t.Fatal(err)
	}
	p := m.(PeerPing)
	if p.MAC != testMAC || p.Name != "Ewa" || p.MeshHops != 2 {
		t.Errorf("identity: %+v", p)
	}
	if p.Lat != 52.25 || p.Lon != 13.5 || p.PosAccuracyM != 5 || p.SpeedKPH != -1 || p.Bearing != 180 {
		t.Errorf("position: %+v", p)
	}
	if p.Color != (RGB{255, 0, 128}) || p.RSSI != -60 || p.MsgRx != 7 || p.MeshSendCount != 10 {
		t.Errorf("link: %+v", p)
	}
	if p.LastUpdate != 555 || !p.LastCoords.Equal(time.Unix(1788220800, 0)) || p.DistanceDiff != -1 {
		t.Errorf("timing: %+v", p)
	}
	if !p.SOS || p.POI || !p.ViaMesh || !p.Hidden || p.Locked || p.Orientation != 2 || p.Volts != 3.75 {
		t.Errorf("flags: %+v", p)
	}
	if p.BattPct != 64 || p.ReleaseID != 335 {
		t.Errorf("tail: %+v", p)
	}
}

func TestParsePeerSync(t *testing.T) {
	m, err := Parse(Data, mustHex(t, "peer_sync"))
	if err != nil {
		t.Fatal(err)
	}
	want := PeerSync{Peers: []MAC{testMAC, {1, 2, 3, 4, 5, 0xff}}}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("got %+v, want %+v", m, want)
	}
}

func TestParseConnStatus(t *testing.T) {
	m, err := Parse(ConnStatus, mustHex(t, "handoff"))
	if err != nil {
		t.Fatal(err)
	}
	if m != (Handoff{ToApp: true}) {
		t.Errorf("handoff: got %+v", m)
	}
	m, err = Parse(ConnStatus, []byte{0, 5, 0x10, 0x27, 0, 0, 0x20, 0x4e, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if m != (DisconnectIntent{Cmd: 5, Params: [2]int32{10000, 20000}}) {
		t.Errorf("disconnect intent: got %+v", m)
	}
}

func TestParseWiFiAndChunkShareHeader(t *testing.T) {
	m, err := Parse(Data, mustHex(t, "wifi"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, WiFiNetworks{SSIDs: []string{"home", "cafe"}}) {
		t.Errorf("wifi: got %+v", m)
	}
	m, err = Parse(Data, mustHex(t, "chunk"))
	if err != nil {
		t.Fatal(err)
	}
	c := m.(FileChunk)
	if c.FileID != 0x0100 || c.Status != 1 || c.FileType != 2 || c.FileSize != 4096 || c.Name != "events-1.bin" || len(c.SHA256) != 32 {
		t.Errorf("chunk: %+v", c)
	}
	if c.LastChunk || c.FromCompass {
		t.Errorf("5.0.2 header has a zero flags byte: %+v", c)
	}
	// 5.0.3 packs pack_flags(is_last_chunk, is_origin_compass) into byte 17.
	b := mustHex(t, "chunk")
	b[17] = 0x03
	m, err = Parse(Data, b)
	if err != nil {
		t.Fatal(err)
	}
	if c := m.(FileChunk); !c.LastChunk || !c.FromCompass {
		t.Errorf("5.0.3 flags not decoded: %+v", c)
	}
}

func TestParseRejectsTruncatedFrames(t *testing.T) {
	for _, name := range []string{"live", "static", "peer_ping", "peer_sync"} {
		b := mustHex(t, name)
		if _, err := Parse(Data, b[:len(b)-1]); err == nil {
			t.Errorf("%s: truncated frame parsed without error", name)
		}
	}
	if m, err := Parse(Data, []byte{0x09, 0x09, 1}); err != nil || m.(Unknown).Cat != 9 {
		t.Errorf("unknown frame: %v %v", m, err)
	}
}

func TestEncoders(t *testing.T) {
	poi, err := AddPOI(POI{ID: testMAC, Name: "Main Stage", Lat: 50.5, Lon: 4.25, Color: RGB{255, 128, 0}, Sticky: true})
	if err != nil {
		t.Fatal(err)
	}
	ota, err := StartOTA(OTARequest{Cmd: 1, Branch: "totem", Version: "latest", EndpointID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		got  Frame
	}{
		{"update_peer", UpdatePeer(PeerUpdate{MAC: testMAC, Color: RGB{10, 20, 30}, Hidden: true})},
		{"add_poi", poi},
		{"peer_location", SetPeerLocation(PeerLocation{MAC: testMAC, Unix: 1788220800, Lat: 48.125, Lon: 11.5, PAcc: 7, SpeedKPH: 30, SOS: true})},
		{"compass_prefs", SetCompassPrefs(CompassPrefs{PersistentNorth: true, CompassLock: true, PeerBlink: true, PowerMode: PowerNormal})},
		{"phone_fix", SendPhoneFix(PhoneFix{Lat: 40.75, Lon: -73.5, HAcc: 12.4, Unix: 1788220800, UnixMS: 250, Focused: true})},
		{"ota", ota},
	} {
		if tc.got.Channel != Data {
			t.Errorf("%s: sent on %s, want data", tc.name, tc.got.Channel)
		}
		if want := mustHex(t, tc.name); !bytes.Equal(tc.got.Bytes, want) {
			t.Errorf("%s:\n got % x\nwant % x", tc.name, tc.got.Bytes, want)
		}
	}
}

func TestConnStatusFrames(t *testing.T) {
	for _, tc := range []struct {
		got  Frame
		want []byte
	}{
		{Ready(SchemaExtended), []byte{0, 1, 1, 72}},
		{Ready(SchemaLegacy), []byte{0, 1, 1, 0}},
		{ReadyWithUploads(SchemaLegacy), []byte{0, 1, 1, 0, 1}},
		{DisconnectRequest(), []byte{0, 3}},
		{GrantTX(), []byte{4, 3, 2}},
		{RevokeTX(), []byte{4, 3, 1}},
		{AppRuntime(RuntimeState{Active: true, Focused: true, AllowDisconnect: true}), []byte{3, 0, 0x23}},
	} {
		if tc.got.Channel != ConnStatus || !bytes.Equal(tc.got.Bytes, tc.want) {
			t.Errorf("got %s % x, want conn_status % x", tc.got.Channel, tc.got.Bytes, tc.want)
		}
	}
}

func TestPhoneFixInternetBit(t *testing.T) {
	f := SendPhoneFix(PhoneFix{Internet: true, Focused: true})
	if flags := f.Bytes[2+4+4+1]; flags != 0x05 {
		t.Errorf("flags byte = %#x, want 0x05 (internet bit0 | focused bit2)", flags)
	}
}

func TestJSONCommands(t *testing.T) {
	f, err := SetName("Lukasz's Totem")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"Lukasz's Totem"}`; string(f.Bytes[2:]) != want || f.Bytes[0] != 5 || f.Bytes[1] != 3 {
		t.Errorf("SetName = %q", f.Bytes)
	}
	f, err = SaveWiFi("home", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"join":"s3cret","nw":"home"}`; string(f.Bytes[2:]) != want || f.Bytes[0] != 2 || f.Bytes[1] != 3 {
		t.Errorf("SaveWiFi = %q", f.Bytes)
	}
	if _, err := SetName(string(make([]byte, 200))); err == nil {
		t.Error("oversized name accepted")
	}
}

func TestRequestPeerDetails(t *testing.T) {
	all, _ := RequestPeerDetails()
	if !bytes.Equal(all.Bytes, []byte{6, 8}) {
		t.Errorf("all peers = % x", all.Bytes)
	}
	one, _ := RequestPeerDetails(testMAC)
	if want := append([]byte{6, 8, 1, 0}, testMAC[:]...); !bytes.Equal(one.Bytes, want) {
		t.Errorf("one peer = % x, want % x", one.Bytes, want)
	}
}

func TestMACAndColourParsing(t *testing.T) {
	for _, s := range []string{"a1b2c3d4e5f6", "A1:B2:C3:D4:E5:F6", "a1-b2-c3-d4-e5-f6"} {
		m, err := ParseMAC(s)
		if err != nil || m != testMAC {
			t.Errorf("ParseMAC(%q) = %v, %v", s, m, err)
		}
	}
	if testMAC.String() != "a1b2c3d4e5f6" || testMAC.Pretty() != "A1:B2:C3:D4:E5:F6" {
		t.Errorf("MAC formatting: %s %s", testMAC, testMAC.Pretty())
	}
	if _, err := ParseMAC("a1b2c3"); err == nil {
		t.Error("short MAC accepted")
	}
	for s, want := range map[string]RGB{"hot_pink": {255, 0, 128}, "#0a141e": {10, 20, 30}, "10,20,30": {10, 20, 30}} {
		if c, err := ParseRGB(s); err != nil || c != want {
			t.Errorf("ParseRGB(%q) = %v, %v", s, c, err)
		}
	}
}

// Inputs fuzzing found mishandled.
func TestFuzzFindings(t *testing.T) {
	// ParseRGB took "1,2,3x" as 1,2,3: fmt.Sscanf ignored the trailing text.
	for _, s := range []string{"1,2,3x", "1,2", "1,2,3,4", "256,0,0", "-1,0,0", "1,,3"} {
		if c, err := ParseRGB(s); err == nil {
			t.Errorf("ParseRGB(%q) = %v, want an error", s, c)
		}
	}
	if c, err := ParseRGB(" 1, 2 ,003"); err != nil || c != (RGB{1, 2, 3}) {
		t.Errorf("ParseRGB with spaces = %v, %v", c, err)
	}

	// AddPOI accepted names up to 132 bytes, but add_new_bond reads the
	// length as a signed byte: 128 bytes went out as -128.
	if _, err := AddPOI(POI{Name: strings.Repeat("x", 128)}); err == nil {
		t.Error("AddPOI accepted a 128-byte name")
	}
	if f, err := AddPOI(POI{Name: strings.Repeat("x", 127)}); err != nil || int8(f.Bytes[52]) != 127 {
		t.Errorf("AddPOI(127-byte name): %v", err)
	}

	// SetName HTML-escaped &, < and >, so a 29-character name of them
	// overflowed the device buffer.
	f, err := SetName(strings.Repeat("&", 32))
	if err != nil || !bytes.Contains(f.Bytes, []byte(strings.Repeat("&", 32))) {
		t.Errorf("SetName(32 ampersands) = %q, %v", f.Bytes, err)
	}
}

// Findings from the PR review.
func TestReviewFindings(t *testing.T) {
	// A file chunk whose FileID starts with byte '[' is still a chunk, not a
	// malformed WiFi list.
	chunk := mustHex(t, "chunk")
	chunk[2] = '['
	if m, err := Parse(Data, chunk); err != nil {
		t.Errorf("chunk with FileID 0x..5b: %v", err)
	} else if c, ok := m.(FileChunk); !ok || c.FileID&0xff != '[' {
		t.Errorf("chunk with FileID 0x..5b parsed as %#v", m)
	}
	if m, err := Parse(Data, mustHex(t, "wifi")); err != nil || !reflect.DeepEqual(m, WiFiNetworks{SSIDs: []string{"home", "cafe"}}) {
		t.Errorf("wifi list = %v, %v", m, err)
	}

	// NaN fails every range comparison, so it used to pass as a coordinate.
	nan := float32(math.NaN())
	for _, p := range []POI{{Lat: nan}, {Lon: nan}, {Lat: float32(math.Inf(1))}, {Lat: 91}} {
		if _, err := AddPOI(p); err == nil {
			t.Errorf("AddPOI(%v, %v) accepted", p.Lat, p.Lon)
		}
	}
	if f := SendPhoneFix(PhoneFix{HAcc: math.NaN()}); int8(f.Bytes[10]) != 127 {
		t.Errorf("NaN accuracy sent as %d, want 127", int8(f.Bytes[10]))
	}
}

// The firmware never sends NaN or ±Inf; a corrupt record with them decodes
// as the firmware's "none" (0, no accuracy), which --json can print: the
// JSON encoder rejects non-finite floats, which used to drop the record.
func TestNonFiniteFloatsDecodeAsNone(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	b, err := LiveData{Lat: nan, Lon: inf, BattVolts: nan, PosAccuracyM: &nan}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(Data, b)
	if err != nil {
		t.Fatal(err)
	}
	d := m.(LiveData)
	if d.Lat != 0 || d.Lon != 0 || d.BattVolts != 0 || d.PosAccuracyM != nil {
		t.Errorf("live data = %+v", d)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Error(err)
	}
	b, err = PeerPing{Lat: nan, Lon: -inf, Volts: nan}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if m, err := Parse(Data, b); err != nil {
		t.Fatal(err)
	} else if p := m.(PeerPing); p.Lat != 0 || p.Lon != 0 || p.Volts != 0 {
		t.Errorf("peer ping = %+v", p)
	} else if _, err := json.Marshal(p); err != nil {
		t.Error(err)
	}
}

// A WiFi list cut off in transit keeps its complete entries and still
// decodes as the list, so the client acks it: the device repeats the list
// until acked. It used to decode as a garbage FileChunk.
func TestTruncatedWiFiList(t *testing.T) {
	for in, want := range map[string]WiFiNetworks{
		`["home", "caf`:      {SSIDs: []string{"home"}, Truncated: true},
		`["home", "cafe"`:    {SSIDs: []string{"home", "cafe"}, Truncated: true},
		`["`:                 {Truncated: true},
		`[]`:                 {SSIDs: []string{}},
		`["a\"b", "c"]`:      {SSIDs: []string{`a"b`, "c"}},
		`["home", "cafe"]  `: {SSIDs: []string{"home", "cafe"}},
	} {
		m, err := Parse(Data, append([]byte{CatWiFi, 0x02}, in...))
		if err != nil || !reflect.DeepEqual(m, want) {
			t.Errorf("Parse(%q) = %#v, %v, want %#v", in, m, err, want)
		}
	}
	if m, err := Parse(Data, append([]byte{CatWiFi, 0x02}, `["a", 1]`...)); err == nil {
		t.Errorf("a list with a number parsed as %#v", m)
	}
}

// CleanName mirrors what update_options stores and rejects what Static Data
// could not report back.
func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  Base Camp \t":        "Base Camp",
		"Zażółć":                "Zażółć",
		strings.Repeat("🧭", 16): strings.Repeat("🧭", 16), // 32 UTF-16 units
		strings.Repeat("x", 32): strings.Repeat("x", 32),
	} {
		if got, err := CleanName(in); err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	// U+2028/U+2029 always JSON-encode to 6-byte escapes: 32 of them passed
	// CleanName and then overflowed the frame.
	for _, in := range []string{"", " \t ", "\xff", "a\x00b", strings.Repeat("x", 33), strings.Repeat("🧭", 17),
		strings.Repeat(" ", 32), "a b"} {
		if got, err := CleanName(in); err == nil {
			t.Errorf("CleanName(%q) = %q, want an error", in, got)
		}
	}
	if f, err := SetName("  Base Camp  "); err != nil || string(f.Bytes[2:]) != `{"name":"Base Camp"}` {
		t.Errorf("SetName trims like the device: %q, %v", f.Bytes, err)
	}
}
