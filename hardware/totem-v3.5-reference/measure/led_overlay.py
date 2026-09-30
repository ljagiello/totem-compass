"""Draw the outline (through led_reg.json) and the edge scans on the LED photo."""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
S = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
poly = np.array(json.load(open(f"{HERE}/outline2_poly.json")))
im = cv2.imread(f"{HERE}/photos/led.jpg")
uv = (np.c_[poly, np.ones(len(poly))] @ S.T)[:, :2]
cv2.polylines(im, [uv.astype(np.int32)], True, (255, 0, 255), 4)
col = {"llh": (0, 0, 255), "lli": (0, 255, 0), "arc": (0, 255, 255), "tab": (255, 255, 0)}
for k, c in col.items():
    for p in json.load(open(f"{HERE}/e_{k}.json")):
        cv2.circle(im, (int(p[0]), int(p[1])), 9, c, -1)
for p in ((1582, 2662), (1590, 2810), (2150, 2650), (2155, 2805)):
    cv2.circle(im, p, 22, (0, 128, 255), 4)
cv2.imwrite(f"{HERE}/out/led_overlay.jpg", cv2.resize(im, None, fx=0.3, fy=0.3))
