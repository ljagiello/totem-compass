"""Put the traced board edge into a board frame through the homography.

1. contour px -> module-frame mm (homog.json), y flipped to point up
2. rotate so the two long side edges are vertical (their mean angle)
3. shift so the side edges sit at +-W/2 and the tab's top is at the
   same height the bottom arc's lowest point is below (bbox centred)
Saves board2.json (the transform) and contour3_mm.npy (edge in the new frame).
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
H = np.array(json.load(open(f"{HERE}/homog.json"))["H_mm_to_px"])
Hi = np.linalg.inv(H)

im = cv2.imread(f"{HERE}/photos/radio.jpg")
hsv = cv2.cvtColor(im, cv2.COLOR_BGR2HSV)
mask = cv2.inRange(hsv, (40, 80, 40), (89, 255, 255))
mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
mask = cv2.morphologyEx(mask, cv2.MORPH_CLOSE, np.ones((31, 31), np.uint8))
cnts, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
px = max(cnts, key=cv2.contourArea)[:, 0, :].astype(float)


def to_mod(P):
    v = np.c_[P, np.ones(len(P))] @ Hi.T
    q = v[:, :2] / v[:, 2:]
    return np.c_[q[:, 0], -q[:, 1]]          # y up


M = to_mod(px)
# side edges: points far left / far right, in the stretches no part covers
lo = M[:, 0] < M[:, 0].min() + 0.4
hi = M[:, 0] > M[:, 0].max() - 0.4


def edge_angle(Q):
    c = Q.mean(0)
    _, _, vt = np.linalg.svd(Q - c)
    d = vt[0] if vt[0][1] > 0 else -vt[0]
    return np.arctan2(d[0], d[1]), c, len(Q)


aL, cL, nL = edge_angle(M[lo])
aR, cR, nR = edge_angle(M[hi])
print(f"left edge  {np.degrees(aL):+.2f} deg from vertical ({nL} pts)")
print(f"right edge {np.degrees(aR):+.2f} deg from vertical ({nR} pts)")
rot = aL                 # the left edge: 1600+ clean points; the right is mostly covered
c, s = np.cos(rot), np.sin(rot)
Rm = np.array([[c, -s], [s, c]])
Mr = M @ Rm.T
xl = np.median(Mr[lo][:, 0])
xr = np.median(Mr[hi][:, 0])
print(f"width between the side edges: {xr - xl:.3f} mm")
ytop, ybot = Mr[:, 1].max(), Mr[:, 1].min()
off = np.array([-(xl + xr) / 2, -(ytop + ybot) / 2])
B = Mr + off
print(f"extent: x {B[:,0].min():.2f}..{B[:,0].max():.2f}, y {B[:,1].min():.2f}..{B[:,1].max():.2f}")
np.save(f"{HERE}/contour3_mm.npy", B)
json.dump({"rot": rot, "off": off.tolist(),
           "doc": "px -> Hinv -> (x, -y) -> rotate by rot -> + off  = board mm, y up"},
          open(f"{HERE}/board2.json", "w"), indent=1)
