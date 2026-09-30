"""Make schematic/totem.pretty/CUI_CMC-4013-SMT_RingGap from KiCad's CUI_CMC-4013-SMT.

KiCad's footprint has the signal pad in the middle of a closed GND ring, so
nothing leaves it on its own layer: the only way out is a filled via in the
pad, a fab extra. Here the ring is a 290-degree arc instead, leaving a gap
at the footprint's local bottom (+y) 0.83 mm wide between the copper, for a
0.15 mm track with 0.15 mm either side. The microphone's ring still solders
on the rest; build.py turns the footprint so the gap faces free board.
"""
import math
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = "/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints/Sensor_Audio.pretty/CUI_CMC-4013-SMT.kicad_mod"
DST = os.path.join(HERE, "..", "schematic", "totem.pretty", "CUI_CMC-4013-SMT_RingGap.kicad_mod")
R, HALF_GAP = 1.375, 35.0
t = open(SRC).read()
t = t.replace('(footprint "CUI_CMC-4013-SMT"', '(footprint "CUI_CMC-4013-SMT_RingGap"', 1)
t = re.sub(r'\(descr "([^"]*)"\)',
           lambda m: '(descr "' + m.group(1) + '; GND ring opened 70 degrees at +y so the centre pad can be reached '
                     'on its own layer (hardware/totem-v3.5-reference/pcb/mic_footprint.py)")', t, count=1)


def p(a):
    a = math.radians(a)
    return f"{R * math.cos(a):.4f} {R + R * math.sin(a):.4f}"      # pad-local: the anchor is at (0, -R)


arc = (f"(gr_arc\n\t\t\t\t(start {p(90 + HALF_GAP)})\n\t\t\t\t(mid {p(270)})\n\t\t\t\t(end {p(90 - HALF_GAP + 360)})"
       f"\n\t\t\t\t(width 0.75)\n\t\t\t)")
old = re.search(r"\(gr_circle\s*\(center 0 1\.375\)\s*\(end 1\.375 1\.375\)\s*\(width 0\.75\)\s*\(fill no\)\s*\)", t)
assert old, "ring primitive not found"
t = t[:old.start()] + arc + t[old.end():]
open(DST, "w").write(t)
print("wrote", DST)
