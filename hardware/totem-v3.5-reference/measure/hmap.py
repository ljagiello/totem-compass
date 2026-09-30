"""hmap.py x y [x y ...] — map photo px to module-frame mm through homog.json;
with two or more points, also print successive distances."""
import sys
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
H = np.array(json.load(open(f"{HERE}/homog.json"))["H_mm_to_px"])
Hi = np.linalg.inv(H)
v = [float(a) for a in sys.argv[1:]]
P = np.array(v).reshape(-1, 2)
Q = np.c_[P, np.ones(len(P))] @ Hi.T
Q = Q[:, :2] / Q[:, 2:]
for p, q in zip(P, Q):
    print(f"px ({p[0]:7.1f}, {p[1]:7.1f}) -> mm ({q[0]:7.3f}, {q[1]:7.3f})")
for a, b in zip(Q, Q[1:]):
    print(f"  step {np.linalg.norm(b - a):.3f} mm")
