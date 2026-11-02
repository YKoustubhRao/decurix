"""
What the sources said that a later task will need: kept (docs/learning-loop.md).

Lessons keep what the person answered; nothing kept what the world said. Found live (2026-10-23): one task read a
client's message - a bill cancelled, "ignore it" - while saving that month's bills; the next task on the same month
found the bill in the folder, the register without it and a payment made without it, and nothing to say why. A person
doing both tasks would have remembered the message.

At the end of an engine task, whatever its outcome (what a source said is so whether or not the task finished), the
model is given what the task read and keeps up to MAX_FACTS statements a later task would need and could not easily find
again: a source's own words about a record, a party or an account that change how it is treated (cancelled, corrected,
replaced, paid, an instruction or a decision). Each quote is checked in code to stand in what the task read, and is
kept in the worker's memory (messaging.remember, category "fact") about the client the request names - shown only to a
later task about that client (memory_context._scoped); a request that names no client keeps no facts, and no fact
holding a secret is kept (keep). Best-effort and off the task's thread: a fact not kept never delays or fails the task.
"""

import json
import re
import threading

from decurix_agent import config, llm_client
from decurix_agent.logging_utils import log

MAX_FACTS = 5
# Where things are: found live (2026-10-23), the next task had no address for the chat the task before had read, and
# told the person the chat was not on its desktop.
MAX_PLACES = 4
MAX_READ_CHARS = 150_000
# How long a task's report waits for its facts to be kept (messaging.write_final_reply): longer, they go on being kept
# behind it.
WAIT_S = 90
# A quote this short says too little about one record to keep (a column's header, a status word).
MIN_QUOTE_WORDS = 4

SYSTEM = f"""A worker has finished a task (below: the request, and everything it read). Keep what a LATER task would \
need to know and could not easily find again: what a PERSON OR PARTY SAID in a message, a mail, a chat or a note about \
records, parties or accounts that changes how they are to be treated - a document cancelled, corrected or replaced, a \
payment made, an instruction or a decision. A later task works from the files, registers and systems; it does not see \
what someone wrote in passing. Never a system's screen, a report, a file's or register's contents (past entries, \
filings, totals - a later task reads them again), never what the worker did or concluded, and never the instructions \
of the person the worker works for (what they told it how to work is kept elsewhere): only what others - clients, \
customers, suppliers, colleagues - said in the sources. Each in their exact words. At most {MAX_FACTS}; none is a \
fine and common answer.
Keep also where the sources a later task will need are: each web address or folder the task read from, with what is \
kept there (whose messages, which documents) - exactly as it appears in the request or what was read. At most \
{MAX_PLACES}.
Never a password, PIN, code or key, whoever wrote it.
Reply with JSON only: {{"client": "the client the REQUEST is for, exactly as the request names them; empty when it \
names none", "facts": [{{"about": "the record or party it concerns, as the source names them", "quote": "the source's \
exact words", "where": "the source (file, page, chat, sender) and its date when it shows one"}}], "places": \
[{{"what": "what is kept there", "where": "the address or folder"}}]}}"""


def _words(text):
    return re.findall(r"[^\W_]+", str(text or "").lower())


def _stands_in(quote, read_words):
    """Whether a quote's words stand, in order and together, in what was read (as word lists)."""

    q = _words(quote)
    if len(q) < MIN_QUOTE_WORDS:
        return False
    joined = " " + " ".join(read_words) + " "
    return (" " + " ".join(q) + " ") in joined


def what_was_read(task_id):
    """Everything a task's tools read (the screen notes, and the other commands' outputs), as one text; "" when none."""

    parts = []
    try:
        from decurix_agent.computer import evidence
        parts.append(evidence.read(task_id) or "")
        try:
            with open(evidence.NOTES_DIR / f"{task_id}.outputs", encoding="utf-8") as f:
                for line in f:
                    try:
                        parts.append(str(json.loads(line).get("text") or ""))
                    except ValueError:
                        continue
        except OSError:
            pass
    except Exception as exc:  # noqa: BLE001 - nothing read, nothing kept
        log(f"Facts: could not read what the task read: {exc}")
    # A page looked at ten times is read once: repeated lines are dropped, keeping where each was first read. Found live
    # (2026-10-23): with the last 60,000 characters only, the chat read early in the task was gone, and the workbook's
    # entries read at its end filled every place.
    seen, lines = set(), []
    for line in "\n".join(p for p in parts if p.strip()).splitlines():
        key = " ".join(line.split())
        if key and key not in seen:
            seen.add(key)
            lines.append(line)
    text = "\n".join(lines)
    if len(text) <= MAX_READ_CHARS:
        return text
    head = MAX_READ_CHARS * 3 // 5
    return text[:head] + "\n...\n" + text[-(MAX_READ_CHARS - head):]


def _answer(request, read):
    """The model's answer about what a task read: {"facts": [...], "places": [...]}, or {}."""

    if not read.strip() or not llm_client.available():
        return {}
    message, _ = llm_client.chat([{"role": "system", "content": SYSTEM},
                                  {"role": "user", "content": f"THE REQUEST:\n{request[:3000]}\n\nWHAT IT READ:\n{read}"}],
                                 max_tokens=1500)
    found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
    try:
        answer = json.loads(found.group(0)) if found else {}
    except ValueError:
        return {}
    return answer if isinstance(answer, dict) else {}


def _facts(answer, read):
    """The answer's facts whose quotes stand in what was read, as [{about, quote, where}]."""

    read_words = _words(read)
    kept = []
    for fact in answer.get("facts") or []:
        if not isinstance(fact, dict) or not _stands_in(fact.get("quote"), read_words):
            continue
        kept.append({k: " ".join(str(fact.get(k) or "").split())[:400] for k in ("about", "quote", "where")})
        if len(kept) == MAX_FACTS:
            break
    return kept


def _places(answer, request, read):
    """The answer's places whose address stands, as written, in the request or what was read, as [{what, where}]."""

    kept = []
    for place in answer.get("places") or []:
        if not isinstance(place, dict):
            continue
        where = " ".join(str(place.get("where") or "").split())
        if len(where) < 4 or (where not in request and where not in read):
            continue
        kept.append({"what": " ".join(str(place.get("what") or "").split())[:200], "where": where[:300]})
        if len(kept) == MAX_PLACES:
            break
    return kept


def distil(request, read):
    """The facts worth keeping from what a task read, as [{about, quote, where}], each quote found in `read`."""

    return _facts(_answer(request, read), read)


def client_of(answer, request):
    """The client the request is for, as the request writes it (the model's reading, checked to stand in the request's
    own words), or "" when it names none."""

    client = " ".join(str(answer.get("client") or "").split())[:120]
    words = _words(client)
    if not words or len(" ".join(words)) < 3:
        return ""
    return client if f" {' '.join(words)} " in f" {' '.join(_words(request))} " else ""


def _holds_secret(*texts):
    """Whether any of the texts holds a secret (computer/privacy.py) - or cannot be checked: fails closed."""

    try:
        from decurix_agent.computer import privacy
        return any(t and (privacy.credential_in(t) or privacy.withhold_known(t) != t) for t in texts)
    except Exception:  # noqa: BLE001 - a fact that cannot be checked is not kept
        return True


def keep(task_id, request):
    """
    Distil and keep the facts and places of a finished task (in the worker's memory). Returns how many were kept.

    Memory is the desktop's, shared by every task whichever client it is for, so each fact is kept scoped to the
    client the request names ("About <client>": memory_context shows it only to a task that names that client), and a
    task whose request names no client keeps no facts - a value from one client's documents is never a cross-task fact
    without the scope it came from. No fact or place that holds a secret is kept (review 2026-11-02).
    """

    request, read = request or "", what_was_read(task_id)
    answer = _answer(request, read)
    facts, places = _facts(answer, read), _places(answer, request, read)
    if not facts and not places:
        return 0
    client = client_of(answer, request)
    if facts and not client:
        log(f"Facts: {len(facts)} fact(s) not kept - the request names no client to keep them for")
        facts = []
    secret = [f for f in facts if _holds_secret(f["about"], f["quote"], f["where"])]
    facts = [f for f in facts if f not in secret]
    unsafe_places = [p for p in places if _holds_secret(p["what"], p["where"])]
    places = [p for p in places if p not in unsafe_places]
    if secret or unsafe_places:
        log(f"Facts: {len(secret) + len(unsafe_places)} not kept - each holds something shaped like a secret")
    from decurix_agent import messaging
    n = 0
    for fact in facts:
        title = f"About {client}"[:120]
        about = f"{fact['about']} - " if fact["about"] else ""
        text = (f"{about}{fact['where']}: \"{fact['quote']}\"" if fact["where"] else f"{about}\"{fact['quote']}\"")
        if messaging.remember(title, text, category="fact"):
            n += 1
    for place in places:
        if messaging.remember(f"Where: {place['what'] or place['where']}"[:120], f"{place['what']}: {place['where']}",
                              category="place"):
            n += 1
    log(f"Facts: kept {len(facts)} fact(s) and {len(places)} place(s) a later task may need")
    return n


def after_task(task_id, request):
    """Keep the task's facts on a thread of their own (the caller may wait for it). Never raises; None when nothing
    is to be kept (no task, or no hosted model)."""

    if not task_id or not config.HOSTED_BRAIN:
        return None

    def run():
        try:
            keep(task_id, request)
        except Exception as exc:  # noqa: BLE001 - what is kept for later must never cost this task
            log(f"Facts: could not keep this task's facts: {exc}")

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    return thread
