//go:build tinygo && esp32

package main

// The settings store's flash backend. A Totem keeps config.json on a
// filesystem; this board has none, so the journal in the store package
// gets one erase block near the end of the 4 MB flash, far past the image.

/*
#include "flash.h"
*/
import "C"

import (
	"fmt"
	"runtime/interrupt"
	"unsafe"

	"github.com/ljagiello/totem-compass/store"
)

// The ESP32 ROM's flash and cache entry points, from ESP-IDF's
// components/esp_rom/esp32/ld/esp32.rom.spiflash.ld (the flash routines)
// and esp32.rom.ld (the cache routines and the chip descriptor). The ROM
// driver is already configured for this board's flash: the bootloader set
// it up to read the image in the first place.
const (
	romSPIFlashRead        = 0x40062ed8
	romSPIFlashWrite       = 0x40062d50
	romSPIFlashErase       = 0x40062ccc
	romSPIFlashReadStatus  = 0x4006226c
	romSPIFlashWriteStatus = 0x400622f0
	romSPIFlashEnableWrite = 0x40062320
	romSPIFlashWaitIdle    = 0x400622c0
	romFlashChip           = 0x3ffae270 // g_rom_flashchip
	romCacheReadDis        = 0x40009ab8 // Cache_Read_Disable_rom
	romCacheReadEnabl      = 0x40009a84 // Cache_Read_Enable_rom
)

const (
	// flashSectorSize is the ESP32's erase unit.
	flashSectorSize = 4096
	// storeSector holds the settings journal, and scratchSector is where
	// the self-test writes.
	//
	// They sit just under 2 MB, not at the end of the 4 MB part, because
	// the ROM's flash driver refuses an erase past the chip_size in its
	// own descriptor — and on this board the bootloader left that at
	// 2 MB even though the chip is 4 MB. The image starts at 0x10000 and
	// is about 1.1 MB, so there is room either side.
	storeSector   = 0x1f0000
	scratchSector = 0x1f1000
)

// romTable is the address table the IRAM helpers work from. It is built
// once and handed to them: with the cache off they cannot read a constant
// out of flash, so every address they need has to arrive as data.
var romTable = C.totem_rom_t{
	read:         C.totem_rom_rw_t(unsafe.Pointer(uintptr(romSPIFlashRead))),
	write:        C.totem_rom_rw_t(unsafe.Pointer(uintptr(romSPIFlashWrite))),
	erase:        C.totem_rom_erase_t(unsafe.Pointer(uintptr(romSPIFlashErase))),
	read_status:  C.totem_rom_read_status_t(unsafe.Pointer(uintptr(romSPIFlashReadStatus))),
	write_status: C.totem_rom_write_status_t(unsafe.Pointer(uintptr(romSPIFlashWriteStatus))),
	enable_write: C.totem_rom_chip_t(unsafe.Pointer(uintptr(romSPIFlashEnableWrite))),
	wait_idle:    C.totem_rom_chip_t(unsafe.Pointer(uintptr(romSPIFlashWaitIdle))),
	chip:         unsafe.Pointer(uintptr(romFlashChip)),
	cache_off:    C.totem_cache_t(unsafe.Pointer(uintptr(romCacheReadDis))),
	cache_on:     C.totem_cache_t(unsafe.Pointer(uintptr(romCacheReadEnabl))),
}

// unlocked records that the chip's block protection has been cleared.
// Flash ships write protected — this board refused every erase until the
// status register said otherwise — and the bits are not volatile, so
// clearing them once per boot is enough.
var unlocked bool

// flashSector is one erase block, as the store package's Sector.
type flashSector struct {
	addr uint32
}

// compile-time check that the backend satisfies the store.
var _ store.Sector = flashSector{}

func (f flashSector) Size() int { return flashSectorSize }

func (f flashSector) ReadAt(p []byte, off int) error {
	if off < 0 || off+len(p) > flashSectorSize {
		return fmt.Errorf("flash: read of %d bytes at %d is outside the sector", len(p), off)
	}
	if len(p) == 0 {
		return nil
	}
	// The ROM reads whole words from a word-aligned address, so read the
	// window that covers the request and copy out the part asked for.
	start := off &^ 3
	buf := make([]uint32, (off+len(p)-start+3)/4)
	addr := f.addr + uint32(start)
	rc := withInterruptsOff(func() C.int {
		return C.totem_flash_read(C.uint32_t(addr), words(buf), C.uint32_t(len(buf)*4), &romTable)
	})
	if err := result("read", addr, rc); err != nil {
		return err
	}
	copy(p, wordBytes(buf)[off-start:])
	return nil
}

func (f flashSector) WriteAt(p []byte, off int) error {
	// Checked before the unlock, as ReadAt checks before the read: the
	// unlock clears the chip's block protection for the rest of the boot,
	// and a request this driver is going to refuse should leave the chip
	// exactly as it found it.
	if off < 0 || off+len(p) > flashSectorSize {
		return fmt.Errorf("flash: write of %d bytes at %d is outside the sector", len(p), off)
	}
	if len(p) == 0 {
		return nil
	}
	if off%4 != 0 {
		return fmt.Errorf("flash: write at %d is not word aligned", off)
	}
	if err := flashUnlock(); err != nil {
		return err
	}
	// Pad with 0xff, the erased value, so the bytes past the record stay
	// writable.
	buf := make([]uint32, (len(p)+3)/4)
	b := wordBytes(buf)
	for i := range b {
		b[i] = 0xff
	}
	copy(b, p)
	addr := f.addr + uint32(off)
	rc := withInterruptsOff(func() C.int {
		return C.totem_flash_write(C.uint32_t(addr), words(buf), C.uint32_t(len(buf)*4), &romTable)
	})
	return result("write", addr, rc)
}

func (f flashSector) Erase() error {
	if err := flashUnlock(); err != nil {
		return err
	}
	sector := f.addr / flashSectorSize
	rc := withInterruptsOff(func() C.int {
		return C.totem_flash_erase(C.uint32_t(sector), &romTable)
	})
	return result("erase", f.addr, rc)
}

// flashChipDescriptor is the ROM's own esp_rom_spiflash_chip_t: device
// id, chip size, block, sector and page size, and the status mask. The
// ROM's erase and write check it, so a zeroed one refuses everything.
func flashChipDescriptor() []uint32 {
	d := (*[6]uint32)(unsafe.Pointer(uintptr(romFlashChip)))
	return d[:]
}

// flashStatus reads the chip's status register, whose low bits say how
// much of the flash is write protected.
func flashStatus() (uint32, error) {
	out := make([]uint32, 1)
	rc := withInterruptsOff(func() C.int {
		return C.totem_flash_status(words(out), &romTable)
	})
	return out[0], result("status", 0, rc)
}

// flashUnlock clears the block protection, once per boot.
func flashUnlock() error {
	if unlocked {
		return nil
	}
	out := make([]uint32, 1)
	rc := withInterruptsOff(func() C.int {
		return C.totem_flash_unlock(words(out), &romTable)
	})
	if err := result("unlock", 0, rc); err != nil {
		return err
	}
	unlocked = true
	return nil
}

// withInterruptsOff runs one flash operation with interrupts masked.
// Everything that could run from flash has to be held off for its
// duration: the helper turns the cache off, and a handler fetching its
// next instruction from flash while it is off would fault.
//
// An erase takes tens of milliseconds, which is a long time to keep the
// radio waiting, so the settings are only written when a bond or a
// setting actually changed.
func withInterruptsOff(op func() C.int) C.int {
	state := interrupt.Disable()
	rc := op()
	interrupt.Restore(state)
	return rc
}

func result(what string, addr uint32, rc C.int) error {
	if rc != C.TOTEM_FLASH_OK {
		return fmt.Errorf("flash: %s at %#x failed with %d", what, addr, int(rc))
	}
	return nil
}

// words is the buffer as the ROM wants it: a word-aligned pointer, which
// a byte slice does not promise.
func words(w []uint32) *C.uint32_t {
	if len(w) == 0 {
		return nil
	}
	return (*C.uint32_t)(unsafe.Pointer(&w[0]))
}

// wordBytes views a word buffer as bytes.
func wordBytes(w []uint32) []byte {
	if len(w) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&w[0])), len(w)*4)
}
