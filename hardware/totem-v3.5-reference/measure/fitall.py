"""Fit every visible stretch of the board edge (contour3_mm.npy, board frame).

Each stretch is a box on the edge where nothing covers it. Lines are total
least squares; circles are algebraic fits. The residuals say how well the
edge is known there; the boxes are the judgement, and are listed so they
can be checked against the photo.
"""
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
P = np.load(f"{HERE}/contour3_mm.npy")


def box(x0, x1, y0, y1):
    m = (P[:, 0] >= x0) & (P[:, 0] <= x1) & (P[:, 1] >= y0) & (P[:, 1] <= y1)
    return P[m]


def line(name, *b):
    Q = box(*b)
    c = Q.mean(0)
    _, _, vt = np.linalg.svd(Q - c)
    d, n = vt[0], vt[1]
    r = (Q - c) @ n
    out = {"kind": "line", "p": c.tolist(), "d": d.tolist(), "n": len(Q),
           "rms": float(np.sqrt(np.mean(r ** 2))), "ang": float(np.degrees(np.arctan2(d[1], d[0])) % 180)}
    print(f"{name:<22} line   through ({c[0]:7.3f}, {c[1]:7.3f}) at {out['ang']:6.2f} deg"
          f"   {len(Q):4d} pts rms {out['rms']*1000:4.0f} um")
    return out


def circle(name, Q):
    A = np.c_[2 * Q[:, 0], 2 * Q[:, 1], np.ones(len(Q))]
    cx, cy, k = np.linalg.lstsq(A, (Q ** 2).sum(1), rcond=None)[0]
    R = float(np.sqrt(k + cx ** 2 + cy ** 2))
    r = np.hypot(Q[:, 0] - cx, Q[:, 1] - cy) - R
    print(f"{name:<22} circle centre ({cx:7.3f}, {cy:7.3f}) R {R:6.3f}"
          f"          {len(Q):4d} pts rms {np.sqrt(np.mean(r**2))*1000:4.0f} um")
    return {"kind": "circle", "c": [float(cx), float(cy)], "R": R, "n": len(Q)}


F = {}
F["left edge"] = line("left edge", -23.0, -22.3, -11.0, 14.5)
F["right edge"] = line("right edge", 22.3, 23.0, 7.9, 9.3)
F["tab left"] = line("tab left", -13.45, -12.9, 20.0, 21.9)
F["tab right"] = line("tab right", 8.35, 8.8, 20.4, 22.1)
F["tab top L"] = line("tab top (left end)", -13.0, -11.3, 21.95, 22.6)
F["tab top R"] = line("tab top (right end)", 7.0, 8.4, 22.2, 22.8)
F["UL notch"] = circle("UL notch", box(-15.75, -13.25, 18.6, 19.75))
F["UL diagonal"] = line("UL diagonal", -20.6, -16.1, 17.5, 19.35)
F["UR notch"] = circle("UR notch", box(8.62, 11.1, 19.1, 20.1))
F["UR diagonal"] = line("UR diagonal", 12.3, 22.0, 9.95, 19.3)
F["LL chamfer"] = line("LL chamfer", -22.45, -21.35, -13.35, -12.45)
F["LL notch top"] = line("LL notch horizontal", -21.0, -16.8, -13.7, -13.2)
F["LL notch inner"] = line("LL notch inner", -16.15, -15.7, -20.0, -14.6)
F["LR notch inner"] = line("LR notch inner", 11.2, 11.55, -19.4, -14.3)
F["LR notch top"] = line("LR notch horizontal", 12.6, 17.9, -13.15, -12.65)


def run(a, b):
    """the contour between the points nearest a and b, in contour order"""
    i = int(np.argmin(np.hypot(*(P - a).T)))
    j = int(np.argmin(np.hypot(*(P - b).T)))
    return P[min(i, j):max(i, j) + 1]


# the buttons hide the middle of the arc; either end is clean, and a box
# around them also catches the switch pads, so take contour runs instead
arc_pts = np.r_[run((-15.93, -20.01), (-11.02, -22.50)), run((6.67, -21.98), (11.28, -19.74))]
F["bottom arc"] = circle("bottom arc", arc_pts)
circle("  left run alone", run((-15.93, -20.01), (-11.02, -22.50)))
circle("  right run alone", run((6.67, -21.98), (11.28, -19.74)))
json.dump(F, open(f"{HERE}/fits.json", "w"), indent=1)
