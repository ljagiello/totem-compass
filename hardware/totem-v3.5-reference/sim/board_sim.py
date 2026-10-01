#!/usr/bin/env python3
"""Simulate the whole board, built from the schematic, in every operating case.

KiCad exports a netlist from totem.kicad_sch and the deck is built from it,
so every part, net and value comes off the drawing. Discrete semiconductors
are SPICE device models; the integrated circuits are behavioural models of
what they present to the circuit (sim/board_models.lib).

The power latch is the part of this design that was designed rather than
recovered, so it is exercised in the time domain, through the sequence the
firmware actually puts it through, at a full cell, a low cell and the
2N7002's worst-case threshold:

   off -> press -> on -> press while running -> ring on -> ring off
   -> firmware off -> press -> on -> hold, firmware off while held
   -> release -> off

Then the steady-state checks: the rail under load across the whole cell
range, the sense dividers, and charging.

Usage: python3 board_sim.py            exit 1 if anything fails
"""

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
HW = ROOT / "hardware/totem-v3.5-reference"
SCH = HW / "schematic/totem.kicad_sch"
MODELS = HW / "sim/board_models.lib"
OUT = HW / "sim/board.cir"

sys.path.insert(0, str(HW / "schematic"))
from kicad_sexp import children, parse, sval  # noqa: E402

# Symbol name -> (model, pin NUMBERS in the model's port order). Numbers,
# because a netlist keys pins by number; getting that wrong is silent —
# every lookup misses and the part is shorted to ground without a word.
MODEL = {
    "ESP32-WROOM-32E": ("ESP32", ["2", "1", "3", "26", "31", "25"]),
    "MAX-M10S": ("MAXM10S", ["8", "1"]),
    "LSM6DSM": ("LSM6DSV16X", ["8", "6"]),       # the LSM6DSV16X, on the LSM6DSM symbol
    "LIS2MDL": ("LIS2MDL", ["9", "6"]),
    "TP4056-42-ESOP8": ("TP4056", ["1", "2", "3", "4", "5", "6", "7", "8", "9"]),
    "AP2112K-3.3": ("AP2112K", ["1", "2", "3", "4", "5"]),
    "AO3401A": ("AO3401A", ["1", "2", "3"]),
    "2N7002": ("N2N7002", ["1", "2", "3"]),
    "1N4148W": ("D1N4148W", ["1", "2"]),
    "BAT54W": ("BAT54W", ["1", "2", "3"]),
    "LED": ("LED_IND", ["1", "2"]),
    "Microphone_Condenser": ("ELECTRET", ["1", "2"]),
}
BY_REF = {"SW1": ("SW_PWR", ["1", "2"]), "SW2": ("SW_SOS", ["1", "2"])}


def spice_node(name):
    if name in ("GND", "/GND"):
        return "0"
    return re.sub(r"[^A-Za-z0-9_]", "_", name.lstrip("/"))


def spice_value(v):
    """KiCad's value into SPICE's dialect.

    The one that bites: in SPICE a trailing M is MILLI. The latch's 10M and
    1M resistors would become 10 milliohms and 1 milliohm, the latch would
    short its own hold node, and the simulation would report a circuit that
    cannot latch — for a reason that is entirely in the translation.
    """
    v = v.strip()
    m = re.fullmatch(r"(\d+)([kKmMRr])(\d+)", v)          # 4k7, 1M5, 330R
    if m:
        unit = {"k": "k", "K": "k", "M": "Meg", "m": "m", "R": "", "r": ""}[m.group(2)]
        return f"{m.group(1)}.{m.group(3)}{unit}"
    m = re.fullmatch(r"(\d+(?:\.\d+)?)[Rr]", v)
    if m:
        return m.group(1)
    m = re.fullmatch(r"(\d+(?:\.\d+)?)M", v)
    if m:
        return f"{m.group(1)}Meg"
    return v


def netlist():
    tmp = HW / "sim/.netlist.net"
    run = subprocess.run(["kicad-cli", "sch", "export", "netlist", "--format", "kicadsexpr",
                          "--output", str(tmp), str(SCH)], capture_output=True, text=True, timeout=300)
    if run.returncode or not tmp.exists():
        raise SystemExit(f"could not export a netlist: {run.stderr.strip()[:300]}")
    tree = parse(tmp.read_text())[0]
    tmp.unlink()
    comps = {}
    for block in children(tree, "components"):
        for comp in children(block, "comp"):
            ref = sval(children(comp, "ref")[0])
            val = children(comp, "value")
            lib = children(comp, "libsource")
            part = sval(children(lib[0], "part")[0]) if lib and children(lib[0], "part") else ""
            comps[ref] = {"value": sval(val[0]) if val else "", "part": part, "pins": {}}
    for block in children(tree, "nets"):
        for n in children(block, "net"):
            name = sval(children(n, "name")[0])
            for node in children(n, "node"):
                ref = sval(children(node, "ref")[0])
                if ref in comps:
                    comps[ref]["pins"][sval(children(node, "pin")[0])] = name
    return comps


def deck(comps):
    L = [
        "* The Totem reference design, built from totem.kicad_sch by board_sim.py.",
        f'.include "{MODELS}"',
        "",
        "* the cell, and the USB supply (off unless an analysis plugs it in)",
        "VCELL VBATX 0 dc 3.8",
        "RCELL VBATX VBAT 0.15",
        "VUSB VBUSX 0 dc 0",
        "RUSB VBUSX VBUS 0.1",
        "",
        "* what the bench does, in time: see the module docstring",
        "VPRESS PRESS_PWR 0 dc 0 PWL(0 0 0.1 0 0.101 1 0.4 1 0.401 0  0.8 0 0.801 1 1.0 1 1.001 0"
        "  2.4 0 2.401 1 2.7 1 2.701 0  3.0 0 3.001 1 4.0 1 4.001 0)",
        "VFW FW_OFF 0 dc 0 PWL(0 0 1.9 0 1.901 1 2.2 1 2.201 0  3.5 0 3.501 1 4.5 1)",
        "VRING RING_ON 0 dc 0 PWL(0 0 1.3 0 1.301 1 1.6 1 1.601 0)",
        "VSOS PRESS_SOS 0 0",
        "VRADIO RADIO 0 0",
        "",
    ]
    for ref in sorted(comps, key=lambda r: (re.sub(r"\d", "", r), int(re.sub(r"\D", "", r) or 0))):
        c = comps[ref]
        pins, part, value = c["pins"], c["part"], c["value"]
        if not pins:
            continue
        if ref[0] in "RC" and part in ("R", "C") and len(pins) == 2:
            a, b = (spice_node(pins[p]) for p in sorted(pins))
            L.append(f"{ref} {a} {b} {spice_value(value)}")
        elif ref in BY_REF or part in MODEL or part == "WS2812B-2020":
            if ref in BY_REF:
                model, order = BY_REF[ref]
            elif part == "WS2812B-2020":
                model = "WS2812_CRY" if ref.startswith("D2") else "WS2812_RING"
                order = ["1", "2", "3", "4"]
            else:
                model, order = MODEL[part]
            L.append(f"X{ref} {' '.join(spice_node(pins.get(p, 'GND')) for p in order)} {model}")
        else:
            for i, n in enumerate(sorted({spice_node(v) for v in pins.values()})):
                L.append(f"R{ref}_{i} {n} 0 1e12")
    # a net only one thing touches cannot be solved against; tie it down
    seen = {}
    for ln in L:
        if ln and ln[0] in "RXCV" and not ln.startswith("*"):
            toks = ln.split()
            for t in toks[1:-1]:
                if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", t):
                    seen[t] = seen.get(t, 0) + 1
    L += [f"RFLOAT_{n} {n} 0 1e12" for n, k in sorted(seen.items())
          if k == 1 and n not in ("PRESS_PWR", "PRESS_SOS", "FW_OFF", "RING_ON", "RADIO", "VBATX", "VBUSX")]
    L.append(".ic v(HOLD)=0 v(VSYS)=0 v(_3V3)=0")
    return L


# name -> (time in s, what it means). All read from the same transient run.
SEQUENCE = """
  tran 0.5m 4.5 uic
  meas tran off_rail      FIND v(_3v3) AT=0.05
  meas tran off_icell     FIND i(vcell) AT=0.05
  meas tran on_rail       FIND v(_3v3) AT=0.7
  meas tran press_gpio4   FIND v(gpio4) AT=0.9
  meas tran after_press   FIND v(_3v3) AT=1.2
  meas tran ring_on       FIND v(vled)  AT=1.5
  meas tran ring_off      FIND v(vled)  AT=1.8
  meas tran fw_off_rail   FIND v(_3v3) AT=2.35
  meas tran again_rail    FIND v(_3v3) AT=2.9
  meas tran held_rail     FIND v(_3v3) AT=3.8
  meas tran released_rail FIND v(_3v3) AT=4.4
  meas tran end_icell     FIND i(vcell) AT=4.45
  meas tran gpio4_max     MAX v(gpio4)
  meas tran gpio4_min     MIN v(gpio4)
"""


def control():
    runs = []
    for tag, vcell, vth in (("full", 4.2, 2.1), ("mid", 3.8, 2.1), ("low", 3.4, 2.1),
                            ("lowvth", 3.4, 2.5), ("fullvth", 4.2, 2.5)):
        block = SEQUENCE.replace("meas tran ", f"meas tran {tag}_")
        runs.append(f"""
  alterparam vth7002={vth}
  reset
  alter vcell dc={vcell}
{block}""")
    return """
.control
  set noaskquit
""" + "".join(runs) + """
  * ---- steady state, button held so the latch is forced on --------------
  * A DC analysis reads each source's dc value, never its PWL, so the held
  * button, the lit ring and the radio are set there. `reset` re-reads the
  * deck and forgets every alter, so the alters come after it, each time.
  alterparam vth7002=2.1
  alterparam ringma=9.5m
  reset
  alter vpress dc = 1
  alter vring dc = 1
  alter vradio dc = 1
  * The firmware's own ladder (docs/subsystems/power.mdx): full features at
  * 3.45 V and up, degraded (BLE forced off, no OTA) below, powered down
  * under 3.15 V. Each band gets the floor that matters in it.
  dc vcell 3.45 4.25 0.05
  let railmin = minimum(v(_3v3))
  echo RESULT normal_rail_min = $&railmin
  let vledmin = minimum(v(vled))
  echo RESULT normal_vled_min = $&vledmin
  dc vcell 3.15 3.45 0.05
  let degmin = minimum(v(_3v3))
  echo RESULT degraded_rail_min = $&degmin
  alterparam ringma=15.5m
  reset
  alter vpress dc = 1
  alter vring dc = 1
  alter vradio dc = 1
  dc vcell 3.45 4.25 0.05
  let fullmin = minimum(v(_3v3))
  echo RESULT fullwhite_rail_min = $&fullmin
  alterparam ringma=0.5m
  reset
  alter vpress dc = 1
  alter vcell dc = 4.2
  alter vusb dc = 5.0
  op
  let vbs = v(vbat_sense)
  let vus = v(vbus_sense)
  echo RESULT vbat_sense = $&vbs
  echo RESULT vbus_sense = $&vus
  * ---- charging, board off ----------------------------------------------
  reset
  alter vpress pwl = [ 0 0 1 0 ]
  alter vcell dc = 3.6
  alter vusb dc = 5.0
  tran 1m 0.05 uic
  meas tran charge_i FIND i(vcell) AT=0.04
  meas tran charge_rail FIND v(_3v3) AT=0.04
  quit
.endc
.end
"""


def checks(tags):
    c = []
    for t in tags:
        c += [
            (f"{t}_off_rail", "<=", 0.10, "off before any press"),
            (f"{t}_off_icell", "<=", 20e-6, "drawing next to nothing from the cell while off"),
            (f"{t}_on_rail", ">=", 3.10, "a press turns it on and it stays on"),
            (f"{t}_press_gpio4", "<=", 0.40, "GPIO 4 reads a press as low"),
            (f"{t}_after_press", ">=", 3.10, "a press while running does NOT turn it off"),
            (f"{t}_ring_on", ">=", 3.20, "GPIO 19 high lights the ring's supply"),
            (f"{t}_ring_off", "<=", 0.30, "GPIO 19 low removes it"),
            (f"{t}_fw_off_rail", "<=", 0.30, "firmware low on GPIO 4 switches it off"),
            (f"{t}_again_rail", ">=", 3.10, "and it turns on again"),
            (f"{t}_held_rail", ">=", 3.10, "held down, it stays on even after the firmware says off"),
            (f"{t}_released_rail", "<=", 0.30, "released, it goes off"),
            (f"{t}_end_icell", "<=", 20e-6, "and draws next to nothing again"),
            (f"{t}_gpio4_max", "<=", 3.60, "GPIO 4 never above the ESP32's 3.6 V maximum"),
            (f"{t}_gpio4_min", ">=", -0.30, "GPIO 4 never below -0.3 V"),
        ]
    c += [
        ("normal_rail_min", ">=", 3.00,
         "cell >= 3.45 V (full features), radio at TX peak, ring lit: rail holds 3.0 V"),
        ("degraded_rail_min", ">=", 2.70,
         "3.15-3.45 V (firmware's low band), same load: above the flash's 2.7 V floor"),
        ("vbat_sense", "<=", 2.45, "cell divider inside the ADC window"),
        ("vbus_sense", ">=", 2.475, "VBUS presence clears the logic threshold"),
        ("vbus_sense", "<=", 3.60, "and stays under the absolute maximum"),
        ("charge_i", ">=", 0.50, "charging a 3.6 V cell at the PROG current (into the cell)"),
        ("charge_rail", "<=", 0.10, "charging does not switch the board on"),
        ("fullwhite_rail_min", ">=", 3.00,
         "every pixel at full white (15.5 mA, the 1515's datasheet), radio at TX peak, cell >= 3.45 V"),
        ("normal_vled_min", ">=", 3.50,
         "XFAIL: the 1515's supply range starts at 3.5 V; from a cell at 3.45 V the ring sits under "
         "it, as on the reference board, whose ring is on the cell the same way"),
    ]
    return c


def main():
    comps = netlist()
    lines = deck(comps)
    OUT.write_text("\n".join(lines) + "\n" + control())
    print(f"{len(comps)} components from the schematic -> {OUT.name}")
    run = subprocess.run(["ngspice", "-b", str(OUT)], capture_output=True, text=True, timeout=1800)
    out = run.stdout + run.stderr
    got = {}
    for m in re.finditer(r"^\s*(\w+)\s*=\s*([-+\d.eE]+)", out, re.M):
        got[m.group(1).lower()] = float(m.group(2))
    for m in re.finditer(r"RESULT (\w+) = ([-+\d.eE]+)", out):
        got[m.group(1).lower()] = float(m.group(2))

    ops = {"<=": lambda a, b: a <= b, ">=": lambda a, b: a >= b}
    tags = ["full", "mid", "low", "lowvth", "fullvth"]
    failed = known = passed = 0
    for name, op, limit, why in checks(tags):
        xfail = why.startswith("XFAIL:")
        v = got.get(name)
        # i(vcell) is positive into the cell; an idle drain is a small
        # negative number, and it is its size that is being limited.
        if v is not None and name.endswith("icell"):
            v = abs(v)
        if v is None:
            print(f"  MISSING {name}")
            failed += 1
            continue
        ok = ops[op](v, limit)
        mark = ("pass " if ok else "FAIL ") if not xfail else ("known" if not ok else "FIXED")
        if (not ok and not xfail) or (ok and xfail):
            failed += 1
        elif xfail:
            known += 1
        else:
            passed += 1
        print(f"  {mark} {name:<24} {v:>11.4g} {op} {limit:<8g} {why.replace('XFAIL: ', '')}")
    errs = [ln for ln in out.splitlines() if re.search(r"(?i)\berror\b|singular|timestep too small", ln)]
    for e in errs[:8]:
        print("  ngspice:", e.strip()[:140])
    print(f"\n{passed} passed, {failed} failed, {known} known limits")
    return 1 if failed or errs else 0


if __name__ == "__main__":
    sys.exit(main())
