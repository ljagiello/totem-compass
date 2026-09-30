"""Which USB-C receptacle fits where the reference board's does?

Run with KiCad's Python. Every USB-C footprint in the library is placed
opening toward +x at the measured centre line (y 3.1) and slid along x.
A position is feasible when every hole-bearing pad (plated or not) keeps
0.25 mm from the ring LEDs' pads on the other side and all copper keeps
0.3 mm inside the board edge. For each footprint the feasible position
with the front of the body furthest out is reported: the reference
board's connector reaches at least 2.3 mm past the edge.
"""
import glob
import json
import math
import os

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
g = json.load(open(os.path.join(HERE, "board.json")))
F = json.load(open(os.path.join(HERE, "..", "measure", "fits.json")))
ring = [p for p in g["pads"] if p["ref"].startswith("D1") and len(p["ref"]) == 4 and p["y"] > -6 and p["y"] < 12]
ra = math.radians(F["right edge"]["ang"])
re_p = F["right edge"]["p"]


def edge_x(y):
    return re_p[0] + (y - re_p[1]) * math.cos(ra) / math.sin(ra)


def rect_dist(px, py, hw, hh, q):
    """distance from point-ish pad (centre px,py half sizes) to an axis-aligned ring pad (approx)"""
    dx = max(abs(px - q["x"]) - hw - 0.3, 0)
    dy = max(abs(py - q["y"]) - hh - 0.3, 0)
    return math.hypot(dx, dy)


YC = 3.1
res = []
for f in sorted(glob.glob("/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints/"
                          "Connector_USB.pretty/USB_C_Receptacle*.kicad_mod")):
    name = os.path.basename(f)[:-10]
    fp = pcbnew.FootprintLoad(os.path.dirname(f), name)
    if fp is None:
        continue
    best = None
    for xc10 in range(150, 240):
        xc = xc10 / 10
        fp.SetOrientationDegrees(0)
        fp.SetPosition(pcbnew.VECTOR2I(0, 0))
        ok, front = True, -99
        holes = copper = 0
        # local (x, y) -> board: opening (+y local) toward +x board: bx = xc + ly, by = YC - lx
        for p in fp.Pads():
            lx, ly = pcbnew.ToMM(p.GetPosition().x), pcbnew.ToMM(p.GetPosition().y)
            sz = p.GetSize(pcbnew.F_Cu)
            w, h = pcbnew.ToMM(sz.x), pcbnew.ToMM(sz.y)
            if abs(p.GetOrientation().AsDegrees()) % 180 == 90:
                w, h = h, w
            bx, by = xc + ly, YC - lx
            hw, hh = h / 2, w / 2
            if bx + hw > edge_x(by) - 0.3:
                ok = False
                break
            if p.GetDrillSize().x > 0:
                holes += 1
                if any(rect_dist(bx, by, hw, hh, q) < 0.25 for q in ring):
                    ok = False
                    break
        if not ok:
            continue
        for item in fp.GraphicalItems():
            if item.GetLayer() == pcbnew.F_Fab:
                bb = item.GetBoundingBox()
                front = max(front, xc + pcbnew.ToMM(bb.GetBottom()))
        if best is None or front > best[1]:
            best = (xc, front)
    if best:
        res.append((best[1] - 22.75, name, best[0]))
for over, name, xc in sorted(res, reverse=True):
    print(f"{over:+5.2f} mm past the edge  x_c {xc:5.1f}  {name}")
print(f"{len(res)} footprints fit")
