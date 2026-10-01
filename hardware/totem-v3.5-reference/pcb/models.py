"""Give every part a 3D model, so the board's STEP export (fab.py) is
complete enough to check against a case.

KiCad's library has no model for two of the parts here:

  XL-1515 LEDs (67)  no model anywhere: a 1.55 x 1.5 x 0.65 mm box, the
                     datasheet's body (see the footprint's descr)
  MAX-M10S (U2)      the library's ublox_MAX.step is not installed: a
                     9.7 x 10.1 x 2.5 mm box, u-blox's package drawing

Everything else, the LSM6DSV16X's and LIS2MDL's LGAs and the THT JST
included, uses the model its library footprint names.

The boxes are envelopes for mechanical fit, not detailed models. They are
written as plain STEP solids to schematic/totem.3dshapes/ and referenced
through ${KIPRJMOD}, so the project carries them.

Run after apply.py, with KiCad's Python (like build.py); it saves the board
and puts the rules back (SaveBoard rewrites the project file).
"""
import os

import pcbnew

import rules

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.path.normpath(os.path.join(HERE, "..", "schematic"))
PCB = os.path.join(PROJ, "totem.kicad_pcb")
SHAPES = os.path.join(PROJ, "totem.3dshapes")
LIBS = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints"

# footprint name -> (library folder, body (w, h) in mm or None for the F.Fab outline, height mm)
BOXES = {
    "LED_XL-1515RGBC-WS2812B": (os.path.join(PROJ, "totem.pretty"), (1.55, 1.5), 0.65),
    "ublox_MAX": (os.path.join(LIBS, "RF_GPS.pretty"), (9.7, 10.1), 2.5),
}


def fab_box(lib, name):
    """The footprint's F.Fab outline, in its own frame (mm, y down)."""
    fp = pcbnew.FootprintLoad(lib, name)
    xs, ys = [], []
    for g in fp.GraphicalItems():
        if g.GetLayer() == pcbnew.F_Fab and g.GetClass() != "PCB_TEXT":
            bb = g.GetBoundingBox()
            xs += [pcbnew.ToMM(bb.GetLeft()), pcbnew.ToMM(bb.GetRight())]
            ys += [pcbnew.ToMM(bb.GetTop()), pcbnew.ToMM(bb.GetBottom())]
    return min(xs), min(ys), max(xs), max(ys)


def step_box(path, name, x0, y0, x1, y1, h):
    """Write an axis-aligned box as a single STEP solid (AP214)."""
    n = [0]
    lines = []

    def e(s):
        n[0] += 1
        lines.append(f"#{n[0]}={s};")
        return f"#{n[0]}"

    def pt(p):
        return e("CARTESIAN_POINT('',(%.4f,%.4f,%.4f))" % p)

    def dr(d):
        return e("DIRECTION('',(%.1f,%.1f,%.1f))" % d)

    corner = [(x0 if i & 1 == 0 else x1, y0 if i & 2 == 0 else y1, 0.0 if i & 4 == 0 else h) for i in range(8)]
    vtx = [e(f"VERTEX_POINT('',{pt(c)})") for c in corner]
    edges = {}
    for a, b in ((0, 1), (2, 3), (4, 5), (6, 7), (0, 2), (1, 3), (4, 6), (5, 7), (0, 4), (1, 5), (2, 6), (3, 7)):
        d = tuple(q - p for p, q in zip(corner[a], corner[b]))
        length = max(abs(c) for c in d)
        line = e(f"LINE('',{pt(corner[a])},{e(f'VECTOR({chr(39)}{chr(39)},{dr(tuple(c / length for c in d))},{length:.4f})')})")
        edges[(a, b)] = e(f"EDGE_CURVE('',{vtx[a]},{vtx[b]},{line},.T.)")
    # each face's corners counter-clockwise seen from outside, and its normal
    faces = [((0, 2, 3, 1), (0, 0, -1)), ((4, 5, 7, 6), (0, 0, 1)), ((0, 4, 6, 2), (-1, 0, 0)),
             ((1, 3, 7, 5), (1, 0, 0)), ((0, 1, 5, 4), (0, -1, 0)), ((2, 6, 7, 3), (0, 1, 0))]
    face_ids = []
    for cyc, nrm in faces:
        oes = []
        for a, b in zip(cyc, cyc[1:] + cyc[:1]):
            if (a, b) in edges:
                oes.append(e(f"ORIENTED_EDGE('',*,*,{edges[(a, b)]},.T.)"))
            else:
                oes.append(e(f"ORIENTED_EDGE('',*,*,{edges[(b, a)]},.F.)"))
        loop = e(f"EDGE_LOOP('',({','.join(oes)}))")
        bound = e(f"FACE_OUTER_BOUND('',{loop},.T.)")
        ref = (1.0, 0.0, 0.0) if nrm[0] == 0 else (0.0, 1.0, 0.0)
        plane = e(f"PLANE('',{e(f'AXIS2_PLACEMENT_3D({chr(39)}{chr(39)},{pt(corner[cyc[0]])},{dr(nrm)},{dr(ref)})')})")
        face_ids.append(e(f"ADVANCED_FACE('',({bound}),{plane},.T.)"))
    shell = e(f"CLOSED_SHELL('',({','.join(face_ids)}))")
    brep = e(f"MANIFOLD_SOLID_BREP('{name}',{shell})")
    mm = e("(LENGTH_UNIT() NAMED_UNIT(*) SI_UNIT(.MILLI.,.METRE.))")
    rad = e("(NAMED_UNIT(*) PLANE_ANGLE_UNIT() SI_UNIT($,.RADIAN.))")
    sr = e("(NAMED_UNIT(*) SI_UNIT($,.STERADIAN.) SOLID_ANGLE_UNIT())")
    unc = e(f"UNCERTAINTY_MEASURE_WITH_UNIT(LENGTH_MEASURE(1.E-07),{mm},'distance_accuracy_value','')")
    ctx = e(f"(GEOMETRIC_REPRESENTATION_CONTEXT(3) GLOBAL_UNCERTAINTY_ASSIGNED_CONTEXT(({unc})) "
            f"GLOBAL_UNIT_ASSIGNED_CONTEXT(({mm},{rad},{sr})) REPRESENTATION_CONTEXT('',''))")
    origin = e(f"AXIS2_PLACEMENT_3D('',{pt((0.0, 0.0, 0.0))},{dr((0, 0, 1))},{dr((1, 0, 0))})")
    rep = e(f"ADVANCED_BREP_SHAPE_REPRESENTATION('{name}',({brep},{origin}),{ctx})")
    ac = e("APPLICATION_CONTEXT('core data for automotive mechanical design processes')")
    e(f"APPLICATION_PROTOCOL_DEFINITION('international standard','automotive_design',2000,{ac})")
    pc = e(f"PRODUCT_CONTEXT('',{ac},'mechanical')")
    prod = e(f"PRODUCT('{name}','{name}','',({pc}))")
    pdf = e(f"PRODUCT_DEFINITION_FORMATION('','',{prod})")
    pdc = e(f"PRODUCT_DEFINITION_CONTEXT('part definition',{ac},'design')")
    pd = e(f"PRODUCT_DEFINITION('design','',{pdf},{pdc})")
    pds = e(f"PRODUCT_DEFINITION_SHAPE('','',{pd})")
    e(f"SHAPE_DEFINITION_REPRESENTATION({pds},{rep})")
    with open(path, "w") as f:
        f.write("ISO-10303-21;\nHEADER;\nFILE_DESCRIPTION(('envelope box for mechanical fit'),'2;1');\n"
                f"FILE_NAME('{os.path.basename(path)}','2026-09-30T00:00:00',(''),(''),'','','');\n"
                "FILE_SCHEMA(('AUTOMOTIVE_DESIGN { 1 0 10303 214 1 1 1 1 }'));\nENDSEC;\nDATA;\n")
        f.write("\n".join(lines))
        f.write("\nENDSEC;\nEND-ISO-10303-21;\n")


def main():
    os.makedirs(SHAPES, exist_ok=True)
    made = {}
    for name, (lib, body, h) in BOXES.items():
        fx0, fy0, fx1, fy1 = fab_box(lib, name)
        if body:                                   # the datasheet's body, on the outline's centre
            cx, cy = (fx0 + fx1) / 2, (fy0 + fy1) / 2
            fx0, fx1, fy0, fy1 = cx - body[0] / 2, cx + body[0] / 2, cy - body[1] / 2, cy + body[1] / 2
        # a model's y runs up, a footprint's down
        path = os.path.join(SHAPES, name + ".step")
        step_box(path, name, fx0, -fy1, fx1, -fy0, h)
        made[name] = "${KIPRJMOD}/totem.3dshapes/" + name + ".step"
        print(f"  {name}: {fx1 - fx0:.2f} x {fy1 - fy0:.2f} x {h} mm")

    B = pcbnew.LoadBoard(PCB)
    count = {}
    for fp in B.GetFootprints():
        name = fp.GetFPID().GetLibItemName().wx_str()
        if name not in made:
            continue
        path = made[name]
        # Replaced, not edited: iterating Models() hands out copies
        models = fp.Models()
        models.clear()
        m = pcbnew.FP_3DMODEL()
        m.m_Filename = path
        models.push_back(m)
        count[name] = count.get(name, 0) + 1
    pcbnew.SaveBoard(PCB, B)
    rules.main()
    print("models set:", count)


if __name__ == "__main__":
    main()
