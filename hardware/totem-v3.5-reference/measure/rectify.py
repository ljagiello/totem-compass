"""Orthorectify radio.png into the board frame: K px per mm, y up.

Output pixel (u, v) is board point x = X0 + u/K, y = Y1 - v/K. Writes
rect_radio.png (plain) and rect_radio_grid.png (1 mm grid, 5 mm labels,
outline in magenta), so parts can be measured by reading pixels.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
K, X0, X1, Y0, Y1 = 40.0, -27.0, 27.0, -27.0, 27.0
H = np.array(json.load(open(f"{HERE}/homog.json"))["H_mm_to_px"])
fr = json.load(open(f"{HERE}/board2.json"))
c, s = np.cos(fr["rot"]), np.sin(fr["rot"])
ox, oy = fr["off"]
# output px -> board mm
Bm = np.array([[1 / K, 0, X0], [0, -1 / K, Y1], [0, 0, 1]])
# board mm -> module mm: subtract offset, rotate back by -rot, flip y
T = np.array([[1, 0, -ox], [0, 1, -oy], [0, 0, 1]])
Rinv = np.array([[c, s, 0], [-s, c, 0], [0, 0, 1]])
Fl = np.diag([1, -1, 1])
M = H @ Fl @ Rinv @ T @ Bm
W, Hh = int((X1 - X0) * K), int((Y1 - Y0) * K)
im = cv2.imread(f"{HERE}/photos/radio.jpg")
out = cv2.warpPerspective(im, M, (W, Hh), flags=cv2.INTER_CUBIC | cv2.WARP_INVERSE_MAP)
cv2.imwrite(f"{HERE}/out/rect_radio.png", out)
g = out.copy()
for mm in range(int(X0), int(X1) + 1):
    u = int((mm - X0) * K)
    cv2.line(g, (u, 0), (u, Hh), (0, 200, 255) if mm % 5 else (0, 255, 255), 2 if mm % 5 == 0 else 1)
    if mm % 5 == 0:
        cv2.putText(g, str(mm), (u + 3, 22), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (0, 255, 255), 2)
for mm in range(int(Y0), int(Y1) + 1):
    v = int((Y1 - mm) * K)
    cv2.line(g, (0, v), (W, v), (0, 200, 255) if mm % 5 else (0, 255, 255), 2 if mm % 5 == 0 else 1)
    if mm % 5 == 0:
        cv2.putText(g, str(mm), (3, v - 4), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (0, 255, 255), 2)
poly = np.array(json.load(open(f"{HERE}/outline2_poly.json")))
uv = np.c_[(poly[:, 0] - X0) * K, (Y1 - poly[:, 1]) * K].astype(np.int32)
cv2.polylines(g, [uv], True, (255, 0, 255), 2)
cv2.imwrite(f"{HERE}/out/rect_radio_grid.png", g)
print(W, Hh)
