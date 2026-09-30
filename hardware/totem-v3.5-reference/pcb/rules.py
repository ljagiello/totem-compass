"""Set the board's design rules and net classes in the KiCad project file.

The numbers are a mainstream 4-layer process's, with margin: JLCPCB's
4-layer minimums are 0.09 mm track/space, 0.15 mm drill, 0.2 mm copper to
a routed edge; this board uses 0.13 / 0.2 (0.45 mm vias) / 0.3.
"""
import json
import os

PRO = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "schematic", "totem.kicad_pro")
POWER = ["VBAT", "VSYS", "VBUS", "VLED"]
RULES = {
    "min_clearance": 0.13,
    "min_track_width": 0.13,
    "min_via_diameter": 0.45,
    "min_via_annular_width": 0.1,
    "min_through_hole_diameter": 0.2,
    "min_hole_clearance": 0.2,
    "min_hole_to_hole": 0.25,
    "min_copper_edge_clearance": 0.3,
    "min_connection": 0.13,
}


def main():
    pro = json.load(open(PRO))
    ds = pro["board"]["design_settings"]
    ds["rules"].update(RULES)
    ds["defaults"]["zones"]["min_clearance"] = 0.2
    ds["defaults"]["zones"]["min_thickness"] = 0.2
    ds["track_widths"] = [0.0, 0.13, 0.2, 0.4, 0.6]
    ds["via_dimensions"] = [{"diameter": 0.0, "drill": 0.0}, {"diameter": 0.45, "drill": 0.2}]
    ns = pro["net_settings"]
    default = ns["classes"][0]
    default.update({"clearance": 0.13, "track_width": 0.13, "via_diameter": 0.45, "via_drill": 0.2})
    power = dict(default)
    power.update({"name": "Power", "clearance": 0.2, "track_width": 0.4, "via_diameter": 0.45, "via_drill": 0.2,
                  "priority": 0})
    ns["classes"] = [default, power]
    ns["netclass_patterns"] = [{"netclass": "Power", "pattern": n} for n in POWER]
    json.dump(pro, open(PRO, "w"), indent=2)
    print("rules set:", RULES)


if __name__ == "__main__":
    main()
