"""Register the LED-side photo (photos/led.jpg) to the board frame.

The photo shows the board from below and turned a quarter: the USB-C's four
shell slots are along its bottom and the ESP32 antenna overhang is off its
left. Its 60-LED ring images round to 1% (ring.py), so the photo itself is
close to a similarity.

Two registrations, and why the second is the one kept:

1. Similarity (angle, scale, shift) on the edges clean from this side: the
   lower-left notch's two edges and the tab's top. It agrees with the radio
   side's outline to 0.1 mm across the board in x, but puts the right side
   1.0-1.5 mm lower in y than the radio photo does: the LR notch edge by
   1.0 mm, the USB-C slots by 1.3-1.5 mm. Locally the two photos agree (slot
   to notch 20.6 vs 20.95 mm); what differs is how each maps the right side
   against the left, a shear one of the two photos carries and neither can
   settle alone.
2. Affine (six numbers) on the same edges plus the LR notch edge and the two
   USB-C slots, i.e. against the radio side's outline, which is the one the
   board is built from. It absorbs the shear so that LED-side parts land
   where they sit relative to that outline, everywhere on the board.

The residual disagreement (~1.3 mm in y at the right edge) is a stated
tolerance; the report asks for calipers on it.
"""
import json
import numpy as np
from scipy.optimize import least_squares

HERE = __file__.rsplit("/", 1)[0]
F = json.load(open(f"{HERE}/fits.json"))
E = {k: np.array(json.load(open(f"{HERE}/e_{k}.json"))) for k in ("llh", "lli", "tab", "lrh")}
E["lrh"] = E["lrh"][:15]                  # past v=2650 the scan runs onto the chamfer
USB_PX = np.array([[1582.0, 2662.0], [1590.0, 2810.0]])
USB_MM = np.array([[18.747, 8.001], [21.422, 8.114]])     # radio side, px2board.py


def line_dist(name, P):
    """signed distance of board points from a fitted edge, at its measured angle"""
    a = np.radians(F[name]["ang"])
    n = np.array([-np.sin(a), np.cos(a)])
    return (P - np.array(F[name]["p"])) @ n


TT_P = np.array(F["tab top L"]["p"])
TT_D = np.array(F["tab top R"]["p"]) - TT_P
TT_N = np.array([-TT_D[1], TT_D[0]]) / np.linalg.norm(TT_D)


def sim(q):
    """board mm -> px: mirror, a quarter turn plus th, scale s, shift"""
    th, s, tx, ty = q
    c, si = np.cos(th), np.sin(th)
    return np.array([[-s * si, -s * c, tx], [s * c, -s * si, ty], [0, 0, 1.0]])


def aff(q):
    return np.array([[q[0], q[1], q[2]], [q[3], q[4], q[5]], [0, 0, 1.0]])


def to_mm(M, P):
    return (np.c_[P, np.ones(len(P))] @ np.linalg.inv(M).T)[:, :2]


def parts(M, use_right):
    p = {"llh": line_dist("LL notch top", to_mm(M, E["llh"])),
         "lli": line_dist("LL notch inner", to_mm(M, E["lli"])),
         "tab": (to_mm(M, E["tab"]) - TT_P) @ TT_N}
    if use_right:
        p["lrh"] = line_dist("LR notch top", to_mm(M, E["lrh"]))
        p["usb"] = (to_mm(M, USB_PX) - USB_MM).ravel()
    return p


def report(tag, M, use_right):
    print(f"[{tag}]")
    for n, part in parts(M, True).items():
        inl = np.abs(part) < 0.3
        rms = np.sqrt(np.mean(part[inl] ** 2)) * 1000 if inl.any() else float("nan")
        print(f"  {n:4}: {inl.sum():3d}/{len(part)} within 0.3 mm, inlier rms {rms:4.0f} um, "
              f"median {np.median(part):+.2f} mm")


r1 = least_squares(lambda q: np.concatenate(list(parts(sim(q), False).values())),
                   [0.0, 57.8, 1966.7, 1572.0], loss="soft_l1", f_scale=0.15, x_scale=[0.01, 1, 10, 10])
report("similarity, left-side edges only", sim(r1.x), False)
M1 = sim(r1.x)
q0 = [M1[0, 0], M1[0, 1], M1[0, 2], M1[1, 0], M1[1, 1], M1[1, 2]]
r2 = least_squares(lambda q: np.concatenate(list(parts(aff(q), True).values())),
                   q0, loss="soft_l1", f_scale=0.15, x_scale=[1, 1, 10, 1, 1, 10])
M2 = aff(r2.x)
report("affine, against the radio-side outline", M2, True)
L_ = M2[:2, :2]
sv = np.linalg.svd(L_, compute_uv=False)
print(f"affine scales {sv[0]:.2f} / {sv[1]:.2f} px/mm (anisotropy {sv[0]/sv[1]-1:.1%})")
json.dump({"S_board_to_px": M2.tolist(), "similarity_board_to_px": M1.tolist()},
          open(f"{HERE}/led_reg.json", "w"), indent=1)
