"""net_items.py NET [x0 x1 y0 y1] — tracks, vias and pads of one net on the board
(board mm), optionally only those inside a box. Run with KiCad's Python."""
import os
import sys

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
B = pcbnew.LoadBoard(os.path.join(HERE, "..", "schematic", "totem.kicad_pcb"))
net = sys.argv[1]
box = [float(v) for v in sys.argv[2:6]] if len(sys.argv) > 5 else None


def bxy(v):
    return pcbnew.ToMM(v.x) - 100, 100 - pcbnew.ToMM(v.y)


def inbox(*pts):
    return box is None or any(box[0] <= x <= box[1] and box[2] <= y <= box[3] for x, y in pts)


for p in B.GetPads():
    if p.GetNetname() == net and inbox(bxy(p.GetPosition())):
        x, y = bxy(p.GetPosition())
        print(f"pad {p.GetParentFootprint().GetReference()}.{p.GetNumber()} ({x:.3f}, {y:.3f}) "
              f"{'F' if p.IsOnLayer(pcbnew.F_Cu) else ''}{'B' if p.IsOnLayer(pcbnew.B_Cu) else ''}")
for t in B.GetTracks():
    if t.GetNetname() != net:
        continue
    if t.GetClass() == "PCB_VIA":
        if inbox(bxy(t.GetPosition())):
            print(f"via ({bxy(t.GetPosition())[0]:.3f}, {bxy(t.GetPosition())[1]:.3f})")
    else:
        a, b = bxy(t.GetStart()), bxy(t.GetEnd())
        if inbox(a, b):
            print(f"track {B.GetLayerName(t.GetLayer())} ({a[0]:.3f}, {a[1]:.3f}) -> ({b[0]:.3f}, {b[1]:.3f}) "
                  f"w {pcbnew.ToMM(t.GetWidth()):.2f}")
