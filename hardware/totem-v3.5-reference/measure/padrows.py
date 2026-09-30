"""Locate each ESP32 castellated pad by brightness profile along its row.

For a row, a thin band is sampled along a line between two hand-picked end
points; the pads show as bright plateaus and the gaps as dark. The pad
centres are the midpoints of the plateaus. Output: px centres per row.

padrows.py x0 y0 x1 y1 halfwidth npads name
"""
import sys
import json
import numpy as np
import cv2
from scipy.ndimage import gaussian_filter1d

HERE = __file__.rsplit("/", 1)[0]
im = cv2.imread(f"{HERE}/photos/radio.jpg").astype(float)
g = im[:, :, 2]          # red: grey pads bright, green gaps dark
x0, y0, x1, y1, hw = (float(v) for v in sys.argv[1:6])
npads, name = int(sys.argv[6]), sys.argv[7]
L = int(np.hypot(x1 - x0, y1 - y0))
t = np.linspace(0, 1, L)
d = np.array([x1 - x0, y1 - y0]) / L
nrm = np.array([-d[1], d[0]])
prof = np.zeros(L)
for o in np.arange(-hw, hw + 1):
    xs = x0 + t * (x1 - x0) + o * nrm[0]
    ys = y0 + t * (y1 - y0) + o * nrm[1]
    prof += cv2.remap(g.astype(np.float32), xs.astype(np.float32)[None], ys.astype(np.float32)[None],
                      cv2.INTER_LINEAR)[0]
prof = gaussian_filter1d(prof / (2 * hw + 1), 2)
# a periodic signal: find the pitch by autocorrelation, then the phase
p = prof - prof.mean()
ac = np.correlate(p, p, "full")[L - 1:]
lo = int(L / npads * 0.6)
pitch = lo + int(np.argmax(ac[lo:int(L / npads * 1.5)]))
# refine: fit centres as local maxima of a smoothed profile near k*pitch+phase
ph = max(range(pitch), key=lambda s: sum(prof[s + k * pitch] for k in range(npads) if s + k * pitch < L))
cent = []
for k in range(npads):
    c = ph + k * pitch
    a, b = max(0, c - pitch // 3), min(L, c + pitch // 3)
    w = prof[a:b] - prof[a:b].min()
    s = a + (w * np.arange(len(w))).sum() / max(w.sum(), 1e-9)   # centroid of the bright plateau
    cent.append([float(x0 + s * d[0]), float(y0 + s * d[1])])
cent = np.array(cent)
steps = np.linalg.norm(np.diff(cent, axis=0), axis=1)
print(f"{name}: pitch {steps.mean():.2f} px (sd {steps.std():.2f}), ends {cent[0].round(1)} {cent[-1].round(1)}")
try:
    db = json.load(open(f"{HERE}/padrows.json"))
except FileNotFoundError:
    db = {}
db[name] = cent.tolist()
json.dump(db, open(f"{HERE}/padrows.json", "w"), indent=1)
ov = cv2.imread(f"{HERE}/photos/radio.jpg")
for c in cent:
    cv2.circle(ov, (int(c[0]), int(c[1])), 8, (0, 0, 255), 2)
xa, ya = cent.min(0).astype(int) - 60
xb, yb = cent.max(0).astype(int) + 60
cv2.imwrite(f"{HERE}/out/row_{name}.jpg", ov[ya:yb, xa:xb])
