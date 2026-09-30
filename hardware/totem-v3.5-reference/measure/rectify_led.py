"""Rectify the LED-side photo into the board frame (as seen from the radio side,
x right, y up), 40 px/mm like rect_radio.png, with the current outline drawn.

Pixel (u, v) of the output is board (X0 + u/K, Y1 - v/K). The LED side is
seen mirrored in the photo; led_reg.json's similarity undoes it.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
K, X0, X1, Y0, Y1 = 40.0, -27.0, 27.0, -27.0, 27.0
S = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
Bm = np.array([[1 / K, 0, X0], [0, -1 / K, Y1], [0, 0, 1]])
W, Hh = int((X1 - X0) * K), int((Y1 - Y0) * K)
im = cv2.imread(f"{HERE}/photos/led.jpg")
out = cv2.warpPerspective(im, S @ Bm, (W, Hh), flags=cv2.INTER_CUBIC | cv2.WARP_INVERSE_MAP)
cv2.imwrite(f"{HERE}/out/rect_led.png", out)
g = out.copy()
for mm in range(int(X0), int(X1) + 1, 5):
    u = int((mm - X0) * K)
    cv2.line(g, (u, 0), (u, Hh), (0, 255, 255), 1)
    cv2.putText(g, str(mm), (u + 3, 22), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (0, 255, 255), 2)
for mm in range(int(Y0), int(Y1) + 1, 5):
    v = int((Y1 - mm) * K)
    cv2.line(g, (0, v), (W, v), (0, 255, 255), 1)
    cv2.putText(g, str(mm), (3, v - 4), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (0, 255, 255), 2)
poly = np.array(json.load(open(f"{HERE}/outline2_poly.json")))
uv = np.c_[(poly[:, 0] - X0) * K, (Y1 - poly[:, 1]) * K].astype(np.int32)
cv2.polylines(g, [uv], True, (255, 0, 255), 2)
cv2.imwrite(f"{HERE}/out/rect_led_grid.png", g)
cv2.imwrite(f"{HERE}/out/rect_led_small.jpg", cv2.resize(g, None, fx=0.5, fy=0.5))
