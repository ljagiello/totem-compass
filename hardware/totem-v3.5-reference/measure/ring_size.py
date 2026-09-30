"""How wide are the ring's LED packages? Measured along the ring.

At the LED centre radius the tangential brightness profile alternates
light package / dark gap with a 6-degree period. Folding the profile over
one period and thresholding halfway between the gap and the package level
gives the package's tangential width. Glare from the lit pixels is dropped
by using only periods whose peak is not saturated.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
M = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
rg = json.load(open(f"{HERE}/ring_board.json"))
c, R = np.array(rg["centre"]), rg["R"]
g = cv2.cvtColor(cv2.imread(f"{HERE}/photos/led.jpg"), cv2.COLOR_BGR2GRAY).astype(np.float32)
step = 0.02                                  # degrees
ts = np.arange(0, 360, step)


def profile(r):
    X, Y = c[0] + r * np.cos(np.radians(ts)), c[1] + r * np.sin(np.radians(ts))
    P = np.c_[X, Y, np.ones(len(X))] @ M.T
    return cv2.remap(g, P[:, 0].astype(np.float32)[None], P[:, 1].astype(np.float32)[None],
                     cv2.INTER_LINEAR)[0]


for dr in (-0.6, -0.3, 0.0, 0.3, 0.6):
    p = np.mean([profile(R + dr + e) for e in (-0.1, 0, 0.1)], axis=0)
    per = int(round(6 / step))
    blocks = p[: len(p) // per * per].reshape(-1, per)
    ok = blocks.max(1) < 235                       # not glare
    fold = np.median(blocks[ok], axis=0)
    lo, hi = np.percentile(fold, 5), np.percentile(fold, 95)
    frac = np.mean(fold > (lo + hi) / 2)
    arc = np.radians(6) * (R + dr)
    print(f"r = {R+dr:6.2f}: pitch {arc:.3f} mm, light fraction {frac:.2f} -> package {frac*arc:.2f} mm, "
          f"gap {(1-frac)*arc:.2f} mm ({ok.sum()} clean periods)")
