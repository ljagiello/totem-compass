"""Record the DRC findings that are true of the reference board, with reasons.

Each one is written into the project file as KiCad's own exclusion (the
violation's key plus a comment), so the GUI and kicad-cli both show them as
excluded and anyone opening the board sees why. Only the pairs listed here
are excluded; anything else DRC finds still counts.
"""
import json
import os
import subprocess
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
PCB = os.path.join(HERE, "..", "schematic", "totem.kicad_pcb")
PRO = os.path.join(HERE, "..", "schematic", "totem.kicad_pro")
IU = 1e6                                       # KiCad internal units per mm

WHY = {
    ("courtyards_overlap", frozenset({"U1", "U3"})):
        "The TP4056 sits 0.3 mm below the ESP32 module's edge on the reference board (measured); "
        "only the module footprint's courtyard margin overlaps, the bodies and pads are clear.",
    ("courtyards_overlap", frozenset({"D3", "SW2"})):
        "The SOS LED sits between the SOS switch's gull-wing legs on the reference board (photo); "
        "it clears the switch body and pads, only the courtyards overlap.",
}


def main():
    out = tempfile.mktemp(suffix=".json")
    subprocess.run(["kicad-cli", "pcb", "drc", "--format", "json", "--output", out, PCB],
                   capture_output=True)
    rep = json.load(open(out))
    os.remove(out)
    pro = json.load(open(PRO))
    ex = []
    for v in rep.get("violations", []):
        refs = frozenset(i["description"].split()[-1] for i in v["items"]
                         if i["description"].startswith("Footprint"))
        why = WHY.get((v["type"], refs))
        if not why:
            continue
        a = v["items"][0]
        b = v["items"][1] if len(v["items"]) > 1 else a
        key = f'{v["type"]}|{round(a["pos"]["x"] * IU)}|{round(a["pos"]["y"] * IU)}|{a["uuid"]}|{b["uuid"]}'
        ex.append([key, why])
    pro["board"]["design_settings"]["drc_exclusions"] = ex
    json.dump(pro, open(PRO, "w"), indent=2)
    print(f"{len(ex)} exclusions written:", [e[0].split('|')[0] for e in ex])


if __name__ == "__main__":
    main()
