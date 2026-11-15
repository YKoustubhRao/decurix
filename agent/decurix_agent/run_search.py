"""
Searching this worker's own past runs: `gui recall --runs WORDS`, `gui recall --runs --task ID [--around N]`.

The control plane keeps every task's stored text (the request, the steps reported, the questions and the answers
people gave, the reports) in a full-text index, and serves a desktop its own runs only (GET /api/agent/runs,
internal/runsearch). What comes back is the stored text, cut at a length and said so, never a summary.

The client scope of memory (memory_context._scoped) holds here too: a past run whose text names a party (a client)
this task's request does not name is not shown, only counted, so one client's run never steers another client's
task. What a past run did is how it went then, not proof of anything now: the screen and the files are checked again.
"""

import json
import urllib.error
import urllib.parse
import urllib.request

from decurix_agent import config

TIMEOUT = 20


class SearchError(Exception):
    """The control plane could not be reached, or refused the search (its own words)."""


def _call(query):
    url = config.CONTROL_PLANE_URL.rstrip("/") + "/api/agent/runs?" + urllib.parse.urlencode(query)
    try:
        with urllib.request.urlopen(urllib.request.Request(url, method="GET"), timeout=TIMEOUT) as response:
            return json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        reason = ""
        try:
            reason = json.loads(exc.read().decode("utf-8")).get("error", "")
        except Exception:  # noqa: BLE001
            pass
        raise SearchError(reason or f"the control plane answered HTTP {exc.code}") from None
    except (urllib.error.URLError, OSError, ValueError) as exc:
        raise SearchError(f"the control plane could not be reached: {exc}") from None


def _current():
    """(this task's id, its request), each "" when unknown."""

    try:
        from decurix_agent import task_context
        return task_context.current() or "", task_context.current_request() or ""
    except Exception:  # noqa: BLE001 - a search never costs the task
        return "", ""


def _other_parties(request):
    """The parties memory keeps facts about that this request does not name (memory_context's client scope)."""

    from decurix_agent import memory_context

    entries = memory_context._entries()
    named = memory_context._named_subjects(entries, request)
    kept = {memory_context._subject_of(e) for e in entries if memory_context._scoped_kind(e)}
    return {p for p in kept if p and p not in named}


def _names_another(text, others):
    from decurix_agent import memory_context

    return any(memory_context.names_whole(p, text) for p in others)


def _texts(hit):
    return [hit.get("snippet", "").replace("[[", "").replace("]]", "")] + [m.get("text", "") for m in hit.get("messages") or ()]


def search(words, since="", limit=5):
    """The best-matching past runs of this desktop, this task left out, scoped by client: (hits, hidden)."""

    task, request = _current()
    query = {"desktop_id": config.DESKTOP_ID, "q": words, "limit": limit}
    if since:
        query["after"] = since
    if task:
        query["exclude"] = task
    answer = _call(query)
    others = _other_parties(request)
    hits, hidden = [], 0
    for hit in answer.get("hits") or ():
        if others and any(_names_another(t, others) for t in _texts(hit)):
            hidden += 1
            continue
        hits.append(hit)
    return hits, hidden, answer.get("note", "")


def read(task_id, around=None, window=None):
    """One past run's texts (all, or a window around text number `around`); None when it names another party."""

    _, request = _current()
    query = {"desktop_id": config.DESKTOP_ID, "task": task_id}
    if around is not None:
        query["around"] = around
        if window:
            query["window"] = window
    run = _call(query).get("run") or {}
    others = _other_parties(request)
    if others and any(_names_another(m.get("text", ""), others) for m in run.get("messages") or ()):
        return None
    return run


def _message_lines(messages):
    lines = []
    for m in messages or ():
        cut = f" [{m['cut']} more characters in the task record]" if m.get("cut") else ""
        lines.append(f"  #{m.get('seq')} {m.get('kind')} ({str(m.get('at', ''))[:19]}): {m.get('text', '')}{cut}")
    return lines


NOTE = ("note: this is what past runs did and were told then, as stored; it is not proof of anything now - check the "
        "screen and the files again before relying on it.")


def render_search(hits, hidden, note=""):
    lines = ["=== PAST RUNS ==="]
    if not hits:
        lines.append("No past run of this worker has those words.")
    for i, hit in enumerate(hits):
        how = "automated (" + hit.get("origin", "") + ")" if hit.get("automated") else "started by a person"
        lines.append(f"run {hit.get('task_id')}: {hit.get('outcome') or hit.get('status')}, {how}, "
                     f"{str(hit.get('created_at', ''))[:10]}")
        if i == 0 and hit.get("messages"):
            lines.extend(_message_lines(hit["messages"]))
        else:
            lines.append(f"  #{hit.get('seq')} {hit.get('kind')}: {hit.get('snippet', '')}")
    if hidden:
        lines.append(f"({hidden} run(s) about a party this task does not name are not shown: each is for a task about "
                     "its own party.)")
    if note:
        lines.append(f"({note})")
    if hits:
        lines.append("more: gui recall --runs --task ID reads one run; --around N shows the texts around number N.")
        lines.append(NOTE)
    lines.append("=== END PAST RUNS ===")
    return "\n".join(lines)


def render_run(run):
    if run is None:
        return ("=== PAST RUN ===\nThat run is about a party this task does not name, so it is not shown here.\n"
                "=== END PAST RUN ===")
    lines = [f"=== PAST RUN {run.get('task_id')} ===",
             f"outcome: {run.get('outcome') or run.get('status')}; texts: {run.get('total')}"]
    lines.extend(_message_lines(run.get("messages")))
    if run.get("note"):
        lines.append(f"({run['note']})")
    lines.append(NOTE)
    lines.append(f"=== END PAST RUN {run.get('task_id')} ===")
    return "\n".join(lines)


def main(words, task="", around=None, window=None, since=""):
    """What `gui recall --runs` prints; raises SearchError when the control plane cannot answer."""

    if task:
        return render_run(read(task, around=around, window=window))
    if not str(words or "").strip():
        raise SearchError("give words to search for, or --task ID to read one run")
    return render_search(*search(words, since=since))
