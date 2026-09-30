"""The lower-right notch's outer chamfer, from the LED side (the JST hides it
on the radio side). Adds "LR chamfer" to fits.json for outline2.py.

The column scan along the chamfer (e_lrc.json) has two runs: the outer one
is the board edge, the inner one a trace running parallel to it. The outer
run is used, with the chamfer's two ends read off out/lr_corner.jpg.
"""
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
Mi = np.linalg.inv(np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"]))
px = np.array([[2560, 2809.0], [2575, 2798.9], [2590, 2788.9], [2605, 2780.6], [2620, 2770.8],
               [2790, 2665], [2440, 2890]])
B = (np.c_[px, np.ones(len(px))] @ Mi.T)[:, :2]
c = B.mean(0)
_, _, vt = np.linalg.svd(B - c)
d = vt[0]
r = (B - c) @ vt[1]
ang = np.degrees(np.arctan2(d[1], d[0])) % 180
print(f"LR chamfer: through ({c[0]:.3f}, {c[1]:.3f}) at {ang:.2f} deg, rms {np.sqrt(np.mean(r**2))*1000:.0f} um")
F = json.load(open(f"{HERE}/fits.json"))
F["LR chamfer"] = {"kind": "line", "p": c.tolist(), "d": d.tolist(), "n": len(B),
                   "rms": float(np.sqrt(np.mean(r ** 2))), "ang": float(ang),
                   "source": "LED-side photo; e_lrc.json outer run + the ends from out/lr_corner.jpg"}
json.dump(F, open(f"{HERE}/fits.json", "w"), indent=1)
