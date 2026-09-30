"""led2board.py x y [x y ...] — photos/led.jpg px -> board mm (led_reg.json)."""
import sys
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
Mi = np.linalg.inv(np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"]))
P = np.array([float(a) for a in sys.argv[1:]]).reshape(-1, 2)
for p in P:
    b = Mi @ np.array([p[0], p[1], 1.0])
    print(f"px ({p[0]:7.1f}, {p[1]:7.1f}) -> board ({b[0]:7.3f}, {b[1]:7.3f})")
