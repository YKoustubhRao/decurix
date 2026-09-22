#!/usr/bin/env python3
"""
Judges visual.find against the corpus corpus.py built on a real desktop.

    python3 bench/recordings/tune.py <corpus dir> [--verbose] [--set NAME=VALUE ...]

Needs no display and no container: the frames are PNGs. For every kind of
control it takes the recorded frame, makes a look at the click, and asks
visual.find about every other frame - which should still be the control
(found, at the right place) and which must not be taken for it (missing). It
prints what went wrong and the margin the right answers had, so a threshold
is moved because of numbers and not because of a hunch.

There are two ways to be wrong and they do not cost the same. UNSAFE is
finding a control that is not there (or clicking somewhere other than where it
is): a click on the wrong thing. OVER-CAUTIOUS is halting on a control that was
there: a person is asked, and nothing is lost. The exit status is 1 on any
unsafe answer, or on more over-caution than MAX_CAUTIOUS of the controls that
should have been found - so this is also the regression check.
"""

import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agent"))

from decurix_agent.computer import visual  # noqa: E402


class Frame:
    def __init__(self, width, height, rgb):
        self.width, self.height, self.rgb = width, height, rgb


def load(path):
    return Frame(*visual.decode_png(Path(path).read_bytes()))


def main():
    args = sys.argv[1:]
    verbose = "--verbose" in args

    for i, a in enumerate(args):
        if a == "--set":
            name, value = args[i + 1].split("=")
            setattr(visual, name, type(getattr(visual, name))(value))

    corpus = Path(next(a for a in args if not a.startswith("--") and "=" not in a))
    manifest = json.loads((corpus / "manifest.json").read_text())

    MAX_CAUTIOUS = 0.10
    unsafe, cautious, expected_found, total, times = [], [], 0, 0, []
    true_scores, false_near = [], []
    unverifiable = []

    for entry in manifest:
        kind = entry["kind"]
        recorded = (corpus / kind / "recorded.png").read_bytes()
        look = visual.make_look(recorded, entry["click"], entry["size"])

        if look is None:
            unverifiable.append(kind)
            print(f"{kind:9s} no look could be made (too little structure to recognise) - such a click stays as recorded")
            continue

        for v in entry["variants"]:
            frame = load(corpus / kind / f"{v['name']}.png")
            t = time.perf_counter()
            r = visual.find(look, frame)
            times.append((time.perf_counter() - t) * 1000)
            total += 1

            allowed = v["expect"].split("|")
            expected_found += v["expect"] == "found"
            kind_of_wrong = None

            if r["status"] not in allowed:
                # Halting where it could have gone on is safe; going on where it must not is not.
                kind_of_wrong = "cautious" if r["status"] in ("missing", "ambiguous") and "found" in allowed else "unsafe"
            elif r["status"] == "found" and "missing" not in allowed:
                want = (entry["click"][0] + v["dx"], entry["click"][1] + v["dy"])
                if abs(r["point"][0] - want[0]) > 2 or abs(r["point"][1] - want[1]) > 2:
                    kind_of_wrong = "unsafe"
                    v = dict(v, note=v["note"] + f" (clicked {r['point']}, wanted {want})")

            good = kind_of_wrong is None

            (true_scores if v["expect"] == "found" else false_near if v["expect"] == "missing" else []).append(r["closest"] if r["closest"] is not None else 1.0)

            if kind_of_wrong == "unsafe":
                unsafe.append((kind, v, r))
            elif kind_of_wrong == "cautious":
                cautious.append((kind, v, r))
            if verbose or not good:
                print(f"{'ok ' if good else ('UNSAFE' if kind_of_wrong == 'unsafe' else 'cautious')} {kind:9s} {v['name']:9s} want={v['expect']:8s} got={r['status']:9s} "
                      f"score={r['score']} closest={r['closest']} {v['note']}")

    print()
    print(f"{total - len(unsafe) - len(cautious)}/{total} right | UNSAFE {len(unsafe)} | over-cautious "
          f"{len(cautious)} of {expected_found} that should be found ({len(cautious) / max(1, expected_found):.0%}, "
          f"budget {MAX_CAUTIOUS:.0%}) | kinds with no look: {unverifiable or 'none'}")
    if true_scores:
        print(f"should-be-found: worst share of blocks that differed = {max(true_scores):.3f}")
    if false_near:
        print(f"must-not-match:  fewest blocks that differed         = {min(false_near):.3f}  "
              f"(a jump needs {visual.MAX_MISMATCH_MOVED}, the recorded spot allows {visual.MAX_MISMATCH})")
    if times:
        times.sort()
        print(f"time: median {times[len(times)//2]:.1f} ms, p95 {times[int(len(times)*.95)]:.1f} ms, worst {times[-1]:.1f} ms")

    sys.exit(1 if unsafe or len(cautious) > MAX_CAUTIOUS * expected_found else 0)


main()
