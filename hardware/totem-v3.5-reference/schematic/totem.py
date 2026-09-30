#!/usr/bin/env python3
"""The Totem v3.5 reference design as a circuit, for netlist and ERC.

ngspice checks that the analog blocks behave (see ../sim). This checks the
other half: that everything is connected, that no two outputs fight over a
net, and that nothing is left floating. SKiDL's ERC is the machine doing it
rather than me reading a drawing.

Parts are defined here with explicit pins rather than pulled from KiCad's
libraries, so this runs without KiCad installed. Pin numbers are given only
where the package fixes them (U3); the ESP32's are GPIO numbers, which is
how the firmware names them and how the rest of these docs cite them.

Run: python3 totem.py
Writes: totem.net (KiCad netlist), and ERC findings to stdout.
"""

from skidl import ERC, Net, Part, Pin, generate_netlist, SKIDL

# ---------------------------------------------------------------- parts


def ic(name, pins, prefix="U"):
    """A part with explicit pins, so no symbol library is needed."""
    return Part(tool=SKIDL, name=name, ref_prefix=prefix, pins=pins)


PWRIN = Pin.types.PWRIN
PWROUT = Pin.types.PWROUT
IN = Pin.types.INPUT
OUT = Pin.types.OUTPUT
BIDIR = Pin.types.BIDIR
PASSIVE = Pin.types.PASSIVE
OPENCOLL = Pin.types.OPENCOLL

# U1 — ESP32-WROOM-32E. Only the pins this design uses.
u1 = ic(
    "ESP32-WROOM-32E",
    [
        Pin(num="3V3", name="3V3", func=PWRIN),
        Pin(num="GND", name="GND", func=PWRIN),
        Pin(num="0", name="IO0_BTN_SOS", func=IN),
        Pin(num="4", name="IO4_BTN_PWR_LATCH", func=BIDIR),
        Pin(num="14", name="IO14_SOS_LED", func=OUT),
        Pin(num="18", name="IO18_RING_DATA", func=OUT),
        Pin(num="19", name="IO19_RING_EN", func=OUT),
        Pin(num="21", name="IO21_CRYSTAL_DATA", func=OUT),
        Pin(num="25", name="IO25_I2C0_SDA", func=BIDIR),
        Pin(num="26", name="IO26_I2C0_SCL", func=OUT),
        Pin(num="27", name="IO27_TOUCH", func=BIDIR),
        Pin(num="34", name="IO34_VBAT_SENSE", func=IN),
        Pin(num="36", name="IO36_MIC", func=IN),
        Pin(num="39", name="IO39_VBUS_SENSE", func=IN),
        # The GNSS UART pins were never recovered from the board; these are
        # the design's choice, not the Totem's.
        Pin(num="17", name="IO17_GNSS_TX", func=OUT),
        Pin(num="16", name="IO16_GNSS_RX", func=IN),
    ],
)

# U2 — u-blox MAX-M10S
u2 = ic(
    "MAX-M10S",
    [
        Pin(num="VCC", name="VCC", func=PWRIN),
        Pin(num="GND", name="GND", func=PWRIN),
        Pin(num="RXD", name="RXD", func=IN),
        Pin(num="TXD", name="TXD", func=OUT),
        Pin(num="RF", name="RF_IN", func=PASSIVE),
    ],
)

# U3 — TP4056, SOP-8. Pin numbers are the datasheet's.
u3 = ic(
    "TP4056",
    [
        Pin(num="1", name="TEMP", func=IN),
        Pin(num="2", name="PROG", func=PASSIVE),
        Pin(num="3", name="GND", func=PWRIN),
        Pin(num="4", name="VCC", func=PWRIN),
        Pin(num="5", name="BAT", func=PWROUT),
        Pin(num="6", name="BAT", func=PASSIVE),  # same die node as 5
        Pin(num="7", name="CHRG", func=OPENCOLL),
        Pin(num="8", name="STDBY", func=OPENCOLL),
    ],
)

# U4 — 3V3 regulator, SOT-23-5 marked ACH. Part not identified; this is the
# design's regulator, drawn as a generic LDO.
u4 = ic(
    "LDO_3V3",
    [
        Pin(num="1", name="IN", func=PWRIN),
        Pin(num="2", name="GND", func=PWRIN),
        Pin(num="3", name="EN", func=IN),
        Pin(num="4", name="NC", func=Pin.types.NOCONNECT),
        Pin(num="5", name="OUT", func=PWROUT),
    ],
)

# U5 — ICM-20948, 9-axis. The magnetometer is on the die.
u5 = ic(
    "ICM-20948",
    [
        Pin(num="VDD", name="VDD", func=PWRIN),
        Pin(num="GND", name="GND", func=PWRIN),
        Pin(num="SDA", name="SDA", func=BIDIR),
        Pin(num="SCL", name="SCL", func=IN),
    ],
)

# Strips. Each is one chained input; the pixels daisy-chain beyond it.
ring = ic("HALO_RING_60PX", [
    Pin(num="VDD", name="VDD", func=PWRIN),
    Pin(num="GND", name="GND", func=PWRIN),
    Pin(num="DIN", name="DIN", func=IN),
], prefix="DS")
crystal = ic("CRYSTAL_7PX", [
    Pin(num="VDD", name="VDD", func=PWRIN),
    Pin(num="GND", name="GND", func=PWRIN),
    Pin(num="DIN", name="DIN", func=IN),
], prefix="DS")

# Discrete SOS indicator, its series resistor, and the LED supply switch.
sos_led = ic("LED_SOS", [
    Pin(num="A", name="A", func=PASSIVE),
    Pin(num="K", name="K", func=PASSIVE),
], prefix="DS")
q_led = ic("PMOS_LED_SW", [
    Pin(num="G", name="G", func=IN),
    Pin(num="S", name="S", func=PASSIVE),
    Pin(num="D", name="D", func=PASSIVE),
], prefix="Q")

# Connectors, switches, sensor.
j1 = ic("USB-C", [
    Pin(num="VBUS", name="VBUS", func=PWROUT),
    Pin(num="GND", name="GND", func=PWROUT),
], prefix="J")
j2 = ic("JST_BATT", [
    Pin(num="1", name="VBAT", func=PASSIVE),
    Pin(num="2", name="GND", func=PASSIVE),
], prefix="J")
j3 = ic("UFL_GNSS_ANT", [Pin(num="1", name="RF", func=PASSIVE)], prefix="J")
sw1 = ic("SW_PWR", [
    Pin(num="1", name="A", func=PASSIVE),
    Pin(num="2", name="B", func=PASSIVE),
], prefix="SW")
sw2 = ic("SW_SOS", [
    Pin(num="1", name="A", func=PASSIVE),
    Pin(num="2", name="B", func=PASSIVE),
], prefix="SW")
tp1 = ic("TOUCH_PAD", [Pin(num="1", name="PAD", func=PASSIVE)], prefix="TP")
mk1 = ic("MIC_MEMS", [
    Pin(num="VDD", name="VDD", func=PWRIN),
    Pin(num="GND", name="GND", func=PWRIN),
    Pin(num="OUT", name="OUT", func=OUT),
], prefix="MK")


def res(ref_value):
    return Part(
        tool=SKIDL,
        name="R",
        ref_prefix="R",
        value=ref_value,
        pins=[Pin(num="1", func=PASSIVE), Pin(num="2", func=PASSIVE)],
    )


# Values from ../README.md, derived rather than chosen where noted.
r_bat_top = res("120k")     # derived: see the battery divider derivation
r_bat_bot = res("108k")     # derived
r_vbus_top = res("100k")    # chosen to clear VIH at 4.40 V VBUS
r_vbus_bot = res("150k")    # ...and stay under abs max at 5.25 V
r_prog = res("1k2")         # TP4056 PROG: 1000 mA charge
r_sos = res("330R")         # SOS indicator series
r_sda = res("4k7")          # I2C pull-ups
r_scl = res("4k7")
r_gate = res("100k")        # LED switch gate pull-up
r_en = res("100k")          # POWER_EN pull-down: released means off
r_g4 = res("10k")           # GPIO 4 into POWER_EN, so driving it low kills the rail

# ---------------------------------------------------------------- nets

def net(name, *pins):
    """Make a named net and connect pins to it."""
    n = Net(name)
    for pin in pins:
        n += pin
    return n



vbus = Net("VBUS")
vbat = Net("VBAT")
v3v3 = Net("+3V3")
vled = Net("VLED")
gnd = Net("GND")
for n in (vbus, vbat, v3v3, vled, gnd):
    n.drive = Pin.drives.POWER

# Power path: USB charges the cell; the rail comes off the cell, so the
# board behaves the same on and off the cable.
vbus += j1["VBUS"], u3["4"], r_vbus_top[1]
gnd += (
    j1["GND"], j2["2"], u3["3"], u4["2"], u1["GND"], u2["GND"], u5["GND"],
    ring["GND"], crystal["GND"], mk1["GND"], sw2["2"],
    r_bat_bot[2], r_vbus_bot[2], r_prog[2], sos_led["K"],
)
vbat += j2["1"], u3["5"], u3["6"], u4["1"], r_bat_top[1], q_led["S"], r_gate[1], sw1["2"]
v3v3 += u4["5"], u1["3V3"], u2["VCC"], u5["VDD"], mk1["VDD"], r_sda[1], r_scl[1]

# The charger's own programming resistor sets charge current.
u3["2"] += r_prog[1]

# Sense divider into GPIO 34. Ratio is solved from the firmware's ADC
# calibration; see ../README.md.
net("VBAT_SENSE", r_bat_top[2], r_bat_bot[1], u1["34"])

# VBUS presence into GPIO 39. Not the charger's CHRG pin: the firmware reads
# this high while charging and CHRG pulls low.
net("VBUS_SENSE", r_vbus_top[2], r_vbus_bot[1], u1["39"])

# GNSS over UART, and its antenna.
net("GNSS_RX", u1["17"], u2["RXD"])
net("GNSS_TX", u2["TXD"], u1["16"])
net("GNSS_RF", u2["RF"], j3["1"])

# The 9-axis part on I2C bus 0, with its pull-ups.
net("I2C0_SDA", u1["25"], u5["SDA"], r_sda[2])
net("I2C0_SCL", u1["26"], u5["SCL"], r_scl[2])

# The SOS button goes to ground; the ESP32's internal pull-up holds it high.
net("BTN_SOS", u1["0"], sw2["1"])

# The power gate. GPIO 4 is read as an input while the board runs and is
# re-opened as an output and driven low to switch off, so it has to be able
# to pull the regulator's enable down. SW1 raises the same net to start.
#
# This is the least constrained part of the reconstruction. The firmware
# fixes what GPIO 4 does, not how the latch around it is built, and the
# `A7` diode and `J22B` SOT-23 found on the board are plausible members of
# it without being traced. Treat the topology as designed here.
net("POWER_EN", u4["3"], sw1["1"], r_en[1], r_g4[1])
net("GPIO4", u1["4"], r_g4[2])
gnd += r_en[2]

# The charger's thermistor input is tied off, as it must be when no NTC is
# fitted. Its two status outputs, CHRG and STDBY, are left open on purpose:
# charging is sensed from VBUS, not from them. ERC reports both as
# unconnected and that is the right answer — the warning is understood, not
# silenced, because a part that hides it would also hide a real one.
u3["1"] += gnd
net("TOUCH", u1["27"], tp1["1"])
net("MIC", u1["36"], mk1["OUT"])

# LED supply switch: the strips run from the cell, not the 3V3 rail, because
# 3.3 V is already below the pixels' minimum supply.
net("LED_EN", u1["19"], q_led["G"], r_gate[2])
vled += q_led["D"], ring["VDD"], crystal["VDD"]
net("RING_DATA", u1["18"], ring["DIN"])
net("CRYSTAL_DATA", u1["21"], crystal["DIN"])
net("SOS_LED", u1["14"], r_sos[1])
net("SOS_LED_A", r_sos[2], sos_led["A"])

if __name__ == "__main__":
    ERC()
    generate_netlist(file_="totem.net")
    print("netlist written to totem.net")
