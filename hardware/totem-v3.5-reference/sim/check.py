#!/usr/bin/env python3
"""Run the ngspice decks and check each measurement against its limit.

Every check is a claim about the reference design that would be false if a
value were changed carelessly, in the same spirit as the Go tests: the deck
computes, this decides whether the number is allowed.

A check that is expected to fail is marked `xfail` with the reason. Those are
findings about the design, not mistakes in it, and the suite fails if one of
them starts passing without the note being removed.

Usage: python3 check.py
"""

import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent

# deck -> list of (measurement, comparison, limit, why)
CHECKS = {
    "batt_sense.cir": [
        ("vpin_full", "<=", 2.450, "a full cell must stay inside the ADC's 150..2450 mV window"),
        ("vpin_empty", ">=", 0.150, "an empty cell must stay above the bottom of that window"),
        ("err_hi", "<=", 0.010, "the firmware's calibration must read true to 10 mV"),
        ("err_lo", ">=", -0.010, "...in the other direction too"),
        ("idiv_ua", "<=", 25.0, "the divider must not be a meaningful load on the cell"),
    ],
    "vbus_sense.cir": [
        ("vpin_lo", ">=", 2.475, "a sagging charger must still read as a logic high"),
        ("vpin_hi", "<=", 3.600, "a high charger must not exceed the ESP32's absolute maximum"),
        ("mih_worst", ">=", 0.0, "logic-high margin may not go negative anywhere in the sweep"),
        ("max_worst", ">=", 0.0, "nor may the headroom to absolute maximum"),
        ("idiv_ua", "<=", 25.0, "and it must not be a meaningful load either"),
    ],
    "ldo_headroom.cir": [
        ("vout_full", ">=", 3.000, "the rail must hold the ESP32 up at a full cell"),
        ("vout_34", ">=", 3.000, "and at the cell voltage the firmware gives up at"),
        ("mrg_34", ">=", 0.100, "with at least 100 mV of margin there"),
        ("vbat_fail", "<=", 3.400, "the hardware must not fail before the firmware stops it"),
        (
            "pdiss_mw",
            "<=",
            500.0,
            "peak dissipation in the pass element; transient only, see the README on thermals",
        ),
    ],
    "led_budget.cir": [
        ("i_typ_crate", "<=", 1.0, "normal use, north plus peers plus crystal, must fit inside 1C"),
        ("n_at_1c", ">=", 20.0, "at least 20 pixels must be lightable at once at full white"),
        (
            "i_all_crate",
            "<=",
            1.0,
            "XFAIL: every pixel at full white is 2.4C. The firmware never lights "
            "the ring that way, and this records the ceiling rather than pretending it is met",
        ),
        (
            "vws_head",
            ">=",
            0.0,
            "XFAIL: WS2812B want 3.5 V and the firmware runs the cell to 3.4 V, "
            "so the pixels are 100 mV under-supplied at the bottom of the discharge",
        ),
    ],
}

OPS = {
    "<=": lambda a, b: a <= b,
    ">=": lambda a, b: a >= b,
}


def measurements(deck: str) -> dict:
    """Run one deck and return its measurements by name."""
    out = subprocess.run(
        ["ngspice", "-b", deck], cwd=HERE, capture_output=True, text=True, timeout=120
    )
    found = {}
    for line in (out.stdout + out.stderr).splitlines():
        m = re.match(r"\s*(\w+)\s*=\s*([-+0-9.eE]+)", line)
        if m:
            try:
                found[m.group(1)] = float(m.group(2))
            except ValueError:
                pass
        if "failed!" in line.lower():
            print(f"  ngspice: {line.strip()}")
    return found


def main() -> int:
    passed = failed = expected = unexpected = 0
    for deck, checks in CHECKS.items():
        print(f"\n{deck}")
        got = measurements(deck)
        for name, op, limit, why in checks:
            xfail = why.startswith("XFAIL:")
            if name not in got:
                print(f"  MISSING {name}: the deck did not report it")
                failed += 1
                continue
            value = got[name]
            ok = OPS[op](value, limit)
            if ok and not xfail:
                print(f"  pass  {name:<13} {value:>12.4g} {op} {limit:<8g}  {why}")
                passed += 1
            elif not ok and xfail:
                print(f"  known {name:<13} {value:>12.4g} {op} {limit:<8g}")
                print(f"        {why[6:].strip()}")
                expected += 1
            elif ok and xfail:
                print(f"  FIXED {name:<13} {value:>12.4g} — expected to fail and did not.")
                print("        Update the note: the design changed or the limit is wrong.")
                unexpected += 1
            else:
                print(f"  FAIL  {name:<13} {value:>12.4g} {op} {limit:<8g}  {why}")
                failed += 1

    print(
        f"\n{passed} passed, {failed} failed, "
        f"{expected} known limits recorded, {unexpected} unexpectedly met"
    )
    return 1 if (failed or unexpected) else 0


if __name__ == "__main__":
    sys.exit(main())
