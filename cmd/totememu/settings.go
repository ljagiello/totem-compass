//go:build tinygo && (esp32 || esp32s3)

package main

// The flash driver's own self-test. What a reboot remembers lives in the
// settings package beside this one, which is plain Go over a sector and
// can be tested on a host; this is the part that cannot be.

import (
	"bytes"
	"fmt"
	"log/slog"
	"time"

	"github.com/ljagiello/totem-compass/store"
)

// storeAtBoot says whether the settings are read while the device starts.
// Turning it off leaves the board bootable while the flash driver is
// being worked on: the store open command then reads them by hand.
const storeAtBoot = true

// settingsSector is where the settings live. One function rather than
// the literal in each place that needs it: `store open` built its own
// copy, and two spellings of the same sector are two sectors as soon as
// one of them changes.
func settingsSector() store.Sector {
	// A chip whose flash driver is not proven yet hands Open nothing, so
	// it answers ErrNoSector and the node runs on defaults without
	// saving. The settings sector is read during boot, before there is a
	// console to report anything, so an unproven driver must not be
	// reached from here — see storeReady in the per-chip flash file.
	if !storeReady {
		return nil
	}
	return flashSector{addr: storeSector}
}

// describeChip names the words of the ROM's flash descriptor. Anything but
// the six it should have is handed back as it came, because a descriptor of
// the wrong shape is worth seeing rather than mislabelling.
func describeChip(d []uint32) string {
	if len(d) != 6 {
		return fmt.Sprintf("%v", d)
	}
	return fmt.Sprintf("device %#x, chip %d KB, block %d B, sector %d B, page %d B, status mask %#x",
		d[0], d[1]/1024, d[2], d[3], d[4], d[5])
}

// flashSelfTest checks the flash driver on the scratch sector: erase, read
// back, write a pattern, read it again. It never touches the settings
// sector, so a board that fails it still boots with its bonds.
func flashSelfTest(log *slog.Logger) {
	sec := flashSector{addr: scratchSector}
	step := func(what string, err error) bool {
		if err != nil {
			log.Warn("flash test failed", "step", what, "err", err)
			return false
		}
		return true
	}
	before, err := flashStatus()
	if !step("status", err) {
		return
	}
	// Named and in hex, because the raw words are unreadable twice over: a
	// device id of 13123607 says nothing where 0xc84017 is the part number
	// on the chip, and the console's JSON carries a word array out as
	// floating point, so it reached the operator as 1.3123607e+07.
	log.Info("flash chip", "status", fmt.Sprintf("%#06x", before), "rom", describeChip(flashChipDescriptor()))
	start := time.Now()
	if !step("erase", sec.Erase()) {
		return
	}
	erase := time.Since(start)
	buf := make([]byte, 64)
	if !step("read after erase", sec.ReadAt(buf, 0)) {
		return
	}
	for i, b := range buf {
		if b != 0xff {
			log.Warn("flash test failed", "step", "erase left data", "offset", i, "byte", b)
			return
		}
	}
	want := make([]byte, 64)
	for i := range want {
		want[i] = byte(i*7 + 1)
	}
	start = time.Now()
	if !step("write", sec.WriteAt(want, 0)) {
		return
	}
	write := time.Since(start)
	got := make([]byte, 64)
	if !step("read back", sec.ReadAt(got, 0)) {
		return
	}
	if !bytes.Equal(got, want) {
		log.Warn("flash test failed", "step", "read back",
			"want", fmt.Sprintf("%x", want), "got", fmt.Sprintf("%x", got))
		return
	}
	// An unaligned read has to come back right too: the journal reads
	// headers wherever they land.
	if !step("read at 3", sec.ReadAt(got[:16], 3)) {
		return
	}
	if !bytes.Equal(got[:16], want[3:19]) {
		log.Warn("flash test failed", "step", "unaligned read", "got", fmt.Sprintf("%x", got[:16]))
		return
	}
	log.Info("flash test passed", "sector", fmt.Sprintf("%#x", scratchSector),
		"erase_ms", erase.Milliseconds(), "write_us", write.Microseconds())
}
