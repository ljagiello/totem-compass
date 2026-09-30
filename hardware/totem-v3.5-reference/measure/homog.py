"""Fit the photo -> board-plane homography from the ESP32's castellated pads.

Known, from the WROOM-32E footprint (module frame, mm, y down):
  left  column pins 1..14   y = -5.26 + 1.27 k
  bottom row    pins 15..24 x = -5.715 + 1.27 k
  right column  pins 38..25 y = -5.26 + 1.27 k   (listed top to bottom)
Measured: each pad's centre along its row, in px (padrows.json). A pad's
position across its row is where the sampling line was drawn, so it is not
a measurement; instead each row is required to be a straight line in the
module frame, at an offset that is fitted (left and right mirror-symmetric).

Writes homog.json with H (module mm -> px) and its inverse.
"""
import json
import numpy as np
from scipy.optimize import least_squares

HERE = __file__.rsplit("/", 1)[0]
rows = json.load(open(f"{HERE}/padrows.json"))
L = np.array(rows["left"])
R = np.array(rows["right"])
B = np.array(rows["bottom"])
yk = -5.26 + 1.27 * np.arange(14)
xk = -5.715 + 1.27 * np.arange(10)


def Hmat(q):
    return np.array([[q[0], q[1], q[2]], [q[3], q[4], q[5]], [q[6], q[7], 1.0]])


def inv_map(H, P):
    Hi = np.linalg.inv(H)
    v = np.c_[P, np.ones(len(P))] @ Hi.T
    return v[:, :2] / v[:, 2:]


def res(q):
    H = Hmat(q[:8])
    X, YB = q[8], q[9]
    l, r, b = inv_map(H, L), inv_map(H, R), inv_map(H, B)
    return np.concatenate([
        l[:, 1] - yk, r[:, 1] - yk, b[:, 0] - xk,          # along the rows: measured
        0.3 * (l[:, 0] + X), 0.3 * (r[:, 0] - X),          # across: straight lines only
        0.3 * (b[:, 1] - YB),
    ])


# start: similarity from the bottom-row pitch, module centre from the rows
s = np.linalg.norm(B[-1] - B[0]) / (1.27 * 9)
cx = (L[:, 0].mean() + R[:, 0].mean()) / 2
cy = (L[0, 1] + R[0, 1]) / 2 + 5.26 * s
q0 = [s, 0, cx, 0, s, cy, 0, 0, 9.3, 12.9]
r = least_squares(res, q0, x_scale="jac")
H = Hmat(r.x[:8])
fin = res(r.x)
along = np.r_[fin[:38]]
print(f"along-row residual: rms {np.sqrt(np.mean(along**2))*1000:.0f} um, max {np.abs(along).max()*1000:.0f} um")
print(f"row offsets: columns at x = +-{r.x[8]:.3f} mm, bottom row at y = {r.x[9]:.3f} mm")
# local scale at a few places, px per mm in x and y
for name, p in (("module centre", (0, 0)), ("module top", (0, -12.75)), ("module bottom", (0, 12.75)),
                ("22 mm left", (-20, 0)), ("22 mm right", (22, 0))):
    def f(pt):
        v = H @ np.array([pt[0], pt[1], 1.0])
        return v[:2] / v[2]
    sx = np.linalg.norm(f((p[0] + 0.5, p[1])) - f((p[0] - 0.5, p[1])))
    sy = np.linalg.norm(f((p[0], p[1] + 0.5)) - f((p[0], p[1] - 0.5)))
    print(f"  {name:<14} {sx:6.2f} px/mm in x, {sy:6.2f} in y")
json.dump({"H_mm_to_px": H.tolist(), "frame": "ESP32 module footprint frame, mm, y down",
           "col_x": r.x[8], "bottom_y": r.x[9]}, open(f"{HERE}/homog.json", "w"), indent=1)
