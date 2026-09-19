//go:build tinygo && esp32

package main

// espradio's own ESP-NOW receive trampoline passes nine arguments to Go, so
// clang adjusts the stack around the call with MOVSP. When the Go side has
// spilled the caller's register window, that MOVSP raises AllocaCause
// (EXCCAUSE 5), which TinyGo's ESP32 exception vector treats as fatal (ESP-IDF
// handles it in _xt_alloca_exc). This callback replaces it: it only copies the
// frame into a ring buffer, without calling Go from the WiFi task, and the
// main loop drains the buffer.

/*
#include <stdint.h>
#include <string.h>

typedef int esp_err_t;

// esp_now_recv_info_t from esp_now.h. rx_ctrl points at wifi_pkt_rx_ctrl_t,
// whose first field is "signed rssi: 8".
typedef struct {
	const uint8_t *src_addr;
	const uint8_t *des_addr;
	const void *rx_ctrl;
} totem_recv_info_t;

esp_err_t esp_now_register_recv_cb(void (*cb)(const totem_recv_info_t *, const uint8_t *, int));

#define TOTEM_RX_SLOTS 16
#define TOTEM_RX_MAX 250 // ESP_NOW_MAX_DATA_LEN

typedef struct {
	uint8_t src[6], dst[6];
	int8_t rssi;
	uint8_t len;
	uint8_t data[TOTEM_RX_MAX];
} totem_rx_slot;

static totem_rx_slot totem_rx[TOTEM_RX_SLOTS];
static volatile uint32_t totem_rx_head, totem_rx_tail, totem_rx_dropped;

static void totem_rx_cb(const totem_recv_info_t *info, const uint8_t *data, int len) {
	if (info == NULL || info->src_addr == NULL || info->des_addr == NULL ||
	    data == NULL || len <= 0 || len > TOTEM_RX_MAX) {
		return;
	}
	uint32_t head = totem_rx_head;
	if (head - totem_rx_tail >= TOTEM_RX_SLOTS) {
		totem_rx_dropped++;
		return;
	}
	totem_rx_slot *s = &totem_rx[head % TOTEM_RX_SLOTS];
	memcpy(s->src, info->src_addr, 6);
	memcpy(s->dst, info->des_addr, 6);
	s->rssi = info->rx_ctrl != NULL ? *(const int8_t *)info->rx_ctrl : 0;
	s->len = (uint8_t)len;
	memcpy(s->data, data, len);
	__sync_synchronize();
	totem_rx_head = head + 1;
}

static esp_err_t totem_rx_register(void) {
	return esp_now_register_recv_cb(totem_rx_cb);
}

static int totem_rx_pop(totem_rx_slot *out) {
	uint32_t tail = totem_rx_tail;
	if (tail == totem_rx_head) {
		return 0;
	}
	__sync_synchronize();
	*out = totem_rx[tail % TOTEM_RX_SLOTS];
	totem_rx_tail = tail + 1;
	return 1;
}

static uint32_t totem_rx_lost(void) {
	return totem_rx_dropped;
}
*/
import "C"

import (
	"unsafe"

	"github.com/ljagiello/totem-compass/emulator"
	"github.com/ljagiello/totem-compass/mesh"
)

// registerRX installs the ring-buffer receive callback. It must run after
// espradio.ESPNowInit, which registers espradio's own.
func registerRX() error {
	return check("esp_now_register_recv_cb", C.totem_rx_register())
}

// popRX returns the oldest buffered frame.
func popRX() (emulator.Received, bool) {
	var s C.totem_rx_slot
	if C.totem_rx_pop(&s) == 0 {
		return emulator.Received{}, false
	}
	r := emulator.Received{
		RSSI: int8(s.rssi),
		Data: C.GoBytes(unsafe.Pointer(&s.data[0]), C.int(s.len)),
	}
	copy(r.Src[:], unsafe.Slice((*byte)(unsafe.Pointer(&s.src[0])), len(mesh.MAC{})))
	copy(r.Dst[:], unsafe.Slice((*byte)(unsafe.Pointer(&s.dst[0])), len(mesh.MAC{})))
	return r, true
}

// rxLost counts frames dropped because the ring was full.
func rxLost() uint32 { return uint32(C.totem_rx_lost()) }
