"""predict_led.py OUT x y [x y ...] — mark board points on the LED photo (led_reg.json)
and crop around them, to see whether through-board features line up."""
import sys
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
S = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
out = sys.argv[1]
P = np.array([float(a) for a in sys.argv[2:]]).reshape(-1, 2)
uv = (np.c_[P, np.ones(len(P))] @ S.T)[:, :2]
im = cv2.imread(f"{HERE}/photos/led.jpg")
for p, q in zip(P, uv):
    cv2.circle(im, (int(q[0]), int(q[1])), 14, (0, 0, 255), 2)
    print(f"board ({p[0]:6.2f}, {p[1]:6.2f}) -> px ({q[0]:7.1f}, {q[1]:7.1f})")
lo = uv.min(0).astype(int) - 250
hi = uv.max(0).astype(int) + 250
c = im[max(lo[1], 0):hi[1], max(lo[0], 0):hi[0]]
for g in range(0, c.shape[1], 50):
    cv2.putText(c, str(max(lo[0], 0) + g), (g, 12), cv2.FONT_HERSHEY_SIMPLEX, 0.35, (0, 255, 255), 1)
for g in range(0, c.shape[0], 50):
    cv2.putText(c, str(max(lo[1], 0) + g), (0, g), cv2.FONT_HERSHEY_SIMPLEX, 0.35, (0, 255, 255), 1)
cv2.imwrite(f"{HERE}/out/{out}.jpg", c)
