//go:build tinygo && esp32

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
func settingsSector() store.Sector { return flashSector{addr: storeSector} }

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
	log.Info("flash chip", "status", fmt.Sprintf("%#06x", before), "rom_descriptor", flashChipDescriptor())
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
