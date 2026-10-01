"""Build the board: outline, footprints, nets and planes, from measured data.

Run with KiCad's own Python (it has pcbnew):
  /Applications/KiCad/KiCad.app/Contents/Frameworks/Python.framework/Versions/Current/bin/python3 build.py

Inputs: the schematic's netlist (exported by kicad-cli so the footprints
link back to their symbols), measure/outline2.json, placement.py.
Output: schematic/totem.kicad_pcb (the KiCad project), unrouted, and
pcb/board.json, the geometry the router works from.
"""
import json
import math
import os
import re
import subprocess
import sys

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.path.normpath(os.path.join(HERE, "..", "schematic"))
sys.path.insert(0, HERE)
import placement  # noqa: E402

KICAD_FP = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints"
LIBS = {"totem": os.path.join(PROJ, "totem.pretty")}
OX, OY = 100.0, 100.0          # board (0,0) sits at (100, 100) on the KiCad sheet


def mm(v):
    return pcbnew.FromMM(v)


def pt(x, y):
    """board mm (y up) -> KiCad internal units (y down)"""
    return pcbnew.VECTOR2I(mm(OX + x), mm(OY - y))


def board_xy(v):
    return (pcbnew.ToMM(v.x) - OX, OY - pcbnew.ToMM(v.y))


# ------------------------------------------------------------------ netlist
def sexp(text):
    toks = re.findall(r'\(|\)|"(?:[^"\\]|\\.)*"|[^\s()]+', text)
    stack = [[]]
    for t in toks:
        if t == "(":
            stack.append([])
        elif t == ")":
            last = stack.pop()
            stack[-1].append(last)
        else:
            stack[-1].append(t[1:-1] if t.startswith('"') else t)
    return stack[0][0]


def find(node, key):
    return [c for c in node if isinstance(c, list) and c and c[0] == key]


netfile = os.path.join(HERE, "totem_sheet.net")
subprocess.run(["kicad-cli", "sch", "export", "netlist", "--format", "kicadsexpr", "--output", netfile,
                os.path.join(PROJ, "totem.kicad_sch")], check=True, capture_output=True)
N = sexp(open(netfile).read())
comps = {}
for c in find(find(N, "components")[0], "comp"):
    ref = find(c, "ref")[0][1]
    comps[ref] = {"value": find(c, "value")[0][1], "footprint": find(c, "footprint")[0][1],
                  "tstamp": find(c, "tstamps")[0][1] if find(c, "tstamps") else None}
nets = {}
for n in find(find(N, "nets")[0], "net"):
    name = find(n, "name")[0][1]
    nets[name] = [(find(nd, "ref")[0][1], find(nd, "pin")[0][1]) for nd in find(n, "node")]

missing = sorted(set(comps) - set(placement.P))
extra = sorted(set(placement.P) - set(comps))
if missing or extra:
    sys.exit(f"placement and schematic disagree: unplaced {missing}, unknown {extra}")

# ------------------------------------------------------------------ board
B = pcbnew.BOARD()
B.SetCopperLayerCount(4)
ds = B.GetDesignSettings()
ds.SetBoardThickness(mm(1.6))

netinfo = {}
for name in sorted(nets):
    ni = pcbnew.NETINFO_ITEM(B, name)
    B.Add(ni)
    netinfo[name] = ni
pin_net = {(r, p): n for n, nodes in nets.items() for r, p in nodes}

# outline
outline = json.load(open(os.path.join(HERE, "..", "measure", "outline2.json")))["segments"]
for s in outline:
    if s["t"] == "line":
        sh = pcbnew.PCB_SHAPE(B, pcbnew.SHAPE_T_SEGMENT)
        sh.SetStart(pt(*s["a"]))
        sh.SetEnd(pt(*s["b"]))
    else:
        sh = pcbnew.PCB_SHAPE(B, pcbnew.SHAPE_T_ARC)
        sh.SetArcGeometry(pt(*s["a"]), pt(*s["m"]), pt(*s["b"]))
    sh.SetLayer(pcbnew.Edge_Cuts)
    sh.SetWidth(mm(0.1))
    B.Add(sh)


def load(fpid):
    lib, name = fpid.split(":")
    path = LIBS.get(lib, os.path.join(KICAD_FP, lib + ".pretty"))
    fp = pcbnew.FootprintLoad(path, name)
    if fp is None:
        sys.exit(f"footprint {fpid} not found")
    fp.SetFPID(pcbnew.LIB_ID(lib, name))
    return fp


def pads_board(fp):
    return {p.GetNumber(): board_xy(p.GetPosition()) for p in fp.Pads()}


def dist(a, b):
    return math.hypot(a[0] - b[0], a[1] - b[1])


FP = {}
for ref in sorted(comps, key=lambda r: (re.sub(r"\d", "", r), int(re.sub(r"\D", "", r) or 0))):
    c = comps[ref]
    pl = placement.P[ref]
    fp = load(c["footprint"])
    fp.SetReference(ref)
    fp.SetValue(c["value"])
    if c["tstamp"]:
        fp.SetPath(pcbnew.KIID_PATH("/" + c["tstamp"]))
    B.Add(fp)
    fp.SetPosition(pt(pl["x"], pl["y"]))
    if pl["side"] == "B":
        fp.Flip(fp.GetPosition(), pcbnew.FLIP_DIRECTION_LEFT_RIGHT)
    fp.SetOrientationDegrees(pl["rot"])
    for pad in fp.Pads():
        n = pin_net.get((ref, pad.GetNumber()))
        if n:
            pad.SetNet(netinfo[n])
    FP[ref] = fp

# The ESP32 footprint's exposed pad comes with a 3x5 grid of 0.2 mm vias.
# They would come out on the LED side beside a crystal LED, and the pad is
# grounded through the radio side's pour and pins 1/15/38 anyway.
u1 = FP["U1"]
for p in list(u1.Pads()):
    if p.GetNumber() == "39" and p.GetAttribute() == pcbnew.PAD_ATTRIB_PTH:
        u1.Remove(p)
# Its antenna keepout starts 0.1 mm into the ring's top three LEDs, which the
# reference board has there too; raise its lower edge 0.22 mm so the LEDs
# stay while every plane and pour is still kept out from under the antenna.
for z in u1.Zones():
    ol = z.Outline()
    for i in range(ol.FullPointCount()):
        v = ol.CVertex(i)
        if board_xy(v)[1] < 22.0:
            ol.SetVertex(i, pcbnew.VECTOR2I(v.x, v.y - mm(0.22)))

# ------------------------------------------------------------ legalise
# Photo-measured parts stay put; this design's own small parts are pushed
# apart where their courtyards overlap, and back inside the outline.
FIXED = {"U1", "U2", "U3", "U4", "U5", "U6", "J1", "J2", "J3", "SW1", "SW2", "D3", "Q4", "TP1", "MK1", "C16", "C15",
         "D4", "D5", "R27", "R28"} | \
    {r for r in placement.P if re.fullmatch(r"D(1\d\d|2\d\d)", r)}
poly = json.load(open(os.path.join(HERE, "..", "measure", "outline2_poly.json")))


def inside(x, y):
    c = False
    for (x1, y1), (x2, y2) in zip(poly, poly[1:] + poly[:1]):
        if (y1 > y) != (y2 > y) and x < x1 + (y - y1) * (x2 - x1) / (y2 - y1):
            c = not c
    return c


def edge_ok(x, y, m=0.35):
    return all(inside(x + dx, y + dy) for dx in (-m, 0, m) for dy in (-m, 0, m))


def cboxes(fp):
    """courtyard as boxes: one per outline, so U1's T (module + antenna band) stays a T"""
    layer = pcbnew.B_CrtYd if fp.IsFlipped() else pcbnew.F_CrtYd
    cy = fp.GetCourtyard(layer)
    if fp.GetReference() == "U1":
        x, y = board_xy(fp.GetPosition())
        return [(x - 9.75, y - 13.54, x + 9.75, y + 6.31), (x - 24.25, y + 6.31, x + 24.25, y + 28)]
    bb = cy.BBox()
    x0, y0 = board_xy(pcbnew.VECTOR2I(bb.GetLeft(), bb.GetBottom()))
    x1, y1 = board_xy(pcbnew.VECTOR2I(bb.GetRight(), bb.GetTop()))
    return [(x0, y0, x1, y1)]


def cbox(fp):
    bs = cboxes(fp)
    return (min(b[0] for b in bs), min(b[1] for b in bs), max(b[2] for b in bs), max(b[3] for b in bs))


def overlap(ra, rb, boxes):
    """largest overlapping box pair between two parts: (ox, oy, ca, cb) or None"""
    best = None
    for A in boxes[ra]:
        for Bb in boxes[rb]:
            ox = min(A[2], Bb[2]) - max(A[0], Bb[0])
            oy = min(A[3], Bb[3]) - max(A[1], Bb[1])
            if ox > 0 and oy > 0 and (best is None or min(ox, oy) > min(best[0], best[1])):
                best = (ox, oy, ((A[0] + A[2]) / 2, (A[1] + A[3]) / 2), ((Bb[0] + Bb[2]) / 2, (Bb[1] + Bb[3]) / 2))
    return best


def move(fp, dx, dy):
    x, y = board_xy(fp.GetPosition())
    fp.SetPosition(pt(x + dx, y + dy))


for it in range(400):
    moved = 0
    refs = list(FP)
    boxes = {r: cboxes(FP[r]) for r in refs}
    for i, a in enumerate(refs):
        for b in refs[i + 1:]:
            if FP[a].IsFlipped() != FP[b].IsFlipped() or (a in FIXED and b in FIXED):
                continue
            o = overlap(a, b, boxes)
            if o is None:
                continue
            ox, oy, ca, cb = o
            if ox < oy:
                v = ((ox + 0.05) * (1 if cb[0] >= ca[0] else -1), 0)
            else:
                v = (0, (oy + 0.05) * (1 if cb[1] >= ca[1] else -1))
            mov = [r for r in (a, b) if r not in FIXED]
            share = 1.0 / len(mov)
            for r in mov:
                s = share if r == b else -share
                move(FP[r], v[0] * s, v[1] * s)
                boxes[r] = cboxes(FP[r])
            moved += 1
    for r in FP:
        if r in FIXED:
            continue
        x0, y0, x1, y1 = cbox(FP[r])
        for cx, cy in ((x0, y0), (x1, y0), (x0, y1), (x1, y1)):
            if not edge_ok(cx, cy, 0.3):
                x, y = board_xy(FP[r].GetPosition())
                n = math.hypot(x, y) or 1
                move(FP[r], -0.1 * x / n, -0.1 * y / n)
                moved += 1
                break
    if not moved:
        break
still = []
boxes = {r: cboxes(FP[r]) for r in FP}
LEDS = {r for r in FP if re.fullmatch(r"D(1\d\d|2\d\d)", r)}
for i, a in enumerate(FP):
    for b in list(FP)[i + 1:]:
        if FP[a].IsFlipped() != FP[b].IsFlipped() or (a in LEDS and b in LEDS):
            continue            # LED pairs: boxes of rotated squares overlap; DRC checks the real outlines
        o = overlap(a, b, boxes)
        if o and o[0] > 0.01 and o[1] > 0.01:
            still.append((a, b))
print(f"legalised in {it + 1} passes; courtyard overlaps left: {still}")
for r in sorted(FP):
    if r not in FIXED:
        x, y = board_xy(FP[r].GetPosition())
        d = math.hypot(x - placement.P[r]["x"], y - placement.P[r]["y"])
        if d > 0.8:
            print(f"  {r} moved {d:.2f} mm from where placement.py put it")


def choose(fp, base, ok, score=None):
    """try base + 0/90/180/270 and keep the first rotation whose pads pass ok()"""
    best = None
    for k in range(4):
        fp.SetOrientationDegrees((base + 90 * k) % 360)
        pads = pads_board(fp)
        if ok(pads):
            s = score(pads) if score else 0
            if best is None or s < best[0]:
                best = (s, (base + 90 * k) % 360)
    if best is None:
        sys.exit(f"{fp.GetReference()}: no rotation satisfies its pin rule")
    fp.SetOrientationDegrees(best[1])


# ring: DI (3) and VDD (4) on the outer edge, DO (1) toward the next LED
RC = placement.RC
ring = [f"D{100 + k}" for k in range(60)]
for k, ref in enumerate(ring):
    nxt = placement.P[ring[(k + 1) % 60]]
    nxt = (nxt["x"], nxt["y"])
    r = lambda p: dist(p, RC)
    best = None
    for a in range(360):              # whole degrees: the ring's LEDs sit every 6
        FP[ref].SetOrientationDegrees(a)
        p = pads_board(FP[ref])
        radial = (r(p["3"]) - r(p["1"])) + (r(p["4"]) - r(p["2"]))   # 2.4 when exactly radial
        if radial < 2.3:
            continue
        s = -radial + dist(p["1"], nxt)
        if best is None or s < best[0]:
            best = (s, a)
    if best is None:
        sys.exit(f"{ref}: no rotation puts DI and VDD on the ring's outer edge")
    FP[ref].SetOrientationDegrees(best[1])
# crystal: DI toward the previous LED (D200's toward R12), DO toward the next
cry = [f"D{200 + i}" for i in range(7)]
for i, ref in enumerate(cry):
    prev = (placement.P["R12"]["x"], placement.P["R12"]["y"]) if i == 0 else \
        (placement.P[cry[i - 1]]["x"], placement.P[cry[i - 1]]["y"])
    nxt = (placement.P[cry[i + 1]]["x"], placement.P[cry[i + 1]]["y"]) if i < 6 else None
    best = None
    for a in range(0, 360, 5):
        FP[ref].SetOrientationDegrees(a)
        p = pads_board(FP[ref])
        s = dist(p["3"], prev) + (dist(p["1"], nxt) if nxt else 0)
        if best is None or s < best[0]:
            best = (s, a)
    FP[ref].SetOrientationDegrees(best[1])
# Motion sensor and magnetometer: each with pin 1 at its upper-left seen from
# the radio side and pins 1..4 running along its top edge (on the LED side the
# package is seen mirrored, which puts the pin-1 column along the top rather
# than down the left), so the two sit square to each other and to the board.
# Their axes in the board's frame follow from that and ST's pin-1 drawings
# (README, The board).
for ref in ("U5", "U6"):
    c = (placement.P[ref]["x"], placement.P[ref]["y"])
    choose(FP[ref], 0, lambda p, c=c: p["1"][0] < c[0] - 0.4 and p["1"][1] > c[1] + 0.4
           and abs(p["4"][1] - p["1"][1]) < 0.2 and p["4"][0] > p["1"][0] + 1.2)

# microphone: round, so its turn is free; point its ring's gap (see
# mic_footprint.py) down and toward the edge, where the radio side has room
# for the signal's via
mk = FP["MK1"]
cmk = board_xy(mk.GetPosition())
ring_pad = [p for p in mk.Pads() if p.GetNumber() == "1"][0]
best = None
for a in range(0, 360, 5):
    mk.SetOrientationDegrees(a)
    gap = [t for t in range(0, 360, 5)
           if not ring_pad.HitTest(pt(cmk[0] + 1.375 * math.cos(math.radians(t)),
                                      cmk[1] + 1.375 * math.sin(math.radians(t))), 0)]
    if not gap:
        continue
    gx = sum(math.cos(math.radians(t)) for t in gap)
    gy = sum(math.sin(math.radians(t)) for t in gap)
    err = abs((math.degrees(math.atan2(gy, gx)) - (-115.0) + 180) % 360 - 180)
    if best is None or err < best[0]:
        best = (err, a)
if best is None:
    sys.exit("MK1: its ring shows no gap")
mk.SetOrientationDegrees(best[1])
print(f"MK1 turned {best[1]} degrees: ring gap within {best[0]:.0f} degrees of the target")

# ------------------------------------------------------------------ planes
def zone(net, layer, poly, prio=0, full=True, name=None, minw=0.2):
    z = pcbnew.ZONE(B)
    z.SetLayer(layer)
    z.SetNetCode(netinfo[net].GetNetCode())
    ol = z.Outline()
    ol.NewOutline()
    for x, y in poly:
        v = pt(x, y)
        ol.Append(v.x, v.y)
    z.SetAssignedPriority(prio)
    z.SetMinThickness(mm(minw))
    z.SetLocalClearance(mm(0.2))
    z.SetPadConnection(pcbnew.ZONE_CONNECTION_FULL if full else pcbnew.ZONE_CONNECTION_THERMAL)
    if name:
        z.SetZoneName(name)
    B.Add(z)
    return z


def circle(cx, cy, r, n=120):
    return [(cx + r * math.cos(2 * math.pi * i / n), cy + r * math.sin(2 * math.pi * i / n)) for i in range(n)]


def annulus(cx, cy, r0, r1, n=120):
    """a ring as one polygon: out around r1, back around r0, joined by a slit"""
    outer = circle(cx, cy, r1, n) + [(cx + r1, cy)]
    inner = [(cx + r0, cy)] + circle(cx, cy, r0, n)[::-1]
    return outer + inner


BIG = [(-30, -30), (30, -30), (30, 30), (-30, 30)]
zone("GND", pcbnew.In1_Cu, BIG, 0, name="GND plane")
zone("+3V3", pcbnew.In2_Cu, BIG, 0, name="3V3 plane")
# LED side: VLED outside the ring (the outer pads), GND inside (the inner pads)
zone("VLED", pcbnew.B_Cu, annulus(RC[0], RC[1], placement.RR + 0.35, 30), 2, name="VLED ring")
zone("GND", pcbnew.B_Cu, circle(RC[0], RC[1], placement.RR - 0.35), 1, name="GND inside ring", minw=0.25)  # no 0.2 mm necks between LEDs
zone("GND", pcbnew.F_Cu, BIG, 0, name="GND radio side")

# References: 0603s packed 0.3 mm apart and 67 LEDs 0.4 mm apart leave no room
# for legible silkscreen text that clears the pads. The fab layers keep every
# reference (the assembly drawing and the placement file come from them); the
# silkscreen keeps the part outlines and pin-1 marks.
for ref, fp in FP.items():
    fp.Reference().SetVisible(False)
# the touch post's pad carries a spring that has to be bought and placed;
# KiCad's test-point footprints default to leaving the bill of materials
FP["TP1"].SetExcludedFromBOM(False)
FP["TP1"].SetExcludedFromPosFiles(False)

out = os.path.join(PROJ, "totem.kicad_pcb")
pcbnew.SaveBoard(out, B)
import rules  # noqa: E402  (SaveBoard rewrites the project file; put the rules back)
rules.main()

# geometry for the router
g = {"outline": outline, "pads": [], "nets": sorted(nets), "ring_centre": RC, "ring_R": placement.RR}
for ref, fp in FP.items():
    for p in fp.Pads():
        pos = board_xy(p.GetPosition())
        layers = [l for l in ("F.Cu", "B.Cu") if p.IsOnLayer(pcbnew.F_Cu if l == "F.Cu" else pcbnew.B_Cu)]
        polys = {}
        for l in layers:
            sps = p.GetEffectivePolygon(pcbnew.F_Cu if l == "F.Cu" else pcbnew.B_Cu, pcbnew.ERROR_INSIDE)
            ol = sps.Outline(0)
            polys[l] = [board_xy(ol.CPoint(k)) for k in range(ol.PointCount())]
        g["pads"].append({
            "ref": ref, "num": p.GetNumber(), "net": p.GetNetname(),
            "x": pos[0], "y": pos[1], "layers": layers, "poly": polys,
            "drill": pcbnew.ToMM(p.GetDrillSize().x),
        })
def zpoly(z):
    ol = z.Outline().Outline(0)
    return [board_xy(ol.CPoint(k)) for k in range(ol.PointCount())]


g["zones"] = [{"net": z.GetNetname(), "layer": B.GetLayerName(z.GetLayer()), "name": z.GetZoneName(),
               "prio": z.GetAssignedPriority(), "poly": zpoly(z)} for z in B.Zones()]
g["keepouts"] = [{"ref": fp.GetReference(), "poly": zpoly(z),
                  "layers": [l for l in ("F.Cu", "B.Cu") if z.IsOnLayer(pcbnew.F_Cu if l == "F.Cu" else pcbnew.B_Cu)],
                  "tracks": z.GetDoNotAllowTracks(), "vias": z.GetDoNotAllowVias()}
                 for fp in FP.values() for z in fp.Zones() if z.GetIsRuleArea()]
json.dump(g, open(os.path.join(HERE, "board.json"), "w"), indent=0)
print(f"{len(FP)} footprints, {len(nets)} nets -> {out}")
