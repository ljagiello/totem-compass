package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// State is what survives a reboot. It holds what firmware 5.0.3 keeps in
// config.json — the device's name and crystal colour, whether SOS is
// muted, the bonded peers with the colour each was given — plus the
// counters a Totem carries across boots.
//
// The encoding is this package's own: a Totem writes JSON to a
// filesystem, and the emulator has a flash sector and no filesystem. The
// contents are the same; the bytes are not.
type State struct {
	// Name is what peers see in a status frame.
	Name string
	// ColorID is the crystal's default colour, an index into the
	// firmware's 13-colour palette.
	ColorID int8
	// Brightness is the global LED brightness, 0-255.
	Brightness uint8
	// SOSMuted survives a reboot, as a muted alarm should.
	SOSMuted bool
	// BootCount counts power-ups, as the OTA report's boot_count does.
	BootCount uint32
	// SleepMs and LearnedMaxVolts are a snapshot of what the device last
	// reported, not state it comes back with: the firmware keeps both in
	// modes, whose constructor sets them to 0, so they start again at
	// every boot. They are here so that whoever reads the sector can see
	// what the device was doing, and because a record that carried them
	// once has to keep carrying them.
	//
	// SleepMs is the device's total light sleep, the firmware's
	// dev_total_lightsleep_ms.
	SleepMs uint64
	// LearnedMaxVolts is the highest cell voltage seen, which the battery
	// curve rescales against. Zero means nothing learned yet.
	LearnedMaxVolts float32
	// Peers are the bonds. The firmware keys config.peers by MAC.
	Peers []PeerState
}

// PeerState is one bonded Totem, as config.peers holds it.
type PeerState struct {
	MAC     [6]byte
	Name    string
	ColorID int8
	// Lat and Lon are where the peer was last seen, which lets the device
	// point at it before it has heard from it again.
	Lat, Lon float32
	// LastSeenUnix is when that position was heard, or 0.
	LastSeenUnix int64
}

// stateVersion is the encoding's version. A record written by a different
// version is ignored rather than guessed at.
const stateVersion = 1

// MaxPeers is the most bonds a state holds. The firmware bonds at most 8;
// the extra room is for a peer list that grew before this limit existed.
const MaxPeers = 16

// maxName is the longest name the encoding stores, which is the longest a
// peer frame carries.
const maxName = 32

// ErrBadState means the bytes are not a state this version wrote. It is
// the expected outcome for a sector holding an older format or noise, so
// a caller boots with defaults rather than failing.
var ErrBadState = errors.New("store: not a state record")

// MarshalBinary encodes the state.
func (s State) MarshalBinary() ([]byte, error) {
	if len(s.Peers) > MaxPeers {
		return nil, fmt.Errorf("store: %d peers, at most %d fit", len(s.Peers), MaxPeers)
	}
	if err := checkName(s.Name); err != nil {
		return nil, err
	}
	for _, p := range s.Peers {
		if err := checkName(p.Name); err != nil {
			return nil, err
		}
	}
	b := make([]byte, 0, 64+len(s.Peers)*56)
	b = append(b, stateVersion)
	b = appendString(b, s.Name)
	b = append(b, byte(s.ColorID), s.Brightness, boolByte(s.SOSMuted))
	b = binary.LittleEndian.AppendUint32(b, s.BootCount)
	b = binary.LittleEndian.AppendUint64(b, s.SleepMs)
	b = binary.LittleEndian.AppendUint32(b, math.Float32bits(s.LearnedMaxVolts))
	b = append(b, byte(len(s.Peers)))
	for _, p := range s.Peers {
		b = append(b, p.MAC[:]...)
		b = appendString(b, p.Name)
		b = append(b, byte(p.ColorID))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(p.Lat))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(p.Lon))
		b = binary.LittleEndian.AppendUint64(b, uint64(p.LastSeenUnix))
	}
	return b, nil
}

// UnmarshalBinary decodes a state. Every length in the input is checked
// against what is left, because the bytes come from flash: a torn write, a
// different firmware or noise all arrive here.
func (s *State) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	v, ok := r.byte()
	if !ok || v != stateVersion {
		return fmt.Errorf("%w: version %d", ErrBadState, v)
	}
	var out State
	if out.Name, ok = r.string(); !ok {
		return fmt.Errorf("%w: name", ErrBadState)
	}
	color, ok1 := r.byte()
	bright, ok2 := r.byte()
	muted, ok3 := r.byte()
	if !ok1 || !ok2 || !ok3 || muted > 1 {
		return fmt.Errorf("%w: settings", ErrBadState)
	}
	out.ColorID, out.Brightness, out.SOSMuted = int8(color), bright, muted == 1
	boots, ok1 := r.uint32()
	sleep, ok2 := r.uint64()
	volts, ok3 := r.uint32()
	if !ok1 || !ok2 || !ok3 {
		return fmt.Errorf("%w: counters", ErrBadState)
	}
	out.BootCount, out.SleepMs, out.LearnedMaxVolts = boots, sleep, math.Float32frombits(volts)
	n, ok := r.byte()
	if !ok || int(n) > MaxPeers {
		return fmt.Errorf("%w: %d peers", ErrBadState, n)
	}
	for i := 0; i < int(n); i++ {
		var p PeerState
		mac, ok := r.bytes(6)
		if !ok {
			return fmt.Errorf("%w: peer %d mac", ErrBadState, i)
		}
		p.MAC = [6]byte(mac)
		if p.Name, ok = r.string(); !ok {
			return fmt.Errorf("%w: peer %d name", ErrBadState, i)
		}
		color, ok1 := r.byte()
		lat, ok2 := r.uint32()
		lon, ok3 := r.uint32()
		seen, ok4 := r.uint64()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return fmt.Errorf("%w: peer %d", ErrBadState, i)
		}
		p.ColorID, p.Lat, p.Lon = int8(color), math.Float32frombits(lat), math.Float32frombits(lon)
		p.LastSeenUnix = int64(seen)
		out.Peers = append(out.Peers, p)
	}
	if r.left() != 0 {
		return fmt.Errorf("%w: %d bytes after the state", ErrBadState, r.left())
	}
	*s = out
	return nil
}

// SanitizeName makes a name this package can store: at most maxName
// bytes of valid UTF-8. A peer's name arrives in a frame off the air,
// where nothing checks it, and a name that cannot be encoded would
// otherwise stop every save on the device for good — one malformed frame
// and no bond, colour or setting is ever written again.
func SanitizeName(s string) string {
	if !utf8.ValidString(s) {
		// Keep what is text and drop the rest, rune by rune.
		out := make([]rune, 0, len(s))
		for _, r := range s {
			if r != utf8.RuneError {
				out = append(out, r)
			}
		}
		s = string(out)
	}
	for len(s) > maxName {
		// Cut whole runes, so what is left is still text.
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func checkName(s string) error {
	if len(s) > maxName {
		return fmt.Errorf("store: name of %d bytes, at most %d fit", len(s), maxName)
	}
	if !utf8.ValidString(s) {
		return errors.New("store: name is not UTF-8")
	}
	return nil
}

func appendString(b []byte, s string) []byte {
	return append(append(b, byte(len(s))), s...)
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// reader reads the encoding without ever running past its input.
type reader struct {
	b []byte
	i int
}

func (r *reader) left() int { return len(r.b) - r.i }

func (r *reader) bytes(n int) ([]byte, bool) {
	if n < 0 || r.left() < n {
		return nil, false
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out, true
}

func (r *reader) byte() (byte, bool) {
	b, ok := r.bytes(1)
	if !ok {
		return 0, false
	}
	return b[0], true
}

func (r *reader) uint32() (uint32, bool) {
	b, ok := r.bytes(4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

func (r *reader) uint64() (uint64, bool) {
	b, ok := r.bytes(8)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

// string reads a length-prefixed name and rejects one that is too long or
// not text: a name goes into a frame and onto a console line.
func (r *reader) string() (string, bool) {
	n, ok := r.byte()
	if !ok || int(n) > maxName {
		return "", false
	}
	b, ok := r.bytes(int(n))
	if !ok || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}
