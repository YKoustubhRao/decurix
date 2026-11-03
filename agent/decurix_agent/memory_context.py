import math
import re
from collections import Counter

from decurix_agent import config
from decurix_agent.logging_utils import log


# ============================================================
# Worker memory context
#
# Shared by every harness: both the Ollama loop and the Claude Code
# harness inject this into their reasoning as extra context.
#
# Memory is never pruned or rewritten: a correction is a newer entry, and each
# entry carries its date. What fits in the prompt is only a view of it, so when
# everything fits the view is everything, oldest first. When it does not, the
# room goes to what this task needs (docs/learning-loop.md, "Memory"):
#   - the newest of what the person taught (lessons, workflows), up to a third;
#   - every entry about the party the request names, then the entries sharing
#     the request's words, then the newest;
#   - a fact kept about a party (a client) only when the request names that
#     party (_scoped): a fact is never shared across clients;
# and a first line says how much is not shown and how to search it, so a thing
# not shown never reads as a thing never said.
# ============================================================

PERSON_CATEGORIES = {"lesson", "workflow"}
PERSON_SHARE = 3
_WORD = re.compile(r"[^\W_]+")
_TITLE = re.compile(r"^Title: (.*)$", re.MULTILINE)
_CATEGORY = re.compile(r"^Category: (.*)$", re.MULTILINE)


def _words(text):
    return _WORD.findall(str(text or "").lower())


def _current_request():
    """The request being worked on, from any process; "" when none is known."""

    try:
        from decurix_agent import task_context
        return task_context.current_request() or ""
    except Exception as exc:  # a view of memory never costs the task
        log(f"Could not read the current request: {exc}")
        return ""


def _entries():
    """Every memory entry, newest first: {name, text, title, category, words}."""

    out = []
    for path in sorted(config.MEMORY_DIR.glob("*.txt"), key=lambda p: p.name, reverse=True):
        try:
            content = path.read_text(encoding="utf-8").strip()
        except Exception as exc:
            log(f"Could not read memory file {path}: {exc}")
            continue
        if not content:
            continue
        title = _TITLE.search(content)
        category = _CATEGORY.search(content)
        out.append(dict(name=path.name, text=f"Memory file: {path.name}\n\n{content}",
                        title=title.group(1).strip() if title else "",
                        category=category.group(1).strip().lower() if category else "", words=_words(content)))
    return out


def _scores(entries, query):
    """Okapi BM25 of each entry for the query's words."""

    wanted = set(_words(query))
    if not wanted or not entries:
        return [0.0] * len(entries)
    frequency = Counter(w for e in entries for w in set(e["words"]) if w in wanted)
    average = sum(len(e["words"]) for e in entries) / len(entries)
    scores = []
    for e in entries:
        counts = Counter(w for w in e["words"] if w in wanted)
        score = 0.0
        for word, count in counts.items():
            idf = math.log(1 + (len(entries) - frequency[word] + 0.5) / (frequency[word] + 0.5))
            score += idf * count * 2.2 / (count + 1.2 * (0.25 + 0.75 * len(e["words"]) / (average or 1)))
        scores.append(score)
    return scores


# A place a task read from, kept for the client its request was for (kept_facts.keep): "Where (for <client>): <what>".
_PLACE_FOR = re.compile(r"^Where \(for (.+?)\): ")
_TOKEN = re.compile(r"[^\W_]+")


def _subject_of(entry):
    """The party of an "About <party>" entry or of a place kept for a client, as plain words; None for any other entry
    (a place kept before places had a client has none)."""

    title = entry["title"]
    if title.startswith("About "):
        return " ".join(_words(title[6:]))
    found = _PLACE_FOR.match(title)
    return " ".join(_words(found.group(1))) if found else None


def _scoped_kind(entry):
    """Whether an entry is kept about one party, and so shown only to a task about that party: a fact, or a place."""

    return entry["category"] in ("fact", "place")


def names_whole(name, text):
    """
    Whether `text` names `name` whole: its words in order, as words, and not as a part of a longer name. A word that
    begins with a capital right after it, or right before it (unless that word starts the sentence), continues the
    name: "Ram" is not named by "Ram Kumar", nor "ABC" by "ABC Traders Pvt Ltd". Review 2026-11-03 (E-29): a fact or a
    place kept about one client was shown to another client's task whose name held the first one's words.
    """

    want = _words(name)
    if not want:
        return False
    text = str(text or "")
    tokens = [(m.group(0), m.start(), m.end()) for m in _TOKEN.finditer(text)]
    low = [t[0].lower() for t in tokens]
    n = len(want)
    for i in range(len(tokens) - n + 1):
        if low[i:i + n] != want:
            continue
        if i + n < len(tokens):
            after = tokens[i + n]
            if after[0][:1].isupper() and not text[tokens[i + n - 1][2]:after[1]].strip():
                continue
        if i > 0:
            before = tokens[i - 1]
            starts = i - 1 == 0 or re.search(r"[.!?:;\n]", text[tokens[i - 2][2]:before[1]])
            if before[0][:1].isupper() and not starts and not text[before[2]:tokens[i][1]].strip():
                continue
        return True
    return False


def _named_subject(entries, query):
    """The party the request names; of alike names, the longest one named whole."""

    named = {s for s in map(_subject_of, entries) if s and names_whole(s, query)}
    return max(named, key=len) if named else None


def _rank(entries, query):
    """Entries in the order the room goes to them (see the top of this file)."""

    scores = _scores(entries, query)
    matching = [i for i in sorted(range(len(entries)), key=lambda k: (-scores[k], k)) if scores[i] > 0]
    named = _named_subject(entries, query)
    if named:
        # This party's own entries first; another party's come last whatever their words: a name alike is not the
        # same party.
        mine = {i for i, e in enumerate(entries) if _subject_of(e) == named}
        others = {i for i, e in enumerate(entries) if _subject_of(e) not in (None, named)}
        order = [i for i in matching if i in mine]
        order += [i for i in range(len(entries)) if i in mine and i not in order]
        order += [i for i in matching if i not in mine and i not in others]
    else:
        order = matching
    chosen = set(order)
    return order + [i for i in range(len(entries)) if i not in chosen]


def _take(entries, order, room, chosen):
    """Adds entries in order while they fit; returns the room left."""

    for i in order:
        size = len(entries[i]["text"])
        if i not in chosen and size <= room:
            chosen.add(i)
            room -= size
    return room


def get_memory_context(query=None):
    """
    Load persisted worker memory files up to config.MEMORY_CONTEXT_CHARS of
    content, so the agent reasons with its learned workflows without the prompt
    growing without bound as more entries pile up (memory is never pruned, and
    sharing/spawning can copy another desktop's whole history in at once).

    `query` is what the view is chosen for: by default the current request.
    The same request and memory give the same text, so a task's prompt prefix
    stays byte-identical across its model calls.
    """

    query = _current_request() if query is None else query
    entries, hidden = _scoped(_entries(), query)
    note = (f"({hidden} fact(s) or place(s) kept about parties this request does not name are not shown: each is for "
            "a task about its own party.)\n\n") if hidden else ""
    if not entries:
        return note.strip()

    whole = "\n\n---\n\n".join(e["text"] for e in reversed(entries))
    if len(note) + len(whole) <= config.MEMORY_CONTEXT_CHARS:
        return note + whole

    room = max(0, config.MEMORY_CONTEXT_CHARS - 170 - len(note))  # (the first lines)
    chosen = set()
    person = [i for i, e in enumerate(entries) if e["category"] in PERSON_CATEGORIES]
    person_room = room // PERSON_SHARE
    room -= person_room - _take(entries, person, person_room, chosen)
    _take(entries, _rank(entries, query), room, chosen)

    # A single oversized entry must not leave the worker with no memory at all.
    if not chosen:
        chosen.add(0)

    head = (f"({len(chosen)} of {len(entries)} remembered entries are shown here, chosen for this task, oldest first; "
            "the rest are not lost: gui recall WORDS searches them all.)")
    # Oldest first: chronological order for the model, so a later statement reads as the later one.
    return head + "\n\n" + note + "\n\n---\n\n".join(entries[i]["text"] for i in sorted(chosen, reverse=True))


def _scoped(entries, query):
    """(the entries a task about `query` may see, how many were left out). A fact a source stated, and a place a task
    read from, are kept about one party - a client (kept_facts.py: "About <client>", "Where (for <client>): ...") - and
    are shown only to a task whose request names that party whole (names_whole): one client's message ("bill 455 is
    cancelled") or chat address is never another client's to act on, nor to read. Found in review (2026-11-02, and
    2026-11-03 for places and for a name inside a longer one). A fact or place kept with no party is shown to no task's
    view (gui recall still finds it). Everything else is shared as before."""

    kept, hidden = [], 0
    for entry in entries:
        if _scoped_kind(entry):
            subject = _subject_of(entry)
            if not subject or not names_whole(subject, query):
                hidden += 1
                continue
        kept.append(entry)
    return kept, hidden


def search(words, limit=12):
    """The entries of the whole memory that best match `words`, oldest first, as text; "" when none does."""

    entries = _entries()
    scores = _scores(entries, words)
    best = [i for i in sorted(range(len(entries)), key=lambda k: (-scores[k], k)) if scores[i] > 0][:limit]
    return "\n\n---\n\n".join(entries[i]["text"] for i in sorted(best, reverse=True))
