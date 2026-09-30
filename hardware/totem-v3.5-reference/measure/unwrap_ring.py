"""Unwrap the LED ring into a straight strip, to follow the DOUT->DIN links.

Samples the LED photo along circles about the ring centre (board frame,
through led_reg.json), from R-3 to R+3 mm, all the way round, at ~60 px/mm.
The strip goes counter-clockwise (seen from the radio side, x-ray) from
angle 0 at the left, with a tick and the angle every LED (6 deg).
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
M = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
rg = json.load(open(f"{HERE}/ring_board.json"))
c, R = np.array(rg["centre"]), rg["R"]
im = cv2.imread(f"{HERE}/photos/led.jpg")
K = 60.0
rs = np.linspace(R + 3, R - 3, int(6 * K))
L = 2 * np.pi * R
ts = np.linspace(0, 2 * np.pi, int(L * K), endpoint=False)
T, Rr = np.meshgrid(ts, rs)
X = c[0] + Rr * np.cos(T)
Y = c[1] + Rr * np.sin(T)
P = np.stack([X.ravel(), Y.ravel(), np.ones(X.size)], 1) @ M.T
mapx = P[:, 0].reshape(X.shape).astype(np.float32)
mapy = P[:, 1].reshape(X.shape).astype(np.float32)
strip = cv2.remap(im, mapx, mapy, cv2.INTER_CUBIC)
for k in range(60):
    u = int(k * 6 / 360 * strip.shape[1])
    cv2.line(strip, (u, 0), (u, 12), (0, 255, 255), 2)
    cv2.putText(strip, f"{k*6}", (u + 2, 26), cv2.FONT_HERSHEY_SIMPLEX, 0.45, (0, 255, 255), 1)
n = 8
w = strip.shape[1] // n
rows = [strip[:, i * w:(i + 1) * w] for i in range(n)]
cv2.imwrite(f"{HERE}/out/ring_strip.jpg", np.vstack(rows))
print(strip.shape)
