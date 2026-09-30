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
leg, the ratio asks for 108 kΩ below it, which is a standard E96 value.
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
state: **17 passed, 0 failed, 2 known limits recorded**.

| Block | Result |
| --- | --- |
| `batt_sense` | Pin at 1.989 V on a full cell, inside the ADC's 150–2450 mV window. Calibration true to 3.3 mV. Divider draws 18 µA |
| `vbus_sense` | 2.64 V at 4.40 V in (165 mV over V<sub>IH</sub>), 3.15 V at 5.25 V (450 mV under absolute max). 21 µA |
| `ldo_headroom` | Rail holds 3.293 V at a 3.4 V cell under a 534 mA peak — 293 mV over the ESP32's minimum. Fails only at 3.107 V, *below* the firmware's cutoff |
| `led_budget` | Normal use is 0.58 C. The 1 C ceiling is 27.8 pixels |

The digital parts are modelled as current sinks at their datasheet peaks, and
the regulator and charger behaviourally, because no vendor models are to hand.
These verify the design's arithmetic, not anyone's silicon.

## The schematic, and its ERC

```
cd schematic && python3 totem.py
```

Requires `skidl` (`pip3 install --user skidl`). The circuit is described as
code rather than drawn, which makes the connectivity machine-checkable:
SKiDL's ERC looks for pins left floating and for nets with two things driving
them. It writes `totem.net`, a KiCad netlist, and `totem.erc`.

**Current state: 0 ERC errors and 2 ERC warnings, across 25 nets.**

Both warnings are the TP4056's `CHRG` and `STDBY` status outputs, left open
on purpose because charging is sensed from VBUS instead. ERC is right to
report them and they are left reported rather than silenced: a construct that
hid these would hide the next real one too.

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

- **Trace routing and inner layers.** Not recoverable from an assembled board.
- **The GNSS UART pins.** Visible leaving the module, not followable.
- **U6** (`I2948` SOIC-14), the `J22B` SOT-23 and the `A7` diode. Identified as
  markings, their nets untraced. The SOT-23 and the diode are plausible parts
  of the GPIO 4 latch and the LED supply switch, but that is a guess and they
  are left out rather than drawn in wrongly.
- **Decoupling.** Omitted throughout; assume the usual per-rail capacitors.
- **The power gate's topology.** This is the weakest part of the
  reconstruction and is marked so in the source. The firmware fixes what
  GPIO 4 *does* — read as an input while running, re-opened as an output and
  driven low to switch off — but not how the latch around it is built. The
  `A7` diode and the `J22B` SOT-23 on the board are plausible members of it.
  Neither was traced, and the arrangement here is designed, not recovered.
