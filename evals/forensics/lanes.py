#!/usr/bin/env python3
"""Run the forensics lanes over a COPY of a control plane's decurix.db (never the live file).

    py -3 evals/forensics/lanes.py --db C:/copies/decurix.db [--bundles <blob store or per-run folder>]
                                   [--origin person|eval|practice|...] [--lane tokens --lane gates ...]
                                   [--out evals/results/forensics]

Each lane writes <out>/<lane>.json and prints its figures, each labelled OBSERVED or MODELED. Standard library only."""

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from forensics import common, gates, halts, questions, rework, tokens  # noqa: E402

LANES = {m.LANE: m for m in (tokens, questions, gates, halts, rework)}


def main(argv=None):
    ap = argparse.ArgumentParser(prog="evals/forensics/lanes.py", description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True, help="a COPY of decurix.db (opened read-only)")
    ap.add_argument("--bundles", default="", help="the blob store (a folder holding sha256/) or <exec id>/<path> folders")
    ap.add_argument("--origin", default="", help="only executions of this origin (person, eval, practice, schedule, ...)")
    ap.add_argument("--lane", action="append", choices=sorted(LANES), help="repeatable; default every lane")
    ap.add_argument("--out", default=str(Path(__file__).resolve().parent.parent / "results" / "forensics"))
    a = ap.parse_args(argv)
    source = common.Source(a.db, a.bundles or None)
    try:
        for name in a.lane or list(LANES):
            doc = LANES[name].run(source, a.origin or None)
            path = common.write(a.out, name, doc)
            print(f"[{name}] {path}")
            for f in doc["figures"]:
                print(f"  {f['label']:8} {f['name']}: {f['value']}")
    finally:
        source.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
