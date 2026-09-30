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
with open(os.path.join(OUT, "totem-bom.csv"), "w", newline="") as fo:
    w = csv.writer(fo)
    w.writerow(["Comment", "Designator", "Footprint", "Quantity", "Datasheet"])
    for (val, fp, ds), refs in sorted(groups.items(), key=lambda kv: kv[1][0]):
        w.writerow([val, ",".join(refs), fp.split(":")[-1], len(refs), ds])

for side in ("top", "bottom"):
    cli("pcb", "render", "--output", os.path.join(OUT, f"render-{side}.png"), "--side", side,
        "--width", "1600", "--height", "1600", "--quality", "high", PCB)
print(f"gerbers: {len(os.listdir(GER))} files; placements: {n}; BOM lines: {len(groups)}; renders: 2")
