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

# The charger's status outputs are left open on purpose — charging is
# sensed from VBUS — but they are now declared unused on the part itself,
# so ERC does not report them and there is nothing to allow here. An empty
# set is the honest state: any warning at all is unexpected.
EXPECTED_WARNINGS = set()


SHEET = HERE / "totem.kicad_sch"
ROUNDTRIP = HERE / "kicad_from_sheet.net"


def refs(text):
    """Real reference designators, ignoring #PWR/#FLG virtuals."""
    return re.findall(r'\(ref(?:erence)? "([^"#][^"]*)"', text)


def kicad_checks():
    """What KiCad can be asked that SKiDL's ERC cannot, plus what neither does.

    Three things:

    - KiCad's own ERC, gated on error severity. It was being written to a
      report that nothing read.
    - Duplicate and unannotated reference designators. Both are default
      *errors* in KiCad and neither fires under `kicad-cli sch erc`, which
      was verified by planting them: a duplicate R1 and an `R?` both passed.
      So the check has to live here.
    - A round trip. KiCad exports a netlist back out of the drawing, and it
      must carry the same parts as the netlist the circuit was built from.
      This is what catches a part that is in the circuit and not on the
      sheet, which has happened once already and was invisible to both ERCs.
    """
    problems = []
    # Regenerate the sheet from the circuit first. Comparing against whatever
    # sheet happens to be on disk made the result depend on whether someone
    # remembered to run gen_sch.py — it caught a stale sheet once, correctly,
    # but a check should not rely on the order commands were typed in.
    gen = subprocess.run([sys.executable, "gen_sch.py"], cwd=HERE,
                         capture_output=True, text=True, timeout=600)
    if gen.returncode != 0:
        return ["gen_sch.py did not complete: " + (gen.stdout + gen.stderr).strip()[-400:]]
    if not SHEET.exists():
        return ["totem.kicad_sch was not written"]

    erc = subprocess.run(
        ["kicad-cli", "sch", "erc", "--severity-error", "--exit-code-violations",
         "--output", str(HERE / "totem_erc.rpt"), str(SHEET)],
        cwd=HERE, capture_output=True, text=True, timeout=300,
    )
    if erc.returncode not in (0,):
        problems.append(f"kicad-cli ERC reported error-severity violations (exit {erc.returncode})")

    sheet_refs = refs(SHEET.read_text())
    dupes = {r for r in sheet_refs if sheet_refs.count(r) > 1}
    if dupes:
        problems.append(f"duplicate reference designators: {sorted(dupes)}")
    if any(r.endswith("?") for r in sheet_refs):
        problems.append("unannotated reference designators present")

    rt = subprocess.run(
        ["kicad-cli", "sch", "export", "netlist", "--format", "kicadsexpr",
         "--output", str(ROUNDTRIP), str(SHEET)],
        cwd=HERE, capture_output=True, text=True, timeout=300,
    )
    if rt.returncode == 0 and ROUNDTRIP.exists():
        drawn = set(refs(ROUNDTRIP.read_text()))
        built = set(refs(NET.read_text()))
        if drawn != built:
            problems.append(
                f"the drawing and the circuit disagree: "
                f"only in circuit {sorted(built - drawn)}, only on sheet {sorted(drawn - built)}"
            )
        ROUNDTRIP.unlink()
    else:
        problems.append("could not export a netlist from the sheet to compare")
    return problems


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
    if errors:
        n_errors = int(errors.group(1))
    elif "No errors or warnings found" in report:
        # SKiDL says this instead of a count when the run is spotless.
        n_errors = 0
    else:
        print("FAIL: the report does not say how many errors were found")
        return 1
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

    extra = kicad_checks()
    if extra:
        for line in extra:
            print(f"  {line}")

    ok = n_errors == 0 and not unexpected and not missing and not extra
    print(
        f"\nERC: {n_errors} errors, {len(warnings)} warnings "
        f"({len(EXPECTED_WARNINGS)} expected) over {nets} nets — {'PASS' if ok else 'FAIL'}"
    )
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
