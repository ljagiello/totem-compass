"""Fit the photo -> board-plane homography from every row of known pitch.

Rows (padrows.json), each a line of pads with a datasheet pitch:
  ESP32-WROOM-32E  left/right columns 14 x 1.27, bottom row 10 x 1.27
  MAX-M10S         left/right columns  9 x 1.10
  TP4056 (SOP-8)   left/right columns  4 x 1.27   (weighted lower: leads
                                                   are gull-wing, centroids softer)
The frame is the ESP32 footprint's (mm, y down). Each other part's
position, rotation and column spacing is free. Along a row, each pad's
position is a measurement; across it, only straightness is imposed.
"""
import json
import numpy as np
from scipy.optimize import least_squares

HERE = __file__.rsplit("/", 1)[0]
rows = {k: np.array(v) for k, v in json.load(open(f"{HERE}/padrows.json")).items()}
yk14 = -5.26 + 1.27 * np.arange(14)
xk10 = -5.715 + 1.27 * np.arange(10)
k9 = (np.arange(9) - 4) * 1.10
k4 = (np.arange(4) - 1.5) * 1.27


def Hmat(q):
    return np.array([[q[0], q[1], q[2]], [q[3], q[4], q[5]], [q[6], q[7], 1.0]])


def inv_map(H, P):
    v = np.c_[P, np.ones(len(P))] @ np.linalg.inv(H).T
    return v[:, :2] / v[:, 2:]


def local(P, cx, cy, th):
    c, s = np.cos(th), np.sin(th)
    d = P - [cx, cy]
    return np.c_[c * d[:, 0] + s * d[:, 1], -s * d[:, 0] + c * d[:, 1]]


def pair(H, a, b, ks, p, w):
    """two parallel columns of a part at p = (cx, cy, th, half-spacing, phase)"""
    la = local(inv_map(H, rows[a]), p[0], p[1], p[2])
    lb = local(inv_map(H, rows[b]), p[0], p[1], p[2])
    return np.concatenate([w * (la[:, 1] - ks - p[4]), w * (lb[:, 1] - ks - p[5]),
                           0.3 * w * (la[:, 0] + p[3]), 0.3 * w * (lb[:, 0] - p[3])])


def res(q, use):
    H = Hmat(q[:8])
    X, YB = q[8], q[9]
    l, r, b = inv_map(H, rows["left"]), inv_map(H, rows["right"]), inv_map(H, rows["bottom"])
    out = [l[:, 1] - yk14, r[:, 1] - yk14, b[:, 0] - xk10,
           0.3 * (l[:, 0] + X), 0.3 * (r[:, 0] - X), 0.3 * (b[:, 1] - YB)]
    if "m10" in use:
        out.append(pair(H, "m10l", "m10r", k9, q[10:16], 1.0))
    if "tp" in use:
        out.append(pair(H, "tpl", "tpr", k4, q[16:22], 0.5))
    return np.concatenate(out)


old = json.load(open(f"{HERE}/homog.json"))
H0 = np.array(old["H_mm_to_px"])
q0 = list(H0.flatten()[:8]) + [old["col_x"], old["bottom_y"]]
m = inv_map(H0, np.r_[rows["m10l"], rows["m10r"]]).mean(0)
t = inv_map(H0, np.r_[rows["tpl"], rows["tpr"]]).mean(0)
q0 += [m[0], m[1], 0.0, 5.0, 0.0, 0.0] + [t[0], t[1], 0.0, 2.7, 0.0, 0.0]


def report(tag, use):
    r = least_squares(lambda q: res(q, use), q0, x_scale="jac")
    H = Hmat(r.x[:8])

    def span(a):
        Q = inv_map(H, rows[a])
        return np.linalg.norm(Q[-1] - Q[0])
    print(f"[{tag}] ESP32 L/R/B spans {span('left'):.3f}/{span('right'):.3f}/{span('bottom'):.3f} "
          f"(16.510/16.510/11.430)  M10S {span('m10l'):.3f}/{span('m10r'):.3f} (8.800)  "
          f"TP4056 {span('tpl'):.3f}/{span('tpr'):.3f} (3.810)")
    print(f"      M10S column spacing {2*r.x[13]:.3f} mm, rotation {np.degrees(r.x[12]):+.2f} deg; "
          f"TP4056 spacing {2*r.x[19]:.3f}, rotation {np.degrees(r.x[18]):+.2f} deg")
    return r, H


report("ESP32 only", set())
report("+M10S", {"m10"})
report("+M10S+TP4056", {"m10", "tp"})
r, H = report("final: ESP32+M10S", {"m10"})
json.dump({"H_mm_to_px": H.tolist(), "frame": "ESP32 module footprint frame, mm, y down",
           "col_x": r.x[8], "bottom_y": r.x[9]}, open(f"{HERE}/homog.json", "w"), indent=1)
