import difflib
import re
import threading
import time
import urllib.parse

from decurix_agent.computer import learning, recording, skills

# ============================================================
# Mimic: do a new request the way a person recorded a similar one
#
# The desktop's own model is small, so what a person showed is the plan and
# the model is not. A request is compared, word by word, with the task each
# recorded ticket (and each taught skill) was made for:
#
#   recorded   google "koustubh is great"   typed: koustubh is great
#   request    google "sanmayee is great"
#
# The words the two share, and in particular every word of the ticket that
# is *not* something the person typed or clicked ("google"), are the task's
# shape; they must all be there. What differs ("koustubh" -> "sanmayee") is
# the part to change, and it is changed where the recording has it: in what
# was typed, the name of a control that was clicked, a page that was opened,
# and the window titles and results the steps expect. Nothing is read into
# the words themselves - "google" is not "a web search", it is whatever the
# person did when they recorded it.
#
# Only a difference the recording does not hold literally needs any
# intelligence: a value in the ticket ("5 March", "INV-1042", a quoted
# phrase) that was typed in another form ("05/03/2026"). That one question -
# what to type now instead of what was typed then - is put to the desktop's
# model (adapt_values), and to the person when there is no model or it cannot
# say. Anything else that differs - a word that is not a value and is nowhere
# in the recording ("bing" for "google") - means it is a different task, and
# the request goes to the engine as before.
# ============================================================

# Words that change nothing about what is being asked.
FILLER = {
    "please", "pls", "can", "could", "would", "you", "kindly", "just", "now", "again", "for", "me",
    "the", "a", "an", "hey", "hi", "ok", "okay",
}

WORD = re.compile(r"[^\W_]+")
QUOTED = re.compile(r"\"[^\"]*\"|'[^']*'|“[^”]*”|‘[^’]*’|`[^`]*`")

# A difference that brings in a second job - "google x and email it to bob" -
# is not an instance of the recording; the engine plans it.
SECOND_JOB = re.compile(
    r"\b(then|after\s+that|and\s+(?:then\s+)?(?:save|send|email|mail|open|copy|write|tell|message|share|download|"
    r"reply|post|print|delete|remove|create|click|close|pay|book|buy|add|go|summari[sz]e|translate))\b|;",
    re.IGNORECASE,
)

MAX_SPANS = 3

# How long a plan for the same request is reused: the watcher asks about a
# queued request every pass while the backend comes up.
CACHE_SECONDS = 5

_cache = {}
_cache_lock = threading.Lock()


# ============================================================
# Reading texts
# ============================================================

def _words(text):
    """(lower-cased word, start, end) for each word of `text`."""

    return [(m.group(0).lower(), m.start(), m.end()) for m in WORD.finditer(text or "")]


def _phrase(tokens):
    """A pattern matching these words in order, whatever lies between them, as whole words."""

    return re.compile(r"(?<![^\W_])" + r"[\W_]+".join(re.escape(t) for t in tokens) + r"(?![^\W_])", re.IGNORECASE)


def _in_quotes(text, start):
    return any(m.start() < start < m.end() for m in QUOTED.finditer(text or ""))


def _value_like(text, words, i1, i2):
    """
    Whether ticket words [i1, i2) read as a value rather than as part of the
    task: a number, words in quotes, or a name (capitalised, not the first word).
    """

    for index in range(i1, i2):
        word, start, _ = words[index]
        original = text[start:start + len(word)]
        if any(ch.isdigit() for ch in word) or _in_quotes(text, start):
            return True
        if index > 0 and original[:1].isupper():
            return True

    return False


# ============================================================
# What a recording holds that a difference can land on
# ============================================================

def _places_from_events(events):
    """Texts in a recording a changed value can be put into: what was typed, controls clicked, pages opened."""

    places = []
    for event in events:
        kind = event.get("kind")
        if kind == "type" and event.get("text") and not event.get("withheld"):
            places.append(("typed", event["text"]))
        name = ((event.get("element") or {}).get("name") or "").strip()
        if name and kind in ("click", "double_click", "right_click", "select", "type"):
            places.append(("control", name))
        if kind == "open" and event.get("target"):
            places.append(("page", urllib.parse.unquote_plus(str(event["target"]))))
    return places


def _places_from_skill(skill):
    places = []
    for param in skill.get("params") or []:
        if param.get("example") and not param.get("list"):
            places.append(("typed", str(param["example"])))
    for step in skill.get("steps") or []:
        name = ((step.get("target") or {}).get("name") or "").strip()
        if name:
            places.append(("control", name))
        if step.get("open"):
            places.append(("page", urllib.parse.unquote_plus(str(step["open"]))))
    return places


# ============================================================
# Comparing a request with a recorded task
# ============================================================

def align(recorded, request, places):
    """
    How `request` differs from the `recorded` task, if it is the same task with
    other values: {"spans": [...], "shared": n}, or None when it is not.

    Each span is {"old": ticket words, "new": the request's text for them,
    "find": the words to look for in the recording, "placed": whether the
    recording holds them literally, "value": whether they read as a value}.
    """

    old_words, new_words = _words(recorded), _words(request)
    if not old_words or not new_words:
        return None

    texts = [text for _, text in places]
    content = set()
    for text in texts:
        content.update(word for word, _, _ in _words(text))

    matcher = difflib.SequenceMatcher(None, [w for w, _, _ in old_words], [w for w, _, _ in new_words], autojunk=False)
    opcodes = matcher.get_opcodes()

    # The task's shape: every ticket word that is not filler, not anything the
    # person typed or clicked, and not a value (a number, a quoted phrase, a
    # name) must be there, unchanged.
    action = [i for i, (w, _, _) in enumerate(old_words)
              if w not in FILLER and w not in content and not _value_like(recorded, old_words, i, i + 1)]
    if not action:
        # Every word of the ticket turned up somewhere in the recording, which
        # says nothing when the recording is of web pages: a search's result
        # page has "search" in its address and in half its controls. Found
        # live: a ticket `search "koustubh rao"`, taught, and then `search
        # "sundar pichai"` matched nothing - not even the ticket's own words -
        # and went to the engine. What the person *typed* is still a value;
        # a word they only clicked past is the task's.
        typed = set()
        for kind, text in places:
            if kind == "typed":
                typed.update(word for word, _, _ in _words(text))
        action = [i for i, (w, _, _) in enumerate(old_words)
                  if w not in FILLER and w not in typed and not _value_like(recorded, old_words, i, i + 1)]
    kept = set()
    for tag, i1, i2, _, _ in opcodes:
        if tag == "equal":
            kept.update(range(i1, i2))
    if not action or any(i not in kept for i in action):
        return None

    spans = []
    for tag, i1, i2, j1, j2 in opcodes:
        if tag == "equal":
            continue

        old = [old_words[i][0] for i in range(i1, i2)]
        new_tokens = [new_words[j][0] for j in range(j1, j2)]
        if all(w in FILLER for w in old + new_tokens):
            continue

        new_text = request[new_words[j1][1]:new_words[j2 - 1][2]] if j2 > j1 else ""
        if new_text and SECOND_JOB.search(new_text):
            return None

        span = _place(old_words, new_words, request, i1, i2, j1, j2, places)
        if span is None:
            # Not in the recording as it stands: a value typed in another
            # form can be adapted; any other word makes it another task.
            if not (i2 > i1 and _value_like(recorded, old_words, i1, i2)):
                return None
            span = {"old": " ".join(old), "new": new_text, "find": old, "placed": False, "value": True, "where": []}
        spans.append(span)

    if len(spans) > MAX_SPANS:
        return None

    return {"spans": spans, "shared": sum(1 for i in kept if old_words[i][0] not in FILLER)}


def _place(old_words, new_words, request, i1, i2, j1, j2, places):
    """
    The span as it can be found in the recording's texts: the changed words
    themselves, or - for words only added or taken away, or not found bare -
    with the unchanged word on either side of them.
    """

    variants = [(i1, i2, j1, j2)]
    if i1 > 0 and j1 > 0:
        variants.append((i1 - 1, i2, j1 - 1, j2))
    if i2 < len(old_words) and j2 < len(new_words):
        variants.append((i1, i2 + 1, j1, j2 + 1))
        if i1 > 0 and j1 > 0:
            variants.append((i1 - 1, i2 + 1, j1 - 1, j2 + 1))

    for a1, a2, b1, b2 in variants:
        if a2 <= a1:
            continue
        find = [old_words[i][0] for i in range(a1, a2)]
        pattern = _phrase(find)
        where = sorted({kind for kind, text in places if pattern.search(text)})
        if where:
            new_text = request[new_words[b1][1]:new_words[b2 - 1][2]] if b2 > b1 else ""
            return {"old": " ".join(find), "new": new_text, "find": find, "placed": True, "value": False,
                    "where": where}

    return None


# ============================================================
# Choosing what to mimic
# ============================================================

def _candidates():
    """Ready skills and finished demonstrations, each with the task it was recorded for."""

    found = []

    for skill in skills.all_skills():
        text = (skill.get("goal") or skill.get("title") or "").strip()
        if skill.get("status") == skills.READY and text:
            found.append({"kind": "skill", "id": skill["name"], "text": text, "when": skill.get("created_at") or 0,
                          "places": lambda s=skill: _places_from_skill(s)})

    for meta in recording.sessions():
        if meta.get("kind") != "demonstration" or meta.get("status") != "complete" or not meta.get("event_count"):
            continue
        text = (meta.get("task") or meta.get("title") or "").strip()
        if text:
            found.append({"kind": "recording", "id": meta["id"], "text": text, "when": meta.get("started_at") or 0,
                          "places": lambda m=meta: _places_from_events(_events(m["id"]))})

    return found


def _events(session_id):
    try:
        return recording.load(session_id)[1]
    except (FileNotFoundError, ValueError, OSError):
        return []


def plan(request):
    """
    What to mimic for `request`: {"kind": "skill"|"recording", "id", "text",
    "spans"}, or None when no recording is this task. No model: this is what
    the command watcher asks before deciding who handles a request.
    """

    text = (request or "").strip()
    if not text or text.startswith("/"):
        return None

    now = time.monotonic()
    with _cache_lock:
        hit = _cache.get(text)
        if hit and now - hit[0] < CACHE_SECONDS:
            return hit[1]

    best, best_key = None, None

    for candidate in _candidates():
        # Cheap before reading a recording's events: the task must share a word.
        if not set(w for w, _, _ in _words(candidate["text"])) & set(w for w, _, _ in _words(text)):
            continue

        found = align(candidate["text"], text, candidate["places"]())
        if found is None:
            continue

        adapted = sum(1 for span in found["spans"] if not span["placed"])
        # More of the task in common first; then fewer changes, fewer needing
        # the model; a taught skill (a person answered its questions) over the
        # recording it came from; the newest.
        key = (found["shared"], -len(found["spans"]), -adapted, candidate["kind"] == "skill", candidate["when"])
        if best_key is None or key > best_key:
            best_key = key
            best = {"kind": candidate["kind"], "id": candidate["id"], "text": candidate["text"], "spans": found["spans"]}

    with _cache_lock:
        _cache[text] = (now, best)
        if len(_cache) > 64:
            _cache.pop(next(iter(_cache)))

    return best


# ============================================================
# Turning a plan into a run
# ============================================================

def substitute(text, spans):
    """`text` with each placed span's old words replaced by the new ones (URL-encoded where the text is a URL)."""

    if not text:
        return text

    out = str(text)
    for span in spans:
        if not span["placed"]:
            continue
        pattern = _phrase(span["find"])
        out = pattern.sub(lambda _m, new=span["new"]: new, out)
        if "://" in out:
            for encode in (urllib.parse.quote_plus, urllib.parse.quote):
                old_encoded = encode(" ".join(span["find"]))
                if old_encoded.lower() in out.lower():
                    out = re.sub(re.escape(old_encoded), lambda _m, new=encode(span["new"]): new, out, flags=re.IGNORECASE)
    return out


def load_skill(found):
    """
    The skill to run for a plan, and whether it is a one-off: a taught skill as
    saved, or - for a recording nobody has taught - one built from it here with
    no questions asked, which is run and never saved.
    """

    if found["kind"] == "skill":
        return skills.load(found["id"]), False

    meta, events = recording.load(found["id"])
    skill = learning.build([(meta, events)])
    skill["status"] = skills.READY
    return skill, True


def values_for(skill, found):
    """
    Each typed value for this run: the recorded text with the request's
    changes put in, the names of the values that still need adapting (a
    difference in the ticket that nothing typed holds literally), and what was
    changed, as (label, old, new) for the chat.
    """

    values, changed = {}, []
    ticket_words = set(w for w, _, _ in _words(found["text"]))
    unplaced = [span for span in found["spans"] if not span["placed"]]

    for param in skill.get("params") or []:
        example = param.get("example")
        if param.get("list") or example is None:
            continue
        new = substitute(str(example), found["spans"])
        if not new.strip():
            # The request drops the value altogether ("google" for
            # `google "koustubh is great"`): typing nothing is not the task.
            # A person says what it is.
            param["varies"] = True
            continue
        if new != example:
            values[param["name"]] = new
            changed.append((param.get("label") or param["name"], str(example), new))
        elif param.get("varies") and not (set(w for w, _, _ in _words(str(example))) <= ticket_words):
            # Taught to change every run, and the task's words do not say
            # what it is: a person has to.
            continue
        else:
            values[param["name"]] = str(example)

    pending = [p["name"] for p in skill.get("params") or []
               if unplaced and p.get("example") and not p.get("list") and p["name"] in values
               and values[p["name"]] == str(p["example"])]

    return values, pending, changed


def adapt_values(skill, found, request, values, pending, ask_model):
    """
    The one question that needs intelligence: the ticket changed a value the
    recording typed in another form. For each value still as recorded, ask the
    model what to type now. Returns (values, changed); a value it could not say
    is left out of `values`, so the run asks the person for it. `values` is
    None when the model would change nothing: then the request is not this task.
    """

    changed, unchanged = [], 0
    system = ("You adapt one value from a task a person recorded to a new, similar task. "
              "Reply with only the new value, exactly as it should be typed. "
              "If it should stay as it is, reply with it unchanged.")

    for name in pending[:3]:
        param = next(p for p in skill["params"] if p["name"] == name)
        recorded = str(param.get("example") or "")
        prompt = (f"Recorded task: {found['text']}\n"
                  f"In it the person typed: {recorded}\n"
                  f"New task: {request}\n"
                  f"What should be typed now?")
        answer = None
        if ask_model is not None:
            try:
                answer = _clean_answer(ask_model(system, prompt), recorded)
            except Exception:
                answer = None

        if answer is None:
            values.pop(name, None)
            param["varies"] = True
        elif answer != recorded:
            values[name] = answer
            changed.append((param.get("label") or name, recorded, answer))
        else:
            unchanged += 1

    # The model looked at every value and would type each as recorded: the
    # difference is not in anything typed, so the recording would do the old
    # task under the new request's name. None: not this recording.
    if unchanged and unchanged == len(pending[:3]):
        return None, changed

    return values, changed


def _clean_answer(text, recorded):
    """A model's reply as a value to type, or None when it is not one."""

    text = (text or "").strip()
    if not text:
        return None
    line = text.splitlines()[0].strip()
    line = re.sub(r"^(answer|value|type|typed)\s*:\s*", "", line, flags=re.IGNORECASE).strip()
    if len(line) >= 2 and line[0] == line[-1] and line[0] in "\"'`":
        line = line[1:-1].strip()
    if not line or len(line) > max(3 * len(recorded), len(recorded) + 60):
        return None
    return line


def step_spans(found, skill, values):
    """
    The changes to put into a run's steps. A typed value goes in whole (see
    value_spans); the request's own words only where the recording has them
    on a control or a page. Never a lone word everywhere: "koustubh" changed in
    a search must not rename the window "Koustubh - Chrome" (a browser profile)
    that every step was recorded in.
    """

    words = [span for span in found["spans"] if span["placed"] and set(span.get("where") or []) - {"typed"}]
    return value_spans(skill, values) + words


def value_spans(skill, values):
    """
    Each typed value that differs from what was recorded, as a change to carry
    into what the steps expect: the search typed as "sanmayee is great" is then
    looked for in a window titled "sanmayee is great - Google Search", not the
    recorded one. Covers every way a value changed - the request's words, the
    model, or a person's answer.
    """

    spans = []
    for param in skill.get("params") or []:
        old, new = param.get("example"), (values or {}).get(param["name"])
        if old is None or new is None or str(new) == str(old):
            continue
        find = [w for w, _, _ in _words(str(old))]
        if find:
            spans.append({"old": str(old), "new": str(new), "find": find, "placed": True, "value": True})
    # Longest first, so a whole value is replaced before any word inside it.
    return sorted(spans, key=lambda s: -len(s["find"]))


def substitute_step(step, spans):
    """A copy of `step` with the changes put into what it targets, opens and expects."""

    if not spans:
        return step

    out = dict(step)

    if isinstance(step.get("target"), dict):
        target = dict(step["target"])
        for key in ("name", "window_title"):
            if target.get(key):
                target[key] = substitute(target[key], spans)
        if target.get("name") != (step["target"] or {}).get("name"):
            # The recorded path names the old control; find the new one by name.
            target.pop("path", None)
        out["target"] = target

    for key in ("window", "open", "summary"):
        if step.get(key):
            out[key] = substitute(step[key], spans)

    checks = []
    for check in step.get("expect") or []:
        check = dict(check)
        for key in ("name", "value"):
            if isinstance(check.get(key), str):
                check[key] = substitute(check[key], spans)
        checks.append(check)
    if "expect" in step:
        out["expect"] = checks

    return out
