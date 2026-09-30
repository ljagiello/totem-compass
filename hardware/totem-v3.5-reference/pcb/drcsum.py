"""drcsum.py REPORT [category ...] — count a kicad-cli DRC report by category and
list the items of the named categories one line each."""
import re
import sys
from collections import Counter

text = open(sys.argv[1]).read()
want = set(sys.argv[2:])
blocks = re.split(r"\n(?=\[)", text)
cnt = Counter()
for b in blocks:
    m = re.match(r"\[(\w+)\]: (.*)", b)
    if not m:
        continue
    cat = m.group(1)
    cnt[cat] += 1
    if cat in want:
        items = re.findall(r"@\(([\d.]+) mm, ([\d.]+) mm\): (.*)", b)
        desc = " | ".join(f"({float(x)-100:.2f},{100-float(y):.2f}) {s}" for x, y, s in items)
        print(f"{cat}: {m.group(2)[:60]} :: {desc}")
for k, v in cnt.most_common():
    print(f"{v:5d} {k}")
print(re.search(r"\*\* Found \d+ unconnected pads \*\*", text).group(0) if "unconnected pads" in text else "")
