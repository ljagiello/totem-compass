// SPI flash access for the settings store, on the ESP32-S3.
//
// Same shape as flash_esp32.h next door and for the same reason: the code
// runs from flash through the cache, over the same SPI pins the ROM's flash
// driver drives, so the cache has to be off while flash is written — and
// while it is off the CPU cannot fetch an instruction or a constant from
// flash. The operation therefore lives in IRAM (flashop_esp32s3.c), takes
// every address it needs in a struct the caller passes, copies that struct
// onto the stack before the cache goes off, and calls nothing but ROM.
//
// What is different on this chip is the cache, and it is why this is a
// separate file rather than a different table of addresses:
//
//  1. There is no Cache_Read_Disable or Cache_Read_Enable. The S3 has
//     Cache_Suspend_* and Cache_Resume_* instead, and their shape differs:
//     suspend takes nothing and returns the autoload state, resume takes
//     that word back. The ESP32's pair takes a CPU number and returns
//     nothing.
//  2. There are two caches to gate, not one. The S3 has a separate ICache
//     and DCache, and both map flash — instructions through ICache, .rodata
//     in DROM through DCache. Suspending only the ICache would leave every
//     constant readable through a cache whose flash is busy erasing.
//     ESP-IDF suspends both (hal/esp32s3/include/hal/cache_ll.h,
//     cache_ll_suspend_cache with CACHE_TYPE_ALL: ICache then DCache, and
//     resume in the same order), which is the order used here.
//  3. The unlock is one call. On the S3 esp_rom_spiflash_unlock() takes no
//     arguments and clears the protection itself, so the read-modify-write
//     of the status register that flashop_esp32.c has to do by hand — and
//     the four ROM entry points and the chip descriptor it needs for it —
//     are not in this table at all.
//
// Prototypes and semantics are from ESP-IDF's
// components/esp_rom/esp32s3/include/esp32s3/rom/spi_flash.h and
// .../rom/cache.h; the struct layout behind the chip descriptor is from
// components/esp_rom/include/esp_rom_spiflash.h.

#ifndef TOTEM_FLASH_ESP32S3_H
#define TOTEM_FLASH_ESP32S3_H

#include <stdint.h>

// The ROM's flash routines. Read and write take a byte address, a
// word-aligned buffer and a byte length; erase takes a sector number.
typedef int (*totem_s3_rom_rw_t)(uint32_t addr, uint32_t *data, int32_t len);
typedef int (*totem_s3_rom_erase_t)(uint32_t sector);
// esp_rom_spiflash_unlock() clears the block protection on its own.
typedef int (*totem_s3_rom_unlock_t)(void);
// esp_rom_spiflash_read_status(chip, &status) reads the status register; it
// is only used for the console's flash diagnostic.
typedef int (*totem_s3_rom_read_status_t)(void *chip, uint32_t *status);

// The cache gates. Suspend returns "auto preload enabled before", which is
// exactly what resume wants back — so the state word is never interpreted
// here, only carried from one call to the other.
typedef uint32_t (*totem_s3_cache_suspend_t)(void);
typedef void (*totem_s3_cache_resume_t)(uint32_t autoload);

typedef struct {
	totem_s3_rom_rw_t read;
	totem_s3_rom_rw_t write;
	totem_s3_rom_erase_t erase;
	totem_s3_rom_unlock_t unlock;
	totem_s3_rom_read_status_t read_status;
	// chip is the ROM's esp_rom_spiflash_chip_t, which on this chip is
	// reached through a pointer in SRAM rather than being a fixed address:
	// see the Go file, which dereferences it.
	void *chip;
	totem_s3_cache_suspend_t suspend_icache;
	totem_s3_cache_resume_t resume_icache;
	totem_s3_cache_suspend_t suspend_dcache;
	totem_s3_cache_resume_t resume_dcache;
} totem_s3_rom_t;

// ESP_ROM_SPIFLASH_RESULT_OK.
#define TOTEM_S3_FLASH_OK 0

// TOTEM_S3_IRAM puts a function in internal RAM, where it can still be
// fetched with the caches suspended.
#define TOTEM_S3_IRAM __attribute__((section(".iram1.totem_flash_s3"), noinline))

// The operations, one function each, all defined in flashop_esp32s3.c.
// addr and len are in bytes and must be multiples of four, as must data's
// alignment; erase takes a sector number. Each returns the ROM's result,
// where 0 is success.
int totem_s3_flash_read(uint32_t addr, uint32_t *data, uint32_t len, const totem_s3_rom_t *rom);
int totem_s3_flash_write(uint32_t addr, uint32_t *data, uint32_t len, const totem_s3_rom_t *rom);
int totem_s3_flash_erase(uint32_t sector, const totem_s3_rom_t *rom);
int totem_s3_flash_status(uint32_t *out, const totem_s3_rom_t *rom);
int totem_s3_flash_unlock(const totem_s3_rom_t *rom);

#endif
