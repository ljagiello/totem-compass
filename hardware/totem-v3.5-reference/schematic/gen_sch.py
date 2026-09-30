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

import datetime
import json
import math
import subprocess
import uuid as uuidlib

import builtins
import totem  # noqa: F401  (builds the circuit as a side effect)
from kicad_sexp import dump, pin_positions, symbol

GRID = 1.27
STUB = 3 * GRID
TODAY = datetime.date.today().isoformat()

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


# A fixed namespace, so the same circuit always produces the same file.
NS = uuidlib.UUID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")


def uid(key=None):
    """A uuid derived from `key`, or a random one when there is no key.

    Symbols and footprints are linked by uuid, not by reference designator,
    so a board updated from a schematic whose uuids changed sees no
    matching footprints: it deletes every one and adds them again, losing
    all placement and routing. Random uuids would make every regeneration
    of this file destroy a layout built on the last one. They also key ERC
    exclusions, which would evaporate the same way.
    """
    return str(uuidlib.uuid5(NS, key) if key else uuidlib.uuid4())


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
        self.footprints = {}
        self.power_n = {}
        self.uuid = uid("sheet:totem")

    def need(self, lib, name):
        lib_id = f"{lib}:{name}"
        if lib_id not in self.libs:
            sym = symbol(lib, name)
            self.libs[lib_id] = rename(sym, lib_id)
            # Several library symbols name the package they are normally
            # fitted in. Carrying that through means those parts arrive with
            # a footprint already chosen rather than blank, which is what
            # "Symbols can specify a preselected footprint" is for. The rest
            # are passives and connectors, where the library cannot know and
            # a person has to decide.
            for child in sym:
                if (isinstance(child, list) and child and child[0] == "property"
                        and len(child) > 2 and isinstance(child[1], tuple)
                        and child[1][1] == "Footprint"
                        and isinstance(child[2], tuple) and child[2][1]):
                    self.footprints[lib_id] = child[2][1]
                    break
        return lib_id

    def power_ref(self, prefix):
        """Sequential #PWR01, #FLG01… rather than random hex.

        References must be unique and ERC checks for duplicates. Four random
        hex characters over 55 symbols is about a two percent chance of a
        collision on any given run — it had not happened yet, which is not
        the same as it being safe.
        """
        self.power_n[prefix] = self.power_n.get(prefix, 0) + 1
        return f"{prefix}{self.power_n[prefix]:02d}"

    def place(self, lib_id, x, y, ref, value, hide=False, on_board=True,
              hide_ref=False, value_below=False):
        """Place a symbol.

        `hide_ref` hides the reference but leaves the value showing, which
        is what a power symbol wants: its Value field *is* the net name, so
        hiding it leaves a sheet full of unlabelled arrows where no rail can
        be told from another. KiCad's own power library hides the reference
        and shows the value, for that reason.
        """
        eff = ["effects", ["font", ["size", "1.27", "1.27"]]]
        if hide:
            eff.append(["hide", "yes"])
        hidden = ["effects", ["font", ["size", "1.27", "1.27"]], ["hide", "yes"]]
        ref_eff = list(hidden) if (hide or hide_ref) else list(eff)
        val_eff = list(hidden) if hide else list(eff)
        # Ground hangs below its connection point, the supplies rise above
        # it, so the name goes on the far side in each case rather than on
        # top of the stub wire.
        val_y = y + 5 if value_below else y - 8
        footprint = self.footprints.get(lib_id, "")
        self.items.append(
            ["symbol",
             ["lib_id", ("str", lib_id)],
             ["at", f"{x:.2f}", f"{y:.2f}", "0"],
             ["unit", "1"],
             ["exclude_from_sim", "no"],
             ["in_bom", "yes" if on_board else "no"],
             ["on_board", "yes" if on_board else "no"],
             ["dnp", "no"],
             ["uuid", ("str", uid(f"sym:{ref}"))],
             ["property", ("str", "Reference"), ("str", ref),
              ["at", f"{x:.2f}", f"{y - 12:.2f}", "0"], ref_eff],
             ["property", ("str", "Value"), ("str", value),
              ["at", f"{x:.2f}", f"{val_y:.2f}", "0"], val_eff],
             ["property", ("str", "Footprint"), ("str", footprint),
              ["at", f"{x:.2f}", f"{y - 4:.2f}", "0"], list(hidden)],
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
             ["uuid", ("str", uid(f"wire:{x1},{y1}-{x2},{y2}"))]]
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
             ["uuid", ("str", uid(f"label:{name}@{x},{y}"))]]
        )

    def no_connect(self, x, y):
        self.items.append(
            ["no_connect", ["at", f"{x:.2f}", f"{y:.2f}"],
             ["uuid", ("str", uid(f"nc:{x},{y}"))]]
        )

    def render(self, paper="A1"):
        return dump(
            ["kicad_sch",
             ["version", "20250114"],
             ["generator", ("str", "gen_sch.py")],
             ["generator_version", ("str", "10.0")],
             ["uuid", ("str", self.uuid)],
             ["paper", ("str", paper)],
             # A sheet with an empty title block says nothing about what it
             # is or how far to trust it, and this one needs to say both.
             ["title_block",
              ["title", ("str", "Totem v3.5 — reference design")],
              ["date", ("str", TODAY)],
              ["rev", ("str", "A")],
              ["company", ("str", "reconstruction, not the Totem's recovered circuit")],
              ["comment", "1", ("str", "Parts and MCU pins are established; passive "
                                       "values are designed to satisfy the firmware.")],
              ["comment", "2", ("str", "Generated by gen_sch.py from totem.py — "
                                       "edit the source, not this sheet.")],
              ["comment", "3", ("str", "U3 (TP4056) has no KiCad symbol and is absent "
                                       "from this sheet; it is in the netlist.")],
              ["comment", "4", ("str", "Verified: kicad-cli sch erc, and ngspice "
                                       "decks under ../sim.")]],
             ["lib_symbols", *self.libs.values()],
             *self.items,
             # The root sheet's own instance. The format documentation lists
             # this as a section of its own, and the path for a root sheet is
             # always "/" because nothing points at it.
             ["sheet_instances", ["path", ("str", "/"), ["page", ("str", "1")]]]]
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
    #
    # Deduplicated by point, because a symbol's pins can be stacked: the
    # USB-C receptacle has four GND contacts and four VBUS contacts at one
    # place each, and drawing per pin put four wires and four ground symbols
    # on top of each other. Stacked pins are one connection, so they get one
    # symbol.
    drawn = set()
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
                if (ax, ay) in drawn:
                    continue
                drawn.add((ax, ay))
                bx, by = snap(ax + dx * STUB), snap(ay + dy * STUB)
                sheet.wire(ax, ay, bx, by)
                sheet.place(lib_id, bx, by, sheet.power_ref("#PWR"), net.name,
                            hide_ref=True, on_board=False,
                            value_below=(net.name == "GND"))

    # PWR_FLAG says a rail is fed by something off-sheet or by a part that
    # is not a power output, which is what KiCad's power_pin_not_driven rule
    # asks for. VBUS comes from a connector, VLED through a switch, and
    # ground from nothing that drives it.
    #
    # VBAT is deliberately NOT flagged: the charger's BAT pin is a real
    # power output, so the rail has a genuine driver. It needed a flag only
    # while U3 was missing from the sheet — the flag was standing in for the
    # part, which is exactly the kind of quiet compensation that hides a
    # hole rather than showing it.
    flag_id = sheet.need("power", "PWR_FLAG")
    for n, rail in enumerate(("VBUS", "VLED", "GND")):
        fx, fy = snap(60 + n * 60), snap(530)
        sheet.place(flag_id, fx, fy, sheet.power_ref("#FLG"), "PWR_FLAG",
                    hide=True, on_board=False)
        fg = pin_positions(sheet.libs[flag_id])
        px, py, rot, _ = next(iter(fg.values()))
        ax, ay = fx + px, fy - py
        dx, dy = outward(rot)
        bx, by = snap(ax + dx * STUB), snap(ay + dy * STUB)
        sheet.wire(ax, ay, bx, by)
        if rail in RAILS:
            sheet.place(sheet.need("power", RAILS[rail]), bx, by,
                        sheet.power_ref("#PWR"), rail, hide_ref=True,
                        on_board=False, value_below=(rail == "GND"))
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

    # Junction dots. KiCad adds these itself in the editor — "junction dots
    # will be automatically added to wires that start or end on top of an
    # existing wire" — but a file written from outside gets none, and
    # without them a reader cannot tell a join from a crossing. They are
    # readability rather than connectivity here: nothing in this sheet ends
    # mid-segment, so the netlist is the same either way.
    ends = {}
    for item in sheet.items:
        if item[0] != "wire":
            continue
        for xy in item[1][1:]:
            ends[(xy[1], xy[2])] = ends.get((xy[1], xy[2]), 0) + 1
    for (x, y), n in sorted(ends.items()):
        if n >= 3:
            sheet.items.append(
                ["junction", ["at", x, y], ["diameter", "0"],
                 ["color", "0", "0", "0", "0"],
                 ["uuid", ("str", uid(f"junction:{x},{y}"))]]
            )

    # The project file goes first, and it matters that it does. A loose
    # .kicad_sch opens in standalone mode, where KiCad disables design
    # synchronisation between the schematic and the board — so without this
    # there is no route from here to a layout at all. It also has to exist
    # before the upgrade below runs, or that step has no project in scope
    # and blanks the project name on all 85 symbol instances.
    #
    # `sheets` keys the root sheet by its own uuid, which is why this is
    # written here rather than kept as a static file: the two cannot drift.
    project = {
        "meta": {"filename": "totem.kicad_pro", "version": 3},
        "sheets": [[sheet.uuid, "Root"]],
        "libraries": {"pinned_footprint_libs": [], "pinned_symbol_libs": []},
        "erc": {}, "net_settings": {}, "schematic": {}, "text_variables": {},
        "board": {}, "boards": [], "cvpcb": {}, "pcbnew": {},
    }
    with open("totem.kicad_pro", "w") as fh:
        json.dump(project, fh, indent=2)
        fh.write("\n")

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
