//go:build tinygo && esp32s3

package main

// The settings store's flash backend on the ESP32-S3. A Totem keeps
// config.json on a filesystem; this board has none, so the journal in the
// store package gets one erase block past the image.
//
// flash_esp32s3.h explains why this is a separate driver rather than the
// ESP32 one with different numbers: the S3 has no Cache_Read_Disable at
// all, it has two caches to gate instead of one, and its suspend and
// resume have a different shape from the ESP32's pair.
//
// The helpers at the bottom are near-copies of the ESP32 driver's. They
// are not shared, deliberately: sharing them means editing the bodies of
// operations on that chip, which is proven on hardware and cannot be
// re-flashed from here today — and on that chip merely moving a call to
// the flash driver has broken every boot once already. Twenty lines of
// duplication is the cheaper risk.

/*
#include "flash_esp32s3.h"
*/
import "C"

import (
	"fmt"
	"runtime/interrupt"
	"unsafe"

	"github.com/ljagiello/totem-compass/store"
)

// The ESP32-S3 ROM's flash and cache entry points, read off ESP-IDF's
// components/esp_rom/esp32s3/ld/esp32s3.rom.ld on two branches — master
// and release/v5.3 — which give the same address for every one of them.
// The ROM driver is already configured for this board's flash: the
// bootloader set it up to read the image in the first place.
//
// They are written down here rather than declared extern because TinyGo's
// targets/esp32s3.ld provides WiFi ROM symbols only — it has not one
// esp_rom_spiflash or Cache_ entry — so there is nothing for the linker to
// resolve against. (The esp32c3 target does have them, which is how
// TinyGo's own machine_esp32c3_flash.go can declare them extern.)
const (
	romSPIFlashRead       = 0x40000a20 // esp_rom_spiflash_read
	romSPIFlashWrite      = 0x40000a14 // esp_rom_spiflash_write
	romSPIFlashErase      = 0x400009fc // esp_rom_spiflash_erase_sector
	romSPIFlashUnlock     = 0x40000a2c // esp_rom_spiflash_unlock
	romSPIFlashReadStatus = 0x40000ab0 // esp_rom_spiflash_read_status
	romSuspendICache      = 0x4000189c // rom_Cache_Suspend_ICache
	romResumeICache       = 0x400018a8 // Cache_Resume_ICache
	romSuspendDCache      = 0x400018b4 // rom_Cache_Suspend_DCache
	romResumeDCache       = 0x400018c0 // Cache_Resume_DCache

	// romLegacyData is rom_spiflash_legacy_data, and unlike the ESP32's
	// g_rom_flashchip it is a *pointer* in SRAM, not the struct itself. On
	// this chip ESP-IDF defines the chip descriptor as
	// rom_spiflash_legacy_data->chip (components/esp_rom/include/
	// esp_rom_spiflash.h), where the pointed-at struct starts with the
	// esp_rom_spiflash_chip_t — so one dereference lands on it.
	romLegacyData = 0x3fceffe4
)

// storeReady says whether the flash driver here has been proven on this
// chip. It has: the `flash` console command's self-test passes on the
// board — erase, read back all 0xff, write a pattern, read it again, and
// an unaligned read of it — with the ROM descriptor coming back as device
// 0xc84017, 4096-byte sectors, an erase at 13 ms and a 64-byte write at
// 141 µs.
//
// So this is true, and so is the esp32 file's copy, which means the branch
// they guard in settingsSector cannot currently be taken. The gate is kept
// for the next chip rather than for these two; what follows is what it does
// when it is false.
//
// While it is false the node runs without saving: settingsSector hands
// store.Open nothing, Open answers ErrNoSector, and settings.Open logs
// "no settings sector on this board, running without saving" and carries
// on with defaults. The settings sector is read during boot, before there
// is a console to report anything, so a wrong address here would not
// produce a message — it would produce a board that will not start, with
// nothing to say why. The self-test comes first, then this.
const storeReady = true

const (
	// flashSectorSize is the erase unit.
	flashSectorSize = 4096
	// storeSector holds the settings journal, and scratchSector is where
	// the self-test writes.
	//
	// The same two sectors as the ESP32 board, just under 2 MB. This part
	// is 8 MB (GigaDevice c8 4017, read off the chip with esptool) and the
	// image is about 1.05 MB from 0, so there is room either side — and
	// staying under 2 MB also keeps them inside the chip_size the ROM's
	// own descriptor carries, which its erase refuses to write past.
	storeSector   = 0x1f0000
	scratchSector = 0x1f1000
)

// romTable is the address table the IRAM helpers work from. It is built
// once and handed to them: with the caches suspended they cannot read a
// constant out of flash, so every address they need has to arrive as data.
//
// chip is missing here on purpose, and filled in by chipPointer below.
var romTable = C.totem_s3_rom_t{
	read:           C.totem_s3_rom_rw_t(unsafe.Pointer(uintptr(romSPIFlashRead))),
	write:          C.totem_s3_rom_rw_t(unsafe.Pointer(uintptr(romSPIFlashWrite))),
	erase:          C.totem_s3_rom_erase_t(unsafe.Pointer(uintptr(romSPIFlashErase))),
	unlock:         C.totem_s3_rom_unlock_t(unsafe.Pointer(uintptr(romSPIFlashUnlock))),
	read_status:    C.totem_s3_rom_read_status_t(unsafe.Pointer(uintptr(romSPIFlashReadStatus))),
	suspend_icache: C.totem_s3_cache_suspend_t(unsafe.Pointer(uintptr(romSuspendICache))),
	resume_icache:  C.totem_s3_cache_resume_t(unsafe.Pointer(uintptr(romResumeICache))),
	suspend_dcache: C.totem_s3_cache_suspend_t(unsafe.Pointer(uintptr(romSuspendDCache))),
	resume_dcache:  C.totem_s3_cache_resume_t(unsafe.Pointer(uintptr(romResumeDCache))),
}

// chipPointer reads rom_spiflash_legacy_data and caches the descriptor it
// points at in the table.
//
// It cannot be part of the initialiser above: TinyGo evaluates
// package-level initialisers at compile time, where romLegacyData is not
// memory but an address in a chip that is not there. (TinyGo's own USB
// driver for this chip leaves initUSB empty for the same reason.) So the
// dereference happens on first use, on the board.
func chipPointer() unsafe.Pointer {
	if romTable.chip == nil {
		romTable.chip = *(*unsafe.Pointer)(unsafe.Pointer(uintptr(romLegacyData)))
	}
	return romTable.chip
}

// unlocked records that the chip's block protection has been cleared.
// Flash ships write protected and the bits are not volatile, so clearing
// them once per boot is enough.
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
		return C.totem_s3_flash_read(C.uint32_t(addr), words(buf), C.uint32_t(len(buf)*4), &romTable)
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
		return C.totem_s3_flash_write(C.uint32_t(addr), words(buf), C.uint32_t(len(buf)*4), &romTable)
	})
	return result("write", addr, rc)
}

func (f flashSector) Erase() error {
	if err := flashUnlock(); err != nil {
		return err
	}
	sector := f.addr / flashSectorSize
	rc := withInterruptsOff(func() C.int {
		return C.totem_s3_flash_erase(C.uint32_t(sector), &romTable)
	})
	return result("erase", f.addr, rc)
}

// flashChipDescriptor is the ROM's own esp_rom_spiflash_chip_t: device id,
// chip size, block, sector and page size, and the status mask. The ROM's
// erase and write check it, so a zeroed one refuses everything.
func flashChipDescriptor() []uint32 {
	p := chipPointer()
	if p == nil {
		return nil
	}
	d := (*[6]uint32)(p)
	return d[:]
}

// flashStatus reads the chip's status register, whose low bits say how
// much of the flash is write protected.
func flashStatus() (uint32, error) {
	if chipPointer() == nil {
		return 0, fmt.Errorf("flash: the ROM's chip descriptor pointer at %#x is nil", romLegacyData)
	}
	out := make([]uint32, 1)
	rc := withInterruptsOff(func() C.int {
		return C.totem_s3_flash_status(words(out), &romTable)
	})
	return out[0], result("status", 0, rc)
}

// flashUnlock clears the block protection, once per boot. One ROM call on
// this chip: esp_rom_spiflash_unlock takes no arguments and keeps the
// quad-enable bit itself, where the ESP32 needs the status register read,
// masked and written back by hand.
func flashUnlock() error {
	if unlocked {
		return nil
	}
	rc := withInterruptsOff(func() C.int {
		return C.totem_s3_flash_unlock(&romTable)
	})
	if err := result("unlock", 0, rc); err != nil {
		return err
	}
	unlocked = true
	return nil
}

// withInterruptsOff runs one flash operation with interrupts masked.
// Everything that could run from flash has to be held off for its
// duration: the helper suspends both caches, and a handler fetching its
// next instruction from flash while they are suspended would fault.
//
// An erase takes tens of milliseconds, which is a long time to keep the
// radio waiting, so the settings are only written when a bond or a setting
// actually changed.
func withInterruptsOff(op func() C.int) C.int {
	state := interrupt.Disable()
	rc := op()
	interrupt.Restore(state)
	return rc
}

func result(what string, addr uint32, rc C.int) error {
	if rc != C.TOTEM_S3_FLASH_OK {
		return fmt.Errorf("flash: %s at %#x failed with %d", what, addr, int(rc))
	}
	return nil
}

// words is the buffer as the ROM wants it: a word-aligned pointer, which a
// byte slice does not promise.
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
