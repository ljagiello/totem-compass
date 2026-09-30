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

**Current state: 0 errors, 0 warnings, across 22 nets.**

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

Parts are declared with explicit pins instead of pulled from KiCad's symbol
libraries, so this runs without KiCad installed. KiCad's own installer needs
root, which is why the drawing is a netlist rather than a sheet; `brew install
--cask kicad` from a terminal would allow `kicad-cli sch erc` and an SVG
export on top of this.

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
