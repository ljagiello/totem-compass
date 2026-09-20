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
// The cache is gated with the ROM's Cache_Read_Disable and
// Cache_Read_Enable, and nothing else. ESP-IDF's note about the ROM pair
// says a disable wants a Cache_Flush before the enable even when nothing
// was written, but on this image the opposite holds: every sequence with
// a flush in it came back from the window with the next flash fetch
// reading 0xbad00bad, and disable-then-enable on its own comes back
// clean. That was found by bisecting the sequence on the board — a
// no-op that only gated the cache was enough to reproduce it — and the
// driver has since erased, written and read back thousands of records.
//
// The sector this driver touches is past the image and is never mapped,
// so there are no cached lines of it to invalidate either way.
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
// Cache_Read_Disable_rom and Cache_Read_Enable_rom both take the CPU
// number. There is no third: the flush is deliberately absent, for the
// reason at the top of this file.
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
	// The ROM's cache routines. There is no flush: see the note at the
	// top of this file, where leaving it out is the thing that works.
	totem_cache_t cache_off;
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
