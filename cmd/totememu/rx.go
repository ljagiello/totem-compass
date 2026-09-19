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
#include "ring.h"

typedef int esp_err_t;

// esp_now_recv_info_t from esp_now.h. rx_ctrl points at wifi_pkt_rx_ctrl_t,
// whose first field is "signed rssi: 8".
typedef struct {
	const uint8_t *src_addr;
	const uint8_t *des_addr;
	const void *rx_ctrl;
} totem_recv_info_t;

esp_err_t esp_now_register_recv_cb(void (*cb)(const totem_recv_info_t *, const uint8_t *, int));

static void totem_rx_cb(const totem_recv_info_t *info, const uint8_t *data, int len) {
	if (info == NULL) {
		return;
	}
	int8_t rssi = info->rx_ctrl != NULL ? *(const int8_t *)info->rx_ctrl : 0;
	totem_rx_push(info->src_addr, info->des_addr, rssi, data, len);
}

static esp_err_t totem_rx_register(void) {
	return esp_now_register_recv_cb(totem_rx_cb);
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
