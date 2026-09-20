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
	// ErrEmptyRecord is a save with nothing in it, which the journal
	// cannot tell apart from no save at all.
	ErrEmptyRecord = errors.New("store: a record must carry something")
	// ErrTooLarge means the payload does not fit a record.
	ErrTooLarge = errors.New("store: state too large")
	// ErrNoSector is Open without one, which is a board whose flash
	// driver is not there. A caller may want to tell that apart from a
	// driver that is there and failing.
	ErrNoSector = errors.New("store: no sector")
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
	// torn counts headers the scan could not believe: a checksum that
	// did not hold, or a length the sector cannot hold. Both are a write
	// a reset cut short, and both are worth a line on the console.
	torn int
	// blank counts well-formed records carrying nothing, which an
	// earlier build could write and this one refuses. They are not torn
	// writes and are counted apart from them so that a console reporting
	// flash trouble does not describe the wrong trouble.
	blank int
}

// Open scans the sector and returns its journal. It fails when there is
// no sector (ErrNoSector) or when the one there cannot be read; a sector
// full of noise opens empty, because a device that cannot read its
// settings still has to boot.
func Open(sec Sector) (*Journal, error) {
	// Here rather than in each caller: this is the line that would
	// panic, so every caller present and future is covered by one check
	// instead of each needing its own copy of it.
	if sec == nil {
		return nil, ErrNoSector
	}
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
	// Position decides which record is newest, not the sequence number:
	// records are only ever appended, so the last good one in the sector
	// is the last one written. Ordering by the number would let a bogus
	// 0xffffffff in old bytes hide every record saved afterwards.
	//
	// The number itself only ever climbs, which is why every branch below
	// takes it through keepSeq — and how far it may climb on one record
	// depends on what that record proved about itself, which keepSeq
	// explains.
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
			// A length the sector cannot hold, which is a write cut
			// short between its magic word and its length word — or
			// noise. Either way this is not a record, and by the same
			// argument as the checksum failure below, its header says
			// nothing about where the next one starts: stopping here
			// would hide every record beyond it.
			j.torn++
			j.keepSeq(seq, false)
			var more bool
			if off, more = j.resume(buf, off); more {
				continue
			}
			break
		}
		body := buf[off+headerLen : end]
		if crc32.ChecksumIEEE(body) != sum {
			// A record that does not check out is a write a reset cut
			// short. Keep the one before it, and step over the wreckage:
			// its bytes are written, so the next record cannot go there,
			// and a save after the reset wrote a good record past it.
			//
			// Its sequence number still counts, by the rule above: a
			// reset caught mid-save is this journal's ordinary failure,
			// and the number in that header is the one the save meant to
			// use, so dropping it had the next save write it again.
			//
			// Its length does not. That is the same header, and the same
			// argument applies to every field in it: sixteen bytes of
			// noise carrying this magic and a length of 2000 would step
			// the scan two kilobytes down the sector, past every good
			// record in between — which Load would then never see, and
			// the next save would append beyond. So the wreckage is
			// stepped over by looking for where the next record actually
			// starts. For a real torn write that is exactly where the
			// length said it would be; for noise it is wherever the
			// records resume.
			j.torn++
			j.keepSeq(seq, false)
			var more bool
			if off, more = j.resume(buf, off); more {
				continue
			}
			break
		}
		// A record with nothing in it is stepped over rather than taken.
		// Load reads a nil payload as "nothing saved", so letting one
		// become j.last would report an empty sector and hide every good
		// record written before it. Save refuses to write one now, but a
		// sector from an earlier build can already hold one. Not counted
		// as torn: its magic, length and checksum all hold, nothing was
		// cut short, and calling it a torn write sends whoever reads the
		// console after a flash problem looking for one that did not
		// happen.
		if len(body) == 0 {
			j.blank++
			j.keepSeq(seq, false)
			off = end + pad(n)
			continue
		}
		j.last = append([]byte(nil), body...)
		j.keepSeq(seq, true)
		off = end + pad(n)
	}
	// The padding that rounds the last record up to the write granularity
	// can land past the end: a record whose length is not a multiple of
	// it, written near the sector's end by a previous firmware or left by
	// noise, would otherwise leave next past the size — and the console
	// would report a sector 4097 bytes used with -1 free.
	j.next = min(off, j.sec.Size())
}

// keepSeq takes a sequence number off a header. The checksum covers the
// body and not the header, so how far a number may be believed depends
// on what the body proved.
//
// A record whose checksum holds over a body with something in it was
// written by this journal: nothing else lands that pattern by accident,
// so its number is taken as it stands, and a board that has been saving
// for a year goes on counting from where it left off.
//
// A torn write and a record carrying nothing prove nothing about their
// own header — an empty body checksums to 0 whatever the header says, so
// sixteen bytes of noise make a whole valid empty record — and they may
// only step the counter on by one. That is exactly right for the case
// they describe, a save a reset cut short, whose number was the next one
// anyway; against a header that claims more it is a bound rather than a
// promise, and the next save can still write a number such a header
// carried. What it does promise is that a sector of noise cannot drive
// the counter: one word of rubbish claiming four billion would otherwise
// pin the journal near the top of the range for good.
//
// Neither kind may reach the value erased flash reads as. All ones is
// the pattern of a sector nobody has written rather than a number
// anybody chose, and a counter parked there stays there: Save cannot go
// past it, so every save afterwards would write the same number, which
// is the one thing counting is for.
func (j *Journal) keepSeq(seq uint32, proven bool) {
	switch {
	case seq == 0xffffffff:
		// Erased flash, not a number.
	case seq <= j.seq:
		// Only ever climbs: a number below the one already seen is an
		// older record, or a sector that holds anything at all.
	case proven:
		j.seq = seq
	case j.seq < 0xfffffffe:
		j.seq++
	}
}

// resume carries the scan past a header it has given up on. It answers
// where to go next and whether there is anything there to read.
//
// Both callers reach it for the same reason — a header that proved
// nothing about itself, whether by a length the sector cannot hold or by
// a checksum that did not hold — and both owe the same two things: the
// sequence number, which climbs by at most one from an unproven header,
// and a position that does not depend on the length that header claimed.
//
// When records resume further down, that is where to go. When they do
// not, this is the ordinary torn write — a reset caught the last save —
// and the next record goes after what is actually written. Taking the
// end of the sector instead threw the rest of it away, so every save
// after a reset cost a full erase, and a second reset in that window
// wiped the settings entirely.
func (j *Journal) resume(buf []byte, off int) (int, bool) {
	if next := resync(buf, off+headerLen); next >= 0 {
		return next, true
	}
	return writeHead(buf, off+headerLen), false
}

// resync finds where the next record starts, at or after off, or -1 when
// nothing further looks like one. Records are written on the flash's
// word granularity, so only those offsets are looked at.
//
// Callers start it past the header they have given up on: a record
// cannot begin inside another record's header, and the four bytes of a
// half-written payload can hold anything — peer names come off the air,
// so a Totem called TTM1 is a magic word sitting in the wreckage.
func resync(buf []byte, off int) int {
	for o := off + pad(off); o+headerLen <= len(buf); o += align {
		if [4]byte(buf[o:o+4]) == magic {
			return o
		}
	}
	return -1
}

// writeHead is where the next record may go when the scan has run out of
// records to read: after everything that is written, rounded up to the
// granularity a write lands on.
//
// Flash only clears bits, so what is written is what is not 0xff, and
// that is a fact about the sector rather than a claim by a header — the
// distinction this whole function exists for. A torn write leaves its
// header and however much of its payload landed; the next save appends
// after that and costs no erase, which is what it cost before any of
// this and what the erase budget in the package comment assumes.
func writeHead(buf []byte, from int) int {
	// from is past the header the scan gave up on, because that is where
	// a later scan will start looking: resync skips a bad header, so a
	// record written inside one would never be found again. The fuzzer
	// found exactly that — four bytes of magic in an otherwise erased
	// sector, a save at offset 4, and a reopen that could not see it.
	last := from
	for i := from; i < len(buf); i++ {
		if buf[i] != 0xff {
			last = i + 1
		}
	}
	return last + pad(last)
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

// Torn is how many headers the scan could not believe — a checksum that
// did not hold, or a length the sector cannot hold — which is a reset
// caught mid-save.
func (j *Journal) Torn() int { return j.torn }

// Blank is how many well-formed records carrying nothing were stepped
// over. A save cannot write one now, so any of these come from an
// earlier build.
func (j *Journal) Blank() int { return j.blank }

// Save appends the state. When the sector has no room it erases and starts
// again, so a save costs an erase only once a sector's worth of them.
func (j *Journal) Save(p []byte) error {
	// Nothing to save is not a save. Load reads "no saved state" off a
	// payload with nothing in it, so writing one reported success and
	// left every reader — and the next scan of the sector — believing it
	// had never held anything: the settings that were there gone, and
	// nobody told.
	if len(p) == 0 {
		return ErrEmptyRecord
	}
	if len(p) > MaxRecord {
		return fmt.Errorf("%w: %d bytes, the record holds %d", ErrTooLarge, len(p), MaxRecord)
	}
	// The next number, which never reaches the value erased flash reads
	// as. Refusing to read all ones and then writing it would be the
	// worst of both: scan skips such a record's number, so a sector of
	// them comes back with the counter at 0, and the save after that
	// writes a number the sector already holds.
	//
	// That is also what stops it wrapping, since the counter can no
	// longer hold the value 1 turns over. Reaching the top takes four
	// billion saves of a sector that erases every few dozen, so this is
	// not a thing that happens — and repeating the last number is a
	// smaller lie than a sector of real records reading as a fresh one.
	next := j.seq + 1
	if next == 0xffffffff {
		next = j.seq
	}
	rec := make([]byte, headerLen+len(p)+pad(len(p)))
	copy(rec, magic[:])
	binary.LittleEndian.PutUint32(rec[4:8], next)
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
	// Everything from here to the end of the sector has to be erased, not
	// only the span this record will occupy. The scan stops at the first
	// thing that is not a record and takes the last one it read as the
	// newest, so anything valid-looking after the write position would
	// come back as newer than what is about to be written — a save that
	// silently does not stick. A previous firmware's layout, a torn write
	// or plain noise can leave exactly that: an erased gap with records
	// beyond it. In normal use the tail is already erased and this costs
	// one read.
	room := j.next+len(rec) <= j.sec.Size()
	erased := false
	if !room || !j.erased(j.next, j.sec.Size()-j.next) {
		if err := j.sec.Erase(); err != nil {
			return fmt.Errorf("store: erasing the sector: %w", err)
		}
		j.next, j.torn, j.blank, erased = 0, 0, 0, true
	}
	at := j.next
	if err := j.sec.WriteAt(rec, at); err != nil {
		// Whatever landed before the driver gave up is written, so the
		// next record cannot go there — leaving the head where it was
		// had the next save find bytes that are not erased and clear the
		// sector, which is the cost this whole design is arranged to
		// avoid.
		//
		// How much landed is a question for the sector, not for the
		// length of the record that was attempted. A driver can refuse
		// before writing a byte — a range check, an alignment check, a
		// lock that would not open — and stepping over the whole record
		// then leaves erased bytes in the middle of the journal, which
		// the scan stops at: every save after it lands past a gap that
		// no reopen can cross, so the settings look like they were never
		// written at all.
		j.next = j.headAfter(at)
		if erased {
			// The sector was cleared and the record that should have
			// replaced its contents did not land, so what this journal
			// held is gone from the flash. Saying otherwise would hand
			// the caller settings the device no longer has, and the next
			// save would compare against a record that is not there.
			j.last = nil
		}
		return fmt.Errorf("store: writing %d bytes at %d: %w", len(rec), at, err)
	}
	j.next += len(rec)
	j.seq = next
	j.last = append([]byte(nil), p...)
	return nil
}

// headAfter is where the next record may go once a write at off has
// failed: after whatever of it reached the flash, which is what the
// sector says rather than what the attempt intended.
//
// A read that fails leaves the head where it was. That is the
// conservative answer — the next save finds bytes it cannot write into
// and erases, which costs a sector but loses nothing.
func (j *Journal) headAfter(off int) int {
	buf := make([]byte, j.sec.Size()-off)
	if err := j.sec.ReadAt(buf, off); err != nil {
		return off
	}
	// Whether a header landed decides where the next record may go, the
	// same way it decides where a scan resumes.
	//
	// One did: a later scan reads it, disbelieves it — the checksum word
	// is the last thing written and will not have landed — and starts
	// looking again past its header, so a record written inside that
	// span would never be found. It goes after the header at the
	// earliest.
	//
	// One did not: then there is nothing here a scan would read as a
	// record, so it stops at these bytes and the head stays where it
	// was. If they are not erased the next save clears the sector, which
	// costs an erase and loses nothing.
	if len(buf) < headerLen || [4]byte(buf[0:4]) != magic {
		return off
	}
	return off + max(writeHead(buf, headerLen), headerLen)
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
