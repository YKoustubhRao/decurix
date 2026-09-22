#!/usr/bin/env python3
"""
Builds the visual-anchor corpus on a real desktop: Chromium pixels of eight
kinds of control, each recorded once and then seen again as what should still
be it (hovered, moved, other surroundings, another theme) and as what must not
be taken for it (a similar control in its place, the control gone).

Runs INSIDE a desktop container, with this tree's agent on PYTHONPATH:

    docker exec -u headless -e PYTHONPATH=/opt <container> python3 corpus.py /tmp/corpus

Writes <out>/<kind>/<variant>.png and <out>/manifest.json. bench/recordings/
tune.py judges visual.find against it on any machine - the frames are plain
PNGs - so thresholds are tuned on real pixels, not on drawings.
"""

import json
import os
import subprocess
import sys
import time

from decurix_agent.computer import core, screen

OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/corpus"
WORK = "/home/headless/work"

# Decoys that first differ from the target further from its click than any window
# around the click can see (a checkbox is 13 px wide and its label runs on for
# another 120): no picture of the surroundings can tell these apart, so finding
# the target where they are is a limit, not a wrong answer.
BEYOND_THE_WINDOW = {("checkbox", "Send me updates daily"), ("checkbox", "Send me offers")}

# kind -> (role the control has, the target's label, its neighbours, decoys that look like it)
KINDS = {
    "button":   ("button", "Save", ["Back", "Next"], ["Cancel", "Delete", "Sav", "Save as"]),
    "link":     ("link", "Download report", ["Read the", "for details"], ["Download reports", "Download receipt", "Upload report"]),
    "checkbox": ("checkbox", "Send me updates", ["Remember me", "Accept terms"], ["Send me updates daily", "Send me offers", "Sell my data"]),
    "icon":     ("button", "⚙", ["✎", "★"], ["✖", "✔", "⚠"]),
    "input":    ("textbox", "Customer name", ["Email", "Phone"], ["Customer number", "Company name", "Customer"]),
    "menuitem": ("menuitem", "Open", ["New", "Close"], ["Open recent", "Save", "Print"]),
    "tab":      ("tab", "Invoices", ["Orders", "Reports"], ["Invoice", "Quotes", "Payments"]),
    "row":      ("cell", "Invoice 1042 Paid", ["Invoice 1041 Paid", "Invoice 1043 Due"], ["Invoice 1042 Due", "Invoice 1024 Paid", "Order 1042 Paid"]),
}

CSS = """
body{margin:0;font:15px 'DejaVu Sans',sans-serif;background:%(bg)s;color:%(fg)s}
.c{position:absolute;left:%(x)dpx;top:%(y)dpx;display:flex;flex-direction:%(dir)s;gap:14px;align-items:flex-start}
.n{position:absolute;left:40px;top:430px}
button{min-width:88px;height:36px;padding:0 14px;border:0;border-radius:6px;background:#2b6cb0;color:#fff;font:600 14px 'DejaVu Sans',sans-serif}
button:hover{background:#1e4e8c}
button.i{min-width:40px;width:40px;padding:0;font-size:20px;background:#e2e8f0;color:#2d3748}
button.i:hover{background:#cbd5e0}
a{color:#2b6cb0;text-decoration:underline}
input[type=text]{width:180px;height:32px;padding:0 8px;border:1px solid #a0aec0;border-radius:4px}
.m{display:block;width:150px;padding:7px 12px;background:%(pane)s;border:1px solid #cbd5e0}
.m:hover{background:#bee3f8;color:#1a202c}
.t{padding:9px 16px;border-bottom:3px solid transparent;background:%(pane)s}
.t:hover{border-bottom-color:#2b6cb0}
td{padding:7px 12px;border:1px solid #cbd5e0;background:%(pane)s;white-space:nowrap}
tr:hover td{background:#bee3f8;color:#1a202c}
"""

DIRECTIONS = {"button": "row", "link": "row", "checkbox": "column", "icon": "row", "input": "column",
              "menuitem": "column", "tab": "row", "row": "column"}


def control(kind, label, hidden=False, focus=False):
    html = _control(kind, label)
    if focus:
        html = html.replace("<input ", "<input autofocus ", 1)
    return f'<div style="visibility:hidden">{html}</div>' if hidden else html


def _control(kind, label):
    if kind == "button":
        return f"<button>{label}</button>"
    if kind == "link":
        return f"<span>{label}</span>" if label in ("Read the", "for details") else f'<a href="#">{label}</a>'
    if kind == "checkbox":
        return f'<label><input type="checkbox"> {label}</label>'
    if kind == "icon":
        return f'<button class="i" aria-label="{label}">{label}</button>'
    if kind == "input":
        return f'<input type="text" aria-label="{label}" placeholder="{label}">'
    if kind == "menuitem":
        return f'<div class="m" role="menuitem" tabindex="0">{label}</div>'
    if kind == "tab":
        return f'<div class="t" role="tab" tabindex="0">{label}</div>'
    if kind == "row":
        return f'<table><tr><td role="cell">{label}</td></tr></table>'


def page(name, kind, labels, x=300, y=200, dark=False, extra="I am some text that is not the control, ",
         hide=None, nudge="", focus=None):
    css = CSS % {"x": x, "y": y, "dir": DIRECTIONS[kind],
                 "bg": "#1c1c1c" if dark else "#f4f4f4", "fg": "#e6e6e6" if dark else "#222",
                 "pane": "#2a2a2a" if dark else "#ffffff"}
    body = "".join(control(kind, l, hidden=(i == hide), focus=(i == focus)) for i, l in enumerate(labels))
    if nudge:
        css += f".c{{transform:translate({nudge})}}"
    html = f"<!doctype html><meta charset=utf-8><title>{name}</title><style>{css}</style><div class=c>{body}</div><p class=n>{extra}</p>"
    path = f"{WORK}/{name}.html"
    open(path, "w", encoding="utf-8").write(html)
    return path


def show(path):
    subprocess.run(["gui", "open", "file://" + path], capture_output=True, timeout=60)
    time.sleep(1.1)


def where(role, label, fallback_page):
    """
    The centre of the control now on the screen: from the accessibility tree,
    else - text in a table cell is not an element - from what disappears when
    `fallback_page` (the same page with it hidden) is shown.
    """

    box = find_box(role, label)
    if box:
        return [int(box[0] + box[2] / 2), int(box[1] + box[3] / 2)]

    here = screen.grab()
    show(fallback_page)
    hidden = screen.grab()
    box = diff_box(here, hidden)
    return [int(box[0] + box[2] / 2), int(box[1] + box[3] / 2)] if box else None


def find_box(role, label):
    obs = core.observe(remember=False)
    for e in obs["elements"]:
        if e.get("box") and (e.get("name") or "").strip() == label.strip() and (role is None or e.get("role") in (role, "listitem", "generic")):
            return e["box"]
    for e in obs["elements"]:
        if e.get("box") and label.strip() in (e.get("name") or ""):
            return e["box"]
    return None


PAGE_TOP = 130  # below the browser's own tabs and toolbar, whose title changes with the page


def diff_box(a, b):
    """The bounding box (x, y, w, h) of where two frames of one size differ within the page, or None."""

    xs, ys = [], []
    stride = a.width * 3
    for y in range(PAGE_TOP, a.height):
        ra, rb = a.rgb[y * stride:(y + 1) * stride], b.rgb[y * stride:(y + 1) * stride]
        if ra != rb:
            cols = [x for x in range(a.width) if ra[x * 3:x * 3 + 3] != rb[x * 3:x * 3 + 3]]
            ys.append(y)
            xs += [cols[0], cols[-1]]

    return [min(xs), min(ys), max(xs) - min(xs) + 1, max(ys) - min(ys) + 1] if xs else None


def save(kind, variant, frame):
    os.makedirs(f"{OUT}/{kind}", exist_ok=True)
    with open(f"{OUT}/{kind}/{variant}.png", "wb") as f:
        f.write(frame.png())


def main():
    os.makedirs(OUT, exist_ok=True)
    manifest = []

    for kind, (role, target, neighbours, decoys) in KINDS.items():
        labels = [neighbours[0], target, neighbours[1]]
        show(page(f"{kind}-rec", kind, labels))
        frame = screen.grab()
        box = find_box(role, target)
        located = "accessibility tree"
        if not box:
            # Text in a table cell is not an element: exactly the click that ends
            # up a coordinate. Where it is comes from what disappears when it is hidden.
            show(page(f"{kind}-hidden", kind, labels, hide=1))
            box = diff_box(frame, screen.grab())
            located = "pixel difference"
            show(page(f"{kind}-rec", kind, labels))
            frame = screen.grab()
            if not box:
                print(f"!! {kind}: control not found on the page - skipped")
                continue

        click = [int(box[0] + box[2] / 2), int(box[1] + box[3] / 2)]
        save(kind, "recorded", frame)
        entry = {"kind": kind, "target": target, "role": role, "click": click, "box": box, "located": located,
                 "size": [frame.width, frame.height], "variants": []}

        def add(variant, expect, frame, dx=0, dy=0, note=""):
            save(kind, variant, frame)
            entry["variants"].append({"name": variant, "expect": expect, "dx": dx, "dy": dy, "note": note})

        # ---- what should still be it ----
        add("again", "found", screen.grab(), note="the page seen again")

        subprocess.run(["xdotool", "mousemove", str(click[0]), str(click[1])])
        time.sleep(0.5)
        add("hover", "found", screen.grab(), note="the pointer is over it")
        subprocess.run(["xdotool", "mousemove", "5", "5"])

        def moved(variant, note, use=None, **page_args):
            """Show the page as asked and record where the control really is now, not where it was nudged to."""

            args = dict(kind=kind, labels=use or labels, **page_args)
            show(page(f"{kind}-{variant}", **args))
            frame = screen.grab()
            now = where(role, target, page(f"{kind}-{variant}-hidden", hide=1, **args))
            if now is None:
                print(f"!! {kind}/{variant}: could not find the control - variant dropped")
                return
            add(variant, "found", frame, now[0] - click[0], now[1] - click[1], note)

        for tag, (dx, dy) in {"shift1": (23, -17), "shift2": (-71, 44), "shift3": (5, 82), "shift4": (-88, -30)}.items():
            moved(tag, "it has moved", x=300 + dx, y=200 + dy)

        show(page(f"{kind}-ctx", kind, labels, extra="Completely different words down here and more of them, 2026."))
        add("context", "found", screen.grab(), note="other text elsewhere on the page")

        moved("neighbours", "the controls beside it changed (a row reflows, so it may move)", use=["Other", target, "Things"])

        show(page(f"{kind}-subpx", kind, labels, nudge="0.5px, 0.4px"))
        add("subpixel", "found", screen.grab(), note="resampled by half a pixel (text re-antialiased)")

        if kind == "input":
            # A focused field has a caret that blinks and a ring around it. Two frames
            # half a blink apart: the same control, with and without its caret.
            show(page(f"{kind}-focus", kind, labels, focus=1))
            time.sleep(0.2)
            add("focus_a", "found", screen.grab(), note="focused: the caret and the ring")
            time.sleep(0.55)
            add("focus_b", "found", screen.grab(), note="focused: half a blink later")

        show(page(f"{kind}-dark", kind, labels, dark=True))
        add("dark", "found|missing", screen.grab(), note="the dark theme (a different look: either is safe, a wrong place is not)")

        # ---- what must not be taken for it ----
        for i, decoy in enumerate(decoys, 1):
            show(page(f"{kind}-decoy{i}", kind, [neighbours[0], decoy, neighbours[1]]))
            unseen = (kind, decoy) in BEYOND_THE_WINDOW
            add(f"decoy{i}", "found|missing" if unseen else "missing", screen.grab(),
                note=f"{decoy!r} where it was" + (" (differs only beyond what a window can see)" if unseen else ""))

        show(page(f"{kind}-gone", kind, [neighbours[0], neighbours[1]]))
        add("gone", "missing", screen.grab(), note="it is not on the page")

        show(page(f"{kind}-elsewhere", kind, ["Something", "else", "entirely"], y=330))
        add("elsewhere", "missing", screen.grab(), note="a different page")

        manifest.append(entry)
        print(f"{kind}: {len(entry['variants'])} variants, click {click}")

    with open(f"{OUT}/manifest.json", "w") as f:
        json.dump(manifest, f, indent=1)


main()
