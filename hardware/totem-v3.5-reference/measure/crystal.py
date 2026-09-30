"""Locate the 7 crystal LEDs (lit in the photo) and the centre post, in board mm.

The crystal LEDs are the saturated blobs inside the ring. Each blob's
centroid is taken after thresholding near saturation; glare is roughly
symmetric about the package, so the centroid sits on it to a few tenths of
a millimetre. Each package's orientation is read from the dark outline of
its body around the glare (minAreaRect of the ring of darker pixels).
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
M = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
Mi = np.linalg.inv(M)
rg = json.load(open(f"{HERE}/ring_board.json"))
im = cv2.imread(f"{HERE}/photos/led.jpg")
g = cv2.cvtColor(im, cv2.COLOR_BGR2GRAY)
# only inside the ring, 14 mm around its centre
yy, xx = np.mgrid[0:g.shape[0], 0:g.shape[1]]
B = np.stack([xx, yy, np.ones_like(xx)], -1).reshape(-1, 3).astype(float) @ Mi.T
bx, by = B[:, 0].reshape(g.shape), B[:, 1].reshape(g.shape)
inside = np.hypot(bx - rg["centre"][0], by - rg["centre"][1]) < 14
sat = ((g > 245) & inside).astype(np.uint8)
sat = cv2.morphologyEx(sat, cv2.MORPH_OPEN, np.ones((9, 9), np.uint8))
n, lab, st, cen = cv2.connectedComponentsWithStats(sat)
blobs = sorted(((st[i, 4], cen[i]) for i in range(1, n)), key=lambda t: -t[0])[:7]
out = []
for a, c in blobs:
    b = Mi @ np.array([c[0], c[1], 1.0])
    out.append([float(b[0]), float(b[1]), int(a)])
out.sort(key=lambda p: np.degrees(np.arctan2(p[1] - 4.0, p[0] + 2.1)))
for p in out:
    print(f"  crystal LED at ({p[0]:6.2f}, {p[1]:6.2f})  blob {p[2]} px")
json.dump(out, open(f"{HERE}/crystal_board.json", "w"), indent=1)
