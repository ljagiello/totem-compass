// Package store keeps a Totem's settings and bonds across reboots, the way
// firmware 5.0.3 keeps config.json on its filesystem: the name, the crystal
// colour, the SOS mute, the battery calibration and every bonded peer.
//
// A Totem writes a file; this writes one flash sector, because the
// emulator has no filesystem. The sector is an append-only journal: a save
// adds a record after the last one, and only a full sector costs an erase.
// That matters on the device, where an erase runs with the flash cache off
// and the radio stalled, so the fewer the better.
//
// Everything a reboot reads back is untrusted: a sector holds whatever a
// torn write, a crash or a previous firmware left. Scan therefore trusts
// nothing about a record beyond what its own header and checksum prove.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Sector is one erasable region of flash. The device implements it over
// the ESP32's SPI flash; the tests over a byte slice.
type Sector interface {
	// Size is the sector's length in bytes.
	Size() int
	// ReadAt fills p from off.
	ReadAt(p []byte, off int) error
	// WriteAt writes p at off. Flash only clears bits, so a write to a
	// region that was not erased first leaves the AND of the two.
	WriteAt(p []byte, off int) error
	// Erase sets the whole sector back to 0xff.
	Erase() error
}

const (
	// headerLen is magic, sequence, length, padding and checksum.
	headerLen = 16
	// MaxRecord is the largest payload a save may hold. It leaves room
	// for the header in the smallest sector the ESP32 can erase.
	MaxRecord = 2048
	// align is the ESP32 flash write granularity: the ROM writes words.
	align = 4
)

// magic marks a record. A sector that has never been written reads as
// 0xff, which is not this.
var magic = [4]byte{'T', 'T', 'M', '1'}

// Errors a caller can tell apart.
var (
	// ErrEmpty means the sector holds no record yet: a first boot, or the
	// sector right after an erase.
	ErrEmpty = errors.New("store: no saved state")
	// ErrTooLarge means the payload does not fit a record.
	ErrTooLarge = errors.New("store: state too large")
)

// Journal is the append-only log in one sector.
type Journal struct {
	sec Sector
	// next is where the following record goes, and seq its sequence
	// number. Both come from scanning the sector at Open.
	next int
	seq  uint32
	// last is the newest valid payload, nil when there is none.
	last []byte
	// torn counts records that did not survive their checksum: a torn
	// write, which the caller may want to log.
	torn int
}

// Open scans the sector and returns its journal. It fails only when the
// sector cannot be read; a sector full of noise opens empty, because a
// device that cannot read its settings still has to boot.
func Open(sec Sector) (*Journal, error) {
	if sec.Size() < headerLen+align {
		return nil, fmt.Errorf("store: sector of %d bytes is too small", sec.Size())
	}
	buf := make([]byte, sec.Size())
	if err := sec.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("store: reading the sector: %w", err)
	}
	j := &Journal{sec: sec}
	j.scan(buf)
	return j, nil
}

// scan walks the records in a sector image and keeps the newest one whose
// checksum holds. It stops at the first header it cannot believe, which is
// where the journal ends: either erased flash, or a write cut short by a
// reset.
func (j *Journal) scan(buf []byte) {
	off := 0
	for off+headerLen <= len(buf) {
		h := buf[off : off+headerLen]
		if [4]byte(h[0:4]) != magic {
			break // erased flash, or something that is not a record
		}
		seq := binary.LittleEndian.Uint32(h[4:8])
		n := int(binary.LittleEndian.Uint16(h[8:10]))
		sum := binary.LittleEndian.Uint32(h[12:16])
		end := off + headerLen + n
		if n > MaxRecord || end > len(buf) {
			break // a length the sector cannot hold: the write was cut short
		}
		body := buf[off+headerLen : end]
		if crc32.ChecksumIEEE(body) != sum {
			// A record that does not check out is a write a reset cut
			// short. Keep the one before it, and step over the wreckage:
			// its bytes are written, so the next record cannot go there.
			j.torn++
			off = end + pad(n)
			continue // a save after the reset wrote a good record past it
		}
		// Position decides which record is newest, not the sequence
		// number: records are only ever appended, so the last good one in
		// the sector is the last one written. The sequence number is what
		// a reader sees, and a sector holding noise can carry any value
		// for it — ordering by it would let a bogus 0xffffffff in old
		// bytes hide every record saved afterwards.
		j.last = append([]byte(nil), body...)
		j.seq = seq
		off = end + pad(n)
	}
	// The padding that rounds the last record up to the write granularity
	// can land past the end: a record whose length is not a multiple of
	// it, written near the sector's end by a previous firmware or left by
	// noise, would otherwise leave next past the size — and the console
	// would report a sector 4097 bytes used with -1 free.
	j.next = min(off, j.sec.Size())
}

// pad is the bytes that round a record up to the flash write granularity.
func pad(n int) int { return (align - n%align) % align }

// Load returns the state saved last, or ErrEmpty.
func (j *Journal) Load() ([]byte, error) {
	if j.last == nil {
		return nil, ErrEmpty
	}
	return append([]byte(nil), j.last...), nil
}

// Torn is how many records failed their checksum, which is a reset caught
// mid-save.
func (j *Journal) Torn() int { return j.torn }

// Save appends the state. When the sector has no room it erases and starts
// again, so a save costs an erase only once a sector's worth of them.
func (j *Journal) Save(p []byte) error {
	if len(p) > MaxRecord {
		return fmt.Errorf("%w: %d bytes, the record holds %d", ErrTooLarge, len(p), MaxRecord)
	}
	rec := make([]byte, headerLen+len(p)+pad(len(p)))
	copy(rec, magic[:])
	binary.LittleEndian.PutUint32(rec[4:8], j.seq+1)
	binary.LittleEndian.PutUint16(rec[8:10], uint16(len(p)))
	binary.LittleEndian.PutUint32(rec[12:16], crc32.ChecksumIEEE(p))
	copy(rec[headerLen:], p)
	// The padding is written as 0xff, the erased value, so a following
	// record can still be written into it if it ever needs to be.
	for i := headerLen + len(p); i < len(rec); i++ {
		rec[i] = 0xff
	}
	// A record that cannot fit an empty sector never will, and finding
	// that out after the erase would cost the settings that were there.
	if len(rec) > j.sec.Size() {
		return fmt.Errorf("%w: %d bytes with its header, the sector holds %d",
			ErrTooLarge, len(rec), j.sec.Size())
	}
	// Flash only clears bits, so a record may only go where nothing has
	// been written. Check rather than trust the scan: the sector may hold
	// a previous firmware's data, or noise, in which case appending would
	// write the AND of the two and lose both.
	room := j.next+len(rec) <= j.sec.Size()
	erased := false
	if !room || !j.erased(j.next, len(rec)) {
		if err := j.sec.Erase(); err != nil {
			return fmt.Errorf("store: erasing the sector: %w", err)
		}
		j.next, j.torn, erased = 0, 0, true
	}
	if err := j.sec.WriteAt(rec, j.next); err != nil {
		if erased {
			// The sector was cleared and the record that should have
			// replaced its contents did not land, so what this journal
			// held is gone from the flash. Saying otherwise would hand
			// the caller settings the device no longer has, and the next
			// save would compare against a record that is not there.
			j.last = nil
		}
		return fmt.Errorf("store: writing %d bytes at %d: %w", len(rec), j.next, err)
	}
	j.next += len(rec)
	j.seq++
	j.last = append([]byte(nil), p...)
	return nil
}

// erased reports whether n bytes at off are still 0xff, the value flash
// holds after an erase and the only state a record may be written into.
func (j *Journal) erased(off, n int) bool {
	if off < 0 || off+n > j.sec.Size() {
		return false
	}
	buf := make([]byte, n)
	if err := j.sec.ReadAt(buf, off); err != nil {
		return false
	}
	for _, b := range buf {
		if b != 0xff {
			return false
		}
	}
	return true
}

// Used is how much of the sector the journal has filled, and Free what is
// left for records.
func (j *Journal) Used() int { return j.next }

// Free is the bytes left before the next save costs an erase.
func (j *Journal) Free() int { return j.sec.Size() - j.next }

// Seq is the sequence number of the record saved last.
func (j *Journal) Seq() uint32 { return j.seq }
