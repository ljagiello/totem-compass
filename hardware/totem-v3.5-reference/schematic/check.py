#!/usr/bin/env python3
"""Build the netlist and insist the ERC actually ran before believing it.

This exists because of a specific mistake. The build was run with its output
sent to /dev/null after the previous report had been deleted; it died on an
import, left no report behind, and the empty file read back as a clean pass.
An absent report is not a passing one, so this checks that the script
succeeded and that the report exists before it reads a single number out of
it.

Two ERC warnings are expected and named below. Any other warning, any error,
a crash, or a missing report fails the run.

Usage: python3 check.py
"""

import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ERC = HERE / "totem.erc"
NET = HERE / "totem.net"

# Left open on purpose: charging is sensed from VBUS, not from these.
EXPECTED_WARNINGS = {
    "Unconnected pin: OPEN-COLLECTOR pin 7/CHRG of TP4056/U3.",
    "Unconnected pin: OPEN-COLLECTOR pin 8/STDBY of TP4056/U3.",
}


def main() -> int:
    for stale in (ERC, NET):
        stale.unlink(missing_ok=True)

    run = subprocess.run(
        [sys.executable, "totem.py"], cwd=HERE, capture_output=True, text=True, timeout=300
    )
    if run.returncode != 0:
        print("FAIL: totem.py did not complete")
        print((run.stdout + run.stderr).strip()[-1500:])
        return 1

    if not ERC.exists():
        print(f"FAIL: {ERC.name} was not written, so there is no result to read")
        return 1
    if not NET.exists():
        print(f"FAIL: {NET.name} was not written")
        return 1

    report = ERC.read_text()
    errors = re.search(r"(\d+) errors found while running ERC", report)
    if not errors:
        print("FAIL: the report does not say how many errors were found")
        return 1

    n_errors = int(errors.group(1))
    warnings = [
        re.sub(r"\s*@ \[.*", "", line).replace("ERC WARNING: ", "").strip()
        for line in report.splitlines()
        if line.startswith("ERC WARNING:")
    ]
    unexpected = [w for w in warnings if w not in EXPECTED_WARNINGS]
    missing = EXPECTED_WARNINGS - set(warnings)

    for line in report.splitlines():
        if line.startswith("ERC ERROR:"):
            print(f"  {re.sub(r'\\s*@ \\[.*', '', line)}")
    for w in unexpected:
        print(f"  unexpected warning: {w}")
    for w in missing:
        print(f"  expected warning no longer reported, update this list: {w}")

    nets = len(re.findall(r"\(net \(code", NET.read_text())) or len(
        set(re.findall(r'\(name "([A-Z0-9_$+]+)"\)', NET.read_text()))
    )

    ok = n_errors == 0 and not unexpected and not missing
    print(
        f"\nERC: {n_errors} errors, {len(warnings)} warnings "
        f"({len(EXPECTED_WARNINGS)} expected) over {nets} nets — {'PASS' if ok else 'FAIL'}"
    )
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
