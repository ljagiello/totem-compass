"""px2board.py x y [x y ...] — radio.png px -> board mm (homog.json + board2.json)."""
import sys
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
H = np.array(json.load(open(f"{HERE}/homog.json"))["H_mm_to_px"])
fr = json.load(open(f"{HERE}/board2.json"))
c, s = np.cos(fr["rot"]), np.sin(fr["rot"])


def px2board(P):
    P = np.atleast_2d(P)
    v = np.c_[P, np.ones(len(P))] @ np.linalg.inv(H).T
    m = v[:, :2] / v[:, 2:]
    m = np.c_[m[:, 0], -m[:, 1]]
    return m @ np.array([[c, -s], [s, c]]).T + fr["off"]


if __name__ == "__main__":
    P = np.array([float(a) for a in sys.argv[1:]]).reshape(-1, 2)
    for p, b in zip(P, px2board(P)):
        print(f"px ({p[0]:7.1f}, {p[1]:7.1f}) -> board ({b[0]:7.3f}, {b[1]:7.3f})")
