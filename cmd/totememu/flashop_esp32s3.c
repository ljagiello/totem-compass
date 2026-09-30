// The ESP32-S3 flash operations, in their own file so each is a real
// function with a real symbol in IRAM. The two traps written up in
// flashop_esp32.c apply here unchanged and are worth repeating, because
// both cost a boot loop on that chip:
//
//  1. As a static inline in a header, the code folds into its caller, which
//     lives in flash. The first instruction fetched after the cache went
//     off was then whatever the cache happened to hold.
//  2. A function that branches on an op number compiles the chain into a
//     jump table, and a jump table lives in .rodata, which is flash. Hence
//     one function per operation, and no branch inside the window except on
//     a value already in a register.
//
// Everything these functions need is in IRAM, in ROM or on the stack, and
// it must stay that way: no literal pool in flash, no call to a function
// that lives in flash, no memcpy, no table.

#include "flash_esp32s3.h"

// load copies the address table onto the stack while the caches are still
// on. The caller's copy may sit in flash — a Go global the compiler decided
// was read-only ends up in .rodata, which is DROM — and reading it once the
// DCache is suspended does not return the table.
TOTEM_S3_IRAM static void load(totem_s3_rom_t *dst, const totem_s3_rom_t *src) {
	*dst = *src;
	// Pin the copy here: a volatile store orders other volatile accesses,
	// not ordinary loads, so without this the compiler may sink the reads
	// into the window where flash cannot be read.
	__asm__ volatile("" ::: "memory");
}

// The window. Both caches are suspended, in ESP-IDF's order, and each
// resume is handed back the autoload word its own suspend returned.
//
// Kept as two one-line helpers rather than a macro so the sequence exists
// in one place: every operation below is suspend, call, resume, and a
// missing resume would leave the chip running without caches.
TOTEM_S3_IRAM static void suspend(const totem_s3_rom_t *rom, uint32_t *i, uint32_t *d) {
	*i = rom->suspend_icache();
	*d = rom->suspend_dcache();
}

TOTEM_S3_IRAM static void resume(const totem_s3_rom_t *rom, uint32_t i, uint32_t d) {
	rom->resume_icache(i);
	rom->resume_dcache(d);
}

TOTEM_S3_IRAM int totem_s3_flash_read(uint32_t addr, uint32_t *data, uint32_t len,
                                      const totem_s3_rom_t *table) {
	totem_s3_rom_t rom;
	uint32_t ia, da;
	load(&rom, table);
	suspend(&rom, &ia, &da);
	int rc = rom.read(addr, data, (int32_t)len);
	resume(&rom, ia, da);
	return rc;
}

TOTEM_S3_IRAM int totem_s3_flash_write(uint32_t addr, uint32_t *data, uint32_t len,
                                       const totem_s3_rom_t *table) {
	totem_s3_rom_t rom;
	uint32_t ia, da;
	load(&rom, table);
	suspend(&rom, &ia, &da);
	int rc = rom.write(addr, data, (int32_t)len);
	resume(&rom, ia, da);
	return rc;
}

TOTEM_S3_IRAM int totem_s3_flash_erase(uint32_t sector, const totem_s3_rom_t *table) {
	totem_s3_rom_t rom;
	uint32_t ia, da;
	load(&rom, table);
	suspend(&rom, &ia, &da);
	int rc = rom.erase(sector);
	resume(&rom, ia, da);
	return rc;
}

TOTEM_S3_IRAM int totem_s3_flash_status(uint32_t *out, const totem_s3_rom_t *table) {
	totem_s3_rom_t rom;
	uint32_t ia, da;
	load(&rom, table);
	suspend(&rom, &ia, &da);
	int rc = rom.read_status(rom.chip, out);
	resume(&rom, ia, da);
	return rc;
}

// totem_s3_flash_unlock clears the chip's block-protection bits. Flash
// ships protected, and the ROM's erase fails until this has run once.
//
// One ROM call, unlike the ESP32's read-modify-write of the status
// register: esp_rom_spiflash_unlock() on this chip reads the status, keeps
// the quad-enable bit and clears the rest itself.
TOTEM_S3_IRAM int totem_s3_flash_unlock(const totem_s3_rom_t *table) {
	totem_s3_rom_t rom;
	uint32_t ia, da;
	load(&rom, table);
	suspend(&rom, &ia, &da);
	int rc = rom.unlock();
	resume(&rom, ia, da);
	return rc;
}
