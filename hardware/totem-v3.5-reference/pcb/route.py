"""A grid maze router for this board: A* over F.Cu and B.Cu with vias.

In1 (GND) and In2 (3V3) are planes, so only the outer
layers are routed. Every clearance comes from a distance transform of the
copper that belongs to other nets, per net class, so a route that is found
is clear by construction; KiCad's DRC checks it again afterwards.

  pads          rasterised from KiCad's own pad polygons (board.json)
  tracks        0.13 mm / 0.13 mm, power nets 0.4 mm / 0.2 mm
  vias          0.45 mm / 0.2 mm, never in or touching a pad, 0.6 mm apart
  board edge    0.3 mm; the ESP32 antenna keepout is not entered
  plane nets    GND, +3V3: each pad gets a short stub to a via; the ring's
                and crystal's GND pads sit in the LED side's inner GND pour,
                the ring's VDD pads in its outer VLED pour, and VLED itself
                is routed from Q4 across the ring band on F.Cu to that pour;
                a radio-side GND pad with no room for a via has the F.Cu pour

Every net and every fan-out is a task whose copper is tagged, so one task
can be ripped up without touching another. Order: LED data links, power
nets, signals shortest first, then the fan-outs, which are short and can go
anywhere near their pad. A task that fails rips up the tasks whose copper
lies around it, goes first, and they are put back after it. Cells right
next to pads cost a little more, so routes leave pins their way out.

Run with a Python that has numpy, scipy and opencv:  python route.py
Output: routes.json for apply.py.
"""
import heapq
import json
import math
import os
import time

import cv2
import numpy as np
from scipy import ndimage

HERE = os.path.dirname(os.path.abspath(__file__))
G = 0.05
X0, Y0 = -24.5, -26.0
NX, NY = int(49.0 / G), int(50.0 / G)
N = NX * NY
F, B = 0, 1
LAYER = {"F.Cu": F, "B.Cu": B}
MARGIN = 0.04
EDGE_MARGIN = 0.07        # the outline is rasterised too; arcs lose a few um
VIA_D = 0.45
VIA_COST = 0.8            # mm-equivalent
NEAR_PAD_COST = 0.6       # extra, as a fraction of a step, within 0.5 mm of a pad
POWER = {"VBAT", "VSYS", "VBUS", "VLED"}
PLANE = {"GND", "+3V3"}
SQ2 = math.sqrt(2)

g = json.load(open(os.path.join(HERE, "board.json")))


def ij(x, y):
    return (x - X0) / G, (y - Y0) / G


def xy(i, j):
    return X0 + i * G, Y0 + j * G


def raster(poly):
    m = np.zeros((NY, NX), np.uint8)
    pts = np.array([[(x - X0) / G * 16, (y - Y0) / G * 16] for x, y in poly], np.int32)
    cv2.fillPoly(m, [pts], 1, lineType=cv2.LINE_8, shift=4)
    return m.astype(bool)


# ---------------------------------------------------------------- the board
outline = json.load(open(os.path.join(HERE, "..", "measure", "outline2_poly.json")))
dist_edge = ndimage.distance_transform_edt(raster(outline)) * G
# footprints' rule areas, per layer: the ESP32 antenna (both), the u.FL (F),
# the IMU (under its package, B)
keep_tr = [np.zeros((NY, NX), bool), np.zeros((NY, NX), bool)]
keep_via = np.zeros((NY, NX), bool)
for kp in g["keepouts"]:
    m = raster(kp["poly"])
    for l in kp["layers"]:
        if kp["tracks"]:
            keep_tr[LAYER[l]] |= m
    if kp["vias"]:
        keep_via |= m
keep = keep_tr[F] & keep_tr[B]
# the LED side's VLED pour reaches the ring's VDD pads through the band just
# outside the ring; keep that band whole by letting no other net's track cross it
yy, xx = np.mgrid[0:NY, 0:NX]
RC, RR = g["ring_centre"], g["ring_R"]
_r = np.hypot(X0 + xx * G - RC[0], Y0 + yy * G - RC[1])
annulus = (_r >= RR + 0.95) & (_r <= RR + 2.0)       # just beyond the LEDs' outer pads
# /RING_D0 too: D100's DI pad faces out, and the gap between two LEDs' inner
# pads is 0.35 mm, so the ring's data can only come in from outside. That cuts
# the band once at D100; VLED is fed to D159 on the far side of the cut as well.
ANNULUS_OK = {"VLED", "/RING_D0"}
# rule areas and the band forbid any overlap, not just a centre inside them
d_keep_tr = [ndimage.distance_transform_edt(~keep_tr[L]) * G for L in (F, B)]
keep_via_d = ndimage.distance_transform_edt(~keep_via) * G < VIA_D / 2 + MARGIN
d_annulus = ndimage.distance_transform_edt(~annulus) * G
annulus_d = d_annulus < VIA_D / 2 + MARGIN
gnd_pour_b = raster([z for z in g["zones"] if z["name"] == "GND inside ring"][0]["poly"])

nets = sorted({p["net"] for p in g["pads"] if p["net"]} | set(g["nets"]))
NID = {n: k for k, n in enumerate(nets)}
owner = np.full((2, NY, NX), -1, np.int32)       # net id of the copper in each cell
tmap = np.full((2, NY, NX), -1, np.int32)        # task id of routed copper
padmask = np.zeros((2, NY, NX), bool)
pads_by_net = {}
for p in g["pads"]:
    n = p["net"] or f"nc-{p['ref']}-{p['num']}"
    if n not in NID:
        NID[n] = len(nets)
        nets.append(n)
    p["nid"] = NID[n]
    p["cells"] = {}
    for l, poly in p["poly"].items():
        m = raster(poly)
        if not m.any():
            ci, cj = ij(p["x"], p["y"])
            m[int(round(cj)), int(round(ci))] = True
        L = LAYER[l]
        owner[L][m] = p["nid"]
        padmask[L] |= m
        p["cells"][L] = m
    pads_by_net.setdefault(n, []).append(p)
dist_pad = [ndimage.distance_transform_edt(~padmask[L]) * G for L in (F, B)]
near_pad = [bytearray((d < 0.5).astype(np.uint8).tobytes()) for d in dist_pad]
print(f"grid {NX}x{NY} at {G} mm, {len(g['pads'])} pads, {len(pads_by_net)} nets")

tracks, vias = [], []
TASKS = {}
# history: every rip-up makes the cells it freed a little dearer, so nets that
# keep fighting over a corridor learn to go round (PathFinder's history cost)
hist = np.zeros((2, NY, NX), np.float32)


# the GNSS antenna feed is a 50-ohm microstrip over In1: on a 1.6 mm 4-layer
# stack-up F.Cu sits ~0.21 mm (7628 prepreg, er 4.4) over In1, which makes
# 50 ohms about 0.38 mm wide
RF = {"/GNSS_RF": (0.38, 0.3)}


def cls(net):
    if net in RF:
        return RF[net]
    if net in POWER:
        return 0.4, 0.2
    return 0.13, 0.13


def via_centres():
    return [(v[1], v[2]) for v in vias if v is not None]


POWER_IDS = np.array(sorted(NID[n] for n in POWER if n in NID))
RF_IDS = np.array(sorted(NID[n] for n in RF if n in NID))


def free_maps(nid, w, c, pads_only=False, annulus_ok=False):
    """per layer: may a track centre of this net sit here? and may a via?
    Clearance to another net is the larger of the two classes': power copper
    keeps 0.2 mm from everything. pads_only: as if nothing were routed yet."""
    hw = w / 2
    vr = VIA_D / 2
    fr, ok_v = [], []
    for L in (F, B):
        own = np.where(padmask[L], owner[L], -1) if pads_only else owner[L]
        foreign = (own >= 0) & (own != nid)
        pw = foreign & np.isin(own, POWER_IDS)
        rf = foreign & np.isin(own, RF_IDS)
        d_o = ndimage.distance_transform_edt(~(foreign & ~pw & ~rf)) * G
        d_p = ndimage.distance_transform_edt(~pw) * G if pw.any() else np.full((NY, NX), 99.0)
        d_r = ndimage.distance_transform_edt(~rf) * G if rf.any() else np.full((NY, NX), 99.0)
        m = (d_o >= hw + c + MARGIN) & (d_p >= hw + max(c, 0.2) + MARGIN) & \
            (d_r >= hw + max(c, 0.3) + MARGIN) & \
            (dist_edge >= hw + 0.3 + EDGE_MARGIN) & (d_keep_tr[L] >= hw + MARGIN)
        if L == B and not annulus_ok:
            m &= d_annulus >= hw + MARGIN       # no other net's track across the band
        fr.append(m)
        ok_v.append((d_o >= vr + max(c, 0.15) + MARGIN) & (d_p >= vr + 0.2 + MARGIN) & (d_r >= vr + 0.3 + MARGIN))
    via = ok_v[F] & ok_v[B] & (dist_edge >= vr + 0.3 + EDGE_MARGIN) & ~keep_via_d & ~keep & \
        (dist_pad[F] >= vr + 0.1) & (dist_pad[B] >= vr + 0.1)
    if not annulus_ok:
        via &= ~annulus_d                            # nor via: a via plugs it as well
    vm = np.zeros((NY, NX), bool)
    for x, y in ([] if pads_only else via_centres()):
        i, j = ij(x, y)
        vm[int(round(j)), int(round(i))] = True
    if vm.any():
        via &= ndimage.distance_transform_edt(~vm) * G >= VIA_D + 0.15
    return fr, via


def astar(sources, goal, target_xy, fr, via_ok, max_cost=1e9):
    frf = [bytearray(fr[L].astype(np.uint8).tobytes()) for L in (F, B)]
    hq = [bytearray(np.clip(hist[L] * 10, 0, 255).astype(np.uint8).tobytes()) for L in (F, B)]
    vf = bytearray(via_ok.astype(np.uint8).tobytes())
    gs, came = {}, {}
    ti, tj = ij(*target_xy)
    pq = []
    for L, j, i in sources:
        k = L * N + j * NX + i
        if k not in gs:
            gs[k] = 0.0
            came[k] = -1
            pq.append((G * math.hypot(i - ti, j - tj), 0.0, k))
    heapq.heapify(pq)
    steps = ((1, 0, G), (-1, 0, G), (0, 1, G), (0, -1, G), (1, 1, G * SQ2), (1, -1, G * SQ2),
             (-1, 1, G * SQ2), (-1, -1, G * SQ2))
    n_exp = 0
    while pq:
        f, gc, k = heapq.heappop(pq)
        if gc > gs.get(k, 1e18) + 1e-9:
            continue
        L, r = divmod(k, N)
        j, i = divmod(r, NX)
        if goal(L, j, i):
            path = []
            while k != -1:
                L2, r2 = divmod(k, N)
                path.append((L2, *divmod(r2, NX)))
                k = came[k]
            return path[::-1], n_exp
        n_exp += 1
        if gc > max_cost:
            continue
        fl, npad, hl = frf[L], near_pad[L], hq[L]
        for di, dj, cst in steps:
            i2, j2 = i + di, j + dj
            if not (0 <= i2 < NX and 0 <= j2 < NY):
                continue
            r2 = j2 * NX + i2
            if not fl[r2]:
                continue
            k2 = L * N + r2
            g2 = gc + cst * (1 + NEAR_PAD_COST * npad[r2] + hl[r2] / 10)
            if g2 < gs.get(k2, 1e18):
                gs[k2] = g2
                came[k2] = k
                heapq.heappush(pq, (g2 + G * math.hypot(i2 - ti, j2 - tj), g2, k2))
        if vf[r]:
            L2 = 1 - L
            if frf[L2][r]:
                k2 = L2 * N + r
                g2 = gc + VIA_COST * (1 + (hq[F][r] + hq[B][r]) / 20)
                if g2 < gs.get(k2, 1e18):
                    gs[k2] = g2
                    came[k2] = k
                    heapq.heappush(pq, (g2 + G * math.hypot(i - ti, j - tj), g2, k2))
    return None, n_exp


def mark(tid, nid, L, m):
    free = m & (owner[L] < 0)
    owner[L][free] = nid
    tmap[L][free] = tid


def add_via(tid, net, i, j):
    x, y = xy(i, j)
    vias.append([net, x, y])
    TASKS[tid]["items"].append(("v", len(vias) - 1))
    m = np.zeros((NY, NX), np.uint8)
    cv2.circle(m, (i, j), int(round(VIA_D / 2 / G)), 1, -1)
    for L in (F, B):
        mark(tid, NID[net], L, m.astype(bool))


def commit(tid, path, w, end_via=False):
    """turn a cell path into tracks and vias, and mark its copper"""
    net = TASKS[tid]["net"]
    nid = NID[net]
    segs, start, prev, pdir = [], path[0], path[0], None
    for cur in path[1:]:
        if cur[0] != prev[0]:                       # a layer change is a via at prev
            if prev != start:
                segs.append((prev[0], start, prev))
            add_via(tid, net, prev[2], prev[1])
            start, pdir = cur, None
        else:
            d = (cur[1] - prev[1], cur[2] - prev[2])
            if pdir is not None and d != pdir:
                segs.append((prev[0], start, prev))
                start = prev
            pdir = d
        prev = cur
    if prev != start:
        segs.append((prev[0], start, prev))
    # a track that stops in the first cell inside a pad only nicks its edge;
    # carry both ends on to the pad's centre, which stays inside the pad
    for L, j, i in (path[0], path[-1]):
        if not padmask[L][j, i]:
            continue
        for p in pads_by_net.get(net, []):
            m = p["cells"].get(L)
            if m is not None and m[j, i]:
                x1, y1 = xy(i, j)
                if math.hypot(p["x"] - x1, p["y"] - y1) > 1e-3:
                    tracks.append([net, "F.Cu" if L == F else "B.Cu", x1, y1, p["x"], p["y"], w])
                    TASKS[tid]["items"].append(("t", len(tracks) - 1))
                break
    if end_via:
        add_via(tid, net, prev[2], prev[1])
    TASKS[tid]["path"].extend(path)
    for L, a, b in segs:
        x1, y1 = xy(a[2], a[1])
        x2, y2 = xy(b[2], b[1])
        tracks.append([net, "F.Cu" if L == F else "B.Cu", x1, y1, x2, y2, w])
        TASKS[tid]["items"].append(("t", len(tracks) - 1))
        m = np.zeros((NY, NX), np.uint8)
        cv2.line(m, (a[2], a[1]), (b[2], b[1]), 1, thickness=max(1, int(math.ceil(w / G)) | 1))
        mark(tid, nid, L, m.astype(bool))


CASCADE = set()


def rip(tid):
    """remove a task's copper; fan-outs that ended on it go too (and are noted)"""
    for k, u in TASKS.items():
        if u.get("parent") == tid and u["done"]:
            u.pop("parent")
            rip(k)
            CASCADE.add(k)
    t = TASKS[tid]
    for kind, k in t["items"]:
        if kind == "t":
            tracks[k] = None
        else:
            vias[k] = None
    t["items"], t["path"], t["done"] = [], [], False
    for L in (F, B):
        m = tmap[L] == tid
        hist[L][m] = np.minimum(hist[L][m] + 0.3, 3.0)
        owner[L][m] = -1
        tmap[L][m] = -1


def cells_of(p, L):
    m = p["cells"].get(L)
    if m is None:
        return []
    js, is_ = np.nonzero(m)
    return [(L, int(a), int(b)) for a, b in zip(js, is_)]


def run_net(tid, log=True, dry=False):
    """route a net; dry: ignore routed copper and return the paths, commit nothing"""
    t = TASKS[tid]
    net, pads = t["net"], t["pads"]
    w, c = cls(net)
    nid = NID[net]
    done, todo = [pads[0]], pads[1:]
    acc = []
    aok = net in ANNULUS_OK
    while todo:
        fr, via_ok = free_maps(nid, w, c, pads_only=dry, annulus_ok=aok)
        if net in RF:                        # a microstrip stays on F.Cu, over In1, via-free
            via_ok[:] = False
            fr[B][:] = False
        for p in pads:
            for L, m in p["cells"].items():
                fr[L] = fr[L] | m
        best = min(todo, key=lambda p: min(math.hypot(p["x"] - q["x"], p["y"] - q["y"]) for q in done))
        src = [cc for q in done for L in q["cells"] for cc in cells_of(q, L)] + t["path"] + \
            [cc for pth in acc for cc in pth]
        tgt = best["cells"]
        path, n = astar(src, lambda L, j, i: L in tgt and tgt[L][j, i], (best["x"], best["y"]), fr, via_ok)
        if path is None:
            if log:
                print(f"  FAIL {net}: {best['ref']}.{best['num']} ({n} expanded)")
            return None if dry else False
        if dry:
            acc.append(path)
        else:
            commit(tid, path, w)
        done.append(best)
        todo.remove(best)
    if dry:
        return acc
    t["done"] = True
    return True


def run_fan(tid, log=True, dry=False):
    t = TASKS[tid]
    net, p = t["net"], t["pad"]
    w, c = 0.2, 0.15
    ci, cj = ij(p["x"], p["y"])
    fr, via_ok = free_maps(NID[net], w, c, pads_only=dry, annulus_ok=bool(annulus[int(round(cj)), int(round(ci))]))
    L0 = list(p["cells"])[0]
    fr[L0] = fr[L0] | p["cells"][L0]
    fr[1 - L0] = np.zeros_like(fr[L0])
    # a fan-out may also end on copper of its net that already reaches the
    # plane: another fan-out's stub or via (adjacent supply pins share one)
    # only a via of this net is known to reach the plane (a stub that merely
    # touches the net's other tracks may not)
    joined = np.zeros((NY, NX), bool)
    if not dry:
        for v in vias:
            if v is not None and v[0] == net:
                vi, vj = (int(round(u)) for u in ij(v[1], v[2]))
                cv2.circle(joined.view(np.uint8), (vi, vj), int(VIA_D / 2 / G), 1, -1)
    goal = lambda L, j, i: L == L0 and (via_ok[j, i] or joined[j, i])
    path, n = astar(cells_of(p, L0), goal, (p["x"], p["y"]), fr, np.zeros_like(via_ok), max_cost=5.0)
    if path is None:
        if log:
            print(f"  FAIL fan-out {net}: {p['ref']}.{p['num']}")
        return None if dry else False
    if dry:
        return [path + [(1 - path[-1][0], path[-1][1], path[-1][2])]]     # the via
    last = path[-1]
    if joined[last[1], last[2]]:
        t["parent"] = int(tmap[L0][last[1], last[2]])
    commit(tid, path, w, end_via=not joined[last[1], last[2]])
    t["done"] = True
    return True


def run(tid, log=True, dry=False):
    return run_fan(tid, log, dry) if TASKS[tid]["kind"] == "fan" else run_net(tid, log, dry)


def in_the_way(tid, paths):
    """tasks whose routed copper lies within clearance of these paths"""
    t = TASKS[tid]
    w, c = (0.2, 0.15) if t["kind"] == "fan" else cls(t["net"])
    rad = int(math.ceil((max(w / 2, VIA_D / 2) + max(c, 0.2) + MARGIN) / G))
    mask = np.zeros((2, NY, NX), np.uint8)
    for pth in paths:
        for a, b in zip(pth, pth[1:] + [pth[-1]]):
            if a[0] != b[0]:
                for L in (F, B):
                    cv2.circle(mask[L], (a[2], a[1]), rad, 1, -1)
            else:
                cv2.line(mask[a[0]], (a[2], a[1]), (b[2], b[1]), 1, thickness=2 * rad + 1)
    hit = tmap[mask.astype(bool)]
    return {int(k) for k in np.unique(hit) if k >= 0} - {tid}


def is_led(ref):
    return ref.startswith("D") and ref[1:].isdigit() and int(ref[1:]) >= 100


# ------------------------------------------------------------------ plan
routed = {n: ps for n, ps in pads_by_net.items()
          if len(ps) >= 2 and n not in PLANE and not n.startswith(("unconnected", "nc-"))}
# the ring's VDD pads are joined by the LED side's VLED pour; route only the rest
routed["VLED"] = [p for p in routed["VLED"] if not is_led(p["ref"]) or (p["ref"], p["num"]) == ("D159", "4")]


def span(ps):
    return max(math.hypot(a["x"] - b["x"], a["y"] - b["y"]) for a in ps for b in ps)


def new(kind, net, **kw):
    tid = len(TASKS)
    TASKS[tid] = dict(kind=kind, net=net, items=[], path=[], done=False, **kw)
    return tid


links = [new("net", n, pads=routed[n], link=True) for n in sorted(routed)
         if all(is_led(p["ref"]) or p["ref"] in ("R11", "R12") for p in routed[n])]
linkset = {TASKS[t]["net"] for t in links}
power = [new("net", n, pads=routed[n]) for n in sorted(routed) if n in POWER]
signals = [new("net", n, pads=routed[n]) for n in sorted((n for n in routed if n not in linkset and n not in POWER),
                                                         key=lambda n: span(routed[n]))]


def in_gnd_pour(p):
    return B in p["cells"] and gnd_pour_b[int(round(ij(p['x'], p['y'])[1])), int(round(ij(p['x'], p['y'])[0]))]


# GND pads the LED side's inner pour already covers need no via of their own
# the IMU's supply pins: a local rail on the LED side to its own passives, and
# one via from the passive; seven vias round a 3 mm QFN do not fit
GROUPS = {"+3V3": ["U5", "C7", "R7", "R8"], "GND": ["U5", "C1", "C8", "R26"]}
local = []
grouped = set()
for n, refs in GROUPS.items():
    ps = [p for p in pads_by_net.get(n, []) if p["ref"] in refs and B in p["cells"]]
    if len(ps) >= 2:
        ps.sort(key=lambda p: p["ref"] == "U5")          # start from a passive
        local.append(new("net", n, pads=ps))
        grouped |= {id(p) for p in ps[1:]}
fans = [new("fan", n, pad=p) for n in ("GND", "+3V3") for p in pads_by_net.get(n, [])
        if id(p) not in grouped and (not is_led(p["ref"]) or (n == "GND" and B in p["cells"]))]
t0 = time.time()
print(f"{len(links)} LED links, {len(power)} power nets, {len(signals)} signals, {len(fans)} fan-outs")
link_fails = [tid for tid in links if not run(tid)]
print(f"links ({time.time()-t0:.0f} s): {len(link_fails)} failed")
order = power + local + signals + fans
fails = link_fails + [tid for tid in order if not run(tid)]
print(f"first pass ({time.time()-t0:.0f} s): {len(fails)} failed")
import copy  # noqa: E402
best = (len(fails), copy.deepcopy(tracks), copy.deepcopy(vias), list(fails))

for attempt in range(50):
    if not fails:
        break
    again = set()
    for tid in fails:
        if TASKS[tid]["done"]:
            continue
        # where it would go if nothing else were routed, and whose copper is there
        rip(tid)
        ideal = run(tid, log=False, dry=True)
        if ideal is None:
            print(f"  {TASKS[tid]['net']}: no route even with nothing else routed")
            again.add(tid)
            continue
        blockers = in_the_way(tid, ideal) - set(links)
        for b in blockers:
            rip(b)
        rip(tid)
        redo = [tid] + sorted(blockers | CASCADE, key=lambda b: (TASKS[b]["kind"] == "fan", b))
        CASCADE.clear()
        for b in redo:
            if not run(b, log=False):
                again.add(b)
    fails = sorted(again)
    if len(fails) < best[0]:
        best = (len(fails), copy.deepcopy(tracks), copy.deepcopy(vias), list(fails))
    print(f"rip-up pass {attempt + 1} ({time.time()-t0:.0f} s): {len(fails)} failed: "
          f"{[TASKS[t]['net'] + ('' if TASKS[t]['kind'] == 'net' else ' ' + TASKS[t]['pad']['ref']) for t in fails]}")

if best[0] < len(fails):
    # the last pass was worse than the best one: go back to the best
    print(f"restoring the best pass ({best[0]} failed)")
    for tid in list(TASKS):
        if TASKS[tid]["items"]:
            rip(tid)
    tracks[:] = []
    vias[:] = []
    for t in best[1]:
        if t:
            tracks.append(t)
    for v in best[2]:
        if v:
            vias.append(v)
    for t in tracks:
        net, layer, x1, y1, x2, y2, w = t
        m = np.zeros((NY, NX), np.uint8)
        a = tuple(int(round(v)) for v in ij(x1, y1))
        b = tuple(int(round(v)) for v in ij(x2, y2))
        cv2.line(m, a, b, 1, thickness=max(1, int(math.ceil(w / G)) | 1))
        L = LAYER[layer]
        owner[L][m.astype(bool) & (owner[L] < 0)] = NID[net]
    for v in vias:
        m = np.zeros((NY, NX), np.uint8)
        cv2.circle(m, tuple(int(round(u)) for u in ij(v[1], v[2])), int(round(VIA_D / 2 / G)), 1, -1)
        for L in (F, B):
            owner[L][m.astype(bool) & (owner[L] < 0)] = NID[v[0]]
    fails = best[3]

def broken():
    """tasks whose copper, as it now stands, does not connect what it should"""
    bad = []
    for tid, t in TASKS.items():
        if t["kind"] not in ("net", "fan") or not t["done"]:
            continue
        nid = NID[t["net"]]
        lab = []
        for L in (F, B):
            lab.append(ndimage.label(owner[L] == nid, structure=np.ones((3, 3)))[0])
        # union the two layers' pieces at this net's vias
        parent = {}

        def find(a):
            while parent.get(a, a) != a:
                a = parent[a]
            return a
        via_pieces = set()
        for v in vias:
            if v is None or v[0] != t["net"]:
                continue
            i, j = (int(round(u)) for u in ij(v[1], v[2]))
            a, b = (F, int(lab[F][j, i])), (B, int(lab[B][j, i]))
            if a[1] and b[1]:
                parent[find(a)] = find(b)
                via_pieces.add(find(b))
        via_roots = {find(p) for p in via_pieces}

        def piece(p):
            for L, m in p["cells"].items():
                k = int(np.max(lab[L][m]))
                if k:
                    return find((L, k))
            return None
        if t["kind"] == "net":
            roots = {piece(p) for p in t["pads"]}
            if len(roots) != 1 or None in roots:
                bad.append(tid)
        elif piece(t["pad"]) not in via_roots:
            bad.append(tid)
    return bad


for rep in range(5):
    bad = broken()
    print(f"connectivity check {rep + 1}: {len(bad)} tasks not connected: "
          f"{[TASKS[t]['net'] + ('' if TASKS[t]['kind'] == 'net' else ' ' + TASKS[t]['pad']['ref']) for t in bad]}")
    if not bad:
        break
    for tid in bad:
        rip(tid)
    for tid in bad:
        if not run(tid, log=True) and tid not in fails:
            fails.append(tid)

# GND stitching: a via on a 2 mm grid wherever one fits, tying the F.Cu pour
# and the LED side's inner GND pour to the In1 plane
stitch_tid = new("stitch", "GND")
n_stitch = 0
grid = [(x, y) for y in np.arange(-24, 23, 2.0) for x in np.arange(-23, 24, 2.0)] + \
    [(x, y) for y in np.arange(-15, 20, 1.0) for x in np.arange(-19, 16, 1.0)
     if math.hypot(x - RC[0], y - RC[1]) < RR - 1.5]
_, vok = free_maps(NID["GND"], 0.2, 0.2)
vok = vok.astype(np.uint8)
for xx, yy in grid:
    i, j = (int(round(v)) for v in ij(xx, yy))
    if not (0 <= i < NX and 0 <= j < NY):
        continue
    if vok[j, i] and owner[F][j, i] < 0 and owner[B][j, i] < 0:
        add_via(stitch_tid, "GND", i, j)
        cv2.circle(vok, (i, j), int(math.ceil((VIA_D + 0.15) / G)), 0, -1)     # hole to hole
        n_stitch += 1
print(f"{n_stitch} GND stitching vias")

# a radio-side GND pad whose fan-out found no room still has the F.Cu GND pour
pour_only = [t for t in fails if TASKS[t]["kind"] == "fan" and TASKS[t]["net"] == "GND"
             and (F in TASKS[t]["pad"]["cells"] or is_led(TASKS[t]["pad"]["ref"]))]
fails = [t for t in fails if t not in pour_only]
failed = [TASKS[t]["net"] + ("" if TASKS[t]["kind"] == "net" else f" fan-out {TASKS[t]['pad']['ref']}.{TASKS[t]['pad']['num']}")
          for t in fails]
json.dump({"tracks": [t for t in tracks if t], "vias": [v for v in vias if v], "failed": failed,
           "pour_only": [f"{TASKS[t]['pad']['ref']}.{TASKS[t]['pad']['num']}" for t in pour_only]},
          open(os.path.join(HERE, "routes.json"), "w"), indent=0)
print(f"{sum(1 for t in tracks if t)} tracks, {sum(1 for v in vias if v)} vias in {time.time()-t0:.0f} s; "
      f"failed: {failed}")

# a picture of the result, failed pads ringed, for looking at what went wrong
img = np.full((NY, NX, 3), 255, np.uint8)
img[dist_edge <= 0] = (200, 200, 200)
img[owner[B] >= 0] = (255, 170, 120)
img[owner[F] >= 0] = (90, 90, 230)
both = (owner[F] >= 0) & (owner[B] >= 0)
img[both] = (180, 60, 180)
img[padmask[F]] = (0, 0, 140)
img[padmask[B] & ~padmask[F]] = (140, 60, 0)
for v in vias:
    if v:
        i, j = ij(v[1], v[2])
        cv2.circle(img, (int(i), int(j)), 5, (0, 160, 0), -1)
for t in fails:
    ps = TASKS[t]["pads"] if TASKS[t]["kind"] == "net" else [TASKS[t]["pad"]]
    for p in ps:
        i, j = ij(p["x"], p["y"])
        cv2.circle(img, (int(i), int(j)), 18, (0, 200, 255), 3)
cv2.imwrite(os.path.join(HERE, "route.png"), img[::-1])
