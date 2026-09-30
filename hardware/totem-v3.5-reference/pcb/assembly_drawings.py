"""Assembly drawings: one PDF per side, every part's body outline (its fab
layer, which carries the pin-1 and polarity marks) with its reference
designator and nothing else, on the board outline.

The fab layer as the library draws it prints each part's value in large
type — 67 overlapping "XL-1515RGBC-WS2812B"s round the ring — and the ring
LEDs carry no reference there at all. So this works on a copy of the board:
every footprint's own fab text is hidden and one reference, sized to fit
the part, is added at its centre and turned to its long side. The LED side
is plotted mirrored, as it is seen when the board is turned over.

Run with KiCad's Python:  $KP assembly_drawings.py OUT_DIR
Writes OUT_DIR/totem-v3.5-assembly-top.pdf and -bottom.pdf.
"""
import math
import os
import subprocess
import sys
import tempfile

import pcbnew

HERE = os.path.dirname(os.path.abspath(__file__))
PCB = os.path.normpath(os.path.join(HERE, "..", "schematic", "totem.kicad_pcb"))
OUT = sys.argv[1]


def courtyard(fp, layer):
    """Width, height (mm) and the angle of the long side of the footprint's
    body, from its courtyard in its own frame."""
    xs, ys = [], []
    for g in fp.GraphicalItems():
        if g.GetLayer() == layer and g.GetClass() != "PCB_TEXT":
            bb = g.GetBoundingBox()
            xs += [bb.GetLeft(), bb.GetRight()]
            ys += [bb.GetTop(), bb.GetBottom()]
    if not xs:
        return 1.0, 1.0
    # the courtyard as placed is rotated with the part; undo that by
    # measuring in the footprint's frame through its orientation
    return pcbnew.ToMM(max(xs) - min(xs)), pcbnew.ToMM(max(ys) - min(ys))


def outside(fp, centre, gap):
    """A label spot for an LED too small to hold its own reference: gap mm
    past it, directly away from centre, and the radial angle to run along."""
    p = fp.GetPosition()
    dx, dy = pcbnew.ToMM(p.x - centre.x), pcbnew.ToMM(p.y - centre.y)
    r = math.hypot(dx, dy) or 1.0
    at = pcbnew.VECTOR2I(p.x + pcbnew.FromMM(dx / r * gap), p.y + pcbnew.FromMM(dy / r * gap))
    return at, -math.degrees(math.atan2(dy, dx))


def main():
    B = pcbnew.LoadBoard(PCB)
    # The ring's LEDs (D100-D159) are labelled outside the ring and the
    # crystal's (D200-D206) outside the crystal, each along its radius:
    # a 1.5 mm part cannot hold a legible "D123" under its own pads.
    ring = next(f for f in B.GetFootprints() if f.GetReference() == "D100")
    ring_centre = pcbnew.VECTOR2I(pcbnew.FromMM(100 - 2.019), pcbnew.FromMM(100 - 2.446))
    crystal_centre = next(f for f in B.GetFootprints() if f.GetReference() == "TP1").GetPosition()
    assert ring.IsFlipped()
    for fp in B.GetFootprints():
        back = fp.IsFlipped()
        fab = pcbnew.B_Fab if back else pcbnew.F_Fab
        # through the accessors: GetFields() hands out copies
        fp.Reference().SetVisible(False)
        fp.Value().SetVisible(False)
        # the library's own "${REFERENCE}" texts on the fab layer are plain
        # texts, which plot whatever their visibility says: blank them
        for g in fp.GraphicalItems():
            if g.GetClass() == "PCB_TEXT" and g.GetLayer() == fab:
                g.SetText("")
        # size from the unrotated footprint: measure a copy at 0 degrees
        probe = pcbnew.FOOTPRINT(fp)
        probe.SetOrientationDegrees(0)
        w, h = courtyard(probe, pcbnew.B_CrtYd if back else pcbnew.F_CrtYd)
        ref = fp.GetReference()
        long_side, short_side = max(w, h), min(w, h)
        size = max(0.25, min(1.2, 0.45 * short_side, 0.9 * long_side / max(len(ref), 1)))
        t = pcbnew.PCB_TEXT(B)
        t.SetText(ref)
        t.SetLayer(fab)
        t.SetTextSize(pcbnew.VECTOR2I(pcbnew.FromMM(size), pcbnew.FromMM(size)))
        t.SetTextThickness(pcbnew.FromMM(size * 0.15))
        angle = fp.GetOrientationDegrees() + (90 if h > w else 0)
        at = fp.GetPosition()
        if ref.startswith("D1") and len(ref) == 4 or ref.startswith("D2") and len(ref) == 4:
            centre = ring_centre if ref.startswith("D1") else crystal_centre
            at, angle = outside(fp, centre, 2.4)
            size = 0.55
            t.SetTextSize(pcbnew.VECTOR2I(pcbnew.FromMM(size), pcbnew.FromMM(size)))
            t.SetTextThickness(pcbnew.FromMM(size * 0.15))
        angle = (angle + 90) % 180 - 90          # keep it readable, never upside down
        t.SetTextAngleDegrees(angle)
        t.SetPosition(at)
        t.SetMirrored(back)
        B.Add(t)
    for side, layer, extra, what in (
            ("top", "F.Fab", [], "radio side (F): ESP32, GNSS, charger, connectors, buttons"),
            ("bottom", "B.Fab", ["--mirror"], "LED side (B), seen from that side: ring, crystal, IMU, mic, latch")):
        tb = B.GetTitleBlock()
        tb.SetTitle(f"Totem v3.5 reference: assembly, {side}")
        tb.SetRevision("1")
        tb.SetComment(0, what)
        tb.SetComment(1, "Pads sketched; pin 1 and polarity as marked on each outline")
        tb.SetComment(2, "Ring LEDs: DI/VDD pads face out; D100 by the buttons, CCW from LED side")
        with tempfile.TemporaryDirectory() as tmp:
            tmp_pcb = os.path.join(tmp, "totem.kicad_pcb")
            pcbnew.SaveBoard(tmp_pcb, B)
            out = os.path.join(OUT, f"totem-v3.5-assembly-{side}.pdf")
            subprocess.run(["kicad-cli", "pcb", "export", "pdf", "-o", out, "--layers", f"{layer},Edge.Cuts",
                            "--include-border-title", "--mode-single", "--scale", "2.4", "--black-and-white",
                            "--sketch-pads-on-fab-layers", *extra, tmp_pcb],
                           check=True, capture_output=True)
            print("wrote", out)


if __name__ == "__main__":
    main()
