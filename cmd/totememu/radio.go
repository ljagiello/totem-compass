//go:build tinygo && esp32

package main

// The Totem firmware (espnow_conn_v2 EspConn.power_on) configures the radio
// with WLAN.config(protocol=MODE_LR, channel=6, pm=PM_NONE, txpower=21) and
// ESPNow.config(rate=41). espradio does not wrap these calls, but the WiFi
// blobs it links export them, so they are declared here.

/*
#include <stdint.h>
typedef int esp_err_t;
esp_err_t esp_wifi_set_protocol(int ifx, uint8_t protocol_bitmap);
esp_err_t esp_wifi_set_channel(uint8_t primary, int second);
esp_err_t esp_wifi_set_max_tx_power(int8_t power);
esp_err_t esp_wifi_config_espnow_rate(int ifx, int rate);
esp_err_t esp_wifi_get_mac(int ifx, uint8_t *mac);
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"

	"tinygo.org/x/espradio"

	"github.com/ljagiello/totem-compass/mesh"
)

const (
	wifiIfSTA      = 0 // WIFI_IF_STA
	wifiSecondNone = 0 // WIFI_SECOND_CHAN_NONE
	// esp_wifi_set_max_tx_power takes 0.25 dBm steps; the firmware asks
	// for 21 dBm and the driver caps it at 20.
	maxTxPower = 21 * 4
)

type espError struct {
	call string
	code C.esp_err_t
}

func (e espError) Error() string { return fmt.Sprintf("%s: esp_err_t 0x%x", e.call, int(e.code)) }

func check(call string, code C.esp_err_t) error {
	if code != 0 {
		return espError{call, code}
	}
	return nil
}

// startRadio brings the radio up the way a Totem does and returns the local
// ESP-NOW (station) MAC address.
func startRadio() (mac mesh.MAC, err error) {
	if err := espradio.Enable(espradio.Config{}); err != nil {
		return mac, fmt.Errorf("enable: %w", err)
	}
	if err := espradio.Start(); err != nil {
		return mac, fmt.Errorf("start: %w", err)
	}
	if err := check("esp_wifi_set_protocol", C.esp_wifi_set_protocol(wifiIfSTA, mesh.Protocol)); err != nil {
		return mac, err
	}
	if err := check("esp_wifi_set_channel", C.esp_wifi_set_channel(mesh.Channel, wifiSecondNone)); err != nil {
		return mac, err
	}
	if err := check("esp_wifi_set_max_tx_power", C.esp_wifi_set_max_tx_power(maxTxPower)); err != nil {
		return mac, err
	}
	if err := espradio.ESPNowInit(); err != nil {
		return mac, fmt.Errorf("esp_now_init: %w", err)
	}
	if err := check("esp_wifi_config_espnow_rate", C.esp_wifi_config_espnow_rate(wifiIfSTA, mesh.PHYRate)); err != nil {
		return mac, err
	}
	if err := check("esp_wifi_get_mac", C.esp_wifi_get_mac(wifiIfSTA, (*C.uint8_t)(unsafe.Pointer(&mac[0])))); err != nil {
		return mac, err
	}
	return mac, nil
}

// selfTest counts every 802.11 frame on the mesh channel for a few
// seconds, first in the Totem's LR-only mode, then with 802.11b/g/n
// enabled too, where any nearby access point's beacons show up. It proves
// the receive path works when no Totem is transmitting.
func selfTest() (lrOnly, withBGN uint32, err error) {
	const bgnLR = 0x0f // WIFI_PROTOCOL_11B | 11G | 11N | LR
	if lrOnly, err = espradio.SniffCountOnChannel(mesh.Channel, 3*time.Second); err != nil {
		return
	}
	if err = check("esp_wifi_set_protocol", C.esp_wifi_set_protocol(wifiIfSTA, bgnLR)); err != nil {
		return
	}
	withBGN, err = espradio.SniffCountOnChannel(mesh.Channel, 3*time.Second)
	if err2 := check("esp_wifi_set_protocol", C.esp_wifi_set_protocol(wifiIfSTA, mesh.Protocol)); err == nil {
		err = err2
	}
	return
}
