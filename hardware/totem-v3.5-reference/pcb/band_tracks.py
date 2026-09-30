"""band_tracks.py — which B.Cu tracks run in the band just outside the ring
(where the VLED pour must stay whole), and for how long."""
import json
import math
import os
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
g = json.load(open(os.path.join(HERE, "board.json")))
R = json.load(open(os.path.join(HERE, "routes.json")))
RC, RR = g["ring_centre"], g["ring_R"]
out = defaultdict(float)
for net, layer, x1, y1, x2, y2, w in R["tracks"]:
    if layer != "B.Cu":
        continue
    n = max(2, int(math.hypot(x2 - x1, y2 - y1) / 0.05))
    for k in range(n):
        x = x1 + (x2 - x1) * k / n
        y = y1 + (y2 - y1) * k / n
        if RR + 0.3 <= math.hypot(x - RC[0], y - RC[1]) <= RR + 1.8:
            out[net] += math.hypot(x2 - x1, y2 - y1) / n
for net, l in sorted(out.items(), key=lambda t: -t[1]):
    print(f"{l:6.2f} mm  {net}")
