#!/usr/bin/env python3
"""Write a KiCad sheet from the circuit, so KiCad can check and draw it.

SKiDL 2.3.0 cannot draw this circuit — both of its drawing backends crash on
real multi-unit symbols — so the sheet is built here instead. KiCad's own
symbol definitions are copied into it verbatim, which means the drawing of a
resistor is KiCad's drawing of a resistor rather than anything invented here.

Connections are made with labels rather than routed wires. That is a real
schematic style and an honest one for a generated sheet: automatic routing
would produce a tangle that looks like a drawing but explains nothing, while
a label at every pin says exactly what it is connected to and stays readable.

The sheet comes from the same `totem.py` the ERC checks, so it cannot drift
away from the netlist.

Run: python3 gen_sch.py
Writes: totem.kicad_sch  (then: kicad-cli sch erc / sch export svg)
"""

import math
import uuid as uuidlib

import builtins
import totem  # noqa: F401  (builds the circuit as a side effect)
from kicad_sexp import dump, head, pin_positions, sval, symbol

# Everything here is a whole multiple of KiCad's 1.27 mm grid. It has to be:
# a wire endpoint off the grid is a violation, and with a label on every pin
# that is one violation per pin. The first run produced 124 of them from
# nothing worse than a 40 mm margin.
GRID = 1.27
STUB = 3 * GRID      # wire between a pin and its label
COL_W, ROW_H = 75 * GRID, 82 * GRID
COLS = 6
MARGIN = 30 * GRID


def uid():
    return str(uuidlib.uuid4())


def rename(sym, lib_id):
    """A copy of a symbol definition carrying its full LIB:NAME id."""
    out = list(sym)
    for i, child in enumerate(out):
        if isinstance(child, tuple) and child[0] == "str":
            out[i] = ("str", lib_id)
            break
    return out


def outward(rot):
    """Where a pin's wire should run, away from the body, in sheet coords."""
    dx = -math.cos(math.radians(rot))
    dy = math.sin(math.radians(rot))
    return dx, dy


def label_angle(dx, dy):
    if abs(dx) > abs(dy):
        return 0 if dx > 0 else 180
    return 270 if dy > 0 else 90


def main():
    circuit = builtins.default_circuit
    parts = sorted(circuit.parts, key=lambda p: (p.ref[0], len(p.ref), p.ref))

    lib_symbols, placements, items = {}, [], []

    for i, part in enumerate(parts):
        # SKiDL raises rather than returning a default from getattr, and a
        # part declared inline has no library at all.
        try:
            lib = str(part.lib.filename).replace(".kicad_sym", "")
        except AttributeError:
            lib = None
        lib_id = f"{lib}:{part.name}" if lib else None
        if lib_id is None:
            placements.append((part, None, None, None))
            continue
        if lib_id not in lib_symbols:
            try:
                lib_symbols[lib_id] = rename(symbol(lib, part.name), lib_id)
            except (KeyError, FileNotFoundError, OSError):
                # The TP4056 has no KiCad symbol; it is declared inline in
                # totem.py and so cannot be drawn here. Skipped rather than
                # faked, and reported at the end.
                placements.append((part, None, None, None))
                continue
        col, row = i % COLS, i // COLS
        x = MARGIN + col * COL_W
        y = MARGIN + row * ROW_H
        placements.append((part, lib_id, x, y))

    undrawn = []
    for part, lib_id, x, y in placements:
        if lib_id is None:
            undrawn.append(part)
            continue
        pins = pin_positions(lib_symbols[lib_id])
        items.append(
            [
                "symbol",
                ["lib_id", ("str", lib_id)],
                ["at", f"{x:.2f}", f"{y:.2f}", "0"],
                ["unit", "1"],
                ["exclude_from_sim", "no"],
                ["in_bom", "yes"],
                ["on_board", "yes"],
                ["dnp", "no"],
                ["uuid", ("str", uid())],
                [
                    "property", ("str", "Reference"), ("str", part.ref),
                    ["at", f"{x:.2f}", f"{y - 45:.2f}", "0"],
                    ["effects", ["font", ["size", "1.6", "1.6"]]],
                ],
                [
                    "property", ("str", "Value"), ("str", str(part.value or part.name)),
                    ["at", f"{x:.2f}", f"{y - 41:.2f}", "0"],
                    ["effects", ["font", ["size", "1.27", "1.27"]]],
                ],
                [
                    "instances",
                    [
                        "project", ("str", "totem"),
                        ["path", ("str", "/" + SHEET_UUID),
                         ["reference", ("str", part.ref)], ["unit", "1"]],
                    ],
                ],
            ]
        )

        for pin in part.pins:
            if pin.net is None:
                # A pin this design deliberately does not use gets a
                # no-connect marker, which is how a schematic says "on
                # purpose" rather than leaving the reader to guess.
                nums = pin.num if isinstance(pin.num, list) else [pin.num]
                for num in nums:
                    geom = pins.get(str(num))
                    if geom is None:
                        continue
                    px, py, _, _ = geom
                    items.append(
                        ["no_connect",
                         ["at", f"{x + px:.2f}", f"{y - py:.2f}"],
                         ["uuid", ("str", uid())]]
                    )
                continue
            nums = pin.num if isinstance(pin.num, list) else [pin.num]
            for num in nums:
                geom = pins.get(str(num))
                if geom is None:
                    continue
                px, py, rot, _ = geom
                ax, ay = x + px, y - py
                dx, dy = outward(rot)
                bx, by = ax + dx * STUB, ay + dy * STUB
                items.append(
                    ["wire",
                     ["pts", ["xy", f"{ax:.2f}", f"{ay:.2f}"], ["xy", f"{bx:.2f}", f"{by:.2f}"]],
                     ["stroke", ["width", "0"], ["type", "default"]],
                     ["uuid", ("str", uid())]]
                )
                items.append(
                    ["global_label", ("str", pin.net.name),
                     ["shape", "bidirectional"],
                     ["at", f"{bx:.2f}", f"{by:.2f}", str(label_angle(dx, dy))],
                     ["effects", ["font", ["size", "1.27", "1.27"]],
                      ["justify", "left"]],
                     ["uuid", ("str", uid())]]
                )

    # A power input pin wants a power *output* driving its net, and nothing
    # in this design is one: the rails arrive as labels. PWR_FLAG is the
    # symbol that says "this rail is fed, I know what I am doing", and it is
    # what KiCad's power_pin_not_driven rule is asking for.
    flag_id = "power:PWR_FLAG"
    lib_symbols[flag_id] = rename(symbol("power", "PWR_FLAG"), flag_id)
    flag_pin = pin_positions(lib_symbols[flag_id])
    for n, rail in enumerate(("VBAT", "VLED", "GND")):
        fx = MARGIN + n * (12 * GRID)
        fy = MARGIN + (len(placements) // COLS + 1) * ROW_H
        items.append(
            ["symbol",
             ["lib_id", ("str", flag_id)],
             ["at", f"{fx:.2f}", f"{fy:.2f}", "0"],
             ["unit", "1"],
             ["exclude_from_sim", "no"], ["in_bom", "no"], ["on_board", "no"],
             ["dnp", "no"], ["uuid", ("str", uid())],
             ["property", ("str", "Reference"), ("str", f"#FLG{n}"),
              ["at", f"{fx:.2f}", f"{fy - 5:.2f}", "0"],
              ["effects", ["font", ["size", "1.27", "1.27"]], ["hide", "yes"]]],
             ["property", ("str", "Value"), ("str", "PWR_FLAG"),
              ["at", f"{fx:.2f}", f"{fy - 2:.2f}", "0"],
              ["effects", ["font", ["size", "1.27", "1.27"]], ["hide", "yes"]]],
             ["instances",
              ["project", ("str", "totem"),
               ["path", ("str", "/" + SHEET_UUID),
                ["reference", ("str", f"#FLG{n}")], ["unit", "1"]]]]]
        )
        px, py, rot, _ = next(iter(flag_pin.values()))
        ax, ay = fx + px, fy - py
        dx, dy = outward(rot)
        bx, by = ax + dx * STUB, ay + dy * STUB
        items.append(
            ["wire", ["pts", ["xy", f"{ax:.2f}", f"{ay:.2f}"], ["xy", f"{bx:.2f}", f"{by:.2f}"]],
             ["stroke", ["width", "0"], ["type", "default"]], ["uuid", ("str", uid())]]
        )
        items.append(
            ["global_label", ("str", rail), ["shape", "bidirectional"],
             ["at", f"{bx:.2f}", f"{by:.2f}", str(label_angle(dx, dy))],
             ["effects", ["font", ["size", "1.27", "1.27"]], ["justify", "left"]],
             ["uuid", ("str", uid())]]
        )

    sheet = [
        "kicad_sch",
        ["version", "20250114"],
        ["generator", ("str", "gen_sch.py")],
        ["generator_version", ("str", "10.0")],
        ["uuid", ("str", SHEET_UUID)],
        ["paper", ("str", "A0")],
        ["lib_symbols", *lib_symbols.values()],
        *items,
    ]

    with open("totem.kicad_sch", "w") as fh:
        fh.write(dump(sheet) + "\n")

    print(f"totem.kicad_sch: {len(placements) - len(undrawn)} symbols placed")
    for part in undrawn:
        print(f"  not drawn (no KiCad symbol): {part.ref} {part.name}")


SHEET_UUID = uid()

if __name__ == "__main__":
    main()
