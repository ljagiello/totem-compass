"""Assemble the board outline from the fitted stretches (fits.json).

Every straight edge is drawn on its fitted line, at its measured angle.
An earlier version snapped the near-orthogonal edges to the axes; the
LED-side photo, registered independently, showed that the notch edges
really are a couple of degrees apart and the right edge is not parallel
to the left, and the snapping had moved the right side by up to 1.5 mm.

Corners are the intersections of neighbouring fits. Where a photo shows an
outside corner rounded, a fillet is used; the notches' inside corners are
rounded ~1 mm in the photo. The lower-right notch corner is under the JST
on the radio side; its chamfer comes from the LED-side photo when
fits.json has "LR chamfer", otherwise the lower-left one mirrored.

Writes outline2.json: a closed list of segments, each
  {"t": "line", "a": [x, y], "b": [x, y]} or
  {"t": "arc", "a": ..., "m": ..., "b": ...}   (start, a point on it, end)
in board mm, y up, plus an overlay of the result on the radio photo.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
F = json.load(open(f"{HERE}/fits.json"))


def unit(v):
    v = np.asarray(v, float)
    return v / np.linalg.norm(v)


def fitted(name):
    a = np.radians(F[name]["ang"])
    return (np.array(F[name]["p"]), np.array([np.cos(a), np.sin(a)]))


def through(p, q):
    return (np.asarray(p, float), unit(np.asarray(q, float) - np.asarray(p, float)))


def meet(l1, l2):
    (p, d), (q, e) = l1, l2
    t = np.linalg.solve(np.c_[d, -e], q - p)[0]
    return p + t * d


def circ_line(c, R, ln, pick):
    """intersections of a circle with a line; pick chooses one"""
    p, d = ln
    f = p - c
    b = f @ d
    disc = max(b * b - (f @ f - R * R), 0.0)          # tangent within measurement: touch
    return pick([p + t * d for t in (-b - np.sqrt(disc), -b + np.sqrt(disc))])


def fillet(d_in, d_out, r, P):
    """round corner P: arriving along d_in, leaving along d_out"""
    d1, d2 = unit(d_in), unit(d_out)
    th = np.arccos(np.clip(-d1 @ d2, -1, 1))           # interior angle
    t = r / np.tan(th / 2)
    a, b = P - d1 * t, P + d2 * t
    c = P + unit(-d1 + d2) * r / np.sin(th / 2)
    return a, c + unit(P - c) * r, b


def along(ln, sign_axis, sign):
    """the line's direction, flipped so its component on axis has the given sign"""
    d = ln[1]
    return d if np.sign(d[sign_axis]) == sign else -d


LE, RE = fitted("left edge"), fitted("right edge")
TLn, TRn = fitted("tab left"), fitted("tab right")
TT = through(F["tab top L"]["p"], F["tab top R"]["p"])
LLH, LLI = fitted("LL notch top"), fitted("LL notch inner")
LRH, LRI = fitted("LR notch top"), fitted("LR notch inner")
ULD, URD, LLC = fitted("UL diagonal"), fitted("UR diagonal"), fitted("LL chamfer")
arcC, arcR = np.array(F["bottom arc"]["c"]), F["bottom arc"]["R"]
ulC, ulR = np.array(F["UL notch"]["c"]), F["UL notch"]["R"]
urC, urR = np.array(F["UR notch"]["c"]), F["UR notch"]["R"]
RIN = 1.0

segs = []
L = lambda a, b: segs.append({"t": "line", "a": list(map(float, a)), "b": list(map(float, b))})
A = lambda a, m, b: segs.append({"t": "arc", "a": list(map(float, a)), "m": list(map(float, m)),
                                 "b": list(map(float, b))})
up, down, left, right = (1, 1), (1, -1), (0, -1), (0, 1)

# clockwise from the tab's top-left corner
p_tl, p_tr = meet(TT, TLn), meet(TT, TRn)
L(p_tl, p_tr)
# upper right: tab side down into the notch, around it, out along the diagonal
n1 = circ_line(urC, urR, TRn, lambda c: max(c, key=lambda p: p[1]))
L(p_tr, n1)
n2 = circ_line(urC, urR, URD, lambda c: max(c, key=lambda p: p[0]))   # leaves the notch on its right
A(n1, urC + np.array([0, -urR]), n2)
corner = meet(URD, RE)
a, m, b = fillet(along(URD, 0, 1), along(RE, 1, -1), 0.8, corner)
L(n2, a)
A(a, m, b)
# right edge down to the lower-right notch's outer corner
if "LR chamfer" in F:
    LRC = fitted("LR chamfer")
    r_top, r_bot = meet(LRC, RE), meet(LRC, LRH)
else:
    ll_top, ll_bot = meet(LLC, LE), meet(LLC, LLH)
    cw, ch = ll_bot[0] - ll_top[0], ll_top[1] - ll_bot[1]
    r_bot = meet(LRH, (np.array([meet(LRH, RE)[0] - cw, 0.0]), np.array([0.0, 1.0])))
    r_top = meet(RE, (np.array([0.0, meet(LRH, RE)[1] + ch]), np.array([1.0, 0.0])))
L(b, r_top)
L(r_top, r_bot)
cr = meet(LRH, LRI)
fa, fm, fb = fillet(along(LRH, 0, -1), along(LRI, 1, -1), RIN, cr)
L(r_bot, fa)
A(fa, fm, fb)
# right notch inner side down to the arc, the arc, up the left notch inner side
ar = circ_line(arcC, arcR, LRI, lambda c: min(c, key=lambda p: p[1]))
al = circ_line(arcC, arcR, LLI, lambda c: min(c, key=lambda p: p[1]))
L(fb, ar)
A(ar, arcC + np.array([0, -arcR]), al)
cl = meet(LLI, LLH)
fa, fm, fb = fillet(along(LLI, 1, 1), along(LLH, 0, -1), RIN, cl)
L(al, fa)
A(fa, fm, fb)
ll_top, ll_bot = meet(LLC, LE), meet(LLC, LLH)
L(fb, ll_bot)
L(ll_bot, ll_top)
# left edge up to the upper-left corner (rounded; the photo shows it, the cable hides its size)
corner = meet(ULD, LE)
a, m, b = fillet(along(LE, 1, 1), along(ULD, 0, 1), 2.0, corner)
L(ll_top, a)
A(a, m, b)
n3 = circ_line(ulC, ulR, ULD, lambda c: min(c, key=lambda p: p[0]))   # enters this notch on its left
L(b, n3)
n4 = circ_line(ulC, ulR, TLn, lambda c: max(c, key=lambda p: p[1]))
A(n3, ulC + np.array([0, -ulR]), n4)
L(n4, p_tl)

# closure check
for s, t in zip(segs, segs[1:] + segs[:1]):
    gap = np.linalg.norm(np.array(s["b"]) - np.array(t["a"]))
    assert gap < 1e-6, (s, t, gap)
xs = [p[0] for s in segs for p in (s["a"], s["b"])]
ys = [p[1] for s in segs for p in (s["a"], s["b"])]
ys.append(arcC[1] - arcR)
print(f"{len(segs)} segments, closed; extent x {min(xs):.2f}..{max(xs):.2f} ({max(xs)-min(xs):.2f}), "
      f"y {min(ys):.2f}..{max(ys):.2f} ({max(ys)-min(ys):.2f})")
json.dump({"segments": segs, "assumed": [
    "lower-right notch corner: " + ("chamfer measured on the LED side" if "LR chamfer" in F
                                    else "lower-left chamfer mirrored (under the JST)"),
    "upper-left outside corner radius 2.0 mm (under the u.FL cable)",
    "tab top: the line through its two visible ends (the ESP32 covers the middle)"]},
    open(f"{HERE}/outline2.json", "w"), indent=1)

# overlay on the photo: board mm -> module mm -> px
H = np.array(json.load(open(f"{HERE}/homog.json"))["H_mm_to_px"])
fr = json.load(open(f"{HERE}/board2.json"))
c, s_ = np.cos(fr["rot"]), np.sin(fr["rot"])
Rm = np.array([[c, -s_], [s_, c]])


def to_px(Pm):
    Pm = np.atleast_2d(Pm) - fr["off"]
    Mr = Pm @ Rm                       # inverse rotation
    q = np.c_[Mr[:, 0], -Mr[:, 1], np.ones(len(Mr))] @ H.T
    return q[:, :2] / q[:, 2:]


def sample(s, n=60):
    """points along a segment; an arc goes from a to b through m"""
    a, b = np.array(s["a"]), np.array(s["b"])
    if s["t"] == "line":
        return np.linspace(a, b, 2)
    m = np.array(s["m"])
    A_ = np.array([[2 * (m - a)[0], 2 * (m - a)[1]], [2 * (b - a)[0], 2 * (b - a)[1]]])
    cc = np.linalg.solve(A_, np.array([m @ m - a @ a, b @ b - a @ a]))
    t0, tm, t1 = (np.arctan2(*(p - cc)[::-1]) for p in (a, m, b))
    if (tm - t0) % (2 * np.pi) < (t1 - t0) % (2 * np.pi):
        ts = t0 + np.linspace(0, (t1 - t0) % (2 * np.pi), n)
    else:
        ts = t0 - np.linspace(0, (t0 - t1) % (2 * np.pi), n)
    return cc + np.linalg.norm(a - cc) * np.c_[np.cos(ts), np.sin(ts)]


poly = np.vstack([sample(s) for s in segs])
json.dump(poly.tolist(), open(f"{HERE}/outline2_poly.json", "w"))
im = cv2.imread(f"{HERE}/photos/radio.jpg")
cv2.polylines(im, [to_px(poly).astype(np.int32)], True, (255, 0, 255), 4)
cv2.imwrite(f"{HERE}/out/outline2.jpg", cv2.resize(im[500:3900], None, fx=0.33, fy=0.33))
for nm, (x0, y0, x1, y1) in {"ul": (0, 750, 1100, 1700), "ur": (1900, 750, 3024, 1900),
                             "bl": (0, 2700, 1100, 3900), "br": (2000, 2700, 3024, 3900)}.items():
    cv2.imwrite(f"{HERE}/out/outline2_{nm}.jpg", im[y0:y1, x0:x1])
