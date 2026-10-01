# Totem v3.5 — reference design

A working circuit built from the parts identified on a Totem v3.5, with the
values chosen by calculation and checked in ngspice.

**This is a reconstruction, not a recovery.** The parts and the MCU pin
assignments are established — the parts off the board, the pins out of the
firmware, both cited in [the hardware page](../../docs/reference/hardware.mdx).
Everything else here is *designed to satisfy those facts*, not traced from the
PCB. Resistor values, the regulator topology, the LED supply arrangement: all
chosen here. Where a value is derived from something the firmware does, the
derivation is shown, and that is the strongest claim any of it makes.

Passive values cannot be recovered from photographs of an assembled board.
What can be done is to work out what they would have to be for the firmware to
behave as it does, and then check that circuit holds up. That is what this is.

## What the firmware pins down

Two constants in the firmware constrain the analog design tightly enough to
solve for it.

### The battery divider is determined, not chosen

The firmware converts its ADC reading with a fixed calibration
(`docs/subsystems/power.mdx`, `compass.dis:1446`):

```python
battery_volt = 0.0017 * self.v_supply.read() + 0.206
```

`ADC.read()` returns 0..4095. Writing the whole chain out, with `k` the
divider ratio, `VFS` the ATTN_11DB full scale and `Voff` the ADC's input
offset:

```
cal(V) = 0.0017 * 4095 * (k*V - Voff)/VFS + 0.206
```

For `cal(V) == V` at every V, both coefficients must match:

```
k    = VFS / (0.0017 * 4095) = 3.3 / 6.9615 = 0.4741
Voff = 0.206 * VFS / 6.9615  = 97.7 mV
```

The offset is not fitted — it falls out of the same two equations, and
**97.7 mV is the ESP32 ADC's well-known input offset**. That it lands there
is the evidence the model has the right shape, and it is why the `+0.206` in
the firmware is an ADC artefact rather than a fudge.

A resistor marked `124` — 120 kΩ — is on the board. Taking that as the top
leg, the ratio asks for 108.2 kΩ below it, and no standard series has that
(E96 goes 107, 110; E192 107, 109). An earlier version named a 108 kΩ
part, which is not made; the nearest that is, 109 kΩ, reads a full cell
17 mV high, over the 10 mV the checks allow. So this board uses
**113 kΩ over 102 kΩ**, both E96 and both made in 0.1%: k within 0.08% of
the firmware's, drawing 19.5 µA.
Simulated, the firmware's own calibration then reads the cell **true to
3.3 mV** across 3.4–4.2 V, against 68 mV for the 120k/100k pair tried first.

### GPIO 39 is a VBUS divider, not the charger's status pin

`modes.is_charging = self.v_in.value()` — high means charging. The TP4056's
`CHRG` is open-drain and pulls **low** while charging, and GPIO 34–39 have no
internal pull of any kind, so an open-drain source would float when idle.
Both facts are wrong for `CHRG` and right for a divider off VBUS, which also
matches the firmware's own name for it: `v_in`, the input voltage.

100 kΩ over 150 kΩ clears the logic threshold at a sagging 4.40 V charger and
stays under the absolute maximum at 5.25 V, with margin at both ends.

## Verification

```
cd sim && python3 check.py
```

Requires `ngspice` (`brew install ngspice`). Each deck computes; `check.py`
decides whether the number is allowed, and prints a line per claim. Current
state: **18 passed, 0 failed, 1 known limit recorded**.

| Block | Result |
| --- | --- |
| `batt_sense` | Pin at 1.993 V on a full cell, inside the ADC's 150–2450 mV window. Calibration true to 3.3 mV. Divider (113k over 102k) draws 19.5 µA |
| `vbus_sense` | 2.64 V at 4.40 V in (165 mV over V<sub>IH</sub>), 3.15 V at 5.25 V (450 mV under absolute max). 21 µA |
| `ldo_headroom` | Rail holds 3.293 V at a 3.4 V cell under a 532 mA peak — 293 mV over the ESP32's minimum. Fails only at 3.107 V, *below* the firmware's cutoff |
| `led_budget` | With the 1515's 15.5 mA per pixel: normal use is 0.15 C, all 67 pixels at full white at the firmware's brightness 0.62 C; 1 C would take 107. Known limit: the pixels want 3.5 V and the firmware runs the cell to 3.15 V |

The digital parts are modelled as current sinks at their datasheet peaks, and
the regulator and charger behaviourally, because no vendor models are to hand.
These verify the design's arithmetic, not anyone's silicon.

The whole board is simulated too, built from the schematic's own netlist
rather than from a hand-written deck:

```
cd sim && python3 board_sim.py
```

**78 passed, 0 failed, 1 known limit**, over five corners (cell at 4.2, 3.8
and 3.4 V; the 2N7002's threshold at 2.1 and 2.5 V). Each corner runs one
4.5-second sequence — off, press on, press while running, ring on and off,
firmware power-off, on again, held through a firmware power-off, released —
and checks the rail, GPIO 4's voltage limits and the cell's idle drain at each
step. Steady-state checks cover the rail at a TX peak with the ring lit across
the firmware's battery bands, every pixel at full white, both sense dividers
and charging. The known limit is the ring's supply at a 3.45 V cell, 3.34 V
against the 1515 LEDs' 3.5 V rating, which the reference board shares: its
ring hangs off the cell the same way.

## The board

`schematic/totem.kicad_pcb`, made by scripts in `pcb/` from the schematic
and from measurements of the reference board's photographs (`measure/`,
whose README says how, and how well, each number is known):

```
cd pcb
KP=/Applications/KiCad/KiCad.app/Contents/Frameworks/Python.framework/Versions/Current/bin/python3
$KP build.py      # outline, parts where the photos put them, nets, planes, rules
python route.py   # the router (numpy, scipy, opencv)
$KP apply.py      # tracks and vias into the board, zones filled
$KP models.py     # 3D models for the parts KiCad's library has none for
python3 fab.py    # everything for the fab and the assembler -> pcb/fab/
```

**To have boards made, hand over `pcb/fab/totem-v3.5-assembly-package.zip`.** It
holds gerbers and drills (and ODB++ and IPC-2581), the BOM and placement file,
assembly drawings, the schematic, a STEP model, and
[`pcb/ASSEMBLY.md`](pcb/ASSEMBLY.md): what to order, how to assemble, bring up
and program the board, and what to measure before ordering.

Four layers, 1.6 mm: signals on both outer layers, In1 solid GND, In2 3V3.
Tracks 0.13 mm and 0.13 mm apart, power nets 0.4 mm with 0.2 mm; vias
0.45/0.2 mm; copper 0.3 mm from the edge — a mainstream 4-layer process with
margin. The GNSS antenna feed is a straight, via-free 0.38 mm microstrip on
F.Cu over In1: 50 Ω on a standard 1.6 mm 4-layer stack-up (about 0.21 mm of
7628 prepreg under the top layer); order that stack-up, or re-size the line
for another. The LED side's ring is powered through a VLED pour outside the
ring, fed at C16 and, past the one place the ring's data has to cross it,
at D159.

Both sides carry parts (the LED side: ring, crystal, motion sensor and
magnetometer, microphone, the charge LEDs, the power latch), so assembly is
double-sided. The placement file is KiCad's;
assembly houses differ in how they read rotations on the bottom side, so
check their preview of the LED side — the ring's orientation rule is that
each LED's DI and VDD pads face out.

**`kicad-cli pcb drc --schematic-parity --severity-all`: 0 violations,
0 unconnected pads, 0 footprint errors.** Three places where the reference
board is closer than KiCad's courtyard and silkscreen margins allow (the
TP4056 0.3 mm from the module, the SOS LED between its switch's legs) are
narrow rules in `schematic/totem.kicad_dru`, each with its reason. The
router (`pcb/route.py`) also checks every net's connectivity itself before
it hands over.

What is the reference board's and what is not:

| | |
| --- | --- |
| Outline, notches, the tab | Measured: ±0.3 mm on the left half and bottom, **±1.3 mm in height on the right side** (the two photos disagree there) |
| ESP32, MAX-M10S, TP4056, LDO, u.FL, P-FET, switches, SOS LED | Measured from their pads |
| The ring | Measured: centre (−2.02, 2.45), radius 18.24 mm, 60 LEDs every 6°, **1.5 mm packages** |
| The crystal, its pogo-pin post, the IMU's site, the microphone | Measured |
| The two charge LEDs and their two resistors in the crystal, the JST's pins | Measured from close-ups of the LED side |
| The motion sensor and magnetometer | This design's: the reference board's ICM-20948 is obsolete, so an ST LSM6DSV16X sits on its site with an LIS2MDL beside it (below) |
| Passives, the latch, the ring's driver | This design's, placed beside what they serve |
| About 25 small parts in the reference board's LED-side cluster below the crystal, and a column of about 8 between the ring and the crystal | Photographed, not copied: their values and nets run under the parts and on inner layers. This design's own circuit does the jobs they must do (latch, ring switch, decoupling) |
| Every trace | Routed here; the original's are not recoverable |

**Measure these on the reference board before ordering** — each is where a
photograph could not settle it:

- **The board's height at the right edge**, and the distance from the USB-C
  to the lower-right notch: the right side is known to ±1.3 mm.
- **The USB-C.** The original's is a through-hole-shell part: on the LED
  side its four plated slots sit 4.80 mm either side of the centre line in
  two rows 3.0 mm apart, with two round holes 6.5 mm apart (measured,
  `measure/`). No catalogue footprint checked matches that (HRO
  TYPE-C-31-M-12, GCT USB4105 and USB4125, SHOU HAN TYPE-C 6P and its
  recessed 6P). The one used here, a power-only GCT USB4135, is the library
  part that clears the ring behind it; its plug face ends up **about 0.6 mm
  or more further in** than the original's (1.75 mm overhang). Check it
  against the case, and measure the original's if it can be taken out.
- **The JST** is the through-hole S2B-PH-K-S on the reference board's own
  pin positions (their solder joints, 1.96 mm apart, show on the LED side);
  its housing overhangs the lower-right chamfer as the original's does.
  Check the cable's exit against the case.
- **Button stem height.** The switches are 6 x 6 mm top-actuated tact
  switches with tall stems that go through the rear cover; the BOM names a
  9.5 mm one. The stem has to be matched to the cover.
- **The touch pogo pin**: a gold spring-loaded pin standing on the 2.5 mm
  pad at the centre of the crystal (close-up photo). Measure its length,
  barrel and travel on the reference unit: its length sets the pressure on
  the crystal.

Two things are derived, not seen:

- **Where the ring starts and which way it counts.** The links between the
  ring's LEDs run on an inner layer on the reference board, so its chain can
  not be followed. The firmware maps a bearing to pixel
  `(60 − bearing // 6) mod 60`, counter-clockwise-positive under its
  Madgwick convention, with pixel 0 along the IMU's +X; the IMU's marking puts
  +X toward the buttons. So D100 is the LED nearest the buttons and the chain
  runs counter-clockwise seen from the LED side. If the compass shows
  mirrored or rotated on the first board, this is where to look.
- **The sensors' axes.** U5 (LSM6DSV16X) and U6 (LIS2MDL) sit square to the
  board and to each other, each with pin 1 at its upper-left seen from the
  radio side (`pcb/build.py`). Their axes in the board's frame follow from
  that, from ST's axis drawings (DS13510 Figure 5, DS12144 Figure 2) and from
  their being on the LED side, which mirrors them. Confirm on the first
  board before trusting a heading: lay it flat and see which axis reads 1 g,
  then turn it and see which magnetometer axis follows north.

### Parts changed from the reference board

The board is built from parts in production. Each was checked on DigiKey or
LCSC on 2026-09-30; these are the ones that changed:

| Reference board / earlier version | Here | Why |
| --- | --- | --- |
| TDK ICM-20948 (U5) | ST **LSM6DSV16X** (U5) + **LIS2MDL** (U6), both on I2C0, interrupts to IO32 and IO33 | DigiKey: obsolete, "no longer manufactured". The ST pair is current and its sensors are better on every figure both datasheets give (table below) |
| CUI CMC-4013-SMT-TR microphone | Same Sky **CMC-4013-2-SMT-TR** | Obsolete; the -2 has the same 4 mm can and -42 dB, and its pads fall inside this footprint's |
| Everlight 19-217 red LED (SOS) | Lite-On **LTST-C191KRKT** | Not available; the green of the same series is discontinued |
| Charger status outputs left open (earlier version) | Two 0603 LEDs, red and green (D4, D5), with 1k each | The reference board has two 0603 LEDs in the crystal below the touch post; colours not visible unlit |
| SMD JST S2B-PH-SM4-TB (earlier version) | THT **S2B-PH-K-S** | The reference board's is through-hole (above) |
| "PTS645SH95SMTR92 LFS" (earlier version) | **PTS645SK95SMTR92 LFS** | The earlier part number does not exist; SK95 is the 9.5 mm SMT variant in stock |
| 108k cell-sense resistor (earlier version) | **109k** | No 108k is made (above) |

The motion sensors, from the three datasheets (TDK DS-000189 Rev 1.6;
ST DS13510 Rev 4, DS12144 Rev 6), typical values:

| | ICM-20948 | LSM6DSV16X + LIS2MDL |
| --- | --- | --- |
| Gyro noise density | 0.015 dps/√Hz | 0.0028 dps/√Hz |
| Gyro zero-rate drift over temperature | ±0.05 dps/°C | ±0.006 dps/°C |
| Accelerometer noise density | 230 µg/√Hz | 60 µg/√Hz |
| Accelerometer offset drift over temperature | ±0.80 mg/°C | ±0.07 mg/°C |
| Magnetometer initial offset | ±300 µT | ±6 µT |
| Magnetometer noise | not specified | 0.3 µT RMS |

The LIS2MDL's datasheet asks for currents above 10 mA to be kept "a few
millimeters away" (section 5.2): it sits 4.8 mm inside the ring's LEDs and
13 mm from the latch, and the router keeps every power net's track and via
3.5 mm from its centre (`MAG_KEEPOUT` in `pcb/route.py`). The charger, 5.4 mm
away on the radio side, carries its current only while charging.

## The schematic, and its ERC

```
cd schematic && python3 totem.py
```

Requires `skidl` (`pip3 install --user skidl`). The circuit is described as
code rather than drawn, which makes the connectivity machine-checkable:
SKiDL's ERC looks for pins left floating and for nets with two things driving
them. It writes `totem.net`, a KiCad netlist, and `totem.erc`.

**Current state: 0 ERC errors and 0 ERC warnings.**

An earlier version left the TP4056's `CHRG` and `STDBY` status outputs open
and ERC reported both. They now drive the two charge LEDs the reference
board has in its crystal, so nothing is left open on purpose.

Netlist generation separately reports 27 `No footprint for …` errors. Those
are not design faults — no footprints are assigned because this is a
schematic-level design with no board layout intended.

Getting there took four real corrections, which is the argument for running it
at all rather than drawing a diagram and trusting it:

- Three `POWER-OUT` conflicts, where I had the TP4056's two `BAT` pins and the
  cell all declared as independent drivers of `VBAT`. Pins 5 and 6 are one
  node on the die, and the cell shares that node rather than fighting it.
- A fourth on `GND`, from declaring connector grounds as drivers.
- `TEMP` left floating. The TP4056 needs it tied off when no thermistor is
  fitted, which is easy to forget and silent when forgotten.
- The regulator's `EN` left floating — which is where the GPIO 4 power gate
  has to live, so the warning was pointing at a missing part of the design.

A fifth was mine rather than the design's, and is worth recording because of
how it hid. Trying to silence the two status-pin warnings, I imported a name
SKiDL 2.3.0 does not export. The script then died on the import — but the
run before it had been `rm -f totem.erc` followed by the script with output
sent to `/dev/null`, so an empty file was left behind and read back as a
clean result. **An absent report is not a passing one.** `check.py` fails
loudly on a missing measurement for the same reason; this checker now
insists the ERC file exists before believing it.

### Real symbols, and what that bought

Parts come from **KiCad's own symbol libraries** wherever they exist —
ESP32-WROOM-32E, MAX-M10S, ICM-20948, WS2812B, and the passives — so the
netlist carries real package pin numbers rather than labels invented here.
Only the TP4056 is still declared inline, because KiCad has no symbol for it.

This is not cosmetic. On the module, pins 4 and 5 are named `SENSOR_VP` and
`SENSOR_VN` — GPIO 36 and GPIO 39, the microphone and the VBUS sense.
Wiring against the real symbol is what puts those on the correct physical
pins. Two things the real symbols caught that a hand-written pin list could
not:

- **`EN` was unconnected.** The ESP32 will not run with its enable floating,
  and it needs the pull-up and capacitor that give it a power-on reset. The
  hazard of a pin list written by hand is that a pin nobody thought of is
  also a pin nobody notices is missing.
- **`RESV` is typed NO-CONNECT** on the ICM-20948 symbol, and I had tied it
  to ground. The symbol is authored from the datasheet, so it wins.

### The sheet

```
python3 gen_sch.py                                    # writes totem.kicad_sch
kicad-cli sch erc --output totem_erc.rpt totem.kicad_sch
kicad-cli sch export svg --output . totem.kicad_sch   # writes totem.svg
```

**Open `totem.kicad_sch` in KiCad.** It is generated from the same `totem.py`
the ERC checks, so the drawing cannot drift away from the netlist, and it
embeds KiCad's own symbol definitions — the drawing of a resistor is KiCad's
drawing of a resistor.

It is drawn the way a schematic is drawn. **Rails carry KiCad's power
symbols** at each pin, which is how every real sheet does it and which took
about 150 labels off the drawing. **Short point-to-point runs are wired**, so
a divider or an RC is a line you can follow. **Signals with several
destinations keep a label**, which is also what a real sheet does.

A first version put a label on every pin instead. It passed ERC and was
unreadable — nothing on it *looked* connected, which fails the only job a
schematic has.

Long routes are deliberately not drawn, and ERC is the reason. Routing every
two-ended net with an automatic corner produced this:

```
[multiple_net_names]: Both ESP_EN and POWER_EN are attached to the same items
```

A wire endpoint had landed on another net's wire and shorted two separate
nets together — a drawing that looked right and was electrically wrong. Runs
longer than 45 mm are labelled rather than routed for that reason. Laying
those out properly is hand work in KiCad, on top of this file.

**KiCad's own ERC: 0 errors, 2 warnings.** Both are explainable and left
reported. `SDO/AD0` is the ICM-20948's address-select pin strapped low, which
KiCad notes because a bidirectional pin is sitting on a power net. `PROG` is
isolated because U3 is not on the sheet at all — the TP4056 has no KiCad
symbol, so it is the one part that cannot be drawn, and `gen_sch.py` says so
when it runs rather than quietly omitting it.

Getting there took three rounds, each of which is a thing a hand-drawn sheet
would have hidden:

- **124 off-grid endpoints**, from a 40 mm margin that is not a multiple of
  KiCad's 1.27 mm grid. With a label on every pin, one careless constant is
  one violation per pin.
- **35 unconnected pins**, now carrying explicit no-connect markers, which is
  how a schematic says "deliberately" rather than leaving a reader to guess.
- **A symbol that drew as nothing.** `AP2127K-3.3` *extends* `AP2204K-1.5`,
  so its own block holds properties and no pins at all. Embedded as it comes,
  the regulator would have landed on the sheet with no pins to attach a wire
  to. `kicad_sexp.py` flattens inherited symbols for that reason.

### The earlier drawing, and why it went

SKiDL 2.3.0 cannot draw this circuit: both `generate_svg` and
`generate_schematic` crash in its `kicad10` backend on the real multi-unit
symbols (`gen_svg.py:310`, `AttributeError: 'NoneType' object has no
attribute 'net'`). That is why `gen_sch.py` exists and writes the sheet
directly.

An earlier SVG did exist, made when the parts were simplified inline
definitions, and it was **deleted rather than kept**: once the parts became
real symbols it no longer matched the netlist, and a picture that disagrees
with the netlist is worse than no picture.

## Three findings worth keeping

**The firmware protects the hardware, not the other way round.** The 3V3 rail
only collapses below the ESP32's 3.0 V minimum at a cell voltage of 3.107 V.
The firmware gives up on the cell around 3.4 V, so it always stops first —
about 290 mV of margin.

**Every pixel at full white is 2.41 A, or 2.41 C.** A 1000 mAh pouch cell is
good for about 1 C, so the ring simply cannot be filled white at the default
brightness; the ceiling is 27.8 pixels. This is not a flaw in the design, it
is a constraint the firmware already respects — it lights north and the peers,
and eco mode blanks the ring entirely. It does mean any future animation that
fills the ring has a current budget to answer to.

**The pixels are under-supplied at the bottom of the discharge.** WS2812B want
VDD ≥ 3.5 V and the firmware runs the cell to 3.4 V, leaving them 100 mV
short. The strips cannot move to the 3V3 rail instead — that is further below
their minimum, and 2.4 A through a linear regulator is not a circuit anyone
builds. Either the fitted pixels are a low-voltage variant, or colour accuracy
degrades near empty. Both are testable on the bench; neither is settled here.

A fourth is a caution rather than a finding: 481 mW in the pass element at a
full cell would be 120 °C of rise in a SOT-23-5 at θ<sub>JA</sub> ≈ 250 °C/W.
That figure is the **peak**, at a 500 mA radio transmit burst with a
low duty cycle, so it is transient and the average is far below it. A design
that drew that continuously would need a different package or a switcher.

## Not covered

- **The reference board's traces and inner layers.** Not recoverable from an
  assembled board; this board is routed afresh (see "The board").
- **The GNSS UART pins** on the module side are taken from the firmware's
  `UART(1, rx=16, tx=17)`, not traced.
- **The `J22B` SOT-23-5 and the `A7` / `65KS` SOD-123s** on the LED side.
  Legible markings, nets untraced. The `A7` is the usual marking of a
  1N4148W, which this design's latch uses; the rest are left out rather than
  drawn in wrongly. (What was listed here as a separate "U6" SOIC-14 marked
  `I2948` is the IMU itself, photographed at an angle.)
- **The power gate's topology** is designed to the firmware's behaviour, not
  recovered: GPIO 4 read as an input while running, re-opened as an output and
  driven low to switch off. It is simulated in every corner above.
