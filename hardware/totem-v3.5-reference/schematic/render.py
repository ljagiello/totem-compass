#!/usr/bin/env python3
"""Draw the schematic from the same description the ERC checks.

The drawing is generated rather than laid out by hand, so it cannot drift
away from the netlist: both come from totem.py. Placement is netlistsvg's,
which means it is readable but not arranged the way a person would arrange
it. The netlist is the authority; this is for looking at.

Needs `netlistsvg` (npm install -g netlistsvg) and KiCad's symbol libraries.

Usage: python3 render.py
Writes: totem_sheet.svg
"""

import os
import sys

KICAD_SYMBOLS = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/symbols"
os.environ.setdefault("KICAD8_SYMBOL_DIR", KICAD_SYMBOLS)

import totem  # noqa: E402,F401  (builds the circuit as a side effect)
from skidl import generate_svg  # noqa: E402

if __name__ == "__main__":
    generate_svg(file_="totem_sheet")
    print("totem_sheet.svg written")
    sys.exit(0)
