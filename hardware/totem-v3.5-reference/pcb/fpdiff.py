"""fpdiff.py REF — how a footprint on the board differs from its library copy:
pad offsets and sizes after undoing the placement's rotation and flip, and
the text and graphic item counts. Run with KiCad's Python."""
import math
import os
import sys

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
B = pcbnew.LoadBoard(os.path.join(HERE, "..", "schematic", "totem.kicad_pcb"))
ref = sys.argv[1]
fp = B.FindFootprintByReference(ref)
lib = fp.GetFPID()
libpath = {"totem": os.path.join(HERE, "..", "schematic", "totem.pretty")}.get(
    str(lib.GetLibNickname()),
    f"/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints/{lib.GetLibNickname()}.pretty")
lf = pcbnew.FootprintLoad(libpath, str(lib.GetLibItemName()))
a = math.radians(fp.GetOrientationDegrees())
c = fp.GetPosition()
print(f"{ref}: {lib.GetLibNickname()}:{lib.GetLibItemName()} rot {fp.GetOrientationDegrees()} flipped {fp.IsFlipped()}")
lp = {p.GetNumber(): p for p in lf.Pads()}
for p in fp.Pads():
    dx, dy = pcbnew.ToMM(p.GetPosition().x - c.x), pcbnew.ToMM(p.GetPosition().y - c.y)
    # undo rotation (KiCad: screen-CCW), then undo the left-right flip
    x = dx * math.cos(-a) + dy * math.sin(-a)
    y = -dx * math.sin(-a) + dy * math.cos(-a)
    if fp.IsFlipped():
        x = -x
    q = lp[p.GetNumber()]
    ox, oy = pcbnew.ToMM(q.GetPosition().x), pcbnew.ToMM(q.GetPosition().y)
    print(f"  pad {p.GetNumber()}: board-local ({x:.4f}, {y:.4f}) library ({ox:.4f}, {oy:.4f}) "
          f"size {pcbnew.ToMM(p.GetSize(pcbnew.F_Cu).x):.3f} vs {pcbnew.ToMM(q.GetSize(pcbnew.F_Cu).x):.3f}")
print(f"  graphics {len(list(fp.GraphicalItems()))} vs {len(list(lf.GraphicalItems()))}; "
      f"fields {len(list(fp.GetFields()))} vs {len(list(lf.GetFields()))}")
