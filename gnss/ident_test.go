package gnss

import (
	"os"
	"testing"
)

// monVer builds a UBX-MON-VER reply the way a receiver sends it: a 30-byte
// software version, a 10-byte hardware version and 30-byte extensions,
// each NUL padded, framed and checksummed. The strings are the bench
// T-Beam's own answer.
func monVer(sw, hw string, ext ...string) []byte {
	pad := func(s string, n int) []byte {
		b := make([]byte, n)
		copy(b, s)
		return b
	}
	payload := append(pad(sw, 30), pad(hw, 10)...)
	for _, e := range ext {
		payload = append(payload, pad(e, 30)...)
	}
	body := append([]byte{0x0A, 0x04, byte(len(payload)), byte(len(payload) >> 8)}, payload...)
	var a, b byte
	for _, c := range body {
		a += c
		b += a
	}
	return append(append([]byte{0xB5, 0x62}, body...), a, b)
}

func TestIdentifyTBeamBanner(t *testing.T) {
	// What the bench T-Beam's receiver printed when its rail came up.
	p, err := os.ReadFile("testdata/tbeam_max_m10s_boot.nmea")
	if err != nil {
		t.Fatal(err)
	}
	want := Ident{Vendor: "u-blox", Model: "MAX-M10S", Firmware: "SPG 5.10"}
	if got := Identify(p); got != want {
		t.Errorf("Identify(banner) = %+v, want %+v", got, want)
	}
}

func TestIdentifyMonVerReply(t *testing.T) {
	// The binary answer to UBXMonVerPoll, with NMEA from the regular
	// output on either side of it, as it arrives on the wire.
	p := append([]byte("$GNGLL,,,,,,V,N*7A\r\n"),
		monVer("ROM SPG 5.10 (7b202e)", "000A0000",
			"FWVER=SPG 5.10", "PROTVER=34.10", "MOD=MAX-M10S", "GPS;GLO;GAL;BDS", "SBAS;QZSS")...)
	p = append(p, "$GNRMC,,V,,,,,,,,,,N,V*37\r\n"...)
	want := Ident{Vendor: "u-blox", Model: "MAX-M10S", Firmware: "SPG 5.10"}
	if got := Identify(p); got != want {
		t.Errorf("Identify(MON-VER) = %+v, want %+v", got, want)
	}
}

func TestIdentifyPCASRejection(t *testing.T) {
	// A u-blox receiver's answer to PCASVersionQuery names nothing, and
	// must not be read as a CASIC receiver answering.
	if got := Identify([]byte("$GNTXT,01,01,01,PCAS inv format*23\r\n")); got.Known() {
		t.Errorf("Identify(PCAS rejection) = %+v, want nothing", got)
	}
}

func TestIdentifyQuectel(t *testing.T) {
	// The shape CASIC documents for a PCAS06 answer. Not seen on hardware
	// here; see Identify.
	got := Identify([]byte("$GPTXT,01,01,02,SW=URANUS5,V5.3.0.0*1D\r\n"))
	want := Ident{Vendor: "quectel", Firmware: "URANUS5"}
	if got != want {
		t.Errorf("Identify(PCAS06 answer) = %+v, want %+v", got, want)
	}
}

func TestIdentifyNothing(t *testing.T) {
	for _, p := range [][]byte{nil, []byte("$GNRMC,,V,,,,,,,,,,N,V*37\r\n"), []byte("MOD="), {0, 0, 0}} {
		if got := Identify(p); got.Known() {
			t.Errorf("Identify(%q) = %+v, want nothing", p, got)
		}
	}
}

func TestQueriesAreWellFormed(t *testing.T) {
	// The poll is an empty MON-VER: sync, class 0x0A, ID 0x04, length 0.
	if got, want := string(UBXMonVerPoll[:6]), "\xB5\x62\x0A\x04\x00\x00"; got != want {
		t.Errorf("poll header % x, want % x", got, want)
	}
	var a, b byte
	for _, c := range UBXMonVerPoll[2:6] {
		a += c
		b += a
	}
	if UBXMonVerPoll[6] != a || UBXMonVerPoll[7] != b {
		t.Errorf("poll checksum % x, want %02x %02x", UBXMonVerPoll[6:], a, b)
	}
	// The PCAS06 sentence's checksum is the XOR of what lies between $ and *.
	var x byte
	for _, c := range []byte("PCAS06,0") {
		x ^= c
	}
	if want := "$PCAS06,0*1B\r\n"; string(PCASVersionQuery) != want || x != 0x1B {
		t.Errorf("PCAS query %q (xor %02X), want %q", PCASVersionQuery, x, want)
	}
}

func FuzzIdentify(f *testing.F) {
	if p, err := os.ReadFile("testdata/tbeam_max_m10s_boot.nmea"); err == nil {
		f.Add(p)
	}
	f.Add(monVer("ROM SPG 5.10 (7b202e)", "000A0000", "FWVER=SPG 5.10", "MOD=MAX-M10S"))
	f.Add([]byte("$GPTXT,01,01,02,SW=URANUS5,V5.3.0.0*1D\r\n"))
	f.Add([]byte("MOD=FWVER=SW="))
	f.Fuzz(func(t *testing.T, p []byte) {
		id := Identify(p)
		for _, s := range []string{id.Vendor, id.Model, id.Firmware} {
			if len(s) > maxField {
				t.Fatalf("field %q is longer than %d", s, maxField)
			}
			for _, c := range []byte(s) {
				if c < 0x20 || c >= 0x7f {
					t.Fatalf("field %q holds byte %#x", s, c)
				}
			}
		}
		if (id.Model != "" || id.Firmware != "") && !id.Known() {
			t.Fatalf("%+v names a model or firmware but no vendor", id)
		}
	})
}
