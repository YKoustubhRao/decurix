"""
Choosing at the step: a value a taught step picks from what the application lists, chosen when the replay reaches it.

A value the request and its documents do not give was a hole for the model (skill_task "Driven part by part"): it was
handed the screen, looked, clicked one option and reported - about seven turns and 100-200k tokens for one click, on
every case. Measured in the learning curves (2026-11-17): of a month's GSTR-1 filing, the exported file's name and the
signatory - two such picks - were a third of the cost, the same two holes month after month.

A pick is a choice among what the screen shows, so it is made at the step, in order of cost:

  1. remembered - the entry this party had in a run that ended checked done (keyed by the words of the party's own
     request that the entry was chosen beside), when the list still has it;
  2. the only one - a list with one real entry has no choice in it;
  3. by analogy with the demonstration - the person's entry held words their own request had (their GSTIN, their
     month): the entry holding this request's words of the same kinds (this GSTIN, this month) is this run's, when
     exactly one does;
  4. asked - one model call with the entries, the request and what the person chose then; an answer that is not one of
     the entries, whole, chooses nothing.

What is chosen is said in the trace, kept for the task, and remembered only when the task ends checked done
(settle, from messaging.write_final_reply). Choosing nothing stops the step as before: the model takes it.

  ~/.decurix/choices.json  {skill: {param: [{"value", "context", "at"}]}}  (remembered)
  ~/.decurix/choices-pending.json  {task: [{"skill", "param", "value", "context"}]}
"""

import json
import re
import threading
import time

from decurix_agent import config, filelock
from decurix_agent.logging_utils import log

PATH = config.DECURIX_DIR / "choices.json"
# How much of each of the request's documents, and of all of them, a choice asked of the model is shown.
DOC_CHARS = 5000
DOCS_CHARS = 20000
PENDING = config.DECURIX_DIR / "choices-pending.json"
MAX_REMEMBERED = 40
_lock = threading.Lock()

MONTHS = ["january", "february", "march", "april", "may", "june", "july", "august", "september", "october",
          "november", "december"]
PLACEHOLDER = re.compile(r"^\s*(-+\s*)?(select|choose|none|pick)\b.*$|^\s*-+\s*$|^\s*$", re.IGNORECASE)
TOKEN = re.compile(r"[A-Za-z0-9]+")

ASK_SYSTEM = (
    "A worker replays steps a person taught. At this step the person chose one entry from a list the application "
    "shows; this run must choose its own. You are given the request, what the person chose when they taught it (and "
    "their request then), and the entries the list has now. Answer with the one entry this request's work needs, "
    "copied exactly from the list - or null when the request and the list do not settle it. JSON only: "
    "{\"choice\": \"...\" or null, \"why\": \"one short sentence\"}."
)


def tokens(text):
    """The words of a text, lower-cased, each month name with its three-letter form beside it."""

    out = []
    for t in TOKEN.findall(str(text or "")):
        low = t.lower()
        out.append(low)
        if low in MONTHS:
            out.append(low[:3])
    return out


def shape(token):
    """A word's kind: its length, and whether it has digits and letters ("29aalfs8842n1z6" and "29aagfk7314l1zy" are of
    one kind - two tax numbers - however their letters and digits fall)."""

    return len(token), any(c.isdigit() for c in token), any(c.isalpha() for c in token)


def real(entries):
    """The entries that are entries - not a "Select" placeholder or a blank."""

    return [e for e in dict.fromkeys(" ".join(str(x).split()) for x in entries or []) if not PLACEHOLDER.match(e)]


def by_analogy(example, demo_context, entries, context):
    """The entry holding this request's words of the kinds the person's entry held of theirs, when exactly one does."""

    demo = set(tokens(demo_context))
    identity = [t for t in dict.fromkeys(tokens(example)) if t in demo and len(t) >= 3]
    if not identity:
        return None
    mine = set(tokens(context))
    # (A word of the person's that this request has too - a constant like "gstr1" - is matched as itself.)
    wanted = []
    for t in identity:
        if t in mine:
            wanted.append({t})
            continue
        kin = {u for u in mine if shape(u) == shape(t) and u not in demo}
        if not kin:
            return None
        wanted.append(kin)
    held = [e for e in entries if all(any(k in set(tokens(e)) for k in kin) for kin in wanted)]
    return held[0] if len(held) == 1 else None


def _load(path):
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _change(path, change):
    try:
        with _lock:
            path.parent.mkdir(parents=True, exist_ok=True)
            with open(str(path) + ".lock", "a+") as guard:
                filelock.exclusive(guard)
                data = _load(path)
                if change(data):
                    from decurix_agent.computer import privacy
                    privacy.write_scrubbed(path, data)
    except (OSError, ValueError, TypeError) as exc:
        log(f"choices: not kept ({exc})")


def remembered(skill, param, entries, context):
    """The entry this party had before (its context's words all in this request), when the list still has it."""

    mine = set(tokens(context))
    for item in reversed((_load(PATH).get(skill) or {}).get(param) or []):
        party = set(item.get("context") or [])
        if party and party <= mine and item.get("value") in entries:
            return item["value"]
    return None


def _party(context, value):
    """The words of the request a remembered entry is kept under: its names and numbers, not its common words."""

    words = [t for t in dict.fromkeys(tokens(context)) if len(t) >= 4 and (any(c.isdigit() for c in t) or t.isalpha())]
    # (The request's own identifying words: a number with letters - a tax number - and the capitalised names.)
    named = [t for t in words if any(c.isdigit() for c in t) and any(c.isalpha() for c in t)]
    return named or words[:6]


def ask(skill, param, label, example, demo_context, entries, context, documents=None):
    """One model call choosing among the entries; an answer not one of them, whole, chooses nothing. `documents`: the
    one item's own documents (a record made for one bill), read before the request's."""

    from decurix_agent import binding, llm_client
    if not llm_client.available():
        return None, ""
    content = (f"The request now:\n{context}\n\nThe list: {label}\nIts entries now:\n"
               + "\n".join(f"- {e}" for e in entries)
               + f"\n\nWhen the person taught it, they chose: {example}\nTheir request then: {demo_context}")
    # The request's documents, when it names any: an entry a document settles (a place of supply, a rate) is chosen
    # from it, never guessed from the request's words alone.
    docs, used = [], 0
    own = list(documents or [])
    if own:
        content += "\n\nThis entry is for the record of this document: " + ", ".join(p.rsplit("/", 1)[-1] for p in own)
    for path in list(dict.fromkeys(own + binding.referenced_files(context) + binding.folder_files(context))):
        if used >= DOCS_CHARS:
            break
        try:
            from decurix_agent.tools import office
            text = office.read_document(path)["text"][:DOC_CHARS]
        except Exception:  # noqa: BLE001 - a document not read is left out
            continue
        docs.append(f"--- {path.rsplit('/', 1)[-1]} ---\n{text}")
        used += len(text)
    if docs:
        content += "\n\nThe request's documents:\n" + "\n\n".join(docs)
    # What the person said when they taught it ("Regular when it has a GSTIN"): their rule chooses before any guess.
    try:
        from decurix_agent.computer import skills as skill_store
        notes = [str(n.get("text") or "") for n in (skill_store.load(skill) or {}).get("notes") or []
                 if isinstance(n, dict) and n.get("text")] if skill else []
    except Exception:  # noqa: BLE001 - no notes read: chosen without them
        notes = []
    if notes:
        content += "\n\nWhat the person said when they taught it:\n" + "\n".join(f"- {n}" for n in notes[:12])
    try:
        message, _ = llm_client.chat([{"role": "system", "content": ASK_SYSTEM}, {"role": "user", "content": content}],
                                     max_tokens=4000, response_format={"type": "json_object"},
                                     purpose="py.replay.choose")
        found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
        data = json.loads(found.group(0)) if found else {}
    except Exception as exc:  # noqa: BLE001 - not asked: the step stops as before
        log(f"choices: the model could not be asked ({exc})")
        return None, ""
    choice = " ".join(str(data.get("choice") or "").split())
    return (choice if choice in entries else None), " ".join(str(data.get("why") or "").split())[:200]


def choose(skill, param, label, example, demo_context, entries, context, documents=None):
    """(entry, how) for a step whose value this run has not: remembered, the only one, by analogy, or asked; (None,
    why) when nothing settles it."""

    entries = real(entries)
    if not entries:
        return None, "the list has no entries"
    found = remembered(skill, param, entries, context)
    if found:
        return found, "the entry this party had in a run checked done"
    if len(entries) == 1:
        return entries[0], "the only entry the list has"
    found = by_analogy(example, demo_context, entries, context)
    if found:
        return found, "the entry holding this request's words where the person's held theirs"
    found, why = ask(skill, param, label, example, demo_context, entries, context, documents=documents)
    if found:
        return found, "asked once: " + why
    return None, "nothing settles which of " + ", ".join(entries[:6])


def note(task_id, skill, param, value, context):
    """Keep a choice made for this task, remembered only if the task ends checked done (settle)."""

    if not task_id:
        return

    def change(data):
        data.setdefault(task_id, []).append({"skill": skill, "param": param, "value": value,
                                             "context": _party(context, value)})
        for k in list(data)[:-20]:
            data.pop(k, None)
        return True

    _change(PENDING, change)


def settle(task_id, done):
    """The task ended: its choices are remembered when it ended checked done, and let go either way. Never raises."""

    if not task_id:
        return
    pending = (_load(PENDING).get(task_id) or [])
    if pending:
        _change(PENDING, lambda data: data.pop(task_id, None) is not None)
    if not done or not pending:
        return

    def change(data):
        for item in pending:
            kept = data.setdefault(item["skill"], {}).setdefault(item["param"], [])
            kept[:] = [k for k in kept if set(k.get("context") or []) != set(item["context"])]
            kept.append({"value": item["value"], "context": item["context"], "at": int(time.time())})
            del kept[:-MAX_REMEMBERED]
        return True

    _change(PATH, change)
