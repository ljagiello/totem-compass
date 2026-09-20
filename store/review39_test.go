package store

// A regression for the thirty-ninth review round.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// TestATornRecordOfErasedBytesIsNotARecord: the record checksum covers
// the body and not the header, and crc32 of four 0xff bytes is
// 0xffffffff — which is what the erased checksum field already reads. So
// a save of a 4-byte payload cut short after its header, with body and
// checksum still erased, checked out as a valid record: Load handed back
// four 0xff bytes as the saved state, the good record before it stayed
// hidden, and Torn() reported nothing wrong.
func TestATornRecordOfErasedBytesIsNotARecord(t *testing.T) {
	// The arithmetic the whole thing turns on, stated so a change to it
	// cannot pass quietly.
	if got := crc32.ChecksumIEEE([]byte{0xff, 0xff, 0xff, 0xff}); got != 0xffffffff {
		t.Fatalf("crc32 of four erased bytes is %#x, not the erased value", got)
	}

	sec := newMemSector(4096)
	j, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("the settings that were really saved")
	if err := j.Save(want); err != nil {
		t.Fatal(err)
	}

	// Now tear a second save after its header: magic, sequence and a
	// length of 4 written, body and checksum still erased.
	at := j.next
	var h [headerLen]byte
	copy(h[0:4], magic[:])
	binary.LittleEndian.PutUint32(h[4:8], j.seq+1)
	binary.LittleEndian.PutUint16(h[8:10], 4)
	binary.LittleEndian.PutUint32(h[12:16], 0xffffffff)
	if err := sec.WriteAt(h[:], at); err != nil {
		t.Fatal(err)
	}

	back, err := Open(sec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := back.Load()
	if err != nil {
		t.Fatalf("the journal would not load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("loaded %q, want the record before the tear, %q", got, want)
	}
	if back.Torn() == 0 {
		t.Error("the torn write was not counted")
	}
}

// TestTheOnePayloadARecordCannotCarry: four erased bytes cannot be told
// from the sector they would be written to, so Save refuses rather than
// write something Load will not give back.
func TestTheOnePayloadARecordCannotCarry(t *testing.T) {
	j, err := Open(newMemSector(4096))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save([]byte{0xff, 0xff, 0xff, 0xff}); !errors.Is(err, ErrErasedRecord) {
		t.Errorf("saving four erased bytes: %v, want ErrErasedRecord", err)
	}
	// One byte either side is fine, and so is the same run with anything
	// else in it: the collision is only at this one length.
	for _, p := range [][]byte{
		{0xff, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xfe},
	} {
		if err := j.Save(p); err != nil {
			t.Errorf("saving % x: %v", p, err)
		}
	}
}
