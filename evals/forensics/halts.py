"""Lane: replay halts by cause, from soul's trace in each execution's pulled bundle (needs --bundles).

Each `gui do` / `gui replay` command soul ran is a trace line of kind "command"; its output begins with the replay's
outcome ("halted: ...", "verified: ..."). The outcome is OBSERVED. The cause is the outcome's own words with numbers,
quoted names and ids folded (so "step 7 of 12 (click "Save")" and "step 9 of 12 (click "Post")" group together): a
mechanical grouping, OBSERVED, never a judgement of why."""

import json
import re

from . import common
from .common import OBSERVED, figure
from .tokens import kind_of

LANE = "halts"
OUTCOMES = ("verified", "completed_unverified", "halted_before_effect", "halted", "reconciliation_required",
            "needs_approval", "needs_human")
HALTS = ("halted_before_effect", "halted", "reconciliation_required", "needs_human")
APPROVAL_MARK = "Approval needed"


def cause(text):
    """The words of a replay's outcome with what varies run to run folded: quoted text, numbers, ids."""

    t = re.sub(r'"[^"]*"|\'[^\']*\'', "<q>", text or "")
    t = re.sub(r"\b[0-9a-f]{8,}\b", "<id>", t)
    t = re.sub(r"\d+", "<n>", t)
    t = re.sub(r"\s+", " ", t).strip()
    return t[:160]


def outcome_of(output_head):
    first = (output_head or "").strip().splitlines()[:1]
    if not first:
        return None, ""
    line = first[0]
    if line.startswith(APPROVAL_MARK) or "Approval needed" in line[:40]:
        return "needs_approval", line
    for o in sorted(OUTCOMES, key=len, reverse=True):
        if line.startswith(o + ":"):
            return o, line[len(o) + 1:].strip()
    return None, line


def replays_in(trace_bytes):
    """[(outcome, words)] of every replay command in a soul trace."""

    out = []
    for raw in (trace_bytes or b"").splitlines():
        try:
            line = json.loads(raw)
        except ValueError:
            continue
        cmd = line.get("command") if isinstance(line, dict) else None
        if line.get("kind") != "command" or not isinstance(cmd, dict):
            continue
        words = (cmd.get("text") or "").split()
        if len(words) < 2 or words[0] != "gui" or words[1] not in ("do", "replay"):
            continue
        o, said = outcome_of(cmd.get("output_head"))
        if o:
            out.append((o, said))
    return out


def run(source, origin=None):
    execs = source.executions(origin)
    groups = {"outcomes": {}, "halts_by_cause": {}, "by_kind": {}}
    caveats = [
        "Read from soul's trace in each pulled bundle: a run with no bundle, or a replay outside soul (a taught "
        "skill's engine run), is not seen.",
        "A trace keeps the first bytes of each command's output: a cause is the outcome's own first line.",
        "Causes are grouped by their words with numbers, quotes and ids folded: a grouping, not a diagnosis.",
    ]
    if source.bundles is None:
        caveats.insert(0, "No bundles directory was given: replay halts were not measured.")
        return common.lane_doc(LANE, source, {"origin": origin or "all", "executions": len(execs), "bundles": 0},
                               [figure("replays seen", 0, OBSERVED, "no bundles", "not measured")], groups, caveats)
    with_bundle = read = 0
    replays = halts = 0
    for e in execs:
        files = source.bundle_files(e["id"])
        if not files:
            continue
        with_bundle += 1
        for path, sha in files.items():
            if "/ai/traces/" not in "/" + path or not path.endswith(".jsonl"):
                continue
            data = source.read_bundle_file(e["id"], path, sha)
            if data is None:
                continue
            read += 1
            for o, said in replays_in(data):
                replays += 1
                groups["outcomes"][o] = groups["outcomes"].get(o, 0) + 1
                k = groups["by_kind"].setdefault(kind_of(e), {"replays": 0, "halts": 0})
                k["replays"] += 1
                if o in HALTS:
                    halts += 1
                    k["halts"] += 1
                    c = f"{o}: {cause(said)}"
                    groups["halts_by_cause"][c] = groups["halts_by_cause"].get(c, 0) + 1
    groups["halts_by_cause"] = dict(sorted(groups["halts_by_cause"].items(), key=lambda kv: -kv[1])[:100])
    figures = [
        figure("executions with a bundle", with_bundle, OBSERVED, "run_bundles"),
        figure("soul traces read", read, OBSERVED, "bundle soul_trace"),
        figure("replays seen", replays, OBSERVED, "gui do / gui replay commands"),
        figure("replays that halted or needed a person", halts, OBSERVED, "the replay's own outcome"),
    ]
    population = {"origin": origin or "all", "executions": len(execs), "bundles": with_bundle}
    return common.lane_doc(LANE, source, population, figures, groups, caveats)
