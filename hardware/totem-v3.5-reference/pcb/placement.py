"""Where every part goes, in board mm (y up, seen from the radio side), and why.

Side "F" is the radio side (F.Cu: ESP32, GNSS, charger, connectors, buttons),
side "B" the LED side (B.Cu: ring, crystal, IMU, microphone). Positions that
come from the reference board's photographs are marked "measured" and name
the measurement (hardware/totem-v3.5-reference/measure/); the rest are this
design's own parts, placed next to what they serve.

Rotation is KiCad's (degrees, counter-clockwise on screen) for side F. For
side B parts whose pins matter build.py searches for the rotation that
satisfies each one's pin rule rather than trusting an angle through the
flip: the ring LEDs (DI and VDD out, DO toward the next LED), the crystal
LEDs (DI toward the previous one), the IMU (pin 1 where its marking puts it)
and the microphone (its ring's gap toward free board).
"""
import json
import math
import os

HERE = os.path.dirname(os.path.abspath(__file__))
MEAS = os.path.join(HERE, "..", "measure")
_parts = json.load(open(os.path.join(MEAS, "parts_radio.json")))
_ring = json.load(open(os.path.join(MEAS, "ring_board.json")))
_cry = json.load(open(os.path.join(MEAS, "crystal_board.json")))

P = {}


def at(ref, x, y, rot=0.0, side="F", why=""):
    P[ref] = {"x": float(x), "y": float(y), "rot": float(rot), "side": side, "why": why}


# ---------------------------------------------------------------- radio side
u1 = _parts["U1"]["centre"]
at("U1", u1[0], u1[1], 0, why="measured: ESP32 pads through the homography")
u2 = _parts["U2"]["centre"]
at("U2", u2[0], u2[1], 180, why="measured: MAX-M10S pads; RF_IN toward the u.FL, label upside down as in the photo")
u3 = _parts["U3"]["centre"]
at("U3", u3[0], u3[1], 0, why="measured: TP4056 leads; pin 1 top-left puts VCC at the USB, BAT at the JST")
at("U4", -16.9, 15.0, 90, why="measured: the 'ACH' SOT-23-5, three-lead side down")
at("J1", 20.55, 3.1, 90,
   why="measured: USB-C centre line y 3.1; pushed as far out as its front shell tabs allow (0.3 mm from the edge)")
at("J2", 20.10, -6.07, 270,
   why="measured: the THT JST's pins (solder joints on the LED side, 1.96 mm apart); "
       "pin 1 (+) on the red wire's side, opening toward the board, housing over the corner as in the photo")
at("J3", -16.1, -2.4, 180, why="measured: u.FL under the glue; signal pad faces U2's RF_IN")
at("SW1", 3.4, -17.35, 0, why="measured: the black (power) switch's pads; spread 0.35 mm from the photo so the two courtyards clear")
at("SW2", -6.75, -17.6, 0, why="measured: the red (SOS) switch's pads; spread 0.35 mm, see SW1")
at("D3", -11.9, -17.4, 0, why="measured: the SOS LED beside SW2")
at("R6", -13.9, -17.4, 0, why="SOS LED's resistor")
at("Q4", -13.6, -8.3, 90, why="measured: the '3401' P-FET; drain (pin 3) up")
at("Q5", -17.5, -8.3, 90, why="ring switch driver, by Q4, inside the ring's footprint so its vias clear the LEDs")
at("R9", -19.8, -8.3, 90, why="Q4 gate pull-up")
at("R10", -17.5, -11.2, 0, why="LED_EN pull-down")
at("C13", -19.6, 15.0, 90, why="LDO input cap, left of the LDO")
at("C14", -16.9, 12.3, 0, why="LDO output cap, below the LDO")
at("R1", -13.1, 14.6, 90, why="measured: the '124' (120k), VSYS sense divider top; 0.5 mm down to clear R13")
at("R2", -13.1, 12.6, 90, why="VSYS sense divider bottom, under R1")
at("C4", -15.0, 17.9, 0, why="ESP32 3V3 100n, near pin 2 and clear of the tab notch")
at("C3", -14.9, 11.0, 90, why="3V3 bulk")
at("R13", -13.1, 16.8, 90, why="EN pull-up, by pin 3")
at("C2", -19.6, 12.3, 90, why="EN delay cap")
at("R3", 3.0, 0.8, 0, why="VBUS sense divider top")
at("R4", 3.0, -0.8, 0, why="VBUS sense divider bottom")
at("R20", 10.4, 3.8, 45, why="measured: CC pull-down at 45 degrees by the USB-C")
at("R21", 11.6, 4.6, 45, why="measured: CC pull-down at 45 degrees by the USB-C")
at("C10", 14.2, 6.3, 90, why="VBUS bulk at the USB-C")
at("R5", 6.3, 0.6, 0, why="TP4056 PROG (600 mA)")
at("C11", 15.0, -1.7, 90, why="battery-side bulk at the TP4056's BAT pin, above the JST housing and 5.7 mm from the magnetometer")
at("R22", 8.3, 5.6, 90, why="IO0 pull-up at pin 25")
at("C5", 1.9, -3.0, 90, why="GNSS bulk")
at("C6", 1.9, -5.0, 90, why="GNSS 100n")
for i, (x, y) in enumerate([(-20.6, 7.0), (-18.6, 7.0), (-20.6, 5.0), (-18.6, 5.0), (-20.6, 3.0), (-18.6, 3.0)]):
    at(f"TP{i + 2}", x, y, 0, why="programming pads (3V3, GND, TXD0, RXD0, EN, IO0) for a pogo jig")

# ------------------------------------------------------------------ LED side
RC = _ring["centre"]
RR = _ring["R"]
# the ring: 60 LEDs every 6 degrees; D100 at the bottom (board angle -90),
# counting clockwise seen from the radio side = counter-clockwise from the LED
# side, which is the direction the firmware's pixel mapping implies
RING_ANGLE = {}
for k in range(60):
    phi = -90.0 - 6.0 * k
    RING_ANGLE[f"D{100 + k}"] = phi
    at(f"D{100 + k}", RC[0] + RR * math.cos(math.radians(phi)), RC[1] + RR * math.sin(math.radians(phi)),
       phi, "B", why="measured: ring centre, radius and 6-degree phase (ring_board.py)")
# the crystal: measured positions; order D200.. from the bottom LED, clockwise
# seen from the radio side (the same sense as the ring; an assumption)
_c = {round(math.degrees(math.atan2(p[1] - 4.3, p[0] + 2.05))): p for p in _cry}
_order = sorted(_c, key=lambda a: (-(a + 90) % 360))       # from -90, clockwise
CRY_ROT = {}
for i, a in enumerate(_order):
    x, y = _c[a][0], _c[a][1]
    at(f"D{200 + i}", x, y, 0, "B", why="measured: crystal LED centroid (crystal.py)")
at("TP1", -2.05, 4.3, 0, "B", why="measured: the touch post's spring pad, under the ESP32 so SMD")
at("MK1", -20.0, 15.7, 0, "B", why="measured: the microphone can; 0.28 mm in from the photo to clear the corner")
at("R23", -15.2, 8.6, 0, why="mic bias, radio side: the mic signal crosses the ring on F.Cu")
at("C18", -15.2, 7.0, 0, why="mic coupling")
at("R24", -15.2, 5.4, 0, why="mic mid-rail")
at("R25", -15.2, 3.8, 0, why="mic mid-rail")
# The motion sensor where the reference board's ICM-20948 ('I2948') sits, and
# the magnetometer beside it. The LIS2MDL wants currents above 10 mA "a few
# millimeters away" (DS12144 section 5.2): here it is 4.8 mm inside the
# ring's LEDs, 14 mm from the crystal's and 13 mm from the latch, and the
# router keeps the power nets off a disc round it (route.py, MAG_KEEPOUT).
at("U5", 4.87, -5.8, 0, "B", why="measured: the reference board's IMU site; the LSM6DSV16X in its place")
at("C1", 1.9, -5.8, 90, "B", why="LSM6DSV16X Vdd 100n")
at("C7", 5.6, -2.9, 0, "B", why="LSM6DSV16X Vdd_IO 100n")
at("R7", 3.3, -9.4, 0, "B", why="I2C pull-up")
at("R8", 6.4, -9.4, 0, "B", why="I2C pull-up")
at("R26", 2.6, -2.6, 0, "B", why="LSM6DSV16X address and hub-port strap")
at("U6", 9.4, -5.8, 0, "B", why="LIS2MDL beside the LSM6DSV16X, axes square to it, away from >10 mA nets")
at("C8", 9.4, -8.1, 0, "B", why="LIS2MDL C1 220n, at pins 5-6 as its datasheet asks")
# the rest on its inner side: east of it the ring's LEDs are within 3 mm
at("C21", 8.6, -3.3, 90, "B", why="LIS2MDL Vdd_IO 100n")
at("C19", 10.2, -3.3, 90, "B", why="LIS2MDL Vdd 100n")
at("C20", 9.4, -1.2, 0, "B", why="LIS2MDL Vdd 10u")
# The charger's two status LEDs and their resistors, where the reference
# board has two 0603 LEDs and two 0603 resistors below the touch post
at("D4", 2.33, 4.78, 0, "B", why="measured: 0603 LED in the crystal (charging)")
at("D5", 2.35, 3.17, 0, "B", why="measured: 0603 LED in the crystal (charged)")
at("R27", 4.90, 3.04, 90, "B", why="measured: 0603 beside the LEDs")
at("R28", 4.41, 0.80, 0, "B", why="measured: 0603 beside the LEDs; 0.7 mm down so the two courtyards clear")
at("R11", -4.6, -13.2, 0, "B", why="ring data series resistor, by D100")
at("R12", -2.1, -5.2, 0, "B", why="crystal data series resistor, by D200")
at("C17", -6.6, 3.0, 90, "B", why="crystal supply bulk")
at("C15", -12.27, -4.4, 90, why="ring supply bulk at Q4's drain")
at("C16", -18.34, -11.24, 50, "B", why="ring supply bulk in the ring's VLED pour, where VLED comes across from Q4")
# the power latch, below the ring where the reference board has its latch SOTs
at("Q1", -8.0, -19.0, 0, "B", why="latch P-FET")
at("Q2", -4.6, -19.0, 0, "B", why="latch hold N-FET")
at("Q3", -1.2, -19.0, 0, "B", why="button sense N-FET")
at("D1", 2.4, -19.0, 0, "B", why="button -> hold diode")
at("D2", 5.6, -19.0, 0, "B", why="firmware-off diode")
at("C12", 8.4, -19.0, 90, "B", why="hold node filter")
for i, r in enumerate(["R14", "R15", "R16", "R17", "R18", "R19"]):
    at(r, -8.0 + 2.5 * i, -21.2, 0, "B", why="latch resistors")

if __name__ == "__main__":
    print(len(P), "parts placed")
