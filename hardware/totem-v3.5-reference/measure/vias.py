"""Find via-like dots: small, round, darker than their surroundings.

vias.py IMG OUT.json scale_px_per_mm [x0 y0 x1 y1]
Vias on this board are ~0.3 mm drills in ~0.6 mm rings; a dot of 0.25-0.7
mm diameter that is much darker than a ring around it is a candidate.
Writes candidate centres (px) and an overlay out/<OUT>.jpg.
"""
import sys
import json
import numpy as np
import cv2

src, out, k = sys.argv[1], sys.argv[2], float(sys.argv[3])
im = cv2.imread(src)
roi = [int(v) for v in sys.argv[4:8]] if len(sys.argv) > 7 else [0, 0, im.shape[1], im.shape[0]]
g = cv2.cvtColor(im, cv2.COLOR_BGR2GRAY).astype(np.float32)
# darkness relative to the local background (a top-hat for dark dots)
r = int(max(3, 0.35 * k))
bg = cv2.morphologyEx(g, cv2.MORPH_CLOSE, cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (4 * r + 1, 4 * r + 1)))
dark = bg - g
dark = cv2.GaussianBlur(dark, (0, 0), max(1.0, 0.06 * k))
thr = np.percentile(dark[roi[1]:roi[3], roi[0]:roi[2]], 99.3)
m = (dark > max(thr, 12)).astype(np.uint8)
n, lab, st, cen = cv2.connectedComponentsWithStats(m)
pts = []
for i in range(1, n):
    x, y, w, h, a = st[i]
    cx, cy = cen[i]
    if not (roi[0] <= cx < roi[2] and roi[1] <= cy < roi[3]):
        continue
    d = np.sqrt(4 * a / np.pi) / k
    if 0.2 < d < 0.75 and 0.6 < w / h < 1.6 and a / (w * h) > 0.55:
        pts.append([float(cx), float(cy), float(d)])
json.dump(pts, open(out, "w"))
ov = im.copy()
for x, y, d in pts:
    cv2.circle(ov, (int(x), int(y)), int(0.6 * k), (0, 0, 255), 2)
x0, y0, x1, y1 = roi
c = ov[y0:y1, x0:x1]
s = 1400 / max(c.shape[:2])
cv2.imwrite("out/" + out.rsplit("/", 1)[-1].replace(".json", ".jpg"), cv2.resize(c, None, fx=s, fy=s))
print(len(pts), "candidates")
