//go:build cgo

// Package ring compiles the firmware's ESP-NOW receive ring (ring.h) for
// the host, so it can be tested and fuzzed. The ring is C because it runs
// in the WiFi task, where calling Go crashes TinyGo on the ESP32 (see
// cmd/totememu/rx.go); the firmware includes the same header.
package ring

/*
#cgo CFLAGS: -I${SRCDIR}/..
#include "ring.h"

// reset empties the ring between test cases.
static void totem_rx_reset(void) {
	totem_rx_head = totem_rx_tail = totem_rx_dropped = 0;
}
*/
import "C"

import "unsafe"

// Slots and MaxFrame are the ring's capacity and the largest frame it
// takes.
const (
	Slots    = C.TOTEM_RX_SLOTS
	MaxFrame = C.TOTEM_RX_MAX
)

// Reset empties the ring.
func Reset() { C.totem_rx_reset() }

// Push offers one frame, as the ESP-NOW callback does.
func Push(src, dst [6]byte, rssi int8, data []byte) {
	var p *C.uint8_t
	if len(data) > 0 {
		p = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	C.totem_rx_push((*C.uint8_t)(unsafe.Pointer(&src[0])), (*C.uint8_t)(unsafe.Pointer(&dst[0])),
		C.int8_t(rssi), p, C.int(len(data)))
}

// Pop takes the oldest frame, as the main loop does.
func Pop() (src, dst [6]byte, rssi int8, data []byte, ok bool) {
	var s C.totem_rx_slot
	if C.totem_rx_pop(&s) == 0 {
		return src, dst, 0, nil, false
	}
	copy(src[:], unsafe.Slice((*byte)(unsafe.Pointer(&s.src)), len(src)))
	copy(dst[:], unsafe.Slice((*byte)(unsafe.Pointer(&s.dst)), len(dst)))
	return src, dst, int8(s.rssi), C.GoBytes(unsafe.Pointer(&s.data), C.int(s.len)), true
}

// Lost counts the frames dropped because the ring was full.
func Lost() int { return int(C.totem_rx_lost()) }
