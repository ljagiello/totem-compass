package gnss

import "bytes"

// Ident is what a receiver says it is, in its own words.
//
// It exists because a receiver's part number is easy to assume and wrong
// to. The T-Beam SUPREME is sold with either a u-blox MAX-M10S or a
// Quectel L76K, and this project once wrote down the wrong one, reasoned
// from it, and put the reasoning in its docs. Asked, the board said
// MAX-M10S. So the part is asked, and what it answers is logged.
type Ident struct {
	// Vendor is "u-blox" or "quectel", or "" when the answer named none.
	Vendor string
	// Model is the module's own name for itself, such as "MAX-M10S".
	// u-blox modules report it; an L76K does not, so it stays "" there.
	Model string
	// Firmware is the version string, such as "SPG 5.10".
	Firmware string
}

// Known says whether the receiver said anything that identifies it.
func (i Ident) Known() bool { return i.Vendor != "" }

// UBXMonVerPoll asks a u-blox receiver for UBX-MON-VER: an empty message
// of class 0x0A, ID 0x04, and its Fletcher checksum. The answer carries
// the firmware ("FWVER=") and, since M8, the module ("MOD=").
var UBXMonVerPoll = []byte{0xB5, 0x62, 0x0A, 0x04, 0x00, 0x00, 0x0E, 0x34}

// PCASVersionQuery asks a receiver speaking the CASIC command set, which
// an L76K does, for its firmware version. A u-blox receiver answers it
// with "PCAS inv format", which names nothing and is not mistaken for an
// answer below.
var PCASVersionQuery = []byte("$PCAS06,0*1B\r\n")

// maxField bounds a value taken from the wire, so a receiver that never
// sends a terminator cannot hand back an unbounded string.
const maxField = 30

// Identify reads what a receiver sent, after start-up or after one of the
// queries above, and says what it is. It accepts the answer in either of
// the forms a u-blox receiver gives it: the NMEA text banner it prints at
// power-on ("$GNTXT,01,01,02,MOD=MAX-M10S*4E") and the binary MON-VER
// reply, whose fields are the same "KEY=value" strings padded with NULs.
// Any other bytes around them are ignored.
//
// The u-blox forms are checked against a MAX-M10S on the bench. The
// Quectel form — a TXT sentence carrying "SW=" in answer to PCAS06 — is
// what the CASIC protocol describes and has not been seen on hardware
// here, so it is taken as a vendor and a version and nothing more.
func Identify(p []byte) Ident {
	var id Ident
	if v := field(p, "MOD="); v != "" {
		id.Vendor, id.Model = "u-blox", v
	}
	if v := field(p, "FWVER="); v != "" {
		id.Vendor, id.Firmware = "u-blox", v
	}
	if bytes.Contains(p, []byte("u-blox")) {
		id.Vendor = "u-blox"
	}
	if id.Vendor == "" {
		if v := field(p, "SW="); v != "" {
			id.Vendor, id.Firmware = "quectel", v
		}
	}
	return id
}

// field returns the value after the first occurrence of key: printable
// bytes up to a NUL, a control byte, a NMEA checksum's '*' or a field's
// ','. "" when the key is absent or has no value.
func field(p []byte, key string) string {
	_, v, ok := bytes.Cut(p, []byte(key))
	if !ok {
		return ""
	}
	n := 0
	for n < len(v) && n < maxField && v[n] >= 0x20 && v[n] < 0x7f && v[n] != '*' && v[n] != ',' {
		n++
	}
	return string(bytes.TrimSpace(v[:n]))
}
