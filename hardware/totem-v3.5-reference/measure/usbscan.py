"""For every USB-C receptacle footprint in KiCad's library, report the
geometry the photo can be compared on: where the shell's through-hole tabs
sit relative to the signal-pad row (along the plug axis), and how far the
body reaches in front of the pad row.

Photo: tabs 2.5 and 5.0 mm in front of the pad row; body >= 8.7 mm.
"""
import glob
import re

rows = []
for f in sorted(glob.glob("/Applications/KiCad/KiCad.app/Contents/SharedSupport/footprints/"
                          "Connector_USB.pretty/USB_C_Receptacle*.kicad_mod")):
    t = open(f).read()
    pads = re.findall(r'\(pad "([^"]*)" (\w+) \w+\s*\(at ([-\d.]+) ([-\d.]+)', t)
    sig = [(float(x), float(y)) for n, k, x, y in pads if k == "smd" and re.match(r"[AB]\d+$", n)]
    sh = [(float(x), float(y)) for n, k, x, y in pads if n in ("S1", "SH", "13", "MP") and k == "thru_hole"]
    if not sig or not sh:
        continue
    # the plug axis is y in these footprints; the pad row is at the back
    row = sum(p[1] for p in sig) / len(sig)
    ahead = sorted({round(abs(p[1] - row), 2) for p in sh})
    fab = re.findall(r'\(fp_line\s*\(start ([-\d.]+) ([-\d.]+)\)\s*\(end ([-\d.]+) ([-\d.]+)\).*?\(layer "F.Fab"\)', t, re.S)
    ys = [float(v) for s in fab for v in (s[1], s[3])]
    body = max(abs(y - row) for y in ys) if ys else float("nan")
    name = f.rsplit("/", 1)[1][:-10]
    rows.append((name, ahead, body))
for name, ahead, body in rows:
    mark = " <==" if any(abs(a - 2.5) < 0.4 for a in ahead) and any(abs(a - 5.0) < 0.5 for a in ahead) else ""
    print(f"{name[:70]:70} tabs ahead {ahead}  body {body:.2f}{mark}")
