"""
A person's standing procedure, given in a request, kept for the requests that follow.

People teach recurring work by saying how it is done, once: "From now on you'll
do our weekly intake. Procedure: ... build a register named ap_register_<WEEK>.xlsx
with these columns ... draft an email to ...". The next time they write
"AP intake for W42, usual procedure" or "August bank rec please" - and expect
the same deliverables. Found live (2026-10-13): a worker taught the month-end
bank reconciliation did it right, then asked for August answered in the chat
with the figures and made no workbook at all; nothing had kept the procedure,
and nothing checked the answer, because it wrote no file.

So:
  - note(request): a request that sets a standing procedure (its words say so:
    "from now on", "every month", "you'll be doing", "procedure:" ...) is kept.
    A model decides whether it does and separates the procedure from this one
    run's details (this week's date, this batch); with no model's reading nothing
    is kept (review 2026-11-03, E-28: kept by its words alone, "Whenever an entry
    has no match, flag it" - one request - became a standing procedure).
    It is a file in ~/.decurix/procedures and an entry in the worker's memory
    (category "procedure"), so a person sees it on the card.
  - apply(request): a later request for the same kind of work gets the kept
    procedure written under it, before the files it brings. The engine works
    from it, and the final check (skill_task._gate reads state.current_task)
    judges the result against it - deliverables included. With a hosted model
    the model says which procedure, if any, the request is; without one, a
    request must name the procedure by its whole name.

A request that itself gives a procedure is never given an older one: what the
person says now is the procedure.
"""

import json
import re
import time
from pathlib import Path

from decurix_agent import attachments, config, llm_client, study
from decurix_agent.logging_utils import log

CATEGORY = "procedure"
DIR = Path(config.DECURIX_DIR) / "procedures"
MAX_PROCEDURE_CHARS = 4000
MAX_KEPT = 20
MIN_MATCH = 0.7
# The words apply() writes before a kept procedure; the final check looks for them (in_request).
APPLIED = "The standing procedure the person gave you for it is below"

# Words a person uses when what they say is meant to hold beyond this request.
STANDING = re.compile(
    r"\b(from now on|going forward|from here on|in future|each (?:week|month|day|quarter|time|morning)|"
    r"every (?:week|month|day|quarter|time|morning|month-end|monday|friday)|twice a (?:week|month)|whenever|"
    r"you(?:'ll| will) (?:be )?(?:doing|do|handle|handling|run|running|maintain|maintaining|take over|answer|"
    r"process|route|audit|calculate|prepare|produce|screen|turn)|you(?:'re| are) taking over|"
    r"(?:here is|here's) the procedure|procedure:|our process|the same way every|same way each|follow it exactly|"
    r"keep a running|after each|for each (?:new|incoming))\b", re.IGNORECASE)

FILLER = set("""a an and the of to for in on at by with from as is are be this that these those it its our my your we you i
please now today usual usually same always again new next last week month year time run do make get send attached file files
procedure way as before drill""".split())

KEEP_SYSTEM = """A person gave a computer worker a request. Some requests set a standing procedure for work that will recur (every week, every month, each time something arrives) - how it is done, what is produced and how it is checked. Decide whether this request does, and if so write the procedure down so the worker can follow it on later requests that only say "do it as usual".

Keep the procedure in the person's own terms and keep every rule, threshold, formula, column list, sheet name, file-name pattern (with its placeholders, e.g. report_<PERIOD>.xlsx), recipient address, email content and "never/only/draft only" instruction. Write it as numbered steps when the person did.

Leave out what is only for this one run: this week's or this month's date or period, the particular batch or files attached now, a value given for this run alone. Where the person wrote such a value into a file name or subject, keep the pattern, not the value.

A request that only asks for one piece of work, with no sign it will recur, sets no procedure.

Answer with JSON: {"keep": true|false, "name": "3 to 6 words naming the kind of work", "applies_when": "one sentence: which later requests this procedure is for", "procedure": "the procedure", "why": "one short sentence"}"""

MATCH_SYSTEM = """A computer worker keeps the standing procedures its person taught it for recurring work. A new request has arrived. Decide which kept procedure, if any, the request is asking to be carried out - the same kind of work, possibly for another period, batch or set of files, possibly worded very differently ("usual drill", "same as always", just the name of the work).

Choose none when the request is a different kind of work, a question about a procedure, or a change to one; when two would fit equally, choose none.

Answer with JSON: {"procedure": <the number of the procedure, or 0 for none>, "confidence": <0 to 1>, "why": "one short sentence"}"""


def _squash(text, limit):
    text = re.sub(r"\s+", " ", str(text or "")).strip()
    return text if len(text) <= limit else text[:limit].rstrip() + "..."


def _slug(name):
    return re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-")[:60] or "procedure"


def sets_procedure(request):
    """Whether a request's own words say it is meant to hold beyond this request."""

    words = attachments.text(request or "")
    return len(words) > 160 and bool(STANDING.search(words))


def kept():
    """The kept procedures, newest first."""

    out = []
    for path in DIR.glob("*.json") if DIR.is_dir() else []:
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if data.get("procedure"):
            data["file"] = path.name
            out.append(data)
    return sorted(out, key=lambda d: d.get("updated") or 0, reverse=True)


def _save(name, applies_when, procedure, said, task_id):
    DIR.mkdir(parents=True, exist_ok=True)
    record = {"name": name, "applies_when": applies_when, "procedure": procedure, "said": _squash(said, MAX_PROCEDURE_CHARS),
              "task_id": task_id, "updated": time.time()}
    path = DIR / f"{_slug(name)}.json"
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(path, record, json_kw={"indent": 1, "ensure_ascii": False})
    for old in kept()[MAX_KEPT:]:
        try:
            (DIR / old["file"]).unlink()
        except OSError:
            pass
    return record


def note(request, task_id=""):
    """
    Keep the standing procedure a request sets, if it sets one. Returns the kept
    record or None. Never raises: a procedure that could not be kept must not
    fail the request that gave it.
    """

    try:
        if not sets_procedure(request):
            return None
        words = attachments.text(request)
        if llm_client.available():
            try:
                data = study._call(KEEP_SYSTEM, f"The request:\n{_squash(words, 8000)}", max_tokens=1500)
            except (llm_client.LLMError, ValueError) as exc:
                log(f"procedures: the model could not be asked ({exc}); keeping the request as it was said")
                data = None
            if data is not None:
                if data.get("keep") is not True:
                    log(f"procedures: nothing standing in this request ({_squash(data.get('why'), 120)})")
                    return None
                name = _squash(data.get("name"), 60)
                procedure = str(data.get("procedure") or "").strip()[:MAX_PROCEDURE_CHARS]
                if name and procedure:
                    record = _save(name, _squash(data.get("applies_when"), 300), procedure, words, task_id)
                    _remember(record)
                    log(f"procedures: kept {name!r}")
                    return record
        # No reading of the request: whether it sets a standing procedure is not decided by its words.
        log("procedures: no model's reading of this request, so no standing procedure is kept from it")
        return None
    except Exception as exc:
        log(f"procedures: could not keep the procedure: {exc}")
        return None


def _remember(record):
    from decurix_agent.messaging import remember
    remember(f"Procedure: {record['name']}", (record.get("applies_when") + "\n\n" if record.get("applies_when") else "")
             + record["procedure"], category=CATEGORY)


def _distinctive(text):
    return {w for w in re.findall(r"[a-z][a-z-]{2,}", (text or "").lower()) if w not in FILLER}


def _names_whole(text, name):
    """Whether `text` holds every word of `name`, as whole words in the same order and next to each other."""

    tokens = re.findall(r"[a-z0-9]+", (name or "").lower())
    return bool(tokens) and re.search(r"(?<![a-z0-9])" + r"[^a-z0-9]+".join(map(re.escape, tokens)) + r"(?![a-z0-9])",
                                      (text or "").lower()) is not None


def match(request):
    """The kept procedure a request asks to be carried out, or None."""

    if sets_procedure(request):
        return None
    procedures = kept()
    if not procedures:
        return None
    words = attachments.text(request or "")
    if llm_client.available():
        listing = "\n".join(f"{n}. {p['name']} - {p.get('applies_when') or _squash(p['procedure'], 200)}"
                            for n, p in enumerate(procedures, 1))
        try:
            data = study._call(MATCH_SYSTEM, f"Kept procedures:\n{listing}\n\nThe new request:\n{_squash(words, 3000)}", max_tokens=200)
        except (llm_client.LLMError, ValueError) as exc:
            log(f"procedures: the model could not be asked which procedure applies ({exc})")
            data = None
        if data is not None:
            try:
                pick, confidence = int(data.get("procedure") or 0), float(data.get("confidence") or 0)
            except (TypeError, ValueError):
                return None
            if 1 <= pick <= len(procedures) and confidence >= MIN_MATCH:
                return procedures[pick - 1]
            return None
    # No model: only a request that names a kept procedure by its whole name (its words, in order, as whole words)
    # asks for it; two shared words do not (review 2026-11-03, E-28). Two that are both named: none.
    named = [p for p in procedures if _names_whole(words, p["name"])]
    return named[0] if len(named) == 1 else None


def apply(request):
    """
    The request as the engine and the final check should read it: with the kept
    procedure it asks for written under the person's words (before the files it
    brings), or unchanged when none applies.
    """

    try:
        found = match(request)
    except Exception as exc:
        log(f"procedures: could not look for a kept procedure: {exc}")
        return request
    if not found:
        return request
    log(f"procedures: request follows the kept procedure {found['name']!r}")
    words, paths = attachments.split(request)
    return (f"{words}\n\nThis is the recurring work \"{found['name']}\". {APPLIED} - follow it in full, every deliverable included (files with their names and layout, emails); where this "
            f"request says something different, this request wins:\n{found['procedure']}"
            + (f"\n\n{attachments.block(paths)}" if paths else ""))


def in_request(request):
    """Whether a request carries a kept procedure (apply wrote one under it)."""

    return APPLIED in (request or "")
