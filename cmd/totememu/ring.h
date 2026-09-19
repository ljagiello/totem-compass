// The ESP-NOW receive ring. espradio's own trampoline calls Go from the
// WiFi task, which crashes TinyGo on the ESP32 (see rx.go), so the callback
// only copies each frame in here and the main loop drains it.
//
// The C lives in a header so the firmware and the host fuzz test
// (ring_host_test.go) compile the same code.

#ifndef TOTEM_RING_H
#define TOTEM_RING_H

#include <stdint.h>
#include <string.h>

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

// totem_rx_push copies one frame into the ring. It drops a frame that is
// too long for an ESP-NOW payload or that arrives with the ring full, so
// the WiFi task never blocks and never writes past a slot.
static void totem_rx_push(const uint8_t *src, const uint8_t *dst, int8_t rssi, const uint8_t *data, int len) {
	if (src == NULL || dst == NULL || data == NULL || len <= 0 || len > TOTEM_RX_MAX) {
		return;
	}
	uint32_t head = totem_rx_head;
	if (head - totem_rx_tail >= TOTEM_RX_SLOTS) {
		totem_rx_dropped++;
		return;
	}
	totem_rx_slot *s = &totem_rx[head % TOTEM_RX_SLOTS];
	memcpy(s->src, src, 6);
	memcpy(s->dst, dst, 6);
	s->rssi = rssi;
	s->len = (uint8_t)len;
	memcpy(s->data, data, len);
	__sync_synchronize();
	totem_rx_head = head + 1;
}

// totem_rx_pop takes the oldest frame, or returns 0 when the ring is empty.
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

#endif
