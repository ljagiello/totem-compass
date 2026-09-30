"""Find the 60 ring LEDs in totem_a.png and fit their circle.

The LED bodies are light squares on the dark board. Blobs of LED size in
an annulus around the ring are kept; their centroids are fitted with an
ellipse (the ring is a circle, so the ellipse measures the photo's
anisotropy) and assigned to the 60 positions by angle.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
im = cv2.imread(f"{HERE}/photos/led.jpg")
g = cv2.cvtColor(im, cv2.COLOR_BGR2GRAY)
c0 = np.array([1840.0, 1545.0])
yy, xx = np.mgrid[0:g.shape[0], 0:g.shape[1]]
rr = np.hypot(xx - c0[0], yy - c0[1])
ann = (rr > 900) & (rr < 1200)
bright = ((g > 120) & ann).astype(np.uint8)
bright = cv2.morphologyEx(bright, cv2.MORPH_OPEN, np.ones((7, 7), np.uint8))
n, lab, st, cen = cv2.connectedComponentsWithStats(bright)
pts = []
for i in range(1, n):
    x, y, w, h, a = st[i]
    if 2500 < a < 16000 and 50 < w < 170 and 50 < h < 170:
        pts.append(cen[i])
pts = np.array(pts)
print(len(pts), "blobs")
# ellipse fit, then angles
el = cv2.fitEllipse(pts.astype(np.float32))
(cx, cy), (a1, a2), ang = el
print(f"ellipse centre ({cx:.1f}, {cy:.1f}) axes {a1:.1f} x {a2:.1f} (ratio {max(a1,a2)/min(a1,a2):.4f}) at {ang:.1f} deg")
d = np.hypot(pts[:, 0] - cx, pts[:, 1] - cy)
print(f"radius: mean {d.mean():.1f} px, sd {d.std():.1f}")
th = np.degrees(np.arctan2(pts[:, 1] - cy, pts[:, 0] - cx)) % 360
order = np.argsort(th)
dth = np.diff(np.r_[th[order], th[order][0] + 360])
print("angular gaps (deg): median %.2f, min %.2f, max %.2f" % (np.median(dth), dth.min(), dth.max()))
json.dump({"pts": pts[order].tolist(), "ellipse": [cx, cy, a1, a2, ang]}, open(f"{HERE}/ring_px.json", "w"))
ov = im.copy()
for p in pts:
    cv2.circle(ov, (int(p[0]), int(p[1])), 20, (0, 0, 255), 4)
cv2.ellipse(ov, el, (255, 0, 255), 3)
cv2.imwrite(f"{HERE}/out/ring.jpg", cv2.resize(ov[200:2900, 500:3200], None, fx=0.35, fy=0.35))
