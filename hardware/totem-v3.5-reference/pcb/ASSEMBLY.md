# Totem v3.5 reference board: fabrication and assembly

Everything needed to have this board made, assembled, brought up and
programmed. Made by `pcb/fab.py` from `schematic/totem.kicad_pcb`; the
design's reasoning is in `../README.md`.

## What is in the package

| File | For | What it is |
| --- | --- | --- |
| `totem-v3.5-gerbers.zip` | PCB fab | Gerber X2, 4 copper layers, mask, paste, silk, outline; Excellon drills (plated and unplated) with maps; `totem-netlist.ipc`, the IPC-D-356 netlist for electrical test |
| `totem-v3.5-odb.zip`, `totem-v3.5-ipc2581.zip` | PCB fab | The same board as ODB++ and IPC-2581 C (zipped XML), for fabs that take one file |
| `totem-bom.csv` | Assembler | One line per part: value, designators, footprint, quantity, side, manufacturer, MPN, note |
| `totem-cpl.csv` | Assembler | Pick-and-place: designator, X, Y (mm), side, rotation |
| `totem-v3.5-assembly-top.pdf`, `-bottom.pdf` | Assembler | Every part's outline with its designator, pads, pin 1 and polarity marks. The bottom sheet is drawn as seen from the bottom |
| `totem-offboard.csv` | You | What is bought but not placed: cell, GNSS antenna, programming adapter |
| `totem-v3.5-schematic.pdf` | Everyone | The schematic |
| `totem-v3.5.step` | Mechanical | The assembled board, for checking the case |
| `render-top.png`, `render-bottom.png`, `views/` | Everyone | 3D renders; copper, mask and silk of each side |
| `totem-v3.5-board-stats.txt` | PCB fab | Board size, layer count, hole and pad counts |

## Ordering the bare board

| | |
| --- | --- |
| Layers | 4: F.Cu signals, In1 solid GND, In2 3V3, B.Cu signals |
| Size | 45.75 x 46.36 mm, routed outline with notches (no V-score) |
| Thickness | 1.6 mm |
| Stack-up | Standard 1.6 mm 4-layer with about **0.21 mm of 7628 prepreg** between F.Cu and In1 (JLCPCB JLC04161H-7628 or equivalent) |
| Impedance | **One controlled line:** GNSS_RF, 0.38 mm on F.Cu over In1 = 50 Ω on that stack-up. On any other stack-up, re-size it first |
| Copper | 1 oz outer, 0.5 oz inner |
| Minimums used | Track and space 0.13 mm; vias 0.45 mm / 0.2 mm drill (power 0.6 / 0.3); copper 0.3 mm from the edge |
| Finish | **ENIG**: the 0.5 mm-pitch LGAs (U5, U6), castellated modules (U1, U2) and 1.5 mm LEDs want flat pads |
| Mask / silk | Green / white both sides, as the reference board. Vias tented |
| Test | Electrical test against `totem-netlist.ipc` |

## Assembly

**Double-sided SMT, reflow both sides, and one through-hole part.**
Assemble the **bottom (LED side) first**: small parts only (ring, crystal,
motion sensor and magnetometer, microphone, charge LEDs, latch). Then the
**top (radio side)**: ESP32 module, GNSS module, charger, USB-C,
switches. The heavy parts then see one reflow, and upright. **J2**, the
battery connector, is through-hole: hand or selective solder it last.

- **Stencil:** 0.12 mm, both sides. The ring's 1.5 mm LEDs and the
  sensors' 0.5 mm pitch set this.
- **Moisture:** the LEDs (D100-D159, D200-D206), U2 (MAX-M10S), U5
  (LSM6DSV16X) and U6 (LIS2MDL) are moisture sensitive. Bake to their
  datasheets if their floor life has run out.
- **The magnetometer (U6):** keep magnetised tools, magnetic pick-up heads
  and magnets away from it once placed. A strong field shifts its offset
  (its datasheet excludes drift from magnetic shock from its offset
  figure); calibrate it in the finished unit regardless.
- **Profile:** lead-free (SAC305). The WS2812-type LEDs are the most
  heat-sensitive parts on the board: keep the peak and time above
  liquidus within the XL-1515 datasheet's limits.
- **Rotations, bottom side:** assembly houses read bottom-side rotation in
  the CPL differently. Check their placement preview against
  `totem-v3.5-assembly-bottom.pdf` before they run it. The rule for the
  ring: every LED's **DI and VDD pads face out** from the ring, and its
  triangle mark is on the DO/GND side. D100 is the LED nearest the buttons,
  and the chain runs counter-clockwise seen from the LED side.
- **Inspection:** X-ray U5 and U6 (LGA, no visible joints) and the
  modules' ground pads; AOI the ring for tombstoned or rotated LEDs.
- **Not placed:** TP2-TP7 are bare pads for a pogo jig. There is nothing to
  place on them.

### By hand, after reflow

1. **TP1, the touch pogo pin.** A gold surface-mount spring-loaded pin
   soldered upright on the 2.5 mm pad in the centre of the crystal, LED
   side, as on the reference board. Its length sets the pressure on the
   crystal. Match it to the reference unit (see "Before ordering").
2. **J2**, if the assembler did not fit it: S2B-PH-K-S, through-hole, its
   opening toward the board's centre and its housing over the corner.
3. **Cell:** 3.7 V 1000 mAh Li-Po on J2. **J2 pin 1 is + (VBAT), pin 2 is
   GND.** JST PH leads are not wired the same way by every seller, so check
   the plug before connecting.
4. **GNSS antenna** on J3 (U.FL).

## Bring-up

Do it without a cell first, from a current-limited bench supply.

1. **Before power:** check that VBAT, VSYS, +3V3 and VBUS are not shorted
   to GND. TP2 is +3V3 and TP3 is GND.
2. **Supply on J2:** 3.8 V, limit 100 mA, + on pin 1. The board draws
   almost nothing while off: the latch keeps VSYS off.
3. **Press SW1 (power):** VSYS comes up, and +3V3 on TP2 reads **3.3 V**.
   It stays on after release, because the latch holds itself from +3V3.
4. **USB-C on J1**, with the supply swapped for a discharged cell: the
   TP4056 charges at **600 mA** (R5 = 2 k). The charge LEDs in the crystal
   show it: **D4 (red) while charging, D5 (green) once full**, both only
   with USB-C plugged in. D3, the red LED by the SOS button, is the
   firmware's (SOS), not the charger's.
5. **Program** (below). The firmware then drives the ring and the crystal.
   All 67 pixels at full white is 0.62 C at the firmware's brightness. Run
   that once to check every pixel, and look for a dark one in the chain.

The design's numbers (rails under load, sense dividers, latch behaviour
over cell voltage and FET threshold) are in `../README.md`, Verification.

## Programming

The ESP32 has no USB, and the USB-C carries power only, so the board is
flashed over UART0 through the pads:

| Pad | Signal | Adapter |
| --- | --- | --- |
| TP2 | +3V3 | 3.3 V (only if the board is not powered itself) |
| TP3 | GND | GND |
| TP4 | TXD0 | RX |
| TP5 | RXD0 | TX |
| TP6 | EN | RTS (reset) |
| TP7 | IO0 | DTR (boot) |

`esptool` drives EN and IO0 through RTS and DTR on its own. Without those
lines: hold **SOS (SW2)**, which is IO0, while resetting. The firmware image
is not part of this package.

## Before ordering

These could not be settled from photographs. Measure them on the reference
unit (`../README.md`, The board):

- **The right edge:** its height, and the USB-C slot to the lower-right
  notch. Known only to ±1.3 mm.
- **USB-C position against the case.** Its plug face sits about 0.6 mm
  further in than the original's. The JST is on the original's pin
  positions.
- **Switch stem length** against the rear cover. The BOM names a 9.5 mm
  PTS645.
- **The touch pogo pin's length** and barrel diameter.

**Known limit:** the XL-1515 LEDs are rated from 3.5 V, and the ring runs
from the cell, which the firmware takes down to 3.15 V. The reference board
has the same arrangement.

**What this board is:** the reference unit's outline, and its major parts
where the photographs put them. The passives and every trace are this
design's own, not a copy of the original's (`../README.md`).
