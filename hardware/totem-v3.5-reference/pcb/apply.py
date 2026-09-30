"""Put the router's tracks and vias into the board and fill the zones.

Run with KiCad's Python after build.py and route.py:
  .../bin/python3 apply.py
"""
import json
import os

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
PCB = os.path.join(HERE, "..", "schematic", "totem.kicad_pcb")
B = pcbnew.LoadBoard(PCB)
R = json.load(open(os.path.join(HERE, "routes.json")))
nets = {n.GetNetname(): n for n in B.GetNetsByName().values()}
LAYER = {"F.Cu": pcbnew.F_Cu, "B.Cu": pcbnew.B_Cu}


def pt(x, y):
    return pcbnew.VECTOR2I(pcbnew.FromMM(100 + x), pcbnew.FromMM(100 - y))


# this adds to the board as build.py left it; on an already-routed board it
# would lay a second copy over the first (removing items through the Python
# bindings crashes KiCad 10, so rebuild instead)
if len(B.GetTracks()):
    raise SystemExit("the board already has tracks: run build.py first")
for net, layer, x1, y1, x2, y2, w in R["tracks"]:
    t = pcbnew.PCB_TRACK(B)
    t.SetStart(pt(x1, y1))
    t.SetEnd(pt(x2, y2))
    t.SetWidth(pcbnew.FromMM(w))
    t.SetLayer(LAYER[layer])
    t.SetNet(nets[net])
    B.Add(t)
for net, x, y, d, drill in R["vias"]:
    v = pcbnew.PCB_VIA(B)
    v.SetPosition(pt(x, y))
    v.SetWidth(pcbnew.FromMM(d))
    v.SetDrill(pcbnew.FromMM(drill))
    v.SetViaType(pcbnew.VIATYPE_THROUGH)
    v.SetNet(nets[net])
    B.Add(v)
filler = pcbnew.ZONE_FILLER(B)
filler.Fill(B.Zones())
pcbnew.SaveBoard(PCB, B)
import rules  # noqa: E402
rules.main()
print(f"{len(R['tracks'])} tracks, {len(R['vias'])} vias; zones filled; unrouted: {R['failed']}; "
      f"pour-only GND pads: {R.get('pour_only')}")
