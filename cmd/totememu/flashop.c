// The flash operations, in their own file so each is a real function with
// a real symbol in IRAM. Two traps are why this file looks the way it
// does, and both cost a boot loop to find:
//
//  1. As a static inline in a header, the code was folded into its caller,
//     which lives in flash. The first instruction fetched after the cache
//     went off was whatever the cache happened to hold: an illegal
//     instruction.
//  2. One function that branched on an op number compiled that chain into
//     a jump table — and a jump table lives in .rodata, which is flash.
//     With the cache off the table read back 0xbad00bad and the CPU
//     jumped there. Hence one function per operation, and no branch
//     inside the window except on a value already in a register.
//
// Everything these functions need is in IRAM, in ROM or on the stack, and
// it must stay that way: no literal pool in flash, no call to a function
// that lives in flash, no memcpy, no table.

#include "flash.h"

// load copies the address table onto the stack while the cache is still
// on. The caller's copy may sit in flash — a Go global the compiler
// decided was read-only ends up there — and reading it once the cache is
// off returns 0xbad00bad.
TOTEM_IRAM static void load(totem_rom_t *dst, const totem_rom_t *src) {
	*dst = *src;
	// Pin the copy here: a volatile store orders other volatile accesses,
	// not ordinary loads, so without this the compiler may sink the reads
	// into the window where flash cannot be read.
	__asm__ volatile("" ::: "memory");
}

TOTEM_IRAM int totem_flash_read(uint32_t addr, uint32_t *data, uint32_t len,
                                const totem_rom_t *table) {
	totem_rom_t rom;
	load(&rom, table);
	rom.cache_off(0);
	int rc = rom.read(addr, data, len);
	rom.cache_on(0);
	return rc;
}

TOTEM_IRAM int totem_flash_write(uint32_t addr, uint32_t *data, uint32_t len,
                                 const totem_rom_t *table) {
	totem_rom_t rom;
	load(&rom, table);
	rom.cache_off(0);
	int rc = rom.write(addr, data, len);
	rom.cache_on(0);
	return rc;
}

TOTEM_IRAM int totem_flash_erase(uint32_t sector, const totem_rom_t *table) {
	totem_rom_t rom;
	load(&rom, table);
	rom.cache_off(0);
	int rc = rom.erase(sector);
	rom.cache_on(0);
	return rc;
}

TOTEM_IRAM int totem_flash_status(uint32_t *out, const totem_rom_t *table) {
	totem_rom_t rom;
	load(&rom, table);
	rom.cache_off(0);
	int rc = rom.read_status(rom.chip, out);
	rom.cache_on(0);
	return rc;
}

// totem_flash_unlock clears the chip's block-protection bits, the way
// ESP-IDF's ROM patch does, keeping the quad-enable bit. Flash ships
// protected: the ROM's erase fails until this has run once.
TOTEM_IRAM int totem_flash_unlock(uint32_t *status_out, const totem_rom_t *table) {
	totem_rom_t rom;
	load(&rom, table);
	rom.cache_off(0);
	uint32_t status = 0;
	int rc = rom.read_status(rom.chip, &status);
	if (rc == TOTEM_FLASH_OK) {
		rom.wait_idle(rom.chip);
		rc = rom.enable_write(rom.chip);
	}
	if (rc == TOTEM_FLASH_OK) {
		rc = rom.write_status(rom.chip, status & TOTEM_FLASH_QE);
		rom.wait_idle(rom.chip);
	}
	if (status_out != 0) {
		*status_out = status;
	}
	rom.cache_on(0);
	return rc;
}
