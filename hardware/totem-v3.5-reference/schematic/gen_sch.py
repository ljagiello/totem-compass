#!/usr/bin/env python3
"""Write a KiCad sheet from the circuit, drawn the way a schematic is drawn.

An earlier version of this put a label on every pin. That is technically a
schematic and it passed ERC, but nothing on the sheet *looked* connected,
which fails the only job a schematic has. This draws it properly:

  - **Rails** (GND, +3V3, VBUS, VBAT) get KiCad's power symbols at each pin,
    the way every real schematic does it. That alone takes about 150 labels
    off the sheet.
  - **Point-to-point signals** — the 12 nets with exactly two ends — are
    wired, so the connection is a line you can follow.
  - **Nets with more than two ends** keep a label, which is also what a real
    schematic does once a signal has several destinations.

Parts are placed by hand below, grouped by what they do, because automatic
placement is what made the first attempt unreadable.

Symbols are copied out of KiCad's own libraries by kicad_sexp.py, so the
drawing of a resistor is KiCad's drawing of a resistor. The sheet comes from
the same totem.py the ERC checks, so it cannot drift from the netlist.

Run: python3 gen_sch.py
Writes: totem.kicad_sch (upgraded to the current format by kicad-cli)
"""

import math
import subprocess
import uuid as uuidlib

import builtins
import totem  # noqa: F401  (builds the circuit as a side effect)
from kicad_sexp import dump, pin_positions, symbol

GRID = 1.27
STUB = 3 * GRID

# Rails drawn as power symbols rather than labels.
RAILS = {"GND": "GND", "+3V3": "+3V3", "VBUS": "VBUS", "VBAT": "+BATT"}

# Placed by function: power down the left, the MCU in the middle, the
# things it talks to on the right, the things a person presses at the
# bottom. Coordinates are millimetres and get snapped to the grid.
FLOORPLAN = {
    # power in
    "J1": (55, 55), "U4": (185, 60), "J2": (55, 165), "R5": (120, 120),
    "R3": (300, 40), "R4": (300, 85), "R1": (300, 150), "R2": (300, 195),
    # the MCU
    "U1": (470, 190),
    # reset
    "R13": (370, 95), "C2": (405, 130),
    # GNSS
    "U2": (680, 80), "J3": (790, 80),
    # the 9-axis part
    "U5": (680, 245), "R7": (605, 195), "R8": (640, 195), "C1": (770, 265),
    # the strips
    "R9": (600, 345), "Q1": (660, 345), "DS1": (760, 330), "DS61": (760, 400),
    # what a person touches
    "SW1": (130, 300), "R10": (75, 345), "R11": (240, 300),
    "SW2": (130, 370), "TP1": (130, 425),
    "MK1": (245, 420), "R12": (245, 370),
    # the SOS indicator
    "R6": (600, 450), "DS68": (665, 450),
}


def snap(v):
    return round(v / GRID) * GRID


def uid():
    return str(uuidlib.uuid4())


def rename(sym, lib_id):
    out = list(sym)
    for i, child in enumerate(out):
        if isinstance(child, tuple) and child[0] == "str":
            out[i] = ("str", lib_id)
            break
    return out


def outward(rot):
    """Where a pin's wire runs, away from the body, in sheet coordinates."""
    return -math.cos(math.radians(rot)), math.sin(math.radians(rot))


def label_angle(dx, dy):
    if abs(dx) > abs(dy):
        return 0 if dx > 0 else 180
    return 270 if dy > 0 else 90


class Sheet:
    def __init__(self):
        self.items = []
        self.libs = {}
        self.uuid = uid()

    def need(self, lib, name):
        lib_id = f"{lib}:{name}"
        if lib_id not in self.libs:
            self.libs[lib_id] = rename(symbol(lib, name), lib_id)
        return lib_id

    def place(self, lib_id, x, y, ref, value, hide=False, on_board=True):
        eff = ["effects", ["font", ["size", "1.27", "1.27"]]]
        if hide:
            eff.append(["hide", "yes"])
        self.items.append(
            ["symbol",
             ["lib_id", ("str", lib_id)],
             ["at", f"{x:.2f}", f"{y:.2f}", "0"],
             ["unit", "1"],
             ["exclude_from_sim", "no"],
             ["in_bom", "yes" if on_board else "no"],
             ["on_board", "yes" if on_board else "no"],
             ["dnp", "no"],
             ["uuid", ("str", uid())],
             ["property", ("str", "Reference"), ("str", ref),
              ["at", f"{x:.2f}", f"{y - 12:.2f}", "0"], list(eff)],
             ["property", ("str", "Value"), ("str", value),
              ["at", f"{x:.2f}", f"{y - 8:.2f}", "0"], list(eff)],
             ["instances",
              ["project", ("str", "totem"),
               ["path", ("str", "/" + self.uuid),
                ["reference", ("str", ref)], ["unit", "1"]]]]]
        )

    def wire(self, x1, y1, x2, y2):
        if (x1, y1) == (x2, y2):
            return
        self.items.append(
            ["wire",
             ["pts", ["xy", f"{x1:.2f}", f"{y1:.2f}"], ["xy", f"{x2:.2f}", f"{y2:.2f}"]],
             ["stroke", ["width", "0"], ["type", "default"]],
             ["uuid", ("str", uid())]]
        )

    def label(self, name, x, y, angle):
        """A local label.

        The manual is explicit that local labels connect within a sheet and
        global labels connect across sheets regardless of hierarchy. This
        design is one flat sheet, so globals would be claiming a scope it
        does not have — and their boxed outlines are much of what made the
        first drawing unreadable.
        """
        self.items.append(
            ["label", ("str", name),
             ["at", f"{x:.2f}", f"{y:.2f}", str(angle)],
             ["effects", ["font", ["size", "1.27", "1.27"]], ["justify", "left"]],
             ["uuid", ("str", uid())]]
        )

    def no_connect(self, x, y):
        self.items.append(
            ["no_connect", ["at", f"{x:.2f}", f"{y:.2f}"], ["uuid", ("str", uid())]]
        )

    def render(self, paper="A1"):
        return dump(
            ["kicad_sch",
             ["version", "20250114"],
             ["generator", ("str", "gen_sch.py")],
             ["generator_version", ("str", "10.0")],
             ["uuid", ("str", self.uuid)],
             ["paper", ("str", paper)],
             ["lib_symbols", *self.libs.values()],
             *self.items]
        )


def main():
    circuit = builtins.default_circuit
    sheet = Sheet()
    where = {}          # ref -> (lib_id, x, y)
    pin_at = {}         # (ref, pin num) -> (x, y, outward dx, dy)
    undrawn = []

    spare_x, spare_y = 55, 500
    for part in sorted(circuit.parts, key=lambda p: p.ref):
        try:
            lib = str(part.lib.filename).replace(".kicad_sym", "")
        except AttributeError:
            undrawn.append(part)
            continue
        try:
            lib_id = sheet.need(lib, part.name)
        except (KeyError, FileNotFoundError, OSError):
            undrawn.append(part)
            continue
        x, y = FLOORPLAN.get(part.ref, (spare_x, spare_y))
        if part.ref not in FLOORPLAN:
            spare_x += 45
        where[part.ref] = (lib_id, snap(x), snap(y))

    for part in circuit.parts:
        if part.ref not in where:
            continue
        lib_id, x, y = where[part.ref]
        sheet.place(lib_id, x, y, part.ref, str(part.value or part.name))
        geom = pin_positions(sheet.libs[lib_id])
        for pin in part.pins:
            nums = pin.num if isinstance(pin.num, list) else [pin.num]
            for num in nums:
                g = geom.get(str(num))
                if g is None:
                    continue
                px, py, rot, _ = g
                ax, ay = x + px, y - py
                dx, dy = outward(rot)
                if pin.net is None:
                    sheet.no_connect(ax, ay)
                else:
                    pin_at[(part.ref, str(num))] = (ax, ay, dx, dy)

    # Rails become power symbols, one per pin, which is how a schematic says
    # "this is ground" without drawing a line to every other ground.
    for net in circuit.nets:
        if net.name not in RAILS:
            continue
        lib_id = sheet.need("power", RAILS[net.name])
        for pin in net.pins:
            nums = pin.num if isinstance(pin.num, list) else [pin.num]
            for num in nums:
                spot = pin_at.get((pin.part.ref, str(num)))
                if spot is None:
                    continue
                ax, ay, dx, dy = spot
                bx, by = snap(ax + dx * STUB), snap(ay + dy * STUB)
                sheet.wire(ax, ay, bx, by)
                sheet.place(lib_id, bx, by, f"#PWR{uid()[:4]}", net.name,
                            hide=True, on_board=False)

    # PWR_FLAG says a rail is fed by something off-sheet or by a part that
    # is not a power output, which is what KiCad's power_pin_not_driven rule
    # asks for. VBAT comes from a cell, VBUS from a connector, VLED through
    # a switch, and none of those is a power-output pin.
    flag_id = sheet.need("power", "PWR_FLAG")
    for n, rail in enumerate(("VBAT", "VBUS", "VLED", "GND")):
        fx, fy = snap(60 + n * 60), snap(530)
        sheet.place(flag_id, fx, fy, f"#FLG{n}", "PWR_FLAG", hide=True, on_board=False)
        fg = pin_positions(sheet.libs[flag_id])
        px, py, rot, _ = next(iter(fg.values()))
        ax, ay = fx + px, fy - py
        dx, dy = outward(rot)
        bx, by = snap(ax + dx * STUB), snap(ay + dy * STUB)
        sheet.wire(ax, ay, bx, by)
        if rail in RAILS:
            sheet.place(sheet.need("power", RAILS[rail]), bx, by,
                        f"#PWR{uid()[:4]}", rail, hide=True, on_board=False)
        else:
            sheet.label(rail, bx, by, label_angle(dx, dy))

    # Two-ended signals are wired, so the connection is a line to follow.
    # Everything else keeps a label, as a real sheet does once a signal has
    # more than one destination.
    for net in circuit.nets:
        if net.name in RAILS:
            continue
        ends = []
        for pin in net.pins:
            nums = pin.num if isinstance(pin.num, list) else [pin.num]
            for num in nums:
                spot = pin_at.get((pin.part.ref, str(num)))
                if spot:
                    ends.append(spot)
        near = False
        if len(ends) == 2:
            (ax, ay, adx, ady), (bx, by, bdx, bdy) = ends
            near = math.dist((ax, ay), (bx, by)) <= 45
        if near:
            a2 = (snap(ax + adx * STUB), snap(ay + ady * STUB))
            b2 = (snap(bx + bdx * STUB), snap(by + bdy * STUB))
            sheet.wire(ax, ay, *a2)
            sheet.wire(bx, by, *b2)
            # One corner between them, which keeps the run orthogonal.
            sheet.wire(a2[0], a2[1], b2[0], a2[1])
            sheet.wire(b2[0], a2[1], b2[0], b2[1])
        else:
            for ax, ay, dx, dy in ends:
                bx, by = snap(ax + dx * STUB), snap(ay + dy * STUB)
                sheet.wire(ax, ay, bx, by)
                sheet.label(net.name, bx, by, label_angle(dx, dy))

    with open("totem.kicad_sch", "w") as fh:
        fh.write(sheet.render() + "\n")

    # KiCad writes a newer format than anything that can be hand-built here,
    # and says so in a banner when it opens an older one. Let it do the
    # conversion rather than guessing the version number.
    subprocess.run(["kicad-cli", "sch", "upgrade", "totem.kicad_sch"],
                   capture_output=True, check=False)

    print(f"totem.kicad_sch: {len(where)} symbols, "
          f"{sum(1 for i in sheet.items if i[0] == 'wire')} wires")
    for part in undrawn:
        print(f"  not drawn (no KiCad symbol): {part.ref} {part.name}")


if __name__ == "__main__":
    main()
