"""edgescan.py IMG OUT.json axis a0 a1 b0 b1 step dir
Scan lines across an edge and return the strongest dark->light step on each.
axis=row: for rows y in [a0,a1) every `step`, search x in [b0,b1);
axis=col: for columns x in [a0,a1), search y in [b0,b1).
dir=+1 looks for dark->light going toward larger coordinate, -1 the reverse.
"""
import sys
import json
import numpy as np
import cv2
from scipy.ndimage import gaussian_filter1d

src, out, axis = sys.argv[1], sys.argv[2], sys.argv[3]
a0, a1, b0, b1, step, d = (int(v) for v in sys.argv[4:10])
g = cv2.cvtColor(cv2.imread(src), cv2.COLOR_BGR2GRAY).astype(float)
g = cv2.GaussianBlur(g, (0, 0), 2.0)
pts = []
for a in range(a0, a1, step):
    prof = g[a, b0:b1] if axis == "row" else g[b0:b1, a]
    prof = gaussian_filter1d(prof, 2)
    grad = np.gradient(prof) * d
    i = int(np.argmax(grad))
    if grad[i] < 2.0:
        continue
    # sub-pixel: parabola through the peak
    if 0 < i < len(grad) - 1:
        y0, y1, y2 = grad[i - 1:i + 2]
        i = i + 0.5 * (y0 - y2) / (y0 - 2 * y1 + y2 + 1e-9)
    b = b0 + i
    pts.append([float(b), float(a)] if axis == "row" else [float(a), float(b)])
json.dump(pts, open(out, "w"))
print(len(pts), "edge points")
for p in pts[:: max(1, len(pts) // 12)]:
    print(f"  ({p[0]:7.1f}, {p[1]:7.1f})")
