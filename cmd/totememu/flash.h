// SPI flash access for the settings store.
//
// On the ESP32 the code runs from flash through the MMU cache, over the
// same SPI peripheral the ROM's flash driver drives. Touching flash
// therefore means turning the cache off first — and while it is off the
// CPU cannot fetch a single instruction or constant from flash. So the
// operation lives in IRAM (flashop.c), takes every address it needs in a
// struct the caller passes, copies that struct onto the stack before the
// cache goes off, and calls nothing but ROM routines.
//
// The cache is gated with the ROM's Cache_Read_Disable, Cache_Flush and
// Cache_Read_Enable. The flush between them is not optional: ESP-IDF
// notes that a disable needs a flush before the enable even when nothing
// was written, and a version of this file that left it out came back
// from the window with every flash fetch reading 0xbad00bad.
//
// The caller disables interrupts around it: a handler that happens to
// live in flash would fault the same way.

#ifndef TOTEM_FLASH_H
#define TOTEM_FLASH_H

#include <stdint.h>

// The ROM's flash routines, with the signatures from ESP-IDF's
// components/esp_rom/include/esp32/rom/spi_flash.h. The chip argument is
// the ROM's own g_rom_flashchip, which the bootloader filled in for this
// board's flash before the image ran.
typedef int (*totem_rom_rw_t)(uint32_t addr, uint32_t *data, uint32_t len);
typedef int (*totem_rom_erase_t)(uint32_t sector);
typedef int (*totem_rom_read_status_t)(void *chip, uint32_t *status);
typedef int (*totem_rom_write_status_t)(void *chip, uint32_t status);
typedef int (*totem_rom_chip_t)(void *chip);
// Cache_Read_Disable_rom, Cache_Flush_rom and Cache_Read_Enable_rom all
// take the CPU number.
typedef void (*totem_cache_t)(int cpu);

typedef struct {
	totem_rom_rw_t read;
	totem_rom_rw_t write;
	totem_rom_erase_t erase;
	totem_rom_read_status_t read_status;
	totem_rom_write_status_t write_status;
	totem_rom_chip_t enable_write;
	totem_rom_chip_t wait_idle;
	void *chip;
	// The ROM's cache routines. Flushing between the disable and the
	// enable is not optional: ESP-IDF notes that Cache_Read_Disable
	// needs a Cache_Flush before Cache_Read_Enable even when nothing was
	// written, and without it this board came back from the window with
	// every flash fetch reading 0xbad00bad.
	totem_cache_t cache_off;
	totem_cache_t cache_flush;
	totem_cache_t cache_on;
} totem_rom_t;

// ESP_ROM_SPIFLASH_RESULT_OK.
#define TOTEM_FLASH_OK 0

// TOTEM_FLASH_QE is the quad-enable bit of the status register. The
// unlock clears every protection bit and keeps this one, as ESP-IDF's
// esp_rom_spiflash_unlock does: a chip running in quad mode stops
// answering if it is cleared. This board's image is DIO, but the rule
// costs nothing and a QIO board would need it.
#define TOTEM_FLASH_QE 0x0200

// TOTEM_IRAM puts a function in internal RAM. Code that runs with the
// flash cache off has to be there: the CPU cannot fetch from flash while
// the cache is disabled, and what it fetches instead is nonsense.
#define TOTEM_IRAM __attribute__((section(".iram1.totem_flash"), noinline))

// The operations, one function each, all defined in flashop.c. addr and
// len are in bytes and must be multiples of four, as must data's
// alignment; erase takes a sector number. Each returns the ROM's result,
// where 0 is success.
int totem_flash_read(uint32_t addr, uint32_t *data, uint32_t len, const totem_rom_t *rom);
int totem_flash_write(uint32_t addr, uint32_t *data, uint32_t len, const totem_rom_t *rom);
int totem_flash_erase(uint32_t sector, const totem_rom_t *rom);
int totem_flash_status(uint32_t *out, const totem_rom_t *rom);
int totem_flash_unlock(uint32_t *status_out, const totem_rom_t *rom);

#endif
