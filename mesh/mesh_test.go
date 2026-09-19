package mesh

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

var (
	macA = MAC{0x8c, 0x94, 0xdf, 0x7b, 0x04, 0x78}
	macB = MAC{0x20, 0x9b, 0xa9, 0x70, 0xab, 0xb0}
)

// golden frames were packed by CPython's struct module with the firmware's
// own format strings (TOTEM_MSG_MAP / EXTENDED and the pack_into calls in
// gen_peer_msg, gen_mesh_msg, gen_auto_bond_grp and Parser._smart_group),
// independently of this package.
var golden = []struct {
	name string
	hex  string
	msg  Message
}{
	{
		"peer status",
		"a77400007f191742bcd6f4c205030e010001000000000000000000002e930264d7ae6a010500030c005a008043ffffd2040000000000000000ff0000000000000000000000000e656d755f746f74656d5f616230310101530157000000000000000000000000000000000000",
		Peer{
			Command: PeerStatus, Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 5, SpeedKPH: 3, Azimuth: 270,
			Orientation: OrientationVertical, TimeOfDayMs: 43200000, Unix: 1789843300, PhoneConnected: true,
			Major: 5, Minor: 0, Patch: 3, AltitudeM: 12, UptimeMin: 90, BattVolts: 3.75,
			HeadingOfMotion: -1, OdometerM: 1234, Name: "emu_totem_ab01",
			SolutionID: 1, GNSSSource: GNSSDevice, ReleaseID: 339, BattPct: 87,
		},
	},
	{
		"bond confirm",
		"a77400010000000000000000ffff00000102010000000000000000ffffffffffffffff000500030cfe00000044ffff00000000000000000000ff00000000000000000000000007c581756b61737a000200006400000000000000000000000000000000000000000000000000",
		Peer{
			Command: PeerBond, PosAccuracyM: -1, SpeedKPH: -1, SOS: true, Orientation: OrientationHorizontal,
			Ack: true, TimeOfDayMs: -1, Unix: -1, Major: 5, Patch: 3, AltitudeM: -500, BattVolts: 4,
			HeadingOfMotion: -1, Name: "Łukasz", GNSSSource: GNSSPhone, BattPct: 100,
		},
	},
	{
		"locate request",
		"a77402008c94df7b04787f191742bcd6f4c205009210018100ffffffff0363dcd7ae6a9a1917420ad7f4c20a00",
		Locate{
			Origin: macA, Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 5, UID: 4242, ReplyRequested: true,
			MinRSSI: DefaultMinRSSI, MinDistM: -1, MaxDistM: -1, Hops: 3, MaxHops: DefaultMaxHops,
			Expiry: 1789843420, LastHopLat: 37.775, LastHopLon: -122.42, RelayMinDistM: DefaultRelayMinDist,
		},
	},
	{
		"smart group beacon",
		"a77407007f191742bcd6f4c20500010903020818798c94df7b04787f191742bcd6f4c20500209ba970abb09a191742c9d6f4c2ff02",
		SmartGroup{
			Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 5, Instruction: SmartGroupAdvertise, UID: 777, TimeoutMs: 31000,
			Members: []SmartGroupMember{
				{MAC: macA, Lat: 37.7749, Lon: -122.4194, PosAccuracyM: 5},
				{MAC: macB, Lat: 37.775, Lon: -122.4195, PosAccuracyM: -1, ColorID: 2},
			},
		},
	},
	{
		"smart group leave",
		"a77407010903ff9a191742c9d6f4c203",
		SmartGroupReply{UID: 777, Leave: true, Lat: 37.775, Lon: -122.4195, PosAccuracyM: 3},
	},
	{"demi-god", "a7740106deadbeef", DemiGod{Command: 6, Payload: []byte{0xde, 0xad, 0xbe, 0xef}}},
	{"unknown", "a7740501", Unknown{Category: 5, Command: 1, Payload: []byte{}}},
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGolden(t *testing.T) {
	for _, g := range golden {
		t.Run(g.name, func(t *testing.T) {
			b := unhex(t, g.hex)
			m, err := Parse(b)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(m, g.msg) {
				t.Errorf("Parse =\n%#v\nwant\n%#v", m, g.msg)
			}
			out, err := g.msg.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary: %v", err)
			}
			if !bytes.Equal(out, b) {
				t.Errorf("MarshalBinary =\n%x\nwant\n%x", out, b)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	peer := unhex(t, golden[0].hex)
	tests := []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, ErrNotTotem.Error()},
		{"header only", []byte{0xa7, 0x74, 0}, ErrNotTotem.Error()},
		{"wrong sync word", append([]byte{0x74, 0xa7}, peer[2:]...), ErrNotTotem.Error()},
		{"peer frame cut in the fixed part", peer[:70], "need 71"},
		{"peer frame cut in the name", peer[:75], "ends inside the name"},
		{"peer name length negative", func() []byte { b := bytes.Clone(peer); b[70] = 0x80; return b }(), "name length -128"},
		{"locate cut", unhex(t, golden[2].hex)[:44], "need 45"},
		{"smart group member cut", unhex(t, golden[3].hex)[:50], "need 53"},
		{"smart group count negative", func() []byte { b := unhex(t, golden[3].hex); b[17] = 0xff; return b }(), "member count -1"},
		{"smart group reply cut", unhex(t, golden[4].hex)[:15], "need 16"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse(tt.b)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %#v, %v; want error containing %q", m, err, tt.want)
			}
		})
	}
}

func TestMarshalRejects(t *testing.T) {
	long := Peer{Name: strings.Repeat("x", MaxPeerName+1)}
	if _, err := long.MarshalBinary(); err == nil {
		t.Errorf("a %d-byte name encoded", MaxPeerName+1)
	}
	fits := Peer{Name: strings.Repeat("x", MaxPeerName)}
	b, err := fits.MarshalBinary()
	if err != nil || len(b) != PeerFrameLen {
		t.Errorf("a %d-byte name: %d bytes, %v", MaxPeerName, len(b), err)
	}
	big := SmartGroup{Members: make([]SmartGroupMember, MaxSmartGroupMembers+1)}
	if _, err := big.MarshalBinary(); err == nil {
		t.Errorf("%d smart group members encoded", MaxSmartGroupMembers+1)
	}
	full := SmartGroup{Members: make([]SmartGroupMember, MaxSmartGroupMembers)}
	if b, err := full.MarshalBinary(); err != nil || len(b) > MaxFrame {
		t.Errorf("%d members: %d bytes, %v", MaxSmartGroupMembers, len(b), err)
	}
	if _, err := (Unknown{Category: 9, Payload: make([]byte, MaxFrame)}).MarshalBinary(); err == nil {
		t.Error("an oversized frame encoded")
	}
}

// TestPeerPaddingIsZero: a Totem reuses ENOW_PEER_BUFF, but a fresh
// encoder must never leak bytes past the tail.
func TestPeerPaddingIsZero(t *testing.T) {
	b, err := Peer{Name: "a"}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if tail := b[peerNameOffset+1+peerTailLen:]; !bytes.Equal(tail, make([]byte, len(tail))) {
		t.Errorf("padding = %x", tail)
	}
}

func TestParseNotTotemIsSentinel(t *testing.T) {
	if _, err := Parse([]byte("hello")); !errors.Is(err, ErrNotTotem) {
		t.Errorf("err = %v, want ErrNotTotem", err)
	}
}

// TestHalfRoundTrip checks every normal half-float bit pattern, plus zero
// and infinity: decoding and re-encoding must give the same bits back.
// Subnormals do not round-trip in MicroPython either (see
// TestHalfMatchesMicroPython).
func TestHalfRoundTrip(t *testing.T) {
	for h := range 1 << 16 {
		f := decodeHalf(uint16(h))
		if exp := h >> 10 & 0x1f; f != f || exp == 0 && h&0x3ff != 0 {
			continue
		}
		if got := encodeHalf(f); got != uint16(h) {
			t.Fatalf("encodeHalf(decodeHalf(%#04x) = %g) = %#04x", h, f, got)
		}
	}
}

// TestHalfMatchesMicroPython pins the behavior that differs from IEEE
// round-half-even (CPython's struct 'e'): MicroPython rounds ties up.
func TestHalfMatchesMicroPython(t *testing.T) {
	tests := []struct {
		f    float32
		want uint16
	}{
		{0, 0x0000},
		{1, 0x3c00},
		{3.75, 0x4380},
		{4.2, 0x4433},
		{1 + 1.0/2048, 0x3c01}, // a tie: IEEE gives 0x3c00
		{-2, 0xc000},
		{65504, 0x7bff},
		{float32(math.Inf(1)), 0x7c00},
		{1e-8, 0x0000}, // below the smallest subnormal
		{6e-8, 0x0001},
		// 2^-15 is the subnormal 0x0200, but MicroPython only treats a
		// half exponent below 0 as subnormal, so it encodes 0.
		{1.0 / 32768, 0x0000},
		// Rounding carries out of the mantissa: MicroPython ORs the carry
		// into the exponent, which is right for 3.99994 and wrong for
		// 7.99994 (4, not 8).
		{3.9999695, 0x4400},
		{7.999939, 0x4400},
	}
	for _, tt := range tests {
		if got := encodeHalf(tt.f); got != tt.want {
			t.Errorf("encodeHalf(%g) = %#04x, want %#04x", tt.f, got, tt.want)
		}
	}
}
