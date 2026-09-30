"""Manufacturing outputs from the routed board, all with kicad-cli.

  fab/gerbers/        copper (4 layers), mask, paste, silk, outline; drill
                      files with a map; zipped as fab/totem-v3.5-gerbers.zip
  fab/totem-cpl.csv   pick-and-place in the column names assembly houses
                      (JLCPCB) read: Designator, Mid X, Mid Y, Layer, Rotation
  fab/totem-bom.csv   one line per part type: Comment, Designator, Footprint,
                      Quantity, plus the value and datasheet fields
  fab/render-*.png    3D renders of both sides, to compare with the photos

Run: python3 fab.py   (after build.py, route.py and apply.py)
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
    for row in csv.DictReader(fi):
        w.writerow([row["Ref"], f"{float(row['PosX']):.4f}mm", f"{float(row['PosY']):.4f}mm",
                    "Top" if row["Side"] == "top" else "Bottom", f"{float(row['Rot']):.2f}"])
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
    "100n": ("Samsung CL10B104KB8NNNC", "0603 X7R 50 V"),
    # the cell-sense divider's ratio is what the firmware's ADC calibration
    # expects (see ../README.md); 108k is an E192 value, and 0.1% keeps the
    # ratio within 0.2% so the firmware's 3.15 / 3.45 / 3.65 V steps hold
    "120k": ("Yageo RT0603BRD07120KL", "0603 0.1% thin film: cell-sense divider"),
    "108k": ("Yageo RT0603BRD07108KL", "0603 0.1% thin film: cell-sense divider"),
    "1u": ("Samsung CL10A105KB8NNNC", "0603 X5R 50 V"),
    "10u": ("Samsung CL21A106KAYNNNE", "0805 X5R 25 V"),
    "1N4148W": ("Diodes 1N4148W-7-F", ""),
    "XL-1515RGBC-WS2812B": ("XINGLIGHT XL-1515RGBC-WS2812B", "1.5 x 1.5 mm addressable RGB"),
    "BAT54W": ("Diodes BAT54W-7-F", ""),
    "red": ("Everlight 19-217/R6C-AL1M2VY/3T", "0603 red"),
    "USB4135-GF-A": ("GCT USB4135-GF-A", "power-only USB-C, 6 pin"),
    "LiPo 1000mAh": ("JST S2B-PH-SM4-TB(LF)(SN)", "battery connector; the cell is 1000 mAh 3.7 V"),
    "u.FL": ("Hirose U.FL-R-SMT-1(10)", "GNSS antenna"),
    "electret": ("CUI CMC-4013-SMT-TR", ""),
    "AO3401A": ("AOS AO3401A", ""),
    "2N7002": ("Nexperia 2N7002,215", ""),
    "power": ("C&K PTS645SH95SMTR92 LFS", "6x6 SMD tact, 9.5 mm; stem length to be matched to the rear cover"),
    "SOS": ("C&K PTS645SH95SMTR92 LFS", "6x6 SMD tact, 9.5 mm; stem length to be matched to the rear cover"),
    "touch spring": ("-", "conical contact spring on a 2.5 mm pad, height to suit the crystal; see README"),
    "ESP32-WROOM-32E": ("Espressif ESP32-WROOM-32E-N4", "4 MB flash, as the firmware image"),
    "MAX-M10S": ("u-blox MAX-M10S-00B", ""),
    "TP4056-42-ESOP8": ("NanJing Top Power TP4056-42-ESOP8", ""),
    "AP2112K-3.3": ("Diodes AP2112K-3.3TRG1", ""),
    "ICM-20948": ("TDK InvenSense ICM-20948", ""),
}
with open(os.path.join(OUT, "totem-bom.csv"), "w", newline="") as fo:
    w = csv.writer(fo)
    w.writerow(["Comment", "Designator", "Footprint", "Quantity", "MPN", "Note"])
    for (val, fp, ds), refs in sorted(groups.items(), key=lambda kv: kv[1][0]):
        if val in MPN:
            mpn, note = MPN[val]
        elif fp.split(":")[-1].startswith("R_0603"):
            mpn, note = "Yageo RC0603FR-07" + yageo(val) + "L", "0603 1% 100 mW"
        else:
            raise SystemExit(f"no part number for {val} ({fp})")
        w.writerow([val, ",".join(refs), fp.split(":")[-1], len(refs), mpn, note])

for side in ("top", "bottom"):
    cli("pcb", "render", "--output", os.path.join(OUT, f"render-{side}.png"), "--side", side,
        "--width", "1600", "--height", "1600", "--quality", "high", PCB)
print(f"gerbers: {len(os.listdir(GER))} files; placements: {n}; BOM lines: {len(groups)}; renders: 2")
