"""islands.py — list each filled zone's separate pieces: extent, and which pads
and vias of the zone's net each piece touches. Run with KiCad's Python."""
import os

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
B = pcbnew.LoadBoard(os.path.join(HERE, "..", "schematic", "totem.kicad_pcb"))


def bxy(v):
    return pcbnew.ToMM(v.x) - 100, 100 - pcbnew.ToMM(v.y)


for z in B.Zones():
    if z.GetIsRuleArea():
        continue
    layer = z.GetLayer()
    fp = z.GetFilledPolysList(layer)
    net = z.GetNetname()
    items = [(p.GetParentFootprint().GetReference() + "." + p.GetNumber(), p.GetPosition())
             for p in B.GetPads() if p.GetNetname() == net and p.IsOnLayer(layer)]
    items += [("via", t.GetPosition()) for t in B.GetTracks() if t.GetClass() == "PCB_VIA" and t.GetNetname() == net]
    print(f"{z.GetZoneName()} ({net}, {B.GetLayerName(layer)}): {fp.OutlineCount()} pieces")
    if fp.OutlineCount() <= 1:
        continue
    for k in range(fp.OutlineCount()):
        ol = fp.Outline(k)
        bb = ol.BBox()
        x0, y1 = bxy(pcbnew.VECTOR2I(bb.GetLeft(), bb.GetTop()))
        x1, y0 = bxy(pcbnew.VECTOR2I(bb.GetRight(), bb.GetBottom()))
        hit = [name for name, pos in items if fp.Contains(pos, k) or ol.PointInside(pos)]
        print(f"   piece {k}: x {x0:6.2f}..{x1:6.2f}  y {y0:6.2f}..{y1:6.2f}  area {ol.Area()/1e12:6.2f} mm2  "
              f"touches {len(hit)}: {hit[:6]}")
