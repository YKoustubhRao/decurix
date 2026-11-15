"""Lane: tokens per checked task, by kind of work (the taught skill, else the task type) and by phase (each call's
purpose tag), with the cached share of the input.

Two sources, both OBSERVED, never added: the model calls the control plane forwarded (llm_calls, the provider's own
usage, joined to an execution by worker and time as the run record joins them), and the totals the desktop reported
with its final reply (executions.*_tokens: the worker's own word)."""

from . import common
from .common import OBSERVED, figure, median, share

LANE = "tokens"


def kind_of(e):
    """The kind of work an execution was: its taught skill, else its task type."""

    return e.get("skill_name") or f"(no skill) {e.get('task_type') or '?'}"


def run(source, origin=None):
    execs = source.executions(origin)
    checked = [e for e in execs if e.get("checked") == 1]
    calls = source.calls_by_execution(checked)
    groups = {"by_kind": {}, "by_purpose": {}}
    proxy_total = {"in": 0, "cached": 0, "out": 0, "calls": 0}
    reported_total = {"in": 0, "cached": 0, "out": 0, "said": 0}
    by_kind = {}
    for e in checked:
        cs = calls.get(e["id"], [])
        tin = sum(c["input_tokens"] or 0 for c in cs)
        tca = sum(c["cached_input_tokens"] or 0 for c in cs)
        tout = sum(c["output_tokens"] or 0 for c in cs)
        proxy_total["in"] += tin
        proxy_total["cached"] += tca
        proxy_total["out"] += tout
        proxy_total["calls"] += len(cs)
        k = by_kind.setdefault(kind_of(e), {"tasks": 0, "proxy": [], "proxy_in": 0, "proxy_cached": 0, "reported": []})
        k["tasks"] += 1
        k["proxy"].append(tin + tout)
        k["proxy_in"] += tin
        k["proxy_cached"] += tca
        if e.get("input_tokens") is not None or e.get("output_tokens") is not None:
            rin, rout = e.get("input_tokens") or 0, e.get("output_tokens") or 0
            k["reported"].append(rin + rout)
            reported_total["in"] += rin
            reported_total["cached"] += e.get("cached_input_tokens") or 0
            reported_total["out"] += rout
            reported_total["said"] += 1
        for c in cs:
            p = groups["by_purpose"].setdefault(c["purpose"] or "untagged", {"calls": 0, "in": 0, "cached": 0, "out": 0})
            p["calls"] += 1
            p["in"] += c["input_tokens"] or 0
            p["cached"] += c["cached_input_tokens"] or 0
            p["out"] += c["output_tokens"] or 0
    for name, k in sorted(by_kind.items()):
        groups["by_kind"][name] = {
            "checked_tasks": k["tasks"],
            "proxy_tokens_per_task_median": median(k["proxy"]), "proxy_tokens_total": sum(k["proxy"]),
            "proxy_cached_share": share(k["proxy_cached"], k["proxy_in"]),
            "reported_tokens_per_task_median": median(k["reported"]), "reported_tasks": len(k["reported"]),
            "label": OBSERVED,
        }
    for p in groups["by_purpose"].values():
        p["cached_share"] = share(p["cached"], p["in"])
        p["label"] = OBSERVED
    n = len(checked)
    figures = [
        figure("checked executions", n, OBSERVED, "executions.checked = 1"),
        figure("proxy tokens per checked execution (median)", median(k for kk in by_kind.values() for k in kk["proxy"]),
               OBSERVED, "llm_calls joined by worker and time"),
        figure("proxy tokens, all checked executions", proxy_total["in"] + proxy_total["out"], OBSERVED, "llm_calls"),
        figure("proxy cached share of input", share(proxy_total["cached"], proxy_total["in"]), OBSERVED, "llm_calls"),
        figure("proxy calls", proxy_total["calls"], OBSERVED, "llm_calls"),
        figure("desktop-reported tokens, all checked executions", reported_total["in"] + reported_total["out"], OBSERVED,
               "executions.input_tokens + output_tokens", "the worker's own report, kept apart from the proxy's figure"),
        figure("untagged share of proxy calls", share(groups["by_purpose"].get("untagged", {}).get("calls", 0),
                                                         proxy_total["calls"]), OBSERVED, "llm_calls.purpose"),
    ]
    caveats = [
        "A checked task is an execution whose final check judged it (checked = 1), done or not: correctness is the "
        "gates lane's.",
        "Proxy figures are the provider's reported usage of calls the control plane forwarded; a local brain's calls "
        "never pass the proxy and are not here.",
        "Desktop-reported figures are the worker's own word and are never added to the proxy's.",
        "A call is joined to an execution by its worker and time window, as the run record joins it; the task id a "
        "desktop puts in a header is not trusted.",
        "The phase of a call is its purpose tag; tokens by phase within a skill run (gather, fill, replay) are not kept.",
    ]
    population = {"origin": origin or "all", "executions": len(execs), "checked": n}
    return common.lane_doc(LANE, source, population, figures, groups, caveats)
