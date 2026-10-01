"""Manufacturing outputs from the routed board, all with kicad-cli.

  fab/gerbers/        copper (4 layers), mask, paste, silk, outline; drill
                      files with a map; the IPC-D-356 netlist for the fab's
                      electrical test; zipped as fab/totem-v3.5-gerbers.zip
  fab/totem-v3.5-odb.zip, -ipc2581.zip
                      the same board as ODB++ and IPC-2581, for fabs that
                      take one file instead of gerbers
  fab/totem-cpl.csv   pick-and-place in the column names assembly houses
                      (JLCPCB) read: Designator, Mid X, Mid Y, Layer, Rotation
  fab/totem-bom.csv   one line per part type: Comment, Designator, Footprint,
                      Quantity, Side, Manufacturer, MPN, Note
  fab/totem-offboard.csv
                      what is bought but not placed by the assembler
  fab/totem-v3.5-assembly-top.pdf, -bottom.pdf
                      assembly drawings (assembly_drawings.py)
  fab/totem-v3.5-schematic.pdf
  fab/totem-v3.5.step the assembled board, for fitting a case (models.py
                      supplies the parts KiCad has no model for)
  fab/totem-v3.5-board-stats.txt
  fab/render-*.png    3D renders of both sides, to compare with the photos
  fab/views/          copper, mask and silk of each side as SVG
  fab/totem-v3.5-assembly-package.zip
                      all of the above with ASSEMBLY.md: the one file to
                      hand to a fab and assembler

Run: python3 fab.py   (after build.py, route.py, apply.py and models.py)
"""
import csv
import os
import shutil
import subprocess
import zipfile
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.path.normpath(os.path.join(HERE, "..", "schematic"))
PCB = os.path.join(PROJ, "totem.kicad_pcb")
SCH = os.path.join(PROJ, "totem.kicad_sch")
OUT = os.path.join(HERE, "fab")
GER = os.path.join(OUT, "gerbers")


def cli(*args):
    r = subprocess.run(["kicad-cli", *args], capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"kicad-cli {' '.join(args[:3])} failed:\n{r.stdout}\n{r.stderr}")
    return r.stdout


shutil.rmtree(OUT, ignore_errors=True)
os.makedirs(GER)
layers = "F.Cu,In1.Cu,In2.Cu,B.Cu,F.Paste,B.Paste,F.Silkscreen,B.Silkscreen,F.Mask,B.Mask,Edge.Cuts"
cli("pcb", "export", "gerbers", "--output", GER + "/", "--layers", layers, "--subtract-soldermask",
    "--no-protel-ext", PCB)
cli("pcb", "export", "drill", "--output", GER + "/", "--format", "excellon", "--excellon-separate-th",
    "--generate-map", "--map-format", "gerberx2", PCB)
cli("pcb", "export", "ipcd356", "--output", os.path.join(GER, "totem-netlist.ipc"), PCB)
with zipfile.ZipFile(os.path.join(OUT, "totem-v3.5-gerbers.zip"), "w", zipfile.ZIP_DEFLATED) as z:
    for f in sorted(os.listdir(GER)):
        z.write(os.path.join(GER, f), f)

# pick and place
raw = os.path.join(OUT, "pos-raw.csv")
cli("pcb", "export", "pos", "--output", raw, "--format", "csv", "--units", "mm", "--side", "both",
    "--exclude-dnp", PCB)
with open(raw) as fi, open(os.path.join(OUT, "totem-cpl.csv"), "w", newline="") as fo:
    w = csv.writer(fo)
    w.writerow(["Designator", "Mid X", "Mid Y", "Layer", "Rotation"])
    n = 0
    side_of = {}
    for row in csv.DictReader(fi):
        side_of[row["Ref"]] = "Top" if row["Side"] == "top" else "Bottom"
        w.writerow([row["Ref"], f"{float(row['PosX']):.4f}mm", f"{float(row['PosY']):.4f}mm",
                    side_of[row["Ref"]], f"{float(row['Rot']):.2f}"])
        n += 1
os.remove(raw)

# bill of materials, grouped by value and footprint
rawb = os.path.join(OUT, "bom-raw.csv")
cli("sch", "export", "bom", "--output", rawb, "--fields", "Reference,Value,Footprint,Datasheet",
    "--labels", "Reference,Value,Footprint,Datasheet", "--group-by", "", SCH)
groups = defaultdict(list)
with open(rawb) as fi:
    for row in csv.DictReader(fi):
        groups[(row["Value"], row["Footprint"], row["Datasheet"])].append(row["Reference"])
os.remove(rawb)
# What to order for each value. Passives are generic parts in the stated
# package and rating; any equivalent will do. Parts with a note need a
# decision the photographs cannot make.
def yageo(v):
    """'5.1k' -> '5K1', '100k' -> '100K', '10M' -> '10M', '33' -> '33R'"""
    unit = {"k": "K", "M": "M"}.get(v[-1], "R")
    num = v[:-1] if v[-1] in "kM" else v
    return num.replace(".", unit) if "." in num else num + unit


MPN = {
    "100n": ("Samsung", "CL10B104KB8NNNC", "0603 X7R 50 V"),
    # the cell-sense divider's ratio is what the firmware's ADC calibration
    # expects (see ../README.md): 113k over 102k, both E96, is within 0.08%
    # of it (the reference board's 120k would need a 108.2k nobody makes), and 0.1% keeps the
    # ratio within 0.2% so the firmware's 3.15 / 3.45 / 3.65 V steps hold
    "113k": ("Yageo", "RT0603BRD07113KL", "0603 0.1% thin film: cell-sense divider"),
    "102k": ("Yageo", "RT0603BRD07102KL", "0603 0.1% thin film: cell-sense divider"),
    "1u": ("Samsung", "CL10A105KB8NNNC", "0603 X5R 50 V"),
    "10u": ("Samsung", "CL21A106KAYNNNE", "0805 X5R 25 V"),
    "1N4148W": ("Diodes Inc.", "1N4148W-7-F", ""),
    "XL-1515RGBC-WS2812B": ("XINGLIGHT", "XL-1515RGBC-WS2812B", "1.5 x 1.5 mm addressable RGB; moisture sensitive, bake per datasheet"),
    "BAT54W": ("Diodes Inc.", "BAT54W-7-F", ""),
    # Everlight 19-217 0603s were the first choice; DigiKey lists the red one
    # "Not Available" and the green "Discontinued" (checked 2026-09-30)
    "red": ("Lite-On", "LTST-C191KRKT", "0603 red"),
    "green": ("Lite-On", "LTST-C191KGKT", "0603 green"),
    "USB4135-GF-A": ("GCT", "USB4135-GF-A", "power-only USB-C, 6 pin"),
    "LiPo 1000mAh": ("JST", "S2B-PH-K-S(LF)(SN)", "battery connector, THT side entry; the cell is off-board (totem-offboard.csv)"),
    "u.FL": ("Hirose", "U.FL-R-SMT-1(10)", "GNSS antenna connector"),
    # the CMC-4013-SMT-TR is obsolete; the -2 is the current part, same 4 mm
    # can and -42 dB, and its pads fall inside this footprint's
    "electret": ("Same Sky (CUI Devices)", "CMC-4013-2-SMT-TR", ""),
    "AO3401A": ("Alpha & Omega", "AO3401A", ""),
    "2N7002": ("Nexperia", "2N7002,215", ""),
    "power": ("C&K", "PTS645SK95SMTR92 LFS", "6x6 SMD tact, 9.5 mm; stem length to be matched to the rear cover"),
    "SOS": ("C&K", "PTS645SK95SMTR92 LFS", "6x6 SMD tact, 9.5 mm; stem length to be matched to the rear cover"),
    "touch pogo pin": ("-", "-", "SMT spring-loaded (pogo) pin on a 2.5 mm pad, length to suit the crystal; hand-fitted, see ASSEMBLY.md"),
    "ESP32-WROOM-32E": ("Espressif", "ESP32-WROOM-32E-N4", "4 MB flash, as the firmware image"),
    "MAX-M10S": ("u-blox", "MAX-M10S-00B", "moisture sensitive, bake per u-blox before reflow"),
    "TP4056-42-ESOP8": ("NanJing Top Power", "TP4056-42-ESOP8", ""),
    "AP2112K-3.3": ("Diodes Inc.", "AP2112K-3.3TRG1", ""),
    "LSM6DSV16X": ("STMicroelectronics", "LSM6DSV16XTR", "6-axis IMU, LGA-14L; moisture sensitive"),
    "LIS2MDL": ("STMicroelectronics", "LIS2MDLTR", "3-axis magnetometer, LGA-12; keep it clear of magnetised tools and parts"),
    "220n": ("Samsung", "CL10B224KB8NNNC", "0603 X7R 50 V, low ESR: the LIS2MDL's set/reset capacitor"),
}


def natural(ref):
    """'R10' after 'R9', not before 'R2'."""
    head = ref.rstrip("0123456789")
    return head, int(ref[len(head):] or 0)


with open(os.path.join(OUT, "totem-bom.csv"), "w", newline="") as fo:
    w = csv.writer(fo)
    w.writerow(["Comment", "Designator", "Footprint", "Quantity", "Side", "Manufacturer", "MPN", "Note"])
    # one line per value and side: an assembler runs each side as its own job
    lines = []
    for (val, fp, ds), refs in groups.items():
        missing = [r for r in refs if r not in side_of]
        if missing:
            raise SystemExit(f"{val}: {missing} not in the placement file")
        for side in ("Bottom", "Top"):
            here = sorted((r for r in refs if side_of[r] == side), key=natural)
            if here:
                lines.append((side, val, fp, here))
    for side, val, fp, refs in sorted(lines, key=lambda l: (l[0], natural(l[3][0]))):
        if val in MPN:
            mfr, mpn, note = MPN[val]
        elif fp.split(":")[-1].startswith("R_0603"):
            mfr, mpn, note = "Yageo", "RC0603FR-07" + yageo(val) + "L", "0603 1% 100 mW"
        else:
            raise SystemExit(f"no part number for {val} ({fp})")
        w.writerow([val, ",".join(refs), fp.split(":")[-1], len(refs), side, mfr, mpn, note])

# Bought, but not placed by the assembler.
with open(os.path.join(OUT, "totem-offboard.csv"), "w", newline="") as fo:
    w = csv.writer(fo)
    w.writerow(["Item", "Quantity", "Specification", "Note"])
    w.writerows([
        ["Li-Po cell", 1, "3.7 V 1000 mAh, 1S, with protection, JST PH 2.0 mm 2-pin plug",
         "Check the plug's polarity against J2 before connecting: PH leads are not standardised"],
        ["GNSS antenna", 1, "L1 1575.42 MHz, U.FL (IPEX MHF1) plug, passive patch or flex",
         "The reference unit's antenna was not identified; its cable is visible in the photos"],
        ["Programming adapter", 1, "3.3 V USB-to-UART with DTR/RTS, pogo pins or wires to TP2-TP7",
         "The board has no USB data: see ASSEMBLY.md, Programming"],
        ["Enclosure, crystal, rear cover", 1, "from the reference unit", "Not part of this design"],
    ])

# The board in single-file formats, the 3D model, the documents.
cli("pcb", "export", "odb", "--output", os.path.join(OUT, "totem-v3.5-odb.zip"), PCB)
cli("pcb", "export", "ipc2581", "--output", os.path.join(OUT, "totem-v3.5-ipc2581.zip"), "--compress", PCB)
cli("pcb", "export", "step", "--output", os.path.join(OUT, "totem-v3.5.step"), "--subst-models", "--force", PCB)
cli("sch", "export", "pdf", "--output", os.path.join(OUT, "totem-v3.5-schematic.pdf"), SCH)
cli("pcb", "export", "stats", "--output", os.path.join(OUT, "totem-v3.5-board-stats.txt"), PCB)
KP = "/Applications/KiCad/KiCad.app/Contents/Frameworks/Python.framework/Versions/Current/bin/python3"
subprocess.run([KP, os.path.join(HERE, "assembly_drawings.py"), OUT], check=True, capture_output=True)

for side in ("top", "bottom"):
    cli("pcb", "render", "--output", os.path.join(OUT, f"render-{side}.png"), "--side", side,
        "--width", "1600", "--height", "1600", "--quality", "high", PCB)
VIEWS = os.path.join(OUT, "views")
os.makedirs(VIEWS)
for name, layers, extra in (("radio-side", "F.Cu,F.Silkscreen,F.Mask,Edge.Cuts", []),
                            ("led-side", "B.Cu,B.Silkscreen,B.Mask,Edge.Cuts", ["--mirror"])):
    cli("pcb", "export", "svg", "--output", os.path.join(VIEWS, name + ".svg"), "--layers", layers,
        "--page-size-mode", "2", "--exclude-drawing-sheet", *extra, PCB)

# One file to hand over: everything above, with the instructions.
PACKAGE = os.path.join(OUT, "totem-v3.5-assembly-package.zip")
with zipfile.ZipFile(PACKAGE, "w", zipfile.ZIP_DEFLATED) as z:
    z.write(os.path.join(HERE, "ASSEMBLY.md"), "ASSEMBLY.md")
    for f in sorted(os.listdir(OUT)):
        if f.endswith((".zip", ".xml", ".csv", ".pdf", ".step", ".txt", ".png")) and f != os.path.basename(PACKAGE):
            z.write(os.path.join(OUT, f), f)
    for f in sorted(os.listdir(VIEWS)):
        z.write(os.path.join(VIEWS, f), "views/" + f)
print(f"gerbers: {len(os.listdir(GER))} files; placements: {n}; BOM lines: {len(lines)}; "
      f"package: {os.path.getsize(PACKAGE) // 1024} KB")
