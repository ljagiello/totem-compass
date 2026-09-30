#!/usr/bin/env python3
"""The Totem v3.5 reference design: every part, every net, every footprint.

This is the single source the schematic sheet, the simulation, the ERC and
the PCB are all generated from. It reproduces the circuit of a Totem v3.5 as
far as the board and the firmware establish it, and designs the rest so the
result works with that firmware.

What is established, and where from:
  - The parts: read off the board (docs/reference/hardware.mdx).
  - Every MCU pin, including the GNSS UART on 16/17 (`UART(1, rx=16, tx=17)`
    in ubx_gnss) and the ring power gate on 19 driven HIGH to light the ring
    (`LightStrip.power_on` calls `pwr_gate.on()` in leds).
  - That only the ring is gated; the crystal has no power gate (leds).
  - That GPIO 4 is read as the power button with a pull-up, and is driven
    low to switch the board off (power.mdx, `shut_down(method=0)`).

What is designed here, because a photograph cannot show it, and is marked
as such below: the power latch, the passive values, the programming pads.

Run: python3 check.py   (runs this and checks the result)
"""

import os

KICAD_SYMBOLS = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/symbols"
os.environ.setdefault("KICAD10_SYMBOL_DIR", KICAD_SYMBOLS)

from skidl import ERC, Net, Part, Pin, generate_netlist  # noqa: E402

R0603 = "Resistor_SMD:R_0603_1608Metric"   # the reference board's resistors are 0603
C0805 = "Capacitor_SMD:C_0805_2012Metric"   # and its bulk capacitors 0805
C0603 = "Capacitor_SMD:C_0603_1608Metric"
SOT23 = "Package_TO_SOT_SMD:SOT-23"
# 6x6 mm SMD tact switch, top-actuated, tall stem through the rear cover (PTS645 land pattern)
SW6X6 = "Button_Switch_SMD:SW_SPST_PTS645Sx43SMTR92"
LED1515 = "totem:LED_XL-1515RGBC-WS2812B"   # schematic/totem.pretty, from the datasheet


def leave_unused(part, used, why):
    """Take a part's remaining pins out of ERC, on the record."""
    for pin in part.pins:
        nums = pin.num if isinstance(pin.num, list) else [pin.num]
        if not any(str(n) in used for n in nums):
            pin.do_erc = False
    part.unused_note = why


def R(ref, value, fp=R0603):
    return Part("Device", "R", ref=ref, value=value, footprint=fp)


def C(ref, value, fp=C0603):
    return Part("Device", "C", ref=ref, value=value, footprint=fp)


def net(name, *pins):
    n = Net(name)
    for pin in pins:
        n += pin
    return n


# The rails. VBAT is the cell and is always live; VSYS is VBAT switched by
# the power latch, so nothing downstream draws when the board is off; +3V3
# is the regulator; VLED is VSYS switched again for the ring only.
vbus, vbat, vsys, v3v3, vled, gnd = (
    Net(n) for n in ("VBUS", "VBAT", "VSYS", "+3V3", "VLED", "GND"))
for n in (vbus, vbat, vsys, v3v3, vled, gnd):
    n.drive = Pin.drives.POWER

# ------------------------------------------------------------------ USB-C
# Charge-only. The firmware updates over Wi-Fi and nothing in it reads USB
# data, so D+/D- are left unconnected. CC1/CC2 each need 5.1k to ground or a
# USB-C source will not turn VBUS on at all — the most common way a USB-C
# board ends up charging from an A-to-C cable and nothing else.
# A power-only 6-pin receptacle, then: it has everything a charge-only port
# uses. And it has to be all-SMD. The ring's LEDs pass within 5 mm of the
# board edge right behind the connector, on the other side, and the
# reference board's shell tabs sit just outside them (measured 18.75 and
# 21.4 mm from the centre line, 2.5 mm apart); a common 16-pin part's rear
# tabs (4.2 mm behind its front ones) would land on the ring. GCT USB4135's
# footprint was the one in KiCad's library that clears it and still reaches
# 1.75 mm past the edge (pcb/usbfit.py).
j1 = Part("Connector", "USB_C_Receptacle_PowerOnly_6P", ref="J1", value="USB4135-GF-A",
          footprint="Connector_USB:USB_C_Receptacle_GCT_USB4135-GF-A_6P_TopMnt_Horizontal")
r_cc1, r_cc2 = R("R20", "5.1k"), R("R21", "5.1k")
c_vbus = C("C10", "10u", C0805)
vbus += j1["A9"], j1["B9"], c_vbus[1]
gnd += j1["A12"], j1["B12"], j1["SH"], c_vbus[2], r_cc1[2], r_cc2[2]
net("CC1", j1["A5"], r_cc1[1])
net("CC2", j1["B5"], r_cc2[1])

# ---------------------------------------------------------------- charger
# TP4056, the part on the board (marked 4056). PROG sets the charge current
# as 1200/R: 2k gives 600 mA, 0.6C for the 1000 mAh cell. A linear charger
# burns (VBUS - VBAT) x I, which at 1 A is up to 1.5 W in a sealed enclosure
# and would throttle; 600 mA keeps that under 1 W. CE high enables it, and
# must be driven — floating, the part does nothing.
u3 = Part("Battery_Management", "TP4056-42-ESOP8", ref="U3",
          footprint="Package_SO:SOIC-8-1EP_3.9x4.9mm_P1.27mm_EP2.29x3mm")
leave_unused(u3, {"1", "2", "3", "4", "5", "8", "9"},
             "STDBY and CHRG are status outputs, unused: charging is sensed from VBUS")
r_prog = R("R5", "2k")
c_bat = C("C11", "10u", C0805)
vbus += u3["4"], u3["8"]
gnd += u3["1"], u3["3"], u3["9"], r_prog[2], c_bat[2]
net("PROG", u3["2"], r_prog[1])
vbat += u3["5"], c_bat[1]

j2 = Part("Connector_Generic", "Conn_01x02", ref="J2", value="LiPo 1000mAh",
          footprint="Connector_JST:JST_PH_S2B-PH-SM4-TB_1x02-1MP_P2.00mm_Horizontal")
vbat += j2[1]
gnd += j2[2]

# ------------------------------------------------------------ power latch
# DESIGNED, not recovered. What it has to do comes from the firmware:
#   - a press of SW1 turns the board on from off;
#   - it stays on after the press;
#   - GPIO 4 reads the button, low when pressed, with the pull-up;
#   - GPIO 4 driven low by the firmware turns the board off;
#   - a press while running must NOT turn it off, because GPIO 4 goes low
#     then too.
# The last two conflict unless the button itself holds the board on while
# it is pressed. So SW1 goes to VBAT, and while pressed it charges HOLD
# through D1; a separate FET turns the same press into a low on GPIO 4 for
# the firmware to read. Off: firmware low on GPIO 4 drains HOLD through D2,
# and with the button released nothing refills it.
#
# The first version of this latch put 4.2 V from the button straight onto
# GPIO 4 through a resistor — above the ESP32's 3.6 V absolute maximum —
# and made presses unreadable. This one keeps GPIO 4 inside 0..3.3 V.
sw1 = Part("Switch", "SW_Push", ref="SW1", value="power", footprint=SW6X6)
q_main = Part("Transistor_FET", "AO3401A", ref="Q1", footprint=SOT23)
q_hold = Part("Transistor_FET", "2N7002", ref="Q2", footprint=SOT23)
q_sense = Part("Transistor_FET", "2N7002", ref="Q3", footprint=SOT23)
d_on = Part("Diode", "1N4148W", ref="D1", footprint="Diode_SMD:D_SOD-123")
d_off = Part("Diode", "BAT54W", ref="D2", footprint="Package_TO_SOT_SMD:SOT-323_SC-70")
leave_unused(d_off, {"1", "3"}, "pin 2 is a no-connect on this package")
r_gate_main = R("R14", "100k")
r_hold_pd = R("R15", "10M")
r_self = R("R16", "1M")
r_off = R("R17", "10k")
r_btn_pd = R("R18", "100k")
r_gpio4_pu = R("R19", "10k")
c_hold = C("C12", "100n")

vbat += q_main["S"], r_gate_main[1], sw1[1]
vsys += q_main["D"]
net("GATE_MAIN", q_main["G"], r_gate_main[2], q_hold["D"])
gnd += q_hold["S"], q_sense["S"], r_hold_pd[2], c_hold[2], r_btn_pd[2]
net("BTN", sw1[2], d_on["A"], q_sense["G"], r_btn_pd[1])
net("HOLD", d_on["K"], q_hold["G"], r_hold_pd[1], c_hold[1], r_self[2], r_off[1])
v3v3 += r_self[1], r_gpio4_pu[1]
net("HOLD_OFF", r_off[2], d_off["A"])
gpio4 = net("GPIO4", d_off["K"], q_sense["D"], r_gpio4_pu[2])

# ------------------------------------------------------------- regulator
# The board's SOT-23-5 is marked ACH and was not identified. AP2112K-3.3 is
# pin-compatible and good for 600 mA; the ESP32's radio peaks at 500 mA,
# which a 300 mA part (such as an AP2127K) would not survive. EN tied to its
# own input: VSYS is already switched, so the regulator just follows it.
u4 = Part("Regulator_Linear", "AP2112K-3.3", ref="U4", footprint="Package_TO_SOT_SMD:SOT-23-5")
leave_unused(u4, {"1", "2", "3", "5"}, "pin 4 is a no-connect on this package")
c_ldo_in, c_ldo_out = C("C13", "10u", C0805), C("C14", "10u", C0805)
vsys += u4["VIN"], u4["EN"], c_ldo_in[1]
v3v3 += u4["VOUT"], c_ldo_out[1]
gnd += u4["GND"], c_ldo_in[2], c_ldo_out[2]

# ---------------------------------------------------------------- sensing
# Cell voltage on GPIO 34: the ratio is solved from the firmware's own ADC
# calibration (see ../README.md). Taken from VSYS, so it draws nothing when
# the board is off; VSYS differs from the cell only by the switch's drop.
r_bat_top, r_bat_bot = R("R1", "120k"), R("R2", "108k")
vsys += r_bat_top[1]
gnd += r_bat_bot[2]
# VBUS presence on GPIO 39: high while charging, as `is_charging` expects.
r_vbus_top, r_vbus_bot = R("R3", "100k"), R("R4", "150k")
vbus += r_vbus_top[1]
gnd += r_vbus_bot[2]

# ------------------------------------------------------------------- MCU
u1 = Part("RF_Module", "ESP32-WROOM-32E", ref="U1",
          footprint="RF_Module:ESP32-WROOM-32E")
leave_unused(
    u1,
    {"1", "2", "3", "4", "5", "6", "10", "11", "12", "13", "15",
     "25", "26", "27", "28", "30", "31", "33", "34", "35", "38", "39"},
    "the module's remaining GPIO and its NC pins are not used")
c_3v3_bulk, c_3v3_hf = C("C3", "10u", C0805), C("C4", "100n")
r_en_esp, c_en = R("R13", "10k"), C("C2", "1u")
r_io0 = R("R22", "10k")
v3v3 += u1["VDD"], c_3v3_bulk[1], c_3v3_hf[1], r_en_esp[1], r_io0[1]
gnd += u1["GND"], c_3v3_bulk[2], c_3v3_hf[2], c_en[2]
# Espressif's own reset circuit: 10k and 1 uF, a ~10 ms power-on reset.
esp_en = net("ESP_EN", u1["EN"], r_en_esp[2], c_en[1])
net("VBAT_SENSE", r_bat_top[2], r_bat_bot[1], u1["IO34"])
net("VBUS_SENSE", r_vbus_top[2], r_vbus_bot[1], u1["SENSOR_VN"])
gpio4 += u1["IO4"]

# Programming pads. The ESP32 has no USB of its own and the USB-C carries
# none, so a bare board is flashed through UART0 with GPIO 0 held low.
# Without these the board cannot be programmed at all.
tps = {n: Part("Connector", "TestPoint", ref=f"TP{i}", value=n,
               footprint="TestPoint:TestPoint_Pad_D1.0mm")
       for i, n in enumerate(("3V3", "GND", "TXD0", "RXD0", "EN", "IO0"), start=2)}
v3v3 += tps["3V3"][1]
gnd += tps["GND"][1]
net("TXD0", u1["TXD0/IO1"], tps["TXD0"][1])
net("RXD0", u1["RXD0/IO3"], tps["RXD0"][1])
esp_en += tps["EN"][1]

# SOS button on GPIO 0 — which is also the ESP32's boot-mode strap. Holding
# SOS while powering on enters the bootloader; that is the real board's
# arrangement too, and it doubles as the way to flash it.
sw2 = Part("Switch", "SW_Push", ref="SW2", value="SOS", footprint=SW6X6)
net("BTN_SOS", u1["IO0"], sw2[1], r_io0[2], tps["IO0"][1])
gnd += sw2[2]

# ------------------------------------------------------------------ GNSS
u2 = Part("RF_GPS", "MAX-M10S", ref="U2", footprint="RF_GPS:ublox_MAX")
leave_unused(u2, {"1", "2", "3", "6", "7", "8", "10", "11", "12"},
             "TIMEPULSE, EXTINT, RESET, SAFEBOOT, LNA_EN, VIO_SEL and I2C keep "
             "their defaults; VCC_RF is an output")
c_gnss_bulk, c_gnss_hf = C("C5", "10u", C0805), C("C6", "100n")
v3v3 += u2["VCC"], u2["VCC_IO"], u2["V_BCKP"], c_gnss_bulk[1], c_gnss_hf[1]
gnd += u2["GND"], c_gnss_bulk[2], c_gnss_hf[2]
# Confirmed in the firmware: UART(1, rx=16, tx=17).
net("GNSS_RX", u1["IO17"], u2["RXD"])
net("GNSS_TX", u2["TXD"], u1["IO16"])
# A passive ceramic patch in the housing, on a coax to the u.FL.
j3 = Part("Connector", "Conn_Coaxial", ref="J3", value="u.FL",
          footprint="Connector_Coaxial:U.FL_Hirose_U.FL-R-SMT-1_Vertical")
net("GNSS_RF", u2["RF_IN"], j3[1])
gnd += j3[2]

# ------------------------------------------------------------------- IMU
u5 = Part("Sensor_Motion", "ICM-20948", ref="U5",
          footprint="Sensor_Motion:InvenSense_QFN-24_3x3mm_P0.4mm")
leave_unused(u5, {"8", "9", "10", "13", "18", "20", "22", "23", "24"},
             "the auxiliary I2C, FSYNC, INT1, RESV and the NC pins are unused")
c_imu_vdd, c_imu_io, c_regout = C("C7", "100n"), C("C8", "100n"), C("C1", "100n")
r_sda, r_scl = R("R7", "4.7k"), R("R8", "4.7k")
v3v3 += u5["VDD"], u5["VDDIO"], u5["~{CS}"], c_imu_vdd[1], c_imu_io[1], r_sda[1], r_scl[1]
# SDO/AD0 selects the I2C address; low is 0x68. Strapped through a resistor
# rather than tied straight to ground, which is the usual way to strap a pin:
# the address can be changed by moving one part, and ERC stops reporting a
# bidirectional pin sitting on a power net.
r_ad0 = R("R26", "10k")
net("IMU_AD0", u5["SDO/AD0"], r_ad0[1])
gnd += u5["GND"], r_ad0[2], c_imu_vdd[2], c_imu_io[2], c_regout[2]
net("REGOUT", u5["REGOUT"], c_regout[1])
net("I2C0_SDA", u1["IO25"], u5["SDA/SDI"], r_sda[2])
net("I2C0_SCL", u1["IO26"], u5["SCL/SCLK"], r_scl[2])

# ------------------------------------------------------------------ ring
# 60 x 1.5 x 1.5 mm WS2812B (XL-1515RGBC-WS2812B). Measured on the reference
# board: packages 1.50 mm wide at a 1.91 mm pitch, ring radius 18.24 mm, which
# a 2 x 2 mm WS2812B-2020 could not fit (60 of them would collide). The 1515's
# pins are numbered as the 2020's (1 DO, 2 GND, 3 DI, 4 VDD), so the
# WS2812B-2020 symbol is kept and only the footprint differs. Powered through a high-side switch the firmware
# drives on GPIO 19 — HIGH to light the ring, confirmed in the bytecode —
# so a P-FET needs an N-FET in front of it to invert. Wiring the P-FET's
# gate straight to GPIO 19 would light the ring exactly when told not to.
q_ring = Part("Transistor_FET", "AO3401A", ref="Q4", footprint=SOT23)
q_ring_drv = Part("Transistor_FET", "2N7002", ref="Q5", footprint=SOT23)
r_ring_gate, r_ring_en_pd = R("R9", "100k"), R("R10", "100k")
r_ring_din = R("R11", "33")
c_vled1, c_vled2 = C("C15", "10u", C0805), C("C16", "10u", C0805)
vsys += q_ring["S"], r_ring_gate[1]
vled += q_ring["D"], c_vled1[1], c_vled2[1]
gnd += q_ring_drv["S"], r_ring_en_pd[2], c_vled1[2], c_vled2[2]
net("RING_GATE", q_ring["G"], r_ring_gate[2], q_ring_drv["D"])
net("LED_EN", u1["IO19"], q_ring_drv["G"], r_ring_en_pd[1])
net("RING_DATA", u1["IO18"], r_ring_din[1])

RING = [Part("LED", "WS2812B-2020", ref=f"D{100 + i}", value="XL-1515RGBC-WS2812B", footprint=LED1515)
        for i in range(60)]
prev = r_ring_din[2]
for i, px in enumerate(RING):
    vled += px["VDD"]
    gnd += px["VSS"]
    net(f"RING_D{i}", prev, px["DIN"])
    prev = px["DOUT"]
RING[-1]["DOUT"].do_erc = False          # the chain simply ends

# ---------------------------------------------------------------- crystal
# 7 more under the light pipe, the same 1515 part. No power gate in the
# firmware, so they sit on VSYS: always powered while the board is on, and — unlike on VBAT — drawing
# nothing while it is off. A dark WS2812B still draws about 0.7 mA; seven of
# them straight on the cell would flatten it in about eight days switched off.
r_cry_din = R("R12", "33")
c_vcry = C("C17", "10u", C0805)
vsys += c_vcry[1]
gnd += c_vcry[2]
net("CRYSTAL_DATA", u1["IO21"], r_cry_din[1])
CRYSTAL = [Part("LED", "WS2812B-2020", ref=f"D{200 + i}", value="XL-1515RGBC-WS2812B", footprint=LED1515)
           for i in range(7)]
prev = r_cry_din[2]
for i, px in enumerate(CRYSTAL):
    vsys += px["VDD"]
    gnd += px["VSS"]
    net(f"CRY_D{i}", prev, px["DIN"])
    prev = px["DOUT"]
CRYSTAL[-1]["DOUT"].do_erc = False

# ------------------------------------------------------------- the rest
# SOS indicator on GPIO 14.
r_sos = R("R6", "330")
d_sos = Part("Device", "LED", ref="D3", value="red", footprint="LED_SMD:LED_0603_1608Metric")
net("SOS_LED", u1["IO14"], r_sos[1])
net("SOS_LED_A", r_sos[2], d_sos["A"])
gnd += d_sos["K"]

# Touch Crystal: the gold post at the centre of the crystal cluster is the
# electrode, a plated hole the cover's contact spring lands on.
tp1 = Part("Connector", "TestPoint", ref="TP1", value="touch",
           footprint="TestPoint:TestPoint_Pad_D2.5mm")
net("TOUCH", u1["IO27"], tp1[1])

# Microphone: an electret, read by the firmware as the deviation of the ADC
# around its own mean (new_vibe), so it needs a stable bias and nothing else.
# AC-coupled onto a mid-rail divider, so the DC level sits in the ADC's
# linear window whatever current this particular capsule draws.
mk1 = Part("Device", "Microphone_Condenser", ref="MK1", value="electret",
           footprint="Sensor_Audio:CUI_CMC-4013-SMT")
r_mic_bias, c_mic = R("R23", "2.2k"), C("C18", "1u")
r_mic_hi, r_mic_lo = R("R24", "100k"), R("R25", "100k")
v3v3 += r_mic_bias[1], r_mic_hi[1]
gnd += mk1["-"], r_mic_lo[2]
net("MIC_RAW", mk1["+"], r_mic_bias[2], c_mic[1])
net("MIC", c_mic[2], r_mic_hi[2], r_mic_lo[1], u1["SENSOR_VP"])

# SKiDL gives every part a random tag unless told otherwise, which made the
# netlist differ on every run for no reason. The reference is already unique.
import builtins  # noqa: E402
for _p in builtins.default_circuit.parts:
    _p.tag = _p.ref

if __name__ == "__main__":
    ERC()
    generate_netlist(file_="totem.net")
    print("netlist written to totem.net")
