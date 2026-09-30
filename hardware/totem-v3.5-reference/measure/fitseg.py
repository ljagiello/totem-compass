"""Fit lines and circles to windows of the traced contour (contour2_mm.npy).

fitseg.py line  x0 x1 y0 y1       -> total-least-squares line through the points in the box
fitseg.py circle x0 x1 y0 y1      -> algebraic circle fit
fitseg.py dump  x0 x1 y0 y1 [n]   -> print every n-th point in the box
"""
import sys
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
P = np.load(f"{HERE}/contour3_mm.npy")
kind = sys.argv[1]
x0, x1, y0, y1 = (float(v) for v in sys.argv[2:6])
m = (P[:, 0] >= x0) & (P[:, 0] <= x1) & (P[:, 1] >= y0) & (P[:, 1] <= y1)
Q = P[m]
if kind == "dump":
    n = int(sys.argv[6]) if len(sys.argv) > 6 else 20
    for p in Q[::n]:
        print(f"{p[0]:7.2f} {p[1]:7.2f}")
    sys.exit()
if len(Q) < 5:
    sys.exit(f"only {len(Q)} points")
if kind == "line":
    c = Q.mean(0)
    u, s, vt = np.linalg.svd(Q - c)
    d = vt[0]
    r = (Q - c) @ vt[1]
    ang = np.degrees(np.arctan2(d[1], d[0])) % 180
    print(f"{len(Q)} pts  centre ({c[0]:.3f}, {c[1]:.3f})  angle {ang:.2f} deg  "
          f"rms {np.sqrt(np.mean(r**2)):.3f} mm  max {np.abs(r).max():.3f}")
    print(f"dir {d[0]:.4f} {d[1]:.4f}")
elif kind == "circle":
    A = np.c_[2 * Q[:, 0], 2 * Q[:, 1], np.ones(len(Q))]
    b = (Q ** 2).sum(1)
    cx, cy, k = np.linalg.lstsq(A, b, rcond=None)[0]
    R = np.sqrt(k + cx ** 2 + cy ** 2)
    r = np.hypot(Q[:, 0] - cx, Q[:, 1] - cy) - R
    print(f"{len(Q)} pts  centre ({cx:.3f}, {cy:.3f})  R {R:.3f}  rms {np.sqrt(np.mean(r**2)):.3f}")
