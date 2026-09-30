"""Read the exported gerbers back with a parser that is not KiCad's (gerbonara)
and check what a fab would see: every layer present and non-empty, the
outline's size against the measured outline, drill sizes and count, and a
picture of each copper layer (fab/check-*.svg) to compare with the renders.

Run with a Python that has gerbonara:  python gerber_check.py
"""
import json
import os
import sys

from gerbonara import LayerStack

HERE = os.path.dirname(os.path.abspath(__file__))
GER = os.path.join(HERE, "fab", "gerbers")
stack = LayerStack.open(GER)
problems = []
want = [("top", "copper"), ("inner_1", "copper"), ("inner_2", "copper"), ("bottom", "copper"),
        ("top", "mask"), ("bottom", "mask"), ("top", "paste"), ("bottom", "paste"),
        ("top", "silk"), ("bottom", "silk")]
layers = dict(stack.graphic_layers)
for key in want:
    lay = layers.get(key) if key[0] in ("top", "bottom") else None
    if key[0].startswith("inner"):
        found = [l for k, l in stack.graphic_layers.items() if k[1] == "copper" and k[0].startswith("inner")]
        lay = found[int(key[0][-1]) - 1] if len(found) >= int(key[0][-1]) else None
    n = len(lay.objects) if lay is not None else 0
    print(f"  {key[0]:8} {key[1]:7} {n:6d} objects")
    if n == 0:
        problems.append(f"{key} missing or empty")

out = stack.outline
(x0, y0), (x1, y1) = out.bounding_box()
w, h = x1 - x0 - 0.1, y1 - y0 - 0.1          # less the 0.1 mm outline stroke: centreline size
poly = json.load(open(os.path.join(HERE, "..", "measure", "outline2_poly.json")))
mw = max(p[0] for p in poly) - min(p[0] for p in poly)
mh = max(p[1] for p in poly) - min(p[1] for p in poly)
print(f"  outline {w:.3f} x {h:.3f} mm; measured outline {mw:.3f} x {mh:.3f} mm")
if abs(w - mw) > 0.05 or abs(h - mh) > 0.05:
    problems.append("outline size differs from the measured outline")

sizes = {}
for d in stack.drill_layers:
    for o in d.objects:
        dia = round(o.aperture.diameter, 3)
        sizes[dia] = sizes.get(dia, 0) + 1
print(f"  drills: {sizes}")
if not sizes or min(sizes) < 0.2:
    problems.append("no drills, or a drill under 0.2 mm")

for side in ("top", "bottom"):
    svg = stack.to_pretty_svg(side=side)
    open(os.path.join(HERE, "fab", f"check-{side}.svg"), "w").write(str(svg))
print("problems:", problems or "none")
sys.exit(1 if problems else 0)
