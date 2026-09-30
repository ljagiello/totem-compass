#!/usr/bin/env python3
"""Just enough S-expression handling to lift symbols out of KiCad libraries.

KiCad's `.kicad_sym` files and `.kicad_sch` sheets are both S-expressions, so
a symbol can be copied from one into the other verbatim. That is what makes a
generated sheet possible without reimplementing anyone's symbol: the drawing
of a resistor stays KiCad's drawing of a resistor.

Only what is needed is here: read, find a symbol, ask where its pins are,
write back out.
"""

from pathlib import Path

LIB_DIR = Path("/Applications/KiCad/KiCad.app/Contents/SharedSupport/symbols")


def parse(text):
    """S-expression text to nested lists. Strings keep their quotes stripped."""
    out, stack, token, in_str, esc = [], [], "", False, False
    for ch in text:
        if esc:
            token += ch
            esc = False
            continue
        if in_str:
            if ch == "\\":
                esc = True
            elif ch == '"':
                stack[-1].append(("str", token)) if stack else out.append(("str", token))
                token, in_str = "", False
            else:
                token += ch
            continue
        if ch == '"':
            if token:
                (stack[-1] if stack else out).append(token)
                token = ""
            in_str = True
            continue
        if ch in "() \t\r\n":
            if token:
                (stack[-1] if stack else out).append(token)
                token = ""
            if ch == "(":
                node = []
                (stack[-1] if stack else out).append(node)
                stack.append(node)
            elif ch == ")":
                stack.pop()
            continue
        token += ch
    return out


def dump(node, indent=0):
    """Nested lists back to S-expression text."""
    if isinstance(node, tuple) and node[0] == "str":
        body = node[1].replace("\\", "\\\\").replace('"', '\\"')
        return f'"{body}"'
    if isinstance(node, str):
        return node
    pad = "  " * indent
    inner = []
    simple = all(not isinstance(c, list) for c in node)
    for child in node:
        inner.append(dump(child, indent + 1))
    if simple:
        return "(" + " ".join(inner) + ")"
    head, rest = inner[0], inner[1:]
    body = "\n".join(f"{pad}  {r}" for r in rest)
    return f"({head}\n{body}\n{pad})"


def head(node):
    return node[0] if node and isinstance(node[0], str) else None


def children(node, name):
    return [c for c in node if isinstance(c, list) and head(c) == name]


def sval(node):
    """The value of a (key "value") pair, or None."""
    for c in node[1:]:
        if isinstance(c, tuple) and c[0] == "str":
            return c[1]
        if isinstance(c, str):
            return c
    return None


_cache = {}


def load_library(lib):
    if lib not in _cache:
        _cache[lib] = parse((LIB_DIR / f"{lib}.kicad_sym").read_text())[0]
    return _cache[lib]


def symbol(lib, name):
    """The full definition block for one symbol, ready to embed.

    A symbol that `extends` another carries only its own properties — no
    graphics and no pins, which live on the parent. Those are spliced in
    here, because a sheet embeds self-contained definitions: left as it
    comes, the symbol draws as nothing and, worse, has no pins to attach
    wires to, so it lands on the sheet silently unconnected.
    """
    for sym in children(load_library(lib), "symbol"):
        if sval(sym) != name:
            continue
        ext = children(sym, "extends")
        if not ext:
            return sym
        base_name = sval(ext[0])
        base = symbol(lib, base_name)
        merged = [c for c in sym if not (isinstance(c, list) and head(c) == "extends")]
        # The parent's sub-units are named after the parent; under this
        # symbol they have to be named after this one.
        for unit in children(base, "symbol"):
            unit = list(unit)
            for i, child in enumerate(unit):
                if isinstance(child, tuple) and child[0] == "str":
                    unit[i] = ("str", child[1].replace(base_name, name, 1))
                    break
            merged.append(unit)
        for keep in ("pin_numbers", "pin_names", "exclude_from_sim", "in_bom", "on_board"):
            if not children(sym, keep):
                merged.extend(children(base, keep))
        return merged
    raise KeyError(f"{lib}:{name}")


def pin_positions(sym):
    """Every pin's number mapped to (x, y, rotation) in symbol coordinates.

    Pins live on the symbol's sub-units, so this walks one level down as well
    as the top level.
    """
    found = {}

    def walk(node):
        for pin in children(node, "pin"):
            at = children(pin, "at")
            num = children(pin, "number")
            length = children(pin, "length")
            if not (at and num):
                continue
            x, y, rot = float(at[0][1]), float(at[0][2]), float(at[0][3])
            ln = float(sval(length[0])) if length else 2.54
            found[sval(num[0])] = (x, y, rot, ln)
        for sub in children(node, "symbol"):
            walk(sub)

    walk(sym)
    return found


if __name__ == "__main__":
    for lib, name in [("Device", "R"), ("RF_Module", "ESP32-WROOM-32E")]:
        s = symbol(lib, name)
        pins = pin_positions(s)
        print(f"{lib}:{name}: {len(pins)} pins, e.g. {list(pins.items())[:2]}")
