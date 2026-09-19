package protocol

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// seedFrames adds every golden vector and captured frame, on both channels.
func seedFrames(f *testing.F) {
	for _, m := range []map[string]string{golden, realFrames} {
		for _, h := range m {
			b, err := hex.DecodeString(h)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(byte(Data), b)
			f.Add(byte(ConnStatus), b)
		}
	}
	f.Add(byte(ConnStatus), []byte{CatConn, 0x05, 1, 0, 0, 0, 2, 0, 0, 0})
	f.Add(byte(ConnStatus), []byte{CatConn, 0x02})
	f.Add(byte(Data), []byte{CatWiFi, 0x02, '[', ']'})
	f.Add(byte(Data), []byte{})
}

// sameMessage compares two decoded messages. Records compare by their
// encoding, so NaN floats (which never equal themselves) still match.
func sameMessage(t *testing.T, a, b Message) bool {
	t.Helper()
	am, ok := a.(encoding.BinaryMarshaler)
	if !ok {
		return reflect.DeepEqual(a, b)
	}
	ab, err1 := am.MarshalBinary()
	bb, err2 := b.(encoding.BinaryMarshaler).MarshalBinary()
	if err1 != nil || err2 != nil {
		t.Fatalf("re-encode: %v, %v", err1, err2)
	}
	return bytes.Equal(ab, bb)
}

// FuzzParse feeds arbitrary frames to Parse, which decodes whatever the
// device (or anything else in radio range) sends.
func FuzzParse(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, chb byte, b []byte) {
		ch := Channel(chb & 1)
		orig := bytes.Clone(b)
		m, err := Parse(ch, b)
		if (m == nil) == (err == nil) {
			t.Fatalf("Parse(% x) = %v, %v: want exactly one of message or error", b, m, err)
		}
		if err != nil {
			return
		}
		// The result must not alias the input buffer, which transports reuse.
		for i := range b {
			b[i] ^= 0xff
		}
		again, err := Parse(ch, orig)
		if err != nil {
			t.Fatalf("second Parse failed: %v", err)
		}
		if !sameMessage(t, m, again) {
			t.Fatalf("the parsed message changed with its input buffer\n got %+v\nwant %+v", m, again)
		}
		// Every decoded record re-encodes, and decoding that is stable: encoding
		// normalizes only the bytes Parse ignores.
		rec, ok := m.(encoding.BinaryMarshaler)
		if !ok {
			return
		}
		enc, err := rec.MarshalBinary()
		if err != nil {
			t.Fatalf("decoded %T does not re-encode: %v\n%+v", m, err, m)
		}
		m2, err := Parse(Data, enc)
		if err != nil {
			t.Fatalf("re-encoded %T does not parse: %v\n% x", m, err, enc)
		}
		if reflect.TypeOf(m2) != reflect.TypeOf(m) {
			t.Fatalf("re-encoded %T parses as %T", m, m2)
		}
		enc2, err := m2.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil || !bytes.Equal(enc, enc2) {
			t.Fatalf("encoding is not stable: %v\n% x\n% x", err, enc, enc2)
		}
	})
}

// FuzzParseMAC checks the parser for MACs typed on the command line.
func FuzzParseMAC(f *testing.F) {
	for _, s := range []string{"a1b2c3d4e5f6", "A1:B2:C3:D4:E5:F6", "a1-b2-c3-d4-e5-f6", "a1 b2", "", "zz:zz:zz:zz:zz:zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMAC(s)
		if err != nil {
			return
		}
		for _, form := range []string{m.String(), m.Pretty()} {
			if back, err := ParseMAC(form); err != nil || back != m {
				t.Fatalf("ParseMAC(%q) = %v, but %q parses as %v, %v", s, m, form, back, err)
			}
		}
	})
}

// FuzzParseRGB checks the parser for colors typed on the command line.
func FuzzParseRGB(f *testing.F) {
	for _, s := range []string{"#ff8000", "ff8000", "255,128,0", "hot_pink", "RED", "1,2", "#12345", "300,0,0", "1,2,3x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseRGB(s)
		if err != nil {
			return
		}
		if back, err := ParseRGB(c.String()); err != nil || back != c {
			t.Fatalf("ParseRGB(%q) = %v, but %q parses as %v, %v", s, c, c.String(), back, err)
		}
		// An r,g,b input is accepted only whole: each component is exactly the
		// decimal value it produced, up to spaces and leading zeros.
		if strings.Contains(s, ",") {
			parts := strings.Split(s, ",")
			vals := []uint8{c.R, c.G, c.B}
			if len(parts) != 3 {
				t.Fatalf("ParseRGB(%q) accepted %d components", s, len(parts))
			}
			for i, p := range parts {
				got := strings.TrimLeft(strings.TrimSpace(p), "0")
				if want := strings.TrimLeft(strconv.Itoa(int(vals[i])), "0"); got != want {
					t.Fatalf("ParseRGB(%q) = %v: component %q is not %d", s, c, p, vals[i])
				}
			}
		}
	})
}

// FuzzEncoders checks the frame builders that take free-form input: every
// frame fits the device buffer and its length fields match what follows.
func FuzzEncoders(f *testing.F) {
	f.Add("Base Camp", "home", "s3cret", "totem", "latest", int8(1), int16(1), float32(50.0671), float32(19.9124), []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6})
	f.Add(strings.Repeat("x", 132), "", "", strings.Repeat("b", 127), "", int8(-1), int16(-1), float32(-33.8), float32(151.2), []byte{})
	f.Add("\xff\xfe", "\"quoted\"", "\\", "", strings.Repeat("v", 200), int8(0), int16(0), float32(0), float32(0), bytes.Repeat([]byte{1}, 6*31))
	f.Add(strings.Repeat("\u2028", 32), "", "", "", "", int8(0), int16(0), float32(0), float32(0), []byte{})
	f.Fuzz(func(t *testing.T, name, ssid, key, branch, version string, cmd int8, endpoint int16, lat, lon float32, macs []byte) {
		fits := func(what string, fr Frame) {
			t.Helper()
			if len(fr.Bytes) > DataBufferSize {
				t.Fatalf("%s: %d-byte frame exceeds the %d-byte device buffer", what, len(fr.Bytes), DataBufferSize)
			}
		}

		// Every name CleanName accepts fits the frame, and is what Static
		// Data can report back.
		if clean, err := CleanName(name); err == nil {
			fr, err := SetName(clean)
			if err != nil {
				t.Fatalf("SetName(CleanName(%q)): %v", name, err)
			}
			fits("SetName", fr)
			var v struct{ Name string }
			if err := json.Unmarshal(fr.Bytes[2:], &v); err != nil || v.Name != string(clean) {
				t.Fatalf("SetName(%q) payload %q: %v", clean, fr.Bytes[2:], err)
			}
			if len(clean) > 127 || string(clean) != strings.Trim(string(clean), " \t\n\v\f\r") || !utf8.ValidString(string(clean)) {
				t.Fatalf("CleanName(%q) = %q, which the device cannot report back", name, clean)
			}
		}

		// The Totem gets exactly the network given, or an error.
		if fr, err := SaveWiFi(ssid, key); err == nil {
			fits("SaveWiFi", fr)
			var v struct{ NW, Join string }
			if err := json.Unmarshal(fr.Bytes[2:], &v); err != nil || v.NW != ssid || v.Join != key {
				t.Fatalf("SaveWiFi(%q, %q) payload %q: %v", ssid, key, fr.Bytes[2:], err)
			}
		}

		if fr, err := AddPOI(POI{Name: name, Lat: lat, Lon: lon}); err == nil {
			fits("AddPOI", fr)
			b := fr.Bytes
			var w poiWire
			if err := binary.Read(bytes.NewReader(b[9:poiHeader]), binary.LittleEndian, &w); err != nil {
				t.Fatal(err)
			}
			// add_new_bond reads the name length as a signed byte.
			if int(w.NameLen) != len(name) || string(b[poiHeader:]) != name || int(b[2]) != len(b) {
				t.Fatalf("AddPOI(%q): name length field %d, frame length field %d, frame %d bytes", name, w.NameLen, b[2], len(b))
			}
		}

		if fr, err := StartOTA(OTARequest{Cmd: cmd, Branch: branch, Version: version, EndpointID: endpoint}); err == nil {
			fits("StartOTA", fr)
			b := fr.Bytes
			if int(int8(b[4])) != len(branch) || int(int8(b[5])) != len(version) || string(b[9:]) != branch+version {
				t.Fatalf("StartOTA(%q, %q): length fields %d, %d, frame % x", branch, version, int8(b[4]), int8(b[5]), b)
			}
		}

		peers := make([]MAC, len(macs)/6)
		for i := range peers {
			copy(peers[i][:], macs[6*i:])
		}
		if fr, err := RequestPeerDetails(peers...); err == nil {
			fits("RequestPeerDetails", fr)
			if len(peers) > 0 && (int(fr.Bytes[2]) != len(peers) || len(fr.Bytes) != 4+6*len(peers)) {
				t.Fatalf("RequestPeerDetails(%d peers): count field %d, frame % x", len(peers), fr.Bytes[2], fr.Bytes)
			}
		} else if len(peers) <= (DataBufferSize-4)/6 {
			t.Fatalf("RequestPeerDetails(%d peers) rejected a list that fits: %v", len(peers), err)
		}
	})
}
