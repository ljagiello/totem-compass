"""Centres of the radio side's multi-pad parts, from their measured pad rows.

A part's centre is the mean of its pad centres in board mm; its rotation is
the angle of its pad columns. Errors along a row cancel in the mean.
"""
import json
import numpy as np
from px2board import px2board

HERE = __file__.rsplit("/", 1)[0]
rows = {k: np.array(v) for k, v in json.load(open(f"{HERE}/padrows.json")).items()}
fr = json.load(open(f"{HERE}/board2.json"))
out = {"U1": {"centre": fr["off"], "note": "ESP32 module origin through the homography"}}


def part(name, a, b):
    A, B = px2board(rows[a]), px2board(rows[b])
    c = np.r_[A, B].mean(0)
    col = (A[-1] - A[0]) + (B[-1] - B[0])
    ang = np.degrees(np.arctan2(col[0], -col[1]))   # 0 when the columns run straight down
    sep = np.linalg.norm(A.mean(0) - B.mean(0))
    print(f"{name}: centre ({c[0]:7.3f}, {c[1]:7.3f}), columns {ang:+.2f} deg off vertical, {sep:.2f} mm apart")
    out[name] = {"centre": c.tolist(), "col_angle_deg": float(ang), "col_sep_mm": float(sep)}


part("U2", "m10l", "m10r")
part("U3", "tpl", "tpr")
print(f"U1: centre ({fr['off'][0]:7.3f}, {fr['off'][1]:7.3f})")
json.dump(out, open(f"{HERE}/parts_radio.json", "w"), indent=1)
