#!/usr/bin/env python3
"""The Totem v3.5 reference design as a circuit, for netlist and ERC.

ngspice checks that the analog blocks behave (see ../sim). This checks the
other half: that everything is connected, that no two outputs fight over a
net, and that nothing is left floating by accident.

Parts come from KiCad's own symbol libraries wherever they exist, so the
netlist carries **real package pin numbers** rather than labels invented
here. ESP32-WROOM-32E, MAX-M10S, ICM-20948 and WS2812B all do; the TP4056
does not and is the one part still declared inline.

That matters beyond tidiness. On the module, pin 4 and pin 5 are named
SENSOR_VP and SENSOR_VN — GPIO 36 and GPIO 39, the microphone and the VBUS
sense. Wiring against the real symbol is what puts those on the right
physical pins rather than on names chosen here.

Run: python3 check.py   (which runs this and checks the result)
Writes: totem.net, totem.erc
"""

import os

KICAD_SYMBOLS = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/symbols"
os.environ.setdefault("KICAD10_SYMBOL_DIR", KICAD_SYMBOLS)

from skidl import ERC, Net, Part, Pin, generate_netlist, SKIDL  # noqa: E402

PASSIVE = Pin.types.PASSIVE
PWRIN = Pin.types.PWRIN
PWROUT = Pin.types.PWROUT
OPENCOLL = Pin.types.OPENCOLL
INPUT = Pin.types.INPUT


def leave_unused(part, used, why):
    """Take a part's remaining pins out of ERC, on the record.

    Every pin not named in `used` is one this design deliberately does not
    use. Saying so is the point: letting them report as floating would bury
    the warnings that matter under the ones that do not.
    """
    for pin in part.pins:
        nums = pin.num if isinstance(pin.num, list) else [pin.num]
        if not any(str(n) in used for n in nums):
            pin.do_erc = False
    part.unused_note = why


# ------------------------------------------------------------------ parts

u1 = Part("RF_Module", "ESP32-WROOM-32E", ref="U1")
leave_unused(
    u1,
    {"1", "2", "3", "4", "5", "6", "10", "11", "12", "13", "15",
     "25", "26", "27", "28", "30", "31", "33", "38", "39"},
    "the module's remaining GPIO and its NC pins are not used by this design",
)

u2 = Part("RF_GPS", "MAX-M10S", ref="U2")
leave_unused(
    u2,
    {"1", "2", "3", "6", "7", "8", "10", "11", "12"},
    "TIMEPULSE, EXTINT, RESET, SAFEBOOT, LNA_EN, VIO_SEL and the I2C pair "
    "keep the module's defaults; VCC_RF is an output on this part",
)

# No KiCad symbol exists for the TP4056, so it is declared here with the
# datasheet's pin numbers.
u3 = Part(
    tool=SKIDL,
    name="TP4056",
    ref="U3",
    ref_prefix="U",
    pins=[
        Pin(num="1", name="TEMP", func=INPUT),
        Pin(num="2", name="PROG", func=PASSIVE),
        Pin(num="3", name="GND", func=PWRIN),
        Pin(num="4", name="VCC", func=PWRIN),
        Pin(num="5", name="BAT", func=PWROUT),
        Pin(num="6", name="BAT2", func=PASSIVE),  # same node on the die as 5
        Pin(num="7", name="CHRG", func=OPENCOLL),
        Pin(num="8", name="STDBY", func=OPENCOLL),
    ],
)

# The SOT-23-5 marked ACH was never identified, so a generic 3V3 regulator
# stands in for it. The topology is what is being checked, not the vendor.
u4 = Part("Regulator_Linear", "AP2127K-3.3", ref="U4")
leave_unused(u4, {"1", "2", "3", "5"}, "pin 4 is a no-connect on this package")

u5 = Part("Sensor_Motion", "ICM-20948", ref="U5")
leave_unused(
    u5,
    {"8", "9", "10", "13", "18", "20", "22", "23", "24"},
    "the auxiliary I2C master, FSYNC, INT1 and the NC pins are not used",
)

# One WS2812B stands for the head of each chain; the rest daisy-chain from
# its DOUT, which is why DOUT is left out of ERC.
ring = Part("LED", "WS2812B", ref="DS1", value="halo, 60 px")
crystal = Part("LED", "WS2812B", ref="DS61", value="crystal, 7 px")
for head in (ring, crystal):
    leave_unused(head, {"1", "3", "4"}, "DOUT carries on to the rest of the chain")

sos_led = Part("Device", "LED", ref="DS68", value="SOS")
q_led = Part("Device", "Q_PMOS", ref="Q1", value="LED supply switch")
j1 = Part("Connector", "USB_C_Receptacle_USB2.0_16P", ref="J1")
leave_unused(
    j1,
    {"A1", "A4", "A9", "A12", "B1", "B4", "B9", "B12", "SH"},
    "only the power contacts are used here; the data pairs carry the console "
    "and are not part of this design",
)
j2 = Part("Connector_Generic", "Conn_01x02", ref="J2", value="LiPo 1000mAh")
j3 = Part("Connector_Generic", "Conn_01x01", ref="J3", value="u.FL GNSS")
sw1 = Part("Switch", "SW_Push", ref="SW1", value="power")
sw2 = Part("Switch", "SW_Push", ref="SW2", value="SOS")
tp1 = Part("Connector_Generic", "Conn_01x01", ref="TP1", value="touch pad")
mk1 = Part("Device", "Microphone", ref="MK1", value="MEMS")


def R(ref, value):
    return Part("Device", "R", ref=ref, value=value)


# Values from ../README.md. R1/R2 are derived from the firmware's own ADC
# calibration; the rest are chosen.
r_bat_top = R("R1", "120k")
r_bat_bot = R("R2", "108k")
r_vbus_top = R("R3", "100k")
r_vbus_bot = R("R4", "150k")
r_prog = R("R5", "1k2")
r_sos = R("R6", "330R")
r_sda = R("R7", "4k7")
r_scl = R("R8", "4k7")
r_gate = R("R9", "100k")
r_en = R("R10", "100k")
r_g4 = R("R11", "10k")
r_mic = R("R12", "2k2")   # microphone bias
r_en_esp = R("R13", "10k")  # EN pull-up, with C2 the module's power-on reset
c_reg = Part("Device", "C", ref="C1", value="100n")
c_en = Part("Device", "C", ref="C2", value="100n")

# ------------------------------------------------------------------- nets


def net(name, *pins):
    n = Net(name)
    for pin in pins:
        n += pin
    return n


vbus, vbat, v3v3, vled, gnd = (Net(n) for n in ("VBUS", "VBAT", "+3V3", "VLED", "GND"))
for n in (vbus, vbat, v3v3, vled, gnd):
    n.drive = Pin.drives.POWER

# USB charges the cell; the rail comes off the cell, so the board behaves
# the same on and off the cable.
vbus += j1["A4"], j1["B4"], j1["A9"], j1["B9"], u3["4"], r_vbus_top[1]
gnd += (
    j1["A1"], j1["B1"], j1["A12"], j1["B12"], j1["SH"],
    j2[2], u3["3"], u4["GND"], u2["GND"],
    ring["VSS"], crystal["VSS"], sw2[2],
    r_bat_bot[2], r_vbus_bot[2], r_prog[2], r_en[2], c_reg[2],
    sos_led["K"],
)
# RESV is typed NO-CONNECT in KiCad's symbol, which is authored from the
# datasheet, so it is left alone rather than tied to ground. SDO/AD0 low
# selects the lower of the part's two I2C addresses.
gnd += u1["GND"], u5["GND"], u5["SDO/AD0"]
vbat += j2[1], u3["5"], u3["6"], u4["VIN"], r_bat_top[1], q_led["S"], r_gate[1], sw1[2]
v3v3 += (
    u4["VOUT"], u1["VDD"], u2["VCC"], u2["VCC_IO"], u2["V_BCKP"],
    u5["VDD"], u5["VDDIO"], u5["~{CS}"], r_sda[1], r_scl[1],
)

net("PROG", u3["2"], r_prog[1])
u5["REGOUT"] += c_reg[1]

# The module's own reset. EN must be held high for the ESP32 to run, and the
# capacitor gives it a power-on reset as the rail comes up. This was missing
# until ERC said so — the hazard of a pin list written by hand is that a pin
# nobody thought about is also a pin nobody notices is absent.
net("ESP_EN", u1["EN"], r_en_esp[2], c_en[1])
v3v3 += r_en_esp[1]
gnd += c_en[2]

# Sense divider into GPIO 34. The ratio is solved from the firmware's ADC
# calibration; see ../README.md.
net("VBAT_SENSE", r_bat_top[2], r_bat_bot[1], u1["IO34"])

# VBUS presence into GPIO 39, which is SENSOR_VN on this module. Not the
# charger's CHRG pin: the firmware reads this high while charging, and CHRG
# pulls low.
net("VBUS_SENSE", r_vbus_top[2], r_vbus_bot[1], u1["SENSOR_VN"])

net("GNSS_RX", u1["IO17"], u2["RXD"])
net("GNSS_TX", u2["TXD"], u1["IO16"])
net("GNSS_RF", u2["RF_IN"], j3[1])

net("I2C0_SDA", u1["IO25"], u5["SDA/SDI"], r_sda[2])
net("I2C0_SCL", u1["IO26"], u5["SCL/SCLK"], r_scl[2])

# The SOS button grounds its pin; the ESP32's internal pull-up holds it high.
net("BTN_SOS", u1["IO0"], sw2[1])
net("TOUCH", u1["IO27"], tp1[1])

# The power gate. GPIO 4 is read as an input while the board runs and is
# re-opened as an output and driven low to switch off, so it has to be able
# to pull the regulator's enable down. SW1 raises the same net to start.
#
# This is the least constrained part of the reconstruction: the firmware
# fixes what GPIO 4 does, not how the latch around it is built. The `A7`
# diode and `J22B` SOT-23 found on the board are plausible members of it
# and were not traced.
net("POWER_EN", u4["EN"], sw1[1], r_en[1], r_g4[1])
net("GPIO4", u1["IO4"], r_g4[2])

# The strips run from the cell, not the 3V3 rail: 3.3 V is already below
# the pixels' minimum supply.
net("LED_EN", u1["IO19"], q_led["G"], r_gate[2])
vled += q_led["D"], ring["VDD"], crystal["VDD"]
net("RING_DATA", u1["IO18"], ring["DIN"])
net("CRYSTAL_DATA", u1["IO21"], crystal["DIN"])
net("SOS_LED", u1["IO14"], r_sos[1])
net("SOS_LED_A", r_sos[2], sos_led["A"])

# The microphone is read as a level on GPIO 36 (SENSOR_VP), not as audio.
# KiCad's Microphone symbol is the two-terminal kind, so it is biased
# through a resistor and the ESP32 reads the same node. A MEMS part with its
# own supply would differ; what is being checked here is that the ADC input
# has a defined source rather than the exact biasing of a part whose number
# is not known.
gnd += mk1["-"]
net("MIC", mk1["+"], r_mic[2], u1["SENSOR_VP"])
v3v3 += r_mic[1]

# The charger's thermistor input is tied off, as it must be when no NTC is
# fitted. CHRG and STDBY are left open on purpose — charging is sensed from
# VBUS — and ERC is right to say so, so those two warnings stay.
u3["1"] += gnd

if __name__ == "__main__":
    ERC()
    generate_netlist(file_="totem.net")
    print("netlist written to totem.net")
