"""inspect_fp.py REF [REF ...] — courtyard box and pads of footprints on the board,
in board mm (y up). Run with KiCad's Python."""
import os
import sys

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
B = pcbnew.LoadBoard(os.path.join(HERE, "..", "schematic", "totem.kicad_pcb"))


def bxy(v):
    return pcbnew.ToMM(v.x) - 100, 100 - pcbnew.ToMM(v.y)


for ref in sys.argv[1:]:
    fp = B.FindFootprintByReference(ref)
    layer = pcbnew.B_CrtYd if fp.IsFlipped() else pcbnew.F_CrtYd
    bb = fp.GetCourtyard(layer).BBox()
    x0, y1 = bxy(pcbnew.VECTOR2I(bb.GetLeft(), bb.GetTop()))
    x1, y0 = bxy(pcbnew.VECTOR2I(bb.GetRight(), bb.GetBottom()))
    x, y = bxy(fp.GetPosition())
    print(f"{ref}: at ({x:.2f}, {y:.2f}) rot {fp.GetOrientationDegrees():.0f} {'B' if fp.IsFlipped() else 'F'}; "
          f"courtyard x {x0:.2f}..{x1:.2f}, y {y0:.2f}..{y1:.2f}")
    for p in fp.Pads():
        px, py = bxy(p.GetPosition())
        print(f"   pad {p.GetNumber():>3} {p.GetNetname():<16} ({px:6.2f}, {py:6.2f})")
