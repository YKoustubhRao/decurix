"""
The code checks of a long request's stages and of its cross-check (docs/long-requests.md).

Split out of long_task.py (follow-up O-10): long_task plans a request, runs its stages and reports; this module holds
what code checks before a stage counts and before the run reports - stage_code_findings (every check of a finished
stage, each by name through _checked), the maturity of each check (CHECK_MATURITY: BLOCKING or ADVISORY), the checks
themselves and the helpers only they use, and the code part of the cross-check (code_disagreements,
model_disagreements, odd_one_out). A new check goes into stage_code_findings as ADVISORY and must pass the replay gate
(tests/test_replay_gate.py).

long_task imports this module and re-exports what other modules and tests read through it. This module never imports
long_task: the run's ledger, the workflow runner and the model call are long_task's, handed over at its import
(use_run). Nothing here knows any application or kind of business work: every check is about files, drafts, figures
and what the tools saw.
"""

import json
import os
import re
from pathlib import Path

from decurix_agent import attachments, config, progress, provenance, stage_facts, task_context
from decurix_agent import values as value_rules
from decurix_agent.logging_utils import log
from decurix_agent.provenance import declared_unit, fact_value, identifier_like, parse_number, same_fact, same_figure
from decurix_agent.stage_facts import _number


# ============================================================
# The run the checks look at: long_task's
# ============================================================
#
# The run's ledger, the workflow runner and the planner's model call live in long_task, which imports this module; so
# long_task hands them over at its import (use_run) instead of being imported here. Each is looked up in long_task when
# called: a test that patches long_task.ledger, long_task._runner or long_task._ask_json patches what the checks read.

_RUN = {}


def use_run(ledger, runner, ask_json):
    """Take the run's ledger reader, its workflow runner and its model call from long_task."""

    _RUN.update(ledger=ledger, runner=runner, ask_json=ask_json)


def ledger(run_id):
    """The run's ledger (long_task.ledger)."""

    return _RUN["ledger"](run_id)


def _runner():
    """The workflow runner (long_task._runner)."""

    return _RUN["runner"]()


def _ask_json(*args, **kwargs):
    """One model call answered in JSON (long_task._ask_json)."""

    return _RUN["ask_json"](*args, **kwargs)


# ============================================================
# What the checks read of a request and a run (long_task's prompts read these too)
# ============================================================

def input_emails(request):
    """The request's input emails (.eml): given one by one, or the ones in a folder it names."""

    out = []
    for entry in run_inputs(request):
        p = Path(entry.split(" (= ")[0])
        if p.is_dir():
            out += [str(m) for m in sorted(p.rglob("*.eml")) if m.is_file()]
        elif p.suffix.lower() == ".eml":
            out.append(str(p))
    return list(dict.fromkeys(out))


def reply_coverage(emails, drafts, report):
    """
    For drafts made "where needed": each input email is drafted for (to its sender, or under its subject) or named in
    the report with why it gets none (by its file, subject or sender). The ones with neither, as names; [] when all are.
    """

    import email
    import email.utils
    from email import policy
    text = (report or "").lower()
    to = " ".join(" ".join(str(a) for a in (d.get("to") or [])) for d in drafts or ()).lower()
    subjects = " | ".join(str(d.get("subject") or "") for d in drafts or ()).lower()
    left = []
    for path in emails:
        p = Path(path)
        try:
            message = email.message_from_bytes(p.read_bytes(), policy=policy.default)
        except OSError:
            continue
        sender = (email.utils.parseaddr(str(message.get("From") or ""))[1] or "").lower()
        subject = re.sub(r"(?i)^\s*(?:re|fwd?|fw)\s*:\s*", "", str(message.get("Subject") or "")).strip().lower()
        drafted = (sender and sender in to) or (len(subject) > 4 and subject in subjects)
        explained = (p.name.lower() in text or p.stem.lower() in text or (sender and sender in text)
                     or (len(subject) > 4 and subject in text))
        if not drafted and not explained:
            left.append(p.name)
    return left


# Work that enters records in an application (a portal, a back office, an ERP, a form): what was saved is read back
# from the application itself. Found live (round 5 P2, 2026-10-20): the entry stage checked an order by the portal's
# "Order ORD-3 was saved." and its own CSV; the saved order had a quantity of 60 for 24 and a line missing.
APP_ENTRY = re.compile(r"\b(?:enter|key|post|book|create|record|register|submit|file)\w*\b[^.\n]{0,160}?\b(?:portal|"
                       r"application|app|system|erp|website|web site|software|form|back office)\b|https?://", re.I)


def enters_records(step):
    """Whether a stage enters records in a system: the planner's "enters" when the plan has the field; else (a plan
    without it) the stage's words (APP_ENTRY) - a fallback whose system-entry findings are evidence only."""

    if "enters" in step:
        return bool(step.get("enters"))
    return bool(APP_ENTRY.search(step.get("instruction") or ""))


def _steps(state):
    return state["spec"]["steps"]


# A sentence of a request that names a system to work in or read from: a web address (with its login), a mailbox.
_SYSTEM_SENTENCE = re.compile(r"[^.\n]*(?:https?://\S+|\bmailbox\b)[^\n]*?(?:\.(?=\s|$)|$)", re.I)


def request_systems(request):
    """The request's sentences that name a system it gives (a portal at an address, a mailbox), word for word."""

    text = attachments.text(request or "")
    out = []
    for m in _SYSTEM_SENTENCE.finditer(text):
        sentence = " ".join(m.group(0).split())
        if sentence and sentence not in out and len(sentence) <= 500:
            out.append(sentence)
    return out[:6]


def run_inputs(request):
    """
    The request's own input files, found where they are: what it attached and the files it names (sources.inputs), each
    once; a file in the person's shared folder is given with their path too ("~/PC/x.xlsx = C:\\...\\x.xlsx").
    """

    found = [str(p) for p in attachments.files(request or "")]
    try:
        from decurix_agent import sources
        found += [i["path"] for i in sources.inputs(request or "")]
    except Exception as exc:  # noqa: BLE001 - what cannot be found is not listed
        log(f"long_task: could not find the request's inputs: {exc}")
    # Any file the request names by its path, of any kind - the documents a taught run's values were read from
    # (binding.referenced_files). Found live (2026-11-08): a bill's PDF named in the request was no source, since only
    # tables are found by name, and a code replay's save was refused for "invented" figures the PDF prints.
    try:
        from decurix_agent import binding
        found += binding.referenced_files(request or "")
    except Exception as exc:  # noqa: BLE001 - what cannot be found is not listed
        log(f"long_task: could not find the files the request names: {exc}")
    out = []
    try:
        from decurix_agent import shared
    except Exception:  # noqa: BLE001
        shared = None
    for path in dict.fromkeys(found):
        host = shared.to_host(path) if shared is not None else None
        out.append(f"{path} (= {host} on the person's computer)" if host else path)
    return out


# ============================================================
# The checks of a finished stage
# ============================================================

def code_problems(made, reply, drafts, request, instruction=None, root=None, outputs=None, notes=None, not_made=None):
    """
    What the final check decides about a stage's result without a model: the file facts are produced.file_facts, the
    very function skill_task._gate calls (results stored, a Total row short of its column, a file the stage names not
    made, placeholders, accounts and recipients on no record), then the sums and numbers of the report, the drafts and
    the files. `instruction` is the stage's own text (the files it is asked for); `request` the whole one. [] when
    nothing is wrong. What reads the request or needs judgement (typed totals, layout, a picture's size, page breaks,
    fills, a draft naming another party) is appended to `notes` - evidence, never a verdict (harness audit, 2026-10-21).
    A blocking check that could not be made (produced.FileFacts.could_not) is appended to `not_made`: the stage is then
    not checked, never passed on that point (verify.outcome).
    """

    from decurix_agent import produced, verify

    problems = []
    facts = produced.file_facts(request, made, named_in=instruction, root=root, drafts_made=drafts, own_outputs=outputs)
    notes = notes if notes is not None else []
    if not_made is not None:
        # (FileFacts.not_made: a check that raised, an Office file that could not be opened - E-27.)
        not_made.extend(facts.not_made())
    if facts.disclosures:
        progress.emit("An email draft may name another party's business to its recipient - check it before sending: "
                      + "; ".join(facts.disclosures)[:400])
    # The evidence among the file facts, all of it (FileFacts.notes): what reads the request or needs judgement - an
    # intentional blank, a protected file, a layout. (A draft's fields are draft_checks', said once there.)
    notes.extend(str(n) for n in facts.notes() if n not in facts.draft_fields)
    if facts.unstored:
        problems.append("A workbook has formulas with no results stored in the file (" + "; ".join(facts.unstored)[:400]
                        + "). Store them with office calculate FILE, or make it with office create / office update.")
    if facts.gaps:
        problems.append("A formula in the file shows an error (" + "; ".join(facts.gaps)[:400] + "). Correct it and "
                        "store the results.")
    if facts.missing_files:
        problems.append(facts.missing_files + ". Make it, with the content asked for, where the stage says.")
    wrong = produced.wrong_kind(facts.file_faults)
    if wrong:
        problems.append("; ".join(wrong)[:400] + ". " + produced.WRONG_KIND_REMEDY)
    unfinished = [f for f in facts.file_faults if f not in wrong]
    if unfinished:
        problems.append("The saved file is not finished: " + "; ".join(unfinished)[:400]
                        + ". Correct it in the file itself and save it again.")
    if facts.payee_mismatch:
        problems.append(" ".join(facts.payee_mismatch)[:900] + " Bank or payment account details come only from the "
                        "records the request gives; a message asking to change them is reported to the person, never "
                        "applied.")
    if facts.unknown_recipients:
        problems.append("An email draft goes to an address that is in no file the request gave ("
                        + "; ".join(facts.unknown_recipients)[:700] + "). Look each recipient up in the records by the "
                        "party's name, in a script, and replace the draft (office email --replaces <its id>).")
    unhighlighted = produced.missing_highlight(made, request)
    if unhighlighted:
        notes.append("The request mentions filled or highlighted rows and the saved workbook has none ("
                     + "; ".join(unhighlighted)[:300] + ").")
    drafts_text = produced.drafts_text(drafts)
    # Own arithmetic: a sum written out in the report or a draft ("60 x 35 = 2,100") comes to what it says. (Not the
    # files' text: office read writes a sheet as "W2=8142  X2=20", which reads as sums - the replay gate, 2026-10-21;
    # a file's own arithmetic is its formulas: stage_facts.formula_contradictions.)
    wrong = verify.false_sums("\n".join((reply or "", drafts_text)))
    if wrong:
        problems.append("A sum you wrote is wrong: " + "; ".join(wrong)[:400]
                        + ". Work every total out with a command over the source figures and correct it where it is.")
    odd = verify.program_numbers("\n".join((produced.text(made), drafts_text)))
    if odd:
        problems.append("These numbers are written as a program prints them: " + ", ".join(odd)
                        + ". Write figures the way a person does and correct them where they are.")
    return problems


def source_guard_problems(request):
    """
    The request's given files that changed during the task (source_guard.changed_sources), as evidence (ADVISORY,
    E-12): the judge gets the fact checked in code, with its diff, and the person always sees the change and where the
    original is kept, under "Please check". Nothing is put back while checking - whether the request asked for the
    change is the judge's to weigh, never a verb list's (review 2026-11-03: an in-place ledger update the request asked
    for was reverted as "you changed my file"). Raises source_guard.NotChecked when it cannot be made (_checked:
    MADE_OR_UNCHECKED).
    """

    from decurix_agent import source_guard

    facts = source_guard.changed_sources(task_context.current(), request)
    if not facts:
        return []
    told = source_guard.report_lines(facts)
    for line in told:
        progress.emit(line)
    return [f"{judged} {person}" for judged, person in zip(source_guard.judge_lines(facts), told)]


def _declare_payment_sources(run_state, request):
    """Tell payees whether this run's request gives messages as the source of payment details (the planner's field)."""

    try:
        from decurix_agent import payees
        payees.declare_message_details(request, bool(run_state["spec"].get("payment_details_from_messages")))
    except Exception as exc:  # noqa: BLE001 - not said is not given
        log(f"long_task: could not note where payment details come from: {exc}")


def _payee_approvals(run_state, request, made):
    """payees.message_payments: payments to details from a message the request gives as their source - the person
    approves each (ASK_PERSON), shown with the account and the message it came from."""

    from decurix_agent import payees
    _declare_payment_sources(run_state, request)
    return payees.message_payments(request, made)


def _payment_warnings(run_state, request):
    """Warnings for the top of the final report: payments to accounts not on record, messages that are suspicious."""

    # (A failure raises: final_report makes it "not checked" - never a pass.)
    _declare_payment_sources(run_state, request)
    from decurix_agent import payees
    work = Path(_runner().context(run_state)["work_dir"])
    made = [str(work / e["path"]) for s in _steps(run_state) for e in run_state["steps"][s["id"]]["evidence"]
            if not e.get("drafts")]
    return list(dict.fromkeys(payees.mismatches(request, made) + payees.unchecked(request, made)
                              + payees.suspicious_messages(request, made)))


# "every"/"each"/"all" in a stage's instruction: the records it covers are all of the earlier stage's.
EVERY = re.compile(r"\b(?:every|each|all)\b", re.I)
# A column that names a party (who a record is for).
PARTY_COLUMN = re.compile(r"\b(?:customer|client|vendor|supplier|party|payee|employee|account name|name)\b", re.I)
# A row a decision or rule left out of what follows.
LEFT_OUT = re.compile(r"\b(?:hold|held|cancel\w*|reject\w*|declin\w*|not approved|excluded|withdrawn|not in this run|"
                      r"skip\w*)\b", re.I)
# A column of what goes now (allocated, shipped), and one of what does not (backorder).
NOW_COLUMN = re.compile(r"\b(?:alloc\w*|ship\w*|dispatch\w*|deliver\w*)\b", re.I)
LATER_COLUMN = re.compile(r"back\s*-?\s*order|pending|short|value|amount|total", re.I)
TABLE_KINDS = {".csv", ".tsv", ".xlsx", ".xlsm"}


def _header_key(text):
    return re.sub(r"[^a-z0-9]+", " ", (text or "").lower()).strip()


# A rule by which the stage itself selects among the records (a cap, a limit, an order it fills until): its result is
# a selection, never "every record". Found live (round 5 P3, 2026-10-20): the payment proposal - whole bills in order,
# never over the cash available - was refused for leaving out the two bills the rule left out.
SELECTING = re.compile(r"\b(?:never (?:go )?over|not (?:go )?over|(?:not |never )?exceed\w*|caps?|capped|limits?|within|"
                       r"until|up to|budget|cash available|in this run|selection|selected|priority|prioriti[sz]e\w*|"
                       r"at most|maximum|shortlist\w*|top \d+|first \d+)\b", re.I)
_SCOPE_NOTE = re.compile(r"\(they decide its scope:[^)]*\)")


def covers_every(step):
    """
    Whether a stage's result must hold every record of what it works from: its instruction says every/each/all in the
    sentence that enters records in an application or names a file the stage hands in - never the boilerplate note on
    the request's words - and no rule of its own selects among the records (SELECTING).
    """

    text = _SCOPE_NOTE.sub("", step.get("instruction") or "")
    if SELECTING.search(text):
        return False
    outputs = [Path(c.get("path") or "").name.lower() for c in step.get("checks", []) if c.get("path")]
    # (Entry sentences count only for a stage that enters records: the planner's "enters", else its words.)
    entering = enters_records(step)
    for sentence in re.split(r"(?<=[.;:!?])\s+", text):
        if EVERY.search(sentence) and ((entering and APP_ENTRY.search(sentence))
                                       or any(o and o in sentence.lower() for o in outputs)):
            return True
    return False


# A stage that takes only the records meeting a condition: "a Refund over Rs 5,000", "with anything over 30 days",
# "whose papers are complete", "approved", "that need one".
_THRESHOLD = re.compile(r"(?i)(?:\b(?P<noun>[A-Za-z]+)\s+)?\b(?P<op>over|above|more than|exceeding|greater than|at least|"
                        r"under|below|less than|up to|not more than)\s+(?:(?:rs\.?|inr|₹|\$|€|£|usd|eur|gbp)\s*)?(?P<n>\d[\d,]*(?:\.\d+)?)"
                        r"(?!\s*(?:days?|%|percent|units?|pcs|items?))\b")
_CONDITION = re.compile(r"(?i)\b(?:whose|which|that|who)\s+(?:is|are|has|have|need|needs|meet|meets|qualif\w*)\b|"
                        r"\bwith\s+(?:anything|any|an?\s+\w+)\s+(?:over|above|older|more)\b|\b(?:eligible|"
                        r"complete|overdue|flagged|unpaid)\b|\bover\s+\d+\s*days?\b")
# ("approved" and "held" are a decision's, which the check already reads row by row: LEFT_OUT.)
# (Any currency's sign or code is notation, not a business word: "over $5,000" decided nothing - E-28.)
_MONEY = re.compile(r"(?i)(?:rs\.?|inr|₹|\$|€|£|usd|eur|gbp)\s*(\d[\d,]*(?:\.\d+)?)")
_STAGE_NOUNS = {"every", "each", "all", "the", "a", "an", "any", "is", "are", "of", "to", "for", "from", "with"}


def _condition_test(instruction):
    """
    How a stage selects its records, when code can decide it: a test (row -> True/False) for an amount threshold
    ("a Refund over Rs 5,000": an amount in Rs in the row over 5,000, and the row's own kind when it names one); None
    when it selects by nothing; "unknown" when it selects by a condition code cannot read (nothing is decided then).
    """

    text = _SCOPE_NOTE.sub("", instruction or "")
    text = re.sub(r"\"[^\"]{0,300}\"", " ", text)  # (the request's quoted words: its rules, not this stage's choice)
    m = _THRESHOLD.search(text)
    if not m:
        return "unknown" if _CONDITION.search(text) else None
    limit = float(m.group("n").replace(",", ""))
    op = m.group("op").lower()
    noun = (m.group("noun") or "").lower()
    noun = "" if noun in _STAGE_NOUNS else noun

    def meets(cells):
        amounts = [float(a.replace(",", "")) for c in cells for a in _MONEY.findall(str(c))]
        if noun and not any(str(c).strip().lower() in (noun, noun + "s") for c in cells):
            return False
        if op in ("over", "above", "more than", "exceeding", "greater than"):
            return any(a > limit for a in amounts)
        if op == "at least":
            return any(a >= limit for a in amounts)
        if op in ("under", "below", "less than"):
            return bool(amounts) and any(a < limit for a in amounts)
        return bool(amounts) and any(a <= limit for a in amounts)
    return meets


def coverage_problems(run_state, step, made):
    """
    A stage asked to do something for every record of an earlier stage's table, whose own table (its log) leaves some
    out - decided in code where it is precise: the stage's instruction says every/each/all, its log and a table of a
    stage it depends on share a party column (the same header), and a party there that no decision left out (held,
    cancelled ...) and that has something going now is named nowhere in the log. Found live (round 5 P2, 2026-10-20):
    five approved orders, three entered, the stage ticked done.
    """

    if not covers_every(step):
        return []
    # A stage that selects by a condition is checked only against the records meeting it - where code can tell; when
    # it cannot, nothing is decided (round 5 P6, 2026-10-21: "refunds over Rs 5,000" - 3 of the tracker's 12 - was
    # refused twice for leaving out the other 9).
    selects = _condition_test(step.get("instruction") or "")
    if selects == "unknown":
        return []
    from decurix_agent import payees
    logs = [p for p in made if Path(str(p)).suffix.lower() in TABLE_KINDS]
    log_tables = []
    for path in logs:
        try:
            log_tables += payees._tables(path)
        except Exception:  # noqa: BLE001 - a log that cannot be read decides nothing here
            continue
    keys = {_header_key(h) for header, _ in log_tables for h in header if PARTY_COLUMN.search(h or "")}
    if not keys:
        return []
    named = " ".join(" ".join(row) for _, rows in log_tables for row in rows).lower()
    work = Path(_runner().context(run_state)["work_dir"])
    owed = {}
    gathered = []
    for dep in step.get("depends_on", []):
        for item in run_state["steps"][dep]["evidence"]:
            path = work / item.get("path", "")
            if item.get("drafts") or path.suffix.lower() not in TABLE_KINDS or not path.is_file():
                continue
            try:
                tables = payees._tables(path)
            except Exception:  # noqa: BLE001
                continue
            gathered += [(path, header, rows) for header, rows in tables]
    # A party a table of these files leaves out (held, cancelled ...) is left out, whatever another sheet of them lists
    # without a status (found in the replay gate, series 10's P2: "Orders" had one customer HELD, its "Working" sheet listed
    # the same lines with no status, and the portal stage was refused for not entering it).
    left_out = set()
    for path, header, rows in gathered:
        column = next((i for i, h in enumerate(header) if _header_key(h) in keys), None)
        if column is None:
            continue
        by_party = {}
        for row in rows:
            name = row[column].strip() if column < len(row) else ""
            if name:
                by_party.setdefault(name, []).append(row)
        left_out |= {n for n, party_rows in by_party.items()
                     if all(any(LEFT_OUT.search(cell or "") for cell in row) for row in party_rows)}
    for path, header, rows in gathered:
        column = next((i for i, h in enumerate(header) if _header_key(h) in keys), None)
        if column is None:
            continue
        now = [i for i, h in enumerate(header) if NOW_COLUMN.search(h or "") and not LATER_COLUMN.search(h or "")]
        by_party = {}
        for row in rows:
            name = row[column].strip() if column < len(row) else ""
            if name and not name.lower().startswith("total"):
                by_party.setdefault(name, []).append(row)
        for name, party_rows in by_party.items():
            if name in left_out or all(any(LEFT_OUT.search(cell or "") for cell in row) for row in party_rows):
                continue
            if selects is not None and not any(selects(row) for row in party_rows):
                continue  # (not one the stage's condition takes)
            if now and all((_number(row[i]) or 0) == 0 for row in party_rows for i in now if i < len(row)):
                continue
            owed.setdefault(name, path.name)
    missing = [name for name in owed if name.lower() not in named]
    if not missing:
        return []
    files = ", ".join(sorted({owed[m] for m in missing}))
    which = "every record its condition takes" if selects is not None else "every record"
    entry = bool(APP_ENTRY.search(step.get("instruction") or "") or APP_ENTRY.search(step.get("title") or ""))
    head = (f"The stage asks for {which}, and its {'log' if entry else 'output'} has {len(owed) - len(missing)} of "
            f"{len(owed)}: no row for {', '.join(missing)} ({files} has them, not held).")
    if entry:
        return [head + " Do each one that is left - check the application first for any already there, never enter "
                "one twice - and log it."]
    return [head + " Add the missing rows to the file."]


# How a stage reopened for the person's change of the request says so (amend).
CHANGED_REQUEST = "the person changed the request"


def _earlier_findings(data, step, record):
    """[(record reference, sentence)]: the exceptions, flags and holds this stage found before (its carried facts and
    its previous report) that the person's changes do not name."""

    try:
        texts = [f.get("text") or "" for f in data.get("facts") or [] if f.get("stage") == step["id"]]
        return stage_facts.earlier_findings(texts + [record.get("report") or ""], data.get("amendments"))
    except Exception as exc:  # noqa: BLE001
        log(f"long_task: could not read the stage's earlier findings: {exc}")
        return []


def _lost_findings(run_id, step, record, made):
    """An earlier finding a re-check dropped from the stage's outputs (stage_facts.lost_findings), as problems."""

    try:
        data = ledger(run_id)
        found = stage_facts.lost_findings(_earlier_findings(data, step, record), made)
    except Exception as exc:  # noqa: BLE001
        log(f"long_task: could not check the earlier findings: {exc}")
        return []
    if not found:
        return []
    about = "; ".join(a.get("text", "")[:120] for a in data.get("amendments") or [])
    return [" ".join(found)[:900] + (f". The change was only: {about}" if about else "")]


def removed_still_there(run_id, made):
    """A record the person removed that an output still holds as a full row (stage_facts.removed_still_present)."""

    found = stage_facts.removed_still_present(ledger(run_id)["amendments"], made)
    if not found:
        return []
    # Evidence (ADVISORY): the row is quoted so the judge and the person can see whether it shows the record taken out.
    return ["A record the person removed may still be counted (" + "; ".join(found)[:900] + "). Check that the row "
            "shows it taken out and that no total, summary, sheet, deck or memo figure counts it."]


def _removed_residue(run_id, made, request):
    """A record the person removed whose figures a total still holds (stage_facts.removed_residue), as problems."""

    from decurix_agent import payees
    amendments = ledger(run_id)["amendments"]
    if not stage_facts.removed_records(amendments):
        return []
    found = stage_facts.removed_residue(amendments, made, payees.bill_documents(request, made))
    if not found:
        return []
    # Evidence (ADVISORY): an arithmetic match, not a proof - a total may equal "sum + a removed figure" by chance.
    return ["A total may still include a record the person removed (" + "; ".join(found)[:900] + "). Check how each "
            "such figure is made."]


def _status_conflicts(run_state, step, made, drafts):
    """An item's status recorded in one output that another output - an earlier stage's, or this stage's drafts -
    contradicts (stage_facts.status_conflicts), as problems."""

    try:
        work = Path(_runner().context(run_state)["work_dir"])
        earlier = [str(work / e["path"]) for s in _steps(run_state) if s["id"] != step["id"]
                   and run_state["steps"][s["id"]]["status"] == "completed"
                   for e in run_state["steps"][s["id"]]["evidence"] if not e.get("drafts")]
        found = stage_facts.status_conflicts(list(dict.fromkeys(earlier + list(made))), drafts)
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing
        log(f"long_task: could not compare the statuses across the outputs: {exc}")
        return []
    if not found:
        return []
    return ["An item's status disagrees across the outputs (" + "; ".join(found)[:900] + "). When this stage decides "
            "or changes what an earlier output records about an item (its status, action or decision), update that "
            "output too, so every output says the same."]


def _left_undone_found(made, request, not_made=None):
    """Work a table leaves as an instruction, references given without evidence, amounts whose working does not hold or
    is missing, rows that contradict themselves (stage_facts), as (check, finding), each named by its check
    (CHECK_MATURITY). Each sub-check runs on its own (_checked): one that raises - an unreadable input file - never
    turns off another, the blocking formula check included (E-01: one raise used to return [] before it ran)."""

    from decurix_agent import payees

    not_made = not_made if not_made is not None else []
    inputs = _checked("working_factors", not_made, lambda: [payees.inputs(request)])
    records, messages = inputs[0] if inputs else ([], [])
    own = _checked("working", not_made, stage_facts.formula_contradictions, made)
    deferred = _checked("deferred_actions", not_made, stage_facts.deferred_actions, made)
    unsupported = _checked("unsupported_references", not_made, stage_facts.unsupported_references, made, messages)
    worked = _checked("working_factors", not_made, stage_facts.working_problems, made, records, messages)
    unworked = _checked("unworked_amounts", not_made, stage_facts.unworked_amounts, made, messages)
    contradicts = _checked("self_contradictions", not_made, stage_facts.self_contradictions, made)
    problems = []
    if own:
        problems.append(("working", "A figure is not what its own formula gives (" + "; ".join(own)[:800] + "). Store "
                        "the results again (office calculate FILE) or correct the figure, and every output that states "
                        "it."))
    if worked:
        problems.append(("working_factors", "A computed amount may not hold (" + "; ".join(worked)[:800] + "). Check it "
                        "against the figures as the source gives them and the working recorded for it."))
    if unworked:
        problems.append(("unworked_amounts", "An amount is stated with no working recorded for it ("
                        + "; ".join(unworked)[:600] + ")."))
    if contradicts:
        problems.append(("self_contradictions", "A row's own words may deny its category ("
                        + "; ".join(contradicts)[:600] + ")."))
    if deferred:
        problems.append(("deferred_actions", "A row leaves work as an instruction for later ("
                        + "; ".join(deferred)[:700] + ")."))
    if unsupported:
        problems.append(("unsupported_references", "A reference was recorded for an item whose own content does not give "
                        "it (" + "; ".join(unsupported)[:700] + ")."))
    return problems


# ------------------------------------------------------------ a workbook built across stages keeps each stage's sheets
#
# CE (round 5 P1, 2026-10-20): a workbook of several analyses is built one stage per sheet. A later stage that writes
# the file anew would drop an earlier stage's checked sheet with nothing to notice it. Each completed stage's sheets
# are kept (as their cells) in the run's own folder; a later stage that loses or changes one is not done, unless its
# instruction or a change the person made names that sheet.

def _cell_json(value):
    """A cell's value as kept: numbers and true/false as they are, a date or time as ISO text, anything else as text."""

    import datetime
    if value is None or isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, (datetime.datetime, datetime.date, datetime.time)):
        return value.isoformat(sep=" ") if isinstance(value, datetime.datetime) else value.isoformat()
    return str(value)


def _sheet_cells(path):
    """
    {sheet name: {cell: [value or formula as written, the value the app last computed]}} of a workbook, or None when it
    is not one that can be read. (A file openpyxl wrote holds no computed values: the second is then None.)
    """

    if Path(path).suffix.lower() not in {".xlsx", ".xlsm"}:
        return None
    try:
        import openpyxl
        book = openpyxl.load_workbook(path, data_only=False)
        values = openpyxl.load_workbook(path, data_only=True)
        try:
            out = {}
            for sheet in book.worksheets:
                computed = values[sheet.title] if sheet.title in values.sheetnames else None
                cells = {}
                for row in sheet.iter_rows():
                    for c in row:
                        if c.value is None or (isinstance(c.value, str) and not c.value.strip()):
                            continue
                        cached = computed[c.coordinate].value if computed is not None else None
                        cells[c.coordinate] = [_cell_json(c.value), _cell_json(cached)]
                out[sheet.title] = cells
            return out
        finally:
            book.close()
            values.close()
    except Exception as exc:  # noqa: BLE001 - what cannot be read is not compared
        log(f"long_task: could not read the sheets of {Path(path).name}: {exc}")
        return None


def _formula_key(text):
    """A formula as an app may write it back: case, spacing, $ anchors and sheet-name quotes do not matter."""

    return re.sub(r"[\s$']", "", str(text)).upper().lstrip("=").replace("_XLFN.", "")


def _date_order(day_first):
    """values' date order of the request's convention: "day" (DD/MM), "month" (MM/DD) or None (not said)."""

    return "day" if day_first is True else "month" if day_first is False else None


def _scalar(value, day_first=None):
    """A cell value by meaning: ("empty",), ("bool", b), ("date", (y, m, d, seconds)), ("undated", folded) for a digit
    date its order cannot decide, ("number", value as given) or ("text", folded). Dates and numbers are read by
    values.py under the request's date order (review round 2: this read a > 12 as day first even under MM/DD)."""

    if value is None or (isinstance(value, str) and not value.strip()):
        return ("empty",)
    if isinstance(value, bool):
        return ("bool", value)
    if isinstance(value, str) and value.strip().upper() in ("TRUE", "FALSE"):
        return ("bool", value.strip().upper() == "TRUE")
    if isinstance(value, (int, float)):
        return ("number", value)
    text = str(value).strip()
    day = value_rules.parse_date(text, _date_order(day_first))
    if day:
        if day.kind != "date":
            return ("undated", " ".join(text.split()).lower())
        h, mi, sec = day.time or (0, 0, 0)
        return ("date", tuple(day.parts) + (h * 3600 + mi * 60 + sec,))
    if isinstance(value_rules.parse_amount(text), value_rules.Amount):
        return ("number", text)
    return ("text", " ".join(text.split()).lower())


def _serial_date(number):
    """A spreadsheet date serial (days since 1899-12-30) as (y, m, d, seconds), or None outside dates' range."""

    import datetime
    try:
        number = float(number)
    except (TypeError, ValueError):
        return None
    if not 1 <= number < 2958466:
        return None
    moment = datetime.datetime(1899, 12, 30) + datetime.timedelta(days=number)
    return (moment.year, moment.month, moment.day, moment.hour * 3600 + moment.minute * 60 + moment.second)


def _same_value(a, b, day_first=None, computed=""):
    """Whether two cell values are one: dates as values reads them under the request's order (a date and its serial
    alike), numbers by values within their written precision - the deliberate tolerance of a kept cell and its computed
    value, values' own parameter (`computed`: "a" when `a` is a formula's computed value and `b` a written one, "both"
    when both are computed: then equal, a float's noise aside) - and anything else as written. A digit date its order
    cannot decide is the same only as written."""

    sa, sb = _scalar(a, day_first), _scalar(b, day_first)
    if sa[0] == "date" or sb[0] == "date":
        if sa[0] == "number":
            sa = ("date", _serial_date(value_rules.figure(sa[1])[0] if value_rules.figure(sa[1]) else None))
        if sb[0] == "number":
            sb = ("date", _serial_date(value_rules.figure(sb[1])[0] if value_rules.figure(sb[1]) else None))
    if sa[0] != sb[0]:
        return False
    if sa[0] == "number":
        read = [value_rules.parse_amount(v) if isinstance(v, str) else None for v in (sa[1], sb[1])]
        marked = any(isinstance(r, value_rules.Amount) and (r.unit or r.currency) for r in read)
        if marked:  # (a figure with a unit or a currency: values.compare reads those)
            return value_rules.compare(sa[1], sb[1], precision=True).same is True
        same = value_rules.same_figure(sa[1], sb[1], precision=computed != "both", a_computed=computed == "a")
        return same is True
    return sa == sb


def same_cell(before, after, day_first=None):
    """
    Whether a kept cell ([as written, computed]) still holds the same thing: a number and its text form, a date and its
    serial or its text in the request's convention, a formula an app wrote back in its own way, a formula and its
    computed value when one side has only the value, empty and "", trailing spaces, true/false and TRUE/FALSE. A
    different value, or a formula whose result is now different, is a change.
    """

    if before is None or after is None:
        return before is None and after is None  # (a cell only one side has: a row added or removed)

    def parts(cell):
        if isinstance(cell, list) and len(cell) == 2:
            return cell[0], cell[1]
        return cell, None  # (a snapshot kept before the computed value was)
    (wa, ca), (wb, cb) = parts(before), parts(after)
    fa = isinstance(wa, str) and wa.startswith("=")
    fb = isinstance(wb, str) and wb.startswith("=")
    if fa and fb:
        if _formula_key(wa) == _formula_key(wb):
            return True
        return ca is not None and cb is not None and _same_value(ca, cb, day_first, "both")
    if fa or fb:
        computed, plain = (ca, wb) if fa else (cb, wa)
        if computed is None:
            return True  # (no computed value kept to compare: not a change anyone can see)
        return _same_value(computed, plain, day_first, "a")
    return _same_value(wa, wb, day_first)


def sheets_added_later(run_id, step_id, made):
    """{file name: [sheet names]} in the stage's workbooks now that were not in them when it completed (its snapshot,
    _snapshot_sheets): what later stages added. {} when the stage has no snapshot yet."""

    try:
        target = _sheets_path(run_id, step_id)
        if not target.exists():
            return {}
        kept = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    out = {}
    for path in made:
        own = kept.get(str(Path(path).resolve()))
        if own is None:
            continue
        now = _sheet_cells(path) or {}
        added = [name for name in now if name not in own]
        if added:
            out[Path(path).name] = added
    return out


def _sheets_path(run_id, step_id):
    return _runner().root / run_id / "sheets" / f"{step_id}.json"


_CHANGE_VERB = re.compile(r"(?i)\b(?:update|change|correct|fix|amend|adjust|revise|replace|redo|rebuild|recalculate|"
                          r"add (?:to|a|an|the|rows?|columns?)|remove|delete|edit|overwrite|refresh)\w*\b")


def _named_sheet(name, instruction, amendments):
    """Whether a sheet may change: the stage's instruction asks to change it (a sentence naming it with a change verb),
    or a change the person made names it."""

    named = r"(?<![\w-])" + re.escape(name) + r"(?![\w-])"
    if any(re.search(named, a or "", re.I) for a in amendments):
        return True
    return any(re.search(named, s, re.I) and _CHANGE_VERB.search(s)
               for s in re.split(r"(?<=[.!?;])\s+|\n+", instruction or ""))


def _sheet_owners(run_id, run_state):
    """
    {(file, sheet): stage id} - a sheet belongs to the stage in whose completion snapshot it first appeared (by when
    each stage completed, whatever its number). Every snapshot keeps all of its file's sheets; only the first holds
    it as its own. Found live (round 5 P1, 2026-10-20): "Purchase Working" and "Tax Return" were reported as the AR
    Ageing stage's - the last stage to complete before the Purchase Register stage re-ran.
    """

    folder = _sheets_path(run_id, "x").parent
    entries = []
    for path in folder.glob("*.json") if folder.exists() else ():
        step_id = path.stem
        try:
            kept = json.loads(path.read_text(encoding="utf-8"))
            when = (run_state["steps"].get(step_id) or {}).get("completed_at") or path.stat().st_mtime
        except (OSError, ValueError):
            continue
        entries.append((when, step_id, kept))
    owners = {}
    for _, step_id, kept in sorted(entries, key=lambda e: e[0]):
        for file, sheets in kept.items():
            for name in sheets:
                owners.setdefault((file, name), step_id)
    return owners


def _row_texts(cells):
    """{row number: ((column, text), ...)} of a kept sheet's text cells - its records' names, labels and notes."""

    rows = {}
    for key, cell in cells.items():
        written = cell[0] if isinstance(cell, list) else cell
        if not isinstance(written, str) or written.startswith("=") or _number(written) is not None:
            continue
        m = re.match(r"([A-Z]+)(\d+)$", key)
        if m:
            rows.setdefault(int(m.group(2)), []).append((m.group(1), " ".join(written.split()).lower()))
    return {n: tuple(sorted(v)) for n, v in rows.items()}


def _amended_changes(cells, now, records):
    """
    After a change the person made to the records (one removed, one changed): what in an earlier sheet the change does
    not explain. Its figures may change (totals, summaries, other sheets built on the record) and rows naming the
    changed records may go; every other row's text must still be there, wherever it now stands. [] when all holds.
    """

    before, after = _row_texts(cells), _row_texts(now)
    left = {}
    for row in after.values():
        left[row] = left.get(row, 0) + 1
    lost = []
    for number, row in sorted(before.items()):
        text = " ".join(t for _, t in row)
        if any(r.lower() in text for r in records):
            continue
        if left.get(row):
            left[row] -= 1
        else:
            lost.append(f"row {number}")
    return lost


def lost_sheets(run_id, run_state, step, made):
    """
    The sheets of earlier completed stages, in a file this stage handed in, that are gone or changed - when neither this
    stage's instruction nor a change the person made names the sheet: one finding each, naming the sheet and the stage
    that made it (the one whose completion first held it). After a change the person made to a record, the figures
    built on it may change everywhere and its rows may go (what the removed-record check asks for); only other records'
    rows must stay.
    """

    now = {}
    for path in made:
        cells = _sheet_cells(path)
        if cells is not None:
            now[str(Path(path).resolve())] = cells
    if not now:
        return []
    instruction = step.get("instruction") or ""
    data = ledger(run_id)
    amendments = [a.get("text") or "" for a in data.get("amendments") or []]
    records = sorted({r for a in amendments for r in re.findall(r"\b[A-Z]{1,6}[-/]?\d{2,8}(?:[-/]\d{1,6})?\b", a)})
    day_first = _day_first(data.get("request") or "")
    owners = _sheet_owners(run_id, run_state)
    titles = {s["id"]: s.get("title", s["id"]) for s in _steps(run_state)}
    out = []
    for earlier in _steps(run_state):
        if earlier["id"] == step["id"] or run_state["steps"][earlier["id"]]["status"] != "completed":
            continue
        try:
            kept = json.loads(_sheets_path(run_id, earlier["id"]).read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        for path, sheets in kept.items():
            if path not in now:
                continue
            for name, cells in sheets.items():
                owner = owners.get((path, name), earlier["id"])
                if owner != earlier["id"]:
                    continue  # (another stage's sheet - this stage's own, or checked under the stage that made it)
                title = titles.get(owner, owner)
                if name not in now[path]:
                    out.append(f"The sheet \"{name}\" that the stage \"{title}\" made in {Path(path).name} is gone. Put "
                               "it back as it was (add this stage's own sheets to the workbook, never write it anew).")
                    continue
                if _named_sheet(name, instruction, amendments):
                    continue
                if amendments:
                    lost = _amended_changes(cells, now[path][name], records)
                    if lost:
                        out.append(f"The sheet \"{name}\" that the stage \"{title}\" made in {Path(path).name} lost "
                                   f"or changed {', '.join(lost[:8])}{'...' if len(lost) > 8 else ''} - rows the change "
                                   "you were given does not touch. Put them back; change only what the change explains.")
                    continue
                # Only cells that held something count: a new column, rows appended, an empty cell filled are additions
                # (round 5 P2, 2026-10-20: a stage adding column N to an earlier "Working" sheet was refused). A value
                # changed or cleared, a row removed or the rows reordered still shows here.
                changed = sorted(k for k in cells if not same_cell(cells[k], now[path][name].get(k), day_first))
                if changed:
                    out.append(f"The sheet \"{name}\" that the stage \"{title}\" made in {Path(path).name} was changed "
                               f"({', '.join(changed[:8])}{'...' if len(changed) > 8 else ''}), and this stage was not "
                               "asked to change it. Put it back as it was; change only this stage's own sheets.")
    return out


def request_input_files(request, made=(), limit=400):
    """The request's input files: what run_inputs finds, and every file in the folders the request names (not made)."""

    from decurix_agent import payees
    found = [entry.split(" (= ")[0] for entry in run_inputs(request)]
    made_real = {str(Path(m).resolve()) for m in made}
    try:
        for folder in payees.named_folders(request, [config.WORK_DIR]):
            if Path(folder).resolve() == Path(config.WORK_DIR).resolve():
                continue  # (a request naming no folder: the whole work folder is not its input)
            for path in sorted(Path(folder).rglob("*")):
                if len(found) >= limit:
                    break
                if path.is_file() and str(path.resolve()) not in made_real and not path.name.startswith((".", "~$")):
                    found.append(str(path))
    except Exception as exc:  # noqa: BLE001
        log(f"long_task: could not list the request's folders: {exc}")
    return list(dict.fromkeys(p for p in found if str(Path(p).resolve()) not in made_real))


# The words a restatement may use to say who or what is in or out, besides the stage's own text and the person's words.
_RESTATE_GLUE = {"except", "excepting", "excluding", "without", "only", "also", "until", "unless", "because", "since",
                 "said", "says", "person", "answer", "answered", "their", "them", "they", "this", "that", "these",
                 "those", "with", "from", "into", "which", "each", "every", "other", "others", "left", "leave",
                 "included", "include", "excluded", "exclude", "now", "here", "there", "what", "when", "where", "while"}


def restatement_adds(restated, old, stated):
    """
    The words a restated stage text adds that are neither in the stage's own text nor in the question and the person's
    answer (nor a few words for saying who is in or out): [] when it adds none. A restatement says who or what is in or
    out, in the person's words; it never brings in a rule of its own (round 5 P2, 2026-10-21, CN: a restatement
    added "full ordered quantity as backorder", which nobody had said).
    """

    have = {_stem(w) for w in re.findall(r"[^\W\d_][\w'-]*", f"{old} {stated}")}
    glue = {_stem(w) for w in _RESTATE_GLUE}
    added = [w for w in re.findall(r"[^\W\d_][\w'-]*", restated or "")
             if len(w) > 3 and _stem(w) not in have and _stem(w) not in glue]
    return sorted(set(w.lower() for w in added))


def _stem(word):
    """A word without its possessive and its commonest ending ("Group's" -> "group", "released" -> "releas")."""

    word = word.lower().strip("'-")
    if word.endswith("'s"):
        word = word[:-2]
    for ending in ("ing", "ed", "es", "s", "e"):
        if word.endswith(ending) and len(word) - len(ending) >= 3:
            return word[:-len(ending)]
    return word


# Each code check of a finished stage, and its maturity. BLOCKING: its finding fails the stage. ADVISORY: its finding
# never does - it is kept (ledger "advisories") and shown to the person under "Please check" in the final report, so
# nothing it saw goes unseen. A new check starts ADVISORY; it is made BLOCKING here, by name, only after it passes the
# replay gate (tests/test_replay_gate.py) AND one full live round of the six processes with no false finding. PM
# decision (2026-10-20): new code checks had refused correct live work seven times in one day (BZ1, CH, CI, CK, CO,
# CP2, P3's formulas), each costing a full round.
#
# Harness audit (2026-10-21, docs/harness-audit.md): code BLOCKS only on domain-free mechanical invariants - seen in the
# system and no duplicate, provenance, own arithmetic, the person's files and changes, hygiene, gates. Everything that
# needs judgement is ADVISORY: evidence for the judge (_judge) and "Please check" for the person, never a verdict.
# Business rules (holds, refunds, rates, first come, backorders) live only in the request and the operator's taskRules.
MUST_PASS, ADVISORY = "blocking", "advisory"
# A check whose findings go straight to the person as a question - an approval only they can give - never to a retry.
ASK_PERSON = "ask the person"
CHECK_MATURITY = {
    # produced.file_facts' invariants: unstored formulas, a Total row summing less than its column, a file the stage
    # names not made, placeholders, accounts and recipients on no record, a figure contradicting its own source, false
    # sums, program-printed numbers, a person's file altered.
    "file_facts": MUST_PASS,
    "file_notes": ADVISORY,  # file facts that read the request or need judgement (FileFacts.notes)
    # The person's own given file changed during the task: the fact and its diff go to the judge, and the person is
    # always told. ADVISORY (E-12, review 2026-11-03): whether the request asked for the change is judgement - a verb
    # list reverted correct in-place updates. Its failure to be made still leaves the stage not checked.
    "source_guard": ADVISORY,
    # A record the person removed: a row of its own with figures, or a total equal to a sum plus one of its figures.
    # ADVISORY (E-07, E-08, review 2026-11-03): a correctly cancelled row and an arithmetic coincidence look the same
    # to code, so the quoted row goes to the judge. No mechanical form of these blocks today.
    "removed_residue": ADVISORY,
    "removed_still_there": ADVISORY,
    "filler_values": MUST_PASS,  # provenance: an address, identifier or amount typed into a form that no source and
    # no recorded work holds (provenance.Sources.unsourced: the blocking kinds only)
    # An entry picked from an application's own list into a form the stage saved that nothing given backs, a field it
    # set with no declaration, a record no declaration covers (ledger_checks.findings). ADVISORY at the stage until it
    # passes the replay gate (CLAUDE.md); the run's final report holds these against the run now (review round 9).
    "ledger_findings": ADVISORY,
    "typed_doubtful": ADVISORY,  # dates and codes typed that no source holds, or only the work's own files explain
    "draft_facts": MUST_PASS,  # provenance: a fact a draft states that no source and no recorded work holds
    "draft_fields": ADVISORY,  # DE: a draft's fact against its recipient's own row (which column is meant is judgement)
    # CS: a document an app's save emptied of what the tools wrote - compared with the tools' own copy, nothing guessed.
    "app_emptied": MUST_PASS,
    # #7: a payment to account details taken from a message, where the request itself gives messages as their source
    # (the planner's "payment_details_from_messages") and no record holds an account for that payee: the person approves.
    "payee_approval": ASK_PERSON,
    "files_open": MUST_PASS,  # hygiene: every Office file the run hands in opens with a strict reader (series 14 P3)
    "system_entry": MUST_PASS,  # CR: records claimed entered in a system are seen in that system's own pages
    # The same, when only the stage's words (a plan without "enters") made it an entry stage, or a record that only
    # may be in the system twice: evidence.
    "system_entry_text": ADVISORY,
    "working": MUST_PASS,  # own arithmetic only: a figure its own formula or written "a x b = c" contradicts
    # Evidence, never a verdict:
    "repeated_rows": ADVISORY,  # LOUD: under "Needs you" and in the report's first line (loud_advisories)
    "repeated_rows_quiet": ADVISORY,  # repeats that are often real (no identifier or date column): "Please check"
    "coverage": ADVISORY,  # every record of an earlier table in this stage's output
    "deferred_actions": ADVISORY,  # CC1: a row that says "calculate later"
    "lost_findings": ADVISORY,  # BG: a re-check dropped an earlier flag
    "reply_coverage": ADVISORY,  # an input email with neither a draft nor a reason
    "working_factors": ADVISORY,  # CF/CH/CJ/CK: a working whose factors were guessed from column names
    "unworked_amounts": ADVISORY,  # CI: an amount with no working
    "self_contradictions": ADVISORY,  # CF2: a row its own words deny
    "unsupported_references": ADVISORY,  # CC2: a reference the item's content does not give
    "status_conflicts": ADVISORY,  # BZ1: outputs disagreeing on an item's status
    "lost_sheets": ADVISORY,  # CO/CP2: an earlier stage's sheet changed
    "summary_totals": ADVISORY,  # DC: a summary row against the detail rows it seems to count
    # A blocking check that could not be made (it raised): fails nothing by itself, but the stage it saw is not checked
    # - never "done" (stage_finished hands these to verify.outcome as not made; review 2026-11-02).
    "checks_not_made": ADVISORY,
}
NOT_MADE = "checks_not_made"
# Advisory checks that must still be MADE: their finding is evidence, but a stage they could not look at is not checked
# (source_guard: whether the person's own files were changed is something they must be told, either way).
MADE_OR_UNCHECKED = {"source_guard"}
# Advisories the person must not miss: first in "Needs you", and named in the report's first line.
LOUD = {"repeated_rows": ("includes rows the input repeats", "include rows the input repeats")}


def is_blocking(check):
    """Whether a finding of this check fails the stage (CHECK_MATURITY; a check not listed is advisory)."""

    return CHECK_MATURITY.get(check) == MUST_PASS


def _checked(check, unmade, fn, *args, **kwargs):
    """
    One code check, run so that its failure is never a pass (E-01): an exception in a check whose findings block or
    ask the person (CHECK_MATURITY) is a check not made - a line in `unmade`, and the stage is not checked on that
    point; an advisory check that raises only loses its evidence. Returns the check's findings, [] when it raised.
    Found (review 2026-11-03): each check had its own `except Exception: return []`, so a blocking check that raised
    counted as passed and the run said "Done - every stage was made and checked".
    """

    try:
        return fn(*args, **kwargs) or []
    except Exception as exc:  # noqa: BLE001 - said below; never a pass
        log(f"long_task: the {check} check could not be made: {exc}")
        if is_blocking(check) or asks_person(check) or check in MADE_OR_UNCHECKED:
            unmade.append(f"the {check} check could not be made ({type(exc).__name__})")
        return []


def asks_person(check):
    """Whether a finding of this check is put to the person as an approval before the stage counts (ASK_PERSON)."""

    return CHECK_MATURITY.get(check) == ASK_PERSON


# ------------------------------------------------------------ records entered in a system: seen in that system
#
# CR (round 5 P2, 2026-10-20): "Enter every approved order in the portal and log entries" was reported done,
# "checked by reading the files back" - the portal had no order and the run no screen action; the worker had written a
# log claiming the entries. A record entered in a system is done only with what the tools saw there.

_SKIPPED_ROW = re.compile(r"(?i)\b(?:exclud\w*|held|hold|not (?:entered|created|saved|posted)|skip\w*|left out|"
                          r"cancel\w*|no entry)\b")
_ACTION_KINDS = ("click", "double_click", "right_click", "type", "key", "select", "open", "upload", "drag", "scroll")


# A clause that is the stage's own order to enter records: it opens with the verb ("Enter every approved order in the
# portal", "then onboard each joiner in the portal") and names where (a portal, an app, a system, an address).
_ENTRY_OPENING = re.compile(r"(?i)^(?:(?:and|then|now|next|also|finally)\s+)*(?:enter|key\s+in|create|submit|save|upload|"
                            r"onboard|register|post|book|add)\b\s+\S")
_IN_A_SYSTEM = re.compile(r"(?i)\b(?:in|into|on|to|through|via|using)\s+(?:the\s+|our\s+|their\s+|its\s+)?[^.;,]{0,60}?\b(?:"
                          r"portal|application|app|system|erp|website|web\s*site|software|back\s*office|crm)\b|"
                          r"https?://")
_NOT_NOW = re.compile(r"(?i)\b(?:do\s+not|don'?t|never|not\s+yet|no\s+need\s+to|without|later|before\s+you|until)\b")


def entry_clauses(text):
    """The clauses of a stage's text that order records entered in a system - each opening with the entry verb and
    naming the system, quoted words and file paths left out; a mention ("do not enter anything in the portal yet",
    "for later portal entry", "the portal's SO number") is none."""

    text = re.sub(r"\"[^\"]{0,400}\"|“[^”]{0,400}”", " ", str(text or ""))
    text = re.sub(r"(?:[A-Za-z]:)?[\\/]?(?:[\w.-]+[\\/])+[\w.*-]+\.\w{2,4}", " ", text)
    out = []
    for clause in re.split(r"(?<=[.!?;:])\s+|\n+|,\s+(?=(?:and\s+)?then\b)|\s+and\s+then\s+", text):
        clause = clause.strip(" -*,")
        if _ENTRY_OPENING.match(clause) and _IN_A_SYSTEM.search(clause) and not _NOT_NOW.search(clause):
            out.append(clause)
    return out


def enters_into_system(step, request):
    """
    The request's sentence naming the system a stage enters records in, or "": only when the stage's own title or
    instruction orders it (entry_clauses) and the request gives a system (request_systems). Found live (round 5 P2,
    2026-10-21): a stage told "do not enter anything in the portal yet" was refused for entering nothing. A blocking
    check acts only when it is certain the stage is an entry stage.
    """

    systems = request_systems(request)
    if "enters" in step:
        named = " ".join(str(step.get("enters") or "").split())
        if not named:
            return ""
        # The request's sentence for the system the planner names (its address), else the planner's own words for it.
        low = named.lower()
        for sentence in systems:
            if low in sentence.lower() or any(m and m in low for m in _system_marks(sentence)):
                return sentence
        return systems[0] if len(systems) == 1 else named
    if not systems:
        return ""
    if not (entry_clauses(step.get("title") or "") or entry_clauses(step.get("instruction") or "")):
        return ""
    return systems[0]


def _system_marks(system):
    """What marks a tool's action as on that system: its address (host:port, host), as the request gives it."""

    marks = []
    for url in re.findall(r"https?://([^/\s,)]+)", system or ""):
        marks += [url.lower(), url.split(":")[0].lower()]
        if ":" in url:
            marks.append(":" + url.split(":")[1])
    return [m for m in dict.fromkeys(marks) if m]


def _stage_tool_record(run_id, step, start):
    """
    (actions, read) of this stage's attempt, from the tools' own record: the screen actions since the stage began
    ([event]) and everything the tools read off the screen since then (text, after the last entry action). None when
    there is no task to read them for (nothing is decided then).
    """

    import datetime
    from decurix_agent.computer import evidence, recording
    task_id = task_context.current() or (ledger(run_id).get("task_id") or "")
    if not task_id:
        return None
    began = float((start or {}).get("t") or 0)
    events = []
    try:
        _, events = recording.load(recording.session_for_task(task_id))
    except (FileNotFoundError, OSError, ValueError):
        events = []  # (no recording at all: no action was taken)

    def when(event):
        try:
            return datetime.datetime.strptime(str(event.get("at")), recording.TIME_FORMAT).timestamp()
        except (TypeError, ValueError):
            return 0.0
    actions = [e for e in events if e.get("kind") in _ACTION_KINDS and e.get("outcome") in (None, "done")
               and when(e) >= began]
    # (After the last value put in - a typing, a choice, a file: the saves and the list opened after it are what follow.)
    last_entry = max([when(e) for e in actions if e.get("kind") in ("type", "select", "upload")] or [began])
    notes = []
    try:
        with open(evidence.NOTES_DIR / f"{task_id}.jsonl", encoding="utf-8") as f:
            for line in f:
                try:
                    note = json.loads(line)
                except ValueError:
                    continue
                notes.append((float(note.get("at") or 0), str(note.get("text") or "")))
    except OSError:
        pass
    read = [text for at, text in notes if at >= last_entry]
    read += [str(e.get("change") or "") for e in actions if when(e) >= last_entry]
    # The whole stage's reading too: the system's own page for each record as it was saved ("Order ORD-2 was saved").
    everything = [text for at, text in notes if at >= began]
    everything += [str(e.get("change") or "") for e in actions]

    def pages(texts):
        # What a form's fields held is what was typed, not what the system shows: only the pages' own words count.
        return "\n".join(line for line in "\n".join(texts).splitlines()
                         if not re.search(r"\bholds:|\bvalue=\"|\bvalue \"[^\"]*\" ->", line))
    return actions, pages(read), pages(everything)


_STATUS_HEADER = re.compile(r"(?i)\b(?:status|result|outcome|entered|entry|portal|action|saved|created|done)\b")
# The worker's own explicit claim that a row was entered: an entry word, or the record's number in the system - never
# "OK", "Yes", "Done" or a tick, which a stock count or a check list writes for anything (harness review, 2026-10-21).
_ENTERED_CELL = re.compile(r"(?i)^\W*(?:entered|created|saved|submitted|onboarded|posted|booked|registered)\b|"
                           r"\b(?:entered|created|saved|submitted|onboarded|posted|booked)\b(?:\s+(?:as|with|in)\b|"
                           r"\s+\S*\d{3,}|\s*$)")
# A column that holds the record's number in the system ("Portal ref", "SO number", "Employee ID").
_REFERENCE_HEADER = re.compile(r"(?i)\b(?:ref\w*|number|id|portal|system)\b")
# A record's number in a system, whatever its shape (ORD-0001, EMP0044, INV/2026/0012, 100234): a token with three
# digits in a row - not a date, not a figure.
_SYSTEM_NUMBER = re.compile(r"(?<![\w/.,-])(?=[A-Za-z0-9/_-]*\d{3})[A-Za-z0-9]+(?:[-/_][A-Za-z0-9]+)*(?![\w/-])")
_DATE_SHAPE = re.compile(r"^\d{1,4}[-/]\d{1,2}[-/]\d{1,4}$")


def _system_numbers(text):
    """The tokens of a cell that may be a record's number in a system (dates and plain years left out)."""

    return [t for t in _SYSTEM_NUMBER.findall(str(text or ""))
            if not _DATE_SHAPE.match(t) and not re.fullmatch(r"(?:19|20)\d\d", t)]


def _id_shape(ident):
    """A pattern for numbers of the same shape as a system's record number: its letters and separator, then digits."""

    m = re.fullmatch(r"([A-Za-z]{1,8}[-/_ ]?)?(\d{3,})", str(ident or ""))
    if not m:
        return re.compile(r"(?<![\w-])" + re.escape(str(ident)) + r"(?![\w-])", re.I)
    if m.group(1):
        return re.compile(r"(?<![\w-])" + re.escape(m.group(1)) + r"\d{3,}(?![\w-])", re.I)
    return re.compile(r"(?<![\w.,/-])\d{" + str(len(m.group(2))) + r"}(?![\w.,/-])")


def _name_words(text):
    return re.findall(r"[a-z0-9]+", str(text or "").lower())


def _name_seen(name, text):
    """Whether a name is in a text in any order, case or punctuation ("Rahul Sharma" in "SHARMA/RAHUL")."""

    words = set(_name_words(name))
    return bool(words) and words <= set(_name_words(text))


def _claimed_records(made):
    """
    [(label, [identifying values], [names], [system numbers])] of the rows a stage's tables say it ENTERED: a status,
    result or reference column whose cell says entered/created/saved, or a reference column that holds the record's
    number in the system - never any row of any table ("Include in portal entry" is a plan, not an entry; "OK" is
    anything's).
    """

    out = []
    for label, header, rows in stage_facts.tables_of(made):
        status_cols = [i for i, h in enumerate(header) if _STATUS_HEADER.search(re.sub(r"[_\W]+", " ", h or ""))]
        if not status_cols:
            continue
        for row in rows:
            cells = [str(c).strip() for c in row if c not in (None, "") and str(c).strip()]
            said = [str(row[i]).strip() for i in status_cols if i < len(row) and row[i] not in (None, "")]
            verbs = [c for c in said if _ENTERED_CELL.search(c)]
            numbered = [str(row[i]).strip() for i in status_cols if i < len(row) and row[i] not in (None, "")
                        and _REFERENCE_HEADER.search(re.sub(r"[_\W]+", " ", header[i] or ""))
                        and re.fullmatch(r"\W*\S*\d{3,}\S*\W*", str(row[i]).strip()) and _system_numbers(row[i])]
            if not cells or _SKIPPED_ROW.search(" ".join(said)) or not (verbs or numbered):
                continue
            keys = [c for c in cells if re.search(r"\d{3,}", c) and len(c) <= 40 and not re.fullmatch(r"[\d.,/ -]+", c)]
            names = [c for c in cells[:3] if re.fullmatch(r"[A-Za-z][A-Za-z&.',/ -]{2,60}", c) and c not in said]
            # The record's number in the system, when the status gives it (EMP-0044, ORD-0001, 100234), is what its
            # pages show; else its own reference and name - never other details (a bank code, an amount) a form held.
            system_ids = list(dict.fromkeys([m for c in verbs for m in _system_numbers(c)]
                                            + [m for c in numbered for m in _system_numbers(c)]))
            keys = [c for c in keys if c not in said]
            values = system_ids or (keys[:1] + names)
            if values:
                label = (keys or system_ids or names)[0]
                if names and label != names[0]:
                    label += f" ({names[0]})"  # (a reference several records share says which one only with its name)
                out.append((label, values, names, system_ids))
    return out


def _seen(value, text):
    flat = lambda v: re.sub(r"[^a-z0-9]+", " ", str(v).lower()).strip()  # noqa: E731
    return bool(flat(value)) and f" {flat(value)} " in f" {flat(text)} "


def system_entry_problems(run_id, step, made, request, start, record=None):
    """
    A stage that enters records in a system the request gives is done only with that system's own evidence, gathered
    by the tools: actions on it during the stage, and a read of its pages after the last entry that shows each record
    the stage says it entered. A log or report the worker wrote is never that evidence. [] when the stage enters
    nothing in a system, or when there is no tool record to decide on. (Every finding; system_entry_findings says which
    are certain.)
    """

    return [text for _, text in system_entry_findings(run_id, step, made, request, start, record)]


def system_entry_findings(run_id, step, made, request, start, record=None):
    """
    system_entry_problems as (check, finding): "system_entry" (blocking) where it is certain the stage enters records -
    the planner's "enters", or the tools' own record of values typed on the system - and where the finding itself is
    certain; "system_entry_text" (evidence) when only the stage's words made it an entry stage (a plan without the
    field), and for a record that only may be there twice.
    """

    system = enters_into_system(step, request)
    certain = bool(system) and "enters" in step
    systems = request_systems(request)
    if not system and not systems:
        return []
    record = record if record is not None else _stage_tool_record(run_id, step, start)
    if record is None:
        return []
    actions, read = record[0], record[1]
    stage_read = record[2] if len(record) > 2 else read
    def typed_on(candidate):
        marks = _system_marks(candidate)
        on = [a for a in actions if marks and any(m in json.dumps(a, default=str).lower() for m in marks)]
        return any(a.get("kind") == "type" for a in on) and bool(_claimed_records(made))
    if system and not certain and typed_on(system):
        certain = True  # (the words said so, and the tools' own record shows values typed on it)
    if not system:
        # The tools' own record says it too: values typed and a save on the system the request gives make this an
        # entry stage, whatever its text says (harness audit, 2026-10-21: detection from what was done, not words).
        for candidate in systems:
            if typed_on(candidate):
                system, certain = candidate, True
                break
        if not system:
            return []
    check = "system_entry" if certain else "system_entry_text"
    marks = _system_marks(system)
    on_system = [a for a in actions if not marks or any(m in json.dumps(a, default=str).lower() for m in marks)
                 or re.search(r"(?i)chrom|firefox|edge|browser", json.dumps(a.get("expect") or {}, default=str))]
    claimed = _claimed_records(made)
    called = re.search(r"\b(?:our|the|your|their)\s+([^.,;:()]{3,50}?)\s*\(", system)
    url = re.search(r"https?://[^\s,)]+", system)
    name = called.group(1).strip() if called else (url.group(0) if url else "the system")
    if not on_system:
        said = ", ".join(c[0] for c in claimed[:8])
        return [(check, f"Nothing was entered in the system: the tools took no action on {name} during this stage"
                 + (f", though its log says these were entered: {said}" if said else "") + ". A log or report you "
                 "write is never evidence of an entry: enter each record in the system, then open its list and read "
                 "it back.")]
    out = []
    # The same record twice in the system's own list: a duplicate in a live system - the person removes it. Certain only
    # when two list rows of one claimed record are the same in every field but their numbers in the system, or when
    # both numbers were made during this stage (the system's pages showed each before the list was read) for one row
    # of the stage's log; one customer with two orders, or two people of one name, is evidence, never a refusal.
    shapes = list({c.pattern: c for c in (_id_shape(i) for _, _, _, ids in claimed for i in ids)}.values())
    lines = [x for line in read.splitlines() for x in line.split("||")]
    twice, maybe = [], []
    for label, values, row_names, ids in claimed:
        if not shapes or not row_names:
            continue
        numbered = {}
        for line in lines:
            if not any(_name_seen(n, line) for n in row_names):
                continue
            found_here = list(dict.fromkeys(t for shape in shapes for t in shape.findall(line)))
            if len(found_here) != 1:
                continue  # (a row of several records decides nothing)
            rest = " ".join(_name_words(line.replace(found_here[0], " ")))
            numbered.setdefault(found_here[0], set()).add(rest)
        if len(numbered) < 2:
            continue
        numbers = sorted(numbered)
        # The party's own logged records: as many numbers as records is one number each, never a duplicate - two
        # genuine identical orders the request asked for are two records (E-09, review 2026-11-03).
        party = [c for c in claimed if any(_name_seen(n, " ".join(c[2])) for n in row_names)]
        if len(numbers) <= len(party):
            continue
        same = set.intersection(*numbered.values())
        # (Made here: shown by the stage's own pages more often than by the list read after the last entry alone.)
        made_here = all(stage_read.count(n) > read.count(n) for n in numbers) and len(party) == 1
        (twice if same or made_here else maybe).append(f"{row_names[0]} ({', '.join(numbers)})")
    if twice:
        # The person is asked, with what the list shows - never told to delete: which one is extra is theirs to say.
        return [(check, f"The system's list may hold the same record twice: {'; '.join(twice[:6])} - more numbers than "
                 "records the log entered for them. Not done: ask the person, showing both numbers, whether one is an "
                 "extra entry (never delete a record yourself - it needs their approval), and enter nothing again.")]
    if maybe:
        out.append(("system_entry_text", f"The system's list shows more than one record for: {'; '.join(maybe[:6])}. "
                    "They differ in some field, so each may be its own record - check that none is entered twice."))
    missing = [label for label, values, row_names, ids in claimed
               if not _shown(values, row_names, claimed, stage_read, ids)]
    if not missing:
        return out
    shown = sum(1 for label, values, row_names, ids in claimed if _shown(values, row_names, claimed, read, ids))
    listed = shown >= 2 or any(len(set(shape.findall(read))) >= 2 for shape in shapes)
    if not listed:
        # The system's list was not read after the last entry: asked for, never taken for records missing (round 5 P2
        #, 2026-10-21, series 12: five orders the portal held were "not shown" - the only page read after the
        # last entry was that order's "Saved" page).
        return out + [(check, f"The system's list of records was not read after the last entry, and its pages during "
                       f"the stage did not show: {', '.join(missing[:8])}. Open {name}'s list of records once, read it, "
                       "and report what it shows - enter only what is not there.")]
    return out + [(check, f"The system's own list, read after the last entry, does not show: {', '.join(missing[:8])}"
                   + (f" (+{len(missing) - 8} more)" if len(missing) > 8 else "") + f". Open {name}'s list of "
                   "records, read it, and enter whatever is not there - a record counts only when the system shows it.")]


def _shown(values, names, claimed, text, ids=()):
    """
    Whether the system's pages show one claimed record: by the system's own number when the log gives one (ORD-0002,
    EMP2044, 100234); else by its name (in any order, case or punctuation) and its reference on one line; a reference
    several claimed records share (one PO for three chat orders) never identifies one by itself.
    """

    keys = [v for v in values if v not in names]
    shared = {k for k in keys if sum(1 for c in claimed if k in c[1]) > 1}
    numbers = [v for v in ids if v not in shared]
    if numbers and len(numbers) == len(values):
        return any(_seen(n, text) for n in numbers)
    lines = text.splitlines()
    if names:
        return any(any(_name_seen(n, line) for n in names) and (not keys or any(_seen(k, line) for k in keys))
                   for line in lines) or any(_name_seen(n, text) for n in names) and not keys
    own = [k for k in keys if k not in shared]
    return bool(own) and any(_seen(k, text) for k in own)


def repeated_rows(made, reply, request=""):
    """BK3 as its own check - a LOUD advisory (harness audit, 2026-10-21): each figure in the outputs that is a total
    including rows an input table repeats exactly (tools/table_quality.inflated_totals), one finding each. It fails no
    stage (a repeat may be real); the person sees each under "Needs you" and in the report's first line."""

    return [text for text, loud in _repeats(made, reply, request) if loud]


def quiet_repeated_rows(made, reply, request=""):
    """The same, for repeats that are often real (no identifier or date column, no whole block again: attendance, stock
    counts, cash sales): a quiet advisory under "Please check" (table_quality.inflated_totals' detail)."""

    return [text for text, loud in _repeats(made, reply, request) if not loud]


def _repeats(made, reply, request=""):
    from decurix_agent import produced
    try:
        from decurix_agent.tools import table_quality
        inputs = request_input_files(request, made) if request else []
        inflated = table_quality.inflated_totals(produced.text(made) + "\n" + (reply or ""), reply, inputs, detail=True)
    except Exception as exc:  # noqa: BLE001
        log(f"long_task: could not compare totals with repeated rows: {exc}")
        return []
    return [(f"A figure may include rows the input repeats: {item}. Check whether the repeat is real; if it is not, "
             "the figure and everything built on it change.", bool(loud)) for item, loud in inflated[:12]]


def _page_notes(task_id=None):
    """What the tools read off the screen during this task (computer/evidence notes): the system's own pages."""

    try:
        from decurix_agent.computer import evidence
        task_id = task_id or task_context.current() or ""
        if not task_id:
            return []
        out = []
        with open(evidence.NOTES_DIR / f"{task_id}.jsonl", encoding="utf-8") as f:
            for line in f:
                try:
                    out.append(str(json.loads(line).get("text") or ""))
                except ValueError:
                    continue
        return out
    except Exception:  # noqa: BLE001 - nothing read is nothing held
        return []


def _command_outputs(task_id=None):
    """What the work's commands printed during this task (soul's noteCommandOutput): its own recorded working - a total
    a script summed from the sources and printed is held there, never something the work was given. Found live
    (2026-10-21): totals summed and printed by a script were refused as held by no source."""

    try:
        from decurix_agent.computer import evidence
        task_id = task_id or task_context.current() or ""
        if not task_id:
            return []
        out = []
        with open(evidence.NOTES_DIR / f"{task_id}.outputs", encoding="utf-8") as f:
            for line in f:
                try:
                    out.append(str(json.loads(line).get("text") or ""))
                except ValueError:
                    continue
        return out
    except Exception:  # noqa: BLE001 - nothing printed is nothing held
        return []


def _files_read():
    """The files the tools read during this task (office read): a file the work read is one of its sources, named in the
    request or not (computer/evidence.files_read)."""

    try:
        from decurix_agent.computer import evidence
        return [Path(p) for p in evidence.files_read()]
    except Exception:  # noqa: BLE001 - nothing listed is nothing added
        return []


def work_sources(request, made=()):
    """
    provenance.gather for a run: the request, its input files, the work folder (inputs and every output the work
    recorded), and the system's pages the tools read. What a value typed or written must trace to.
    """

    inputs = (request_input_files(request, made) if request else []) + _files_read()
    return provenance.gather(request, list(inputs) + list(made), roots=[config.WORK_DIR], notes=_page_notes() + _command_outputs())


def given_sources(request, made=()):
    """provenance.gather without the work folder: the request, its input files and messages, the system's pages - what
    the work was given, never what it wrote (independent review, 2026-10-21, #11: the work folder is circular)."""

    inputs = (request_input_files(request, made) if request else []) + _files_read()
    return provenance.gather(request, list(inputs), roots=(), notes=_page_notes())


def own_work_evidence(text, request, made=()):
    """
    Evidence lines for the judge: each fact `text` states that no given source holds and only the work's own files
    explain (provenance.own_work_only) - "explained only by your own file X". Never a verdict: a figure the work
    computed is rightly in its own working.
    """

    try:
        work = [m for m in made if Path(str(m)).is_file()]
        found = provenance.own_work_only(text or "", given_sources(request, made), work) if work else []
    except Exception as exc:  # noqa: BLE001 - evidence that cannot be gathered decides nothing
        log(f"long_task: could not tell what only the work's own files explain: {exc}")
        return []
    return [f"{value!r} is explained only by your own file {', '.join(dict.fromkeys(files))} - no source the request "
            "gave holds it" for value, files in found[:12]]


def draft_checks(drafts, request, made=()):
    """
    (unsourced, fields) for a stage's or task's drafts. `unsourced` - provenance, blocking: a fact a draft states (an
    address, a date, a code, a figure of 100 or more) that no source and no recorded work holds. `fields` - evidence: a
    draft's fact against its recipient's own row of the request's table (stage_facts.draft_field_facts), and a name it
    gives as a field of its recipient's record that no source holds. Found live (round 5 P4, 2026-10-21, DE):
    welcome drafts named managers no source has. Which capitalised words are a name is a guess at meaning ("report to
    our Engineering Lead, <a name>, at the Head Office" gave "Engineering Lead" and "Head Office"), so a name is
    evidence for the judge and the person, never a verdict (harness review, 2026-10-21).
    """

    if not drafts:
        return [], []
    # (A failure raises: the caller makes it a check not made - never a pass.)
    sources = work_sources(request, made)
    inputs = request_input_files(request, made)
    tables = [(h, r) for _, h, r in stage_facts.tables_of(inputs)]
    facts = stage_facts.draft_field_facts(drafts, tables)
    unsourced, fields = [], []
    for draft in drafts:
        who = ", ".join(draft.get("to") or []) or "no one"
        text = " ".join(str(draft.get(k) or "") for k in ("subject", "body"))
        said = sources.unsourced(text)
        if said:
            unsourced.append(f"the draft to {who} states {', '.join(repr(s) for s in said[:6])}, which no source and "
                             "no recorded work holds")
        # Evidence only: dates and codes no source holds (often the work's own, rightly), and facts only the work's
        # own files explain.
        doubtful = sources.doubtful(text) if hasattr(sources, "doubtful") else []
        if doubtful:
            fields.append(f"the draft to {who} states {', '.join(repr(s) for s in doubtful[:6])}, which no source holds")
        fields += [f"the draft to {who}: {line}" for line in own_work_evidence(text, request, made)[:4]]
    for f in facts:
        line = f"the draft to {f['who']} says {f['header']} is \"{f['said']}\"; the source gives \"{f['own']}\""
        if f["kind"] == "name" and " ".join(f["said"].lower().split()) not in sources.flat:
            line += " - a name in no source"
        fields.append(line)
    if unsourced:
        unsourced = ["An email draft states what no source holds (" + "; ".join(unsourced)[:900] + "). Take each fact "
                     "from the sources (the request's files, the person's messages, the system's pages, your recorded "
                     "working), replace the draft (office email --replaces <its id>), and never write a value no "
                     "source gives."]
    if fields:
        fields = ["An email draft's fact may differ from its recipient's own row in the request's table ("
                  + "; ".join(fields)[:900] + ")."]
    return unsourced, fields


def _summary_totals(made):
    """stage_facts.summary_vs_detail as one finding (ADVISORY, DC): a workbook's summary figure against the rows its own
    detail sheet counts."""

    try:
        found = stage_facts.summary_vs_detail(made)
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing
        log(f"long_task: could not compare summary figures with their rows: {exc}")
        return []
    if not found:
        return []
    return ["A summary figure may not be the sum of the detail rows it stands for (" + "; ".join(found)[:800] + ")."]


def stage_code_findings(run_id, run_state, step, made, drafts, request, reply, start):
    """
    Every code-decided check a finished stage's work goes through, in order, as (check, finding) - what a program can
    decide, before any model. CHECK_MATURITY says which fail the stage. Each check runs through _checked: one that
    raises is NOT_MADE (the stage is then not checked), never a pass, and the checks after it still run. The replay
    gate (tests/test_replay_gate.py) runs them all on saved right runs, so a check that flags correct work shows there
    before it ships.
    """

    found = []
    notes, not_made = [], []

    def add(check, fn, *args, **kwargs):
        found.extend((check, p) for p in _checked(check, not_made, fn, *args, **kwargs))

    def typed():
        # What this stage typed: read inside each check, so a record that cannot be read is a check not made (R-1).
        began = start.get("entered", 0)
        if began is None:
            raise LookupError("what was typed before the stage began could not be read")
        return _entered()[began:]
    add("file_facts", code_problems, made, reply, drafts, request, instruction=step.get("instruction"),
        root=_runner().context(run_state)["work_dir"],
        outputs=list(step.get("outputs") or []) + [c["path"] for c in step.get("checks", [])
                                                    if c.get("format") != "drafts"],
        notes=notes, not_made=not_made)
    found.extend(("file_notes", n) for n in notes)
    add("source_guard", source_guard_problems, request)
    add("repeated_rows", repeated_rows, made, reply, request)
    add("repeated_rows_quiet", quiet_repeated_rows, made, reply, request)
    add("coverage", coverage_problems, run_state, step, made)
    add("removed_residue", _removed_residue, run_id, made, request)
    add("removed_still_there", removed_still_there, run_id, made)
    add("status_conflicts", _status_conflicts, run_state, step, made, drafts)
    found.extend(_left_undone_found(made, request, not_made))
    add("filler_values", lambda: invented_typing(typed(), request))
    add("ledger_findings", _stage_ledger_findings, reply)
    add("typed_doubtful", lambda: doubtful_typing(typed(), request, made))
    if CHANGED_REQUEST in (run_state["steps"][step["id"]].get("reopened") or ""):
        # (Only for a change the person made: a decision may well undo a hold - AW's redo.)
        add("lost_findings", _lost_findings, run_id, step, run_state["steps"][step["id"]], made)
    if step.get("drafts_as_needed"):
        add("reply_coverage", _reply_coverage, request, drafts, reply)
    # (A producer of (check, finding) pairs; "system_entry" names it for a failure.)
    found.extend(_checked("system_entry", not_made, system_entry_findings, run_id, step, made, request, start))
    add("app_emptied", app_emptied, made)
    add("payee_approval", _payee_approvals, run_state, request, made)
    add("files_open", files_open, run_state, made)
    add("lost_sheets", lost_sheets, run_id, run_state, step, made)
    add("summary_totals", _summary_totals, made)
    checked_drafts = _checked("draft_facts", not_made, lambda: [draft_checks(drafts, request, made)])
    unsourced, fields = checked_drafts[0] if checked_drafts else ([], [])
    found.extend(("draft_facts", p) for p in unsourced)
    found.extend(("draft_fields", p) for p in fields)
    found.extend((NOT_MADE, n) for n in dict.fromkeys(not_made))
    return found


def _reply_coverage(request, drafts, reply):
    """The input emails with neither a reply draft nor a reason in the report (reply_coverage), as one finding."""

    left = reply_coverage(input_emails(request), drafts, reply)
    if not left:
        return []
    return ["Neither a reply draft nor a reason in the report for: " + ", ".join(left[:12])
            + (f" (+{len(left) - 12} more)" if len(left) > 12 else "") + "."]


def files_open(run_state, made):
    """
    Every Office file the run hands in - this stage's and every completed stage's - that opened when it was handed in
    still opens with a strict reader (produced.no_longer_open). Run-wide on purpose: found live (series 14 P3, 2026-10-21), the approval stage's own
    script rewrote the three workbooks earlier stages had handed in, and none of them opened any more; that stage handed
    in only its log, so no check of its own files could see it.
    """

    from decurix_agent import produced
    # (This stage's own files are file_facts' - said once, there.)
    own = {str(Path(m).resolve()) for m in made}
    broken = produced.no_longer_open([p for _, p in _outputs(run_state) if str(Path(p).resolve()) not in own])
    return [f"A file this run hands in no longer opens: {b}" for b in broken]


def app_emptied(made):
    """
    A document of this stage that an app of the person's computer saved with most of what the tools wrote gone: the
    tools' version is put back, and the stage is told (tool_made.py). Found live (round 5 P1, 2026-10-20, CS):
    the memo, saved from Word, read back with 0 headings.
    """

    from decurix_agent import tool_made
    lost = tool_made.emptied(made)
    out = []
    for local, had, kept in lost:
        back = tool_made.restore(local)
        out.append(f"{Path(local).name}: saved from the app, it keeps {kept} of the {had} words the tools wrote - the app "
                   "saved another copy over it (one it still held from before, a read-only or a blank document). "
                   + ("The tools' version is put back. " if back else "The tools' version could not be put back - make it "
                      "again with the tools. ")
                   + "Open the file in the app again from its place (File > Open, its full path) so the app shows what "
                   "the tools made, check it there, and save it only when the app shows that content - never retype it "
                   "or type over the page.")
    return out


def stage_code_problems(run_id, run_state, step, made, drafts, request, reply, start):
    """Every finding of stage_code_findings, blocking and advisory alike (what the replay gate asserts on)."""

    return [text for _, text in stage_code_findings(run_id, run_state, step, made, drafts, request, reply, start)]


def _entered():
    """What this task's tools have typed into fields so far (computer/evidence.py), oldest first. A record that cannot
    be read raises: "nothing typed" would pass the typed-values checks on input they never saw (R-1, review round 3).
    The checks call it inside _checked, which makes the failure a check not made."""

    from decurix_agent.computer import evidence
    return evidence.entered()


# How invented_typing's finding on options picked with no backing begins (the final gate puts it to the person).
CHOSEN_UNBACKED = "Chosen in a form from the application's own list, and backed by nothing given"


def _stage_ledger_findings(reply):
    """What the shared ledger check holds against the task's saved records after a stage (ledger_checks.findings,
    with the stage's report): its faults. A check of them that could not be made raises; ledger_findings is ADVISORY
    and not in MADE_OR_UNCHECKED, so at the stage that only loses this evidence (_checked) - the stage is not held on
    it. What catches it is the final report, which runs ledger_checks.findings again over every stage's report and
    says a check not made there (never passed)."""

    from decurix_agent import ledger_checks
    found = ledger_checks.findings(task_context.current(), reply)
    unmade = ledger_checks.not_made(found)
    if unmade:
        raise LookupError("; ".join(unmade))
    return [str(f) for f in ledger_checks.faults(found)]


def invented_typing(typed, request, chosen=()):
    """
    Values typed into forms that no source holds: a placeholder (9999999999, test, n/a, x@example.com), or a fact - an
    address, a date, a code, a figure of 100 or more - that neither the request, its files, the person's messages, the
    system's pages nor the work's recorded working holds (provenance, harness audit 2026-10-21). One finding, or [].
    Found live (2026-10-20, P4): a joiner onboarded with phone 9999999999 - the tracker had her number.
    """

    # Options picked from an application's own list that nothing given backs (`chosen`: records.unbacked_picks): the
    # list offering one never says which this record takes (review round 9, A).
    picked = [str(v) for v in dict.fromkeys(chosen or ()) if str(v).strip()]
    picked_line = ([CHOSEN_UNBACKED + " (the request, its "
                    "files, the person's answers): " + ", ".join(repr(v) for v in picked[:6]) + ". Ask the person "
                    "(HELP) which entry it takes, then correct the record in the application."] if picked else [])
    typed = [v for v in dict.fromkeys(typed) if str(v).strip()]
    if not typed:
        return picked_line
    # (A failure raises: the caller makes it a check not made - never a pass.)
    from decurix_agent import filler
    texts = filler.source_texts(request, [i.split(" (= ")[0] for i in run_inputs(request)])
    # Hygiene: a placeholder (test, n/a, 9999999999) that no source holds - never an entry the application's own list
    # held (evidence.listed_picks; found live 2026-11-09, a portal's "NA" unit chosen as the demonstration did).
    from decurix_agent.computer import evidence, records
    listed = {" ".join(v.split()).casefold() for v in evidence.listed_picks()}
    texts = texts + [records._answers_text()]  # (the person's confirmed answers in this task are a source too)
    made_up = [repr(v) for v in typed if filler.looks_filler(v) and not filler.in_sources(v, texts)
               and " ".join(str(v).split()).casefold() not in listed]
    # Provenance: a fact typed (an address, a date, a code, a figure of 100 or more) that no source holds.
    sources = work_sources(request)
    # A figure worked out in one step from another value typed in this work and a source figure is held: the tax of
    # a line typed beside its amount and rate (found live, 2026-10-24: 260 and 6,480 - 5,200 at 5% and 36,000 at
    # 18%, a bill printing only their total - were "held by no source" and a finished task was refused).
    from decurix_agent.provenance import _figures
    typed_figures = _figures(" ".join(typed))
    for value in typed:
        made_up += [repr(s) for s in sources.unsourced(value, operands=typed_figures)]
    made_up = list(dict.fromkeys(made_up))
    if not made_up:
        return picked_line
    return picked_line + [
        "Typed into a form, and held by no source (the request, its files, the person's messages, the system's "
        "pages, your recorded working): " + ", ".join(made_up[:6]) + ". Find the real value in the sources "
        "(format it to what the field accepts) - a value you worked out goes into a working file first - correct "
        "the record in the application, and say what was entered; if it cannot be corrected there, write the "
        "WRONG IN THE APPLICATION line for it."]


def doubtful_typing(typed, request, made=()):
    """
    Evidence for the judge about values typed into forms (never a verdict): dates and letter-and-digit codes no source
    holds (provenance.Sources.doubtful), and facts only the work's own files explain (own_work_evidence).
    """

    typed = [v for v in dict.fromkeys(typed) if str(v).strip()]
    if not typed:
        return []
    try:
        sources = work_sources(request)
        doubtful = list(dict.fromkeys(repr(d) for v in typed for d in (sources.doubtful(v) if hasattr(sources, "doubtful")
                                                                        else [])))
        own = [line for v in typed for line in own_work_evidence(v, request, made)]
    except Exception as exc:  # noqa: BLE001
        log(f"long_task: could not weigh typed values: {exc}")
        return []
    out = []
    if doubtful:
        out.append("Typed into a form, and held by no source the run has: " + ", ".join(doubtful[:6]) + ".")
    if own:
        out.append("Typed into a form: " + "; ".join(dict.fromkeys(own))[:700] + ".")
    return out


def _flat(text):
    return " ".join(str(text or "").split())


# ============================================================
# The cross-check: figures that appear in several outputs
# ============================================================

_TOTALISH = re.compile(r"\b(total|net|sum|balance|payable|receivable|revenue|sales|expenses?|tax|credit|profit|loss|"
                       r"variance|closing|opening|outstanding|due|gross|cost|amount|count|grand)\b", re.I)
_STOP = {"the", "a", "an", "of", "for", "in", "on", "to", "and", "rs", "inr", "amount", "value", "figure", "is", "as",
         "at", "per", "by"}


def _label_key(label):
    words = [w for w in re.findall(r"[a-z]+", (label or "").lower()) if w not in _STOP]
    return " ".join(words[:8])


def _rows(path):
    """(label, [figure texts]) for each row or line of an output, read the way office reads it."""

    suffix = Path(path).suffix.lower()
    rows = []
    if suffix in {".xlsx", ".xlsm"}:
        try:
            import openpyxl
            book = openpyxl.load_workbook(path, data_only=True, read_only=True)
            for sheet in book.worksheets:
                for row in sheet.iter_rows(values_only=True):
                    label, figures = [], []
                    for cell in row:
                        if isinstance(cell, bool) or cell is None:
                            continue
                        if isinstance(cell, (int, float)):
                            if label:
                                figures.append(repr(cell) if isinstance(cell, float) else str(cell))
                        elif str(cell).strip() and not figures:
                            label.append(str(cell).strip())
                    if label and figures:
                        rows.append((" ".join(label), figures))
            book.close()
        except Exception as exc:
            log(f"long_task: could not read {path}: {exc}")
        return rows
    try:
        from decurix_agent.tools import office
        text = office.read_document(path).get("text") or ""
    except Exception as exc:
        log(f"long_task: could not read {path}: {exc}")
        return rows
    for line in text.splitlines():
        cells = [c.strip() for c in re.split(r"\s*(?:\||\t|:\s)\s*", line) if c.strip()]
        label, figures = [], []
        for cell in cells:
            found = parse_number(cell)
            if found and re.fullmatch(r"[^A-Za-z]*\d[\d,.]*[^A-Za-z]*(?:lakhs?|lacs?|crores?|cr|k|m|mn|bn)?\.?", cell, re.I):
                if label:
                    figures.append(cell)
            elif not figures:
                label.append(cell)
        if label and figures:
            rows.append((" ".join(label)[-80:], figures))
    return rows


def _outputs(run_state):
    """(stage id, absolute path) of every checked output file of a completed stage."""

    work = Path(_runner().context(run_state)["work_dir"])
    out, seen = [], set()
    for step in _steps(run_state):
        record = run_state["steps"][step["id"]]
        if record["status"] != "completed":
            continue
        for item in record["evidence"]:
            if item.get("drafts"):
                continue
            path = str(work / item["path"])
            if path not in seen:
                seen.add(path)
                out.append((step["id"], path))
    return out


def code_disagreements(outputs, read=None):
    """
    Figures that two or more outputs give under the same label (a total in
    the workbook and on a slide) and that do not agree, decided in code.
    Returns (disagreements, agreed count); a disagreement is {"figure",
    "values": [{"stage", "file", "text"}]}.
    """

    read = read or _rows
    seen = {}
    for stage, path in outputs:
        for label, figures in read(path):
            if not _TOTALISH.search(label):
                continue
            key = _label_key(label)
            # Only a label with a word of its own is one figure across files: "Total" or "Net amount" alone is every
            # table's, and a register's total against an ageing report's is no disagreement (E-17, review 2026-11-03).
            if not key or not [w for w in key.split() if not _TOTALISH.fullmatch(w)]:
                continue
            seen.setdefault(key, {}).setdefault(path, (stage, label, figures))
    disagreements, agreed = [], 0
    for key, by_file in seen.items():
        if len(by_file) < 2:
            continue
        parsed = {path: [p for p in (parse_number(f) for f in figs) if p] for path, (_, _, figs) in by_file.items()}
        first = next(iter(parsed))
        if all(any(same_figure(a, b) for a in parsed[first] for b in parsed[other]) for other in parsed if other != first):
            agreed += 1
            continue
        disagreements.append({"figure": key, "values": [
            {"stage": stage, "file": path, "text": f"{label}: {', '.join(figs[:3])}"}
            for path, (stage, label, figs) in by_file.items()]})
    return disagreements, agreed


CROSS_SYSTEM = """Several output files of one piece of work are shown. List the figures that appear in two or more of
them and should be the same figure (the same total, count, amount, quantity, percentage or date stated in a workbook, a
slide, a memo, an email). Only those: a file name, an order, invoice or PO number, an ID, a reference or a code is not
a figure - never list one. Quote each place exactly as it is written there (a short snippet that contains the figure), and give the
figure itself exactly as that place writes it ("value": "12/03/2026", "480,000").
Answer with JSON only: {"figures": [{"figure": "what it is", "places": [{"file": "file name", "text": "exact snippet",
"value": "the figure as written there"}]}]}
List at most 15 figures; never list a figure that appears in only one file."""


# ------------------------------------------------------------ one fact's value, however it is written
#
# Found live (2026-10-20, P4, 36/36 right): the report said "Not everything is done" and listed, for every joiner,
# "Figures that do not agree" - 02/11/2026 against "Your date of joining will be 02/11/2026.", 480000 against
# "Rs 480,000" - all the same. A place's figure was the first number of its snippet (a row's employee number, a date's
# day), and nothing put a date or a grouped amount in one form. Now each place gives its figure, found in its snippet,
# and figures are compared as the same kind of thing: a date as a date, an amount as a number.

def _day_first(request):
    """True when the request writes dates day first (DD/MM), False month first (MM/DD), None when it does not say
    (task_context.request_date_marker: one reading of the marker for every check)."""

    from decurix_agent import task_context
    marker = task_context.request_date_marker(request)
    return None if marker is None else marker == "day"


def _place_value(snippet, value):
    """The figure a place gives: its own "value" when the snippet holds it, else the snippet's one figure, else None
    (a row with several figures cannot say which one the label means)."""

    if value and _flat(value) in _flat(snippet):
        return _flat(value)
    figures = re.findall(r"\d{1,2}[/.\-]\d{1,2}[/.\-]\d{4}|\d{4}-\d{2}-\d{2}|(?:Rs\.?\s*|₹\s*)?\d[\d,]*(?:\.\d+)?", snippet or "")
    return figures[0] if len(figures) == 1 else None


def figure_sign(snippet, value):
    """
    The sign a place writes on its figure: -1 or +1 from an explicit -/+, brackets or Dr/Cr; None when it writes none.
    No word near it decides a sign (series 15 H2, 2026-10-21: "short", "over", "less" near equal figures split them, and
    a recon's -3,926 shortage against a journal's 3,926 debit - two right conventions - was "do not agree"). Equal
    magnitudes with opposite written signs are only a quiet note (model_disagreements); the judge reads the files.
    """

    v = " ".join(str(value or "").split())
    # (Brackets are negative only around an unsigned figure: "(+11.55%)" is a positive one in brackets.)
    if re.match(r"^\s*\(?\s*\+", v) or re.search(r"(?i)\bcr\.?\s*$", v):
        return 1
    if re.match(r"^\s*\(.*\)\s*$", v) or re.match(r"^\s*\(?\s*[-\u2212]", v) or re.search(r"(?i)\bdr\.?\s*$", v):
        return -1
    return None


# Words after a figure that scale it (fact_value reads them) or sign it: not a unit of what is counted.
_SCALE_WORDS = {"lakh", "lakhs", "lac", "lacs", "crore", "crores", "cr", "thousand", "million", "mn", "k", "l", "bn",
                "billion", "dr", "cr"}


# Words after a figure that say what it is, not what it counts in ("149 per unit", "3,576 total").
_NOT_A_UNIT = {"per", "total", "each", "of", "in", "at", "and", "or", "to", "for", "the", "a", "an", "on", "from", "by",
               "is", "was", "are", "were", "as", "with", "against", "vs", "net", "gross"}


def _unit_after(snippet, value):
    """The word a place writes right after its figure ("12 BOX" -> "box", "144 PCS" -> "pcs"), or "" (none, a scale)."""

    flat = " ".join(str(snippet or "").split())
    v = " ".join(str(value or "").split())
    at = flat.find(v) if v else -1
    if at < 0:
        return ""
    m = re.match(r"\s*([A-Za-z]+)", flat[at + len(v):])
    word = m.group(1).lower() if m else ""
    return "" if word in _SCALE_WORDS or word in _NOT_A_UNIT else word


def model_disagreements(outputs, ask=None, request="", notes=None):
    """
    Figures the model says should agree across outputs, each place found
    verbatim in its file and compared in code. The model only points; code
    decides. Returns (disagreements, agreed count). A disagreement is a
    different magnitude only, in the same unit; equal magnitudes written with
    opposite signs are a quiet line appended to `notes` (never "do not agree").
    """

    texts = {}
    for stage, path in outputs:
        try:
            from decurix_agent.tools import office
            texts[path] = (stage, office.read_document(path).get("text") or "")
        except Exception as exc:
            log(f"long_task: could not read {path}: {exc}")
    if len(texts) < 2:
        return [], 0
    share = 24000 // len(texts)
    shown = "\n\n".join(f"--- {os.path.basename(p)}\n{t[:share]}" for p, (_, t) in texts.items())
    try:
        found = (ask or _ask_json)(CROSS_SYSTEM, shown).get("figures") or []
    except Exception as exc:
        log(f"long_task: the cross-check could not ask the model: {exc}")
        return [], 0
    by_name = {os.path.basename(p): p for p in texts}
    day_first = _day_first(request)
    disagreements, agreed = [], 0
    for item in found[:15]:
        places = []
        for place in item.get("places") or []:
            path = by_name.get(os.path.basename(str(place.get("file") or "")))
            snippet = str(place.get("text") or "")
            if not path or _flat(snippet) not in _flat(texts[path][1]):
                continue
            value = _place_value(snippet, place.get("value"))
            if value is None:
                continue  # which figure of the snippet is meant cannot be told: never a disagreement
            if identifier_like(value, day_first) or identifier_like(place.get("value"), day_first):
                places = []
                break  # a name or a code, not a figure: never compared
            places.append((path, snippet, value, fact_value(value, day_first, declared_unit(texts[path][1]))))
        if len({p for p, _, _, _ in places}) < 2:
            continue
        units = {_unit_after(snippet, value) for _, snippet, value, _ in places} - {""}
        if len(units) > 1:
            continue  # "12 BOX", "144 PCS", "12 per BOX": counted in other units - never compared (H2)
        # A figure named as a percentage ("Variance %") compares a bare 11.55 or 0.1155 with 11.55% on one scale.
        percent = bool(re.search(r"(?i)%|\bper ?cent|\bpct\b|\bmargin\b|\brate\b", str(item.get("figure") or "")))
        if all(same_fact(places[0][3], other[3], percent) for other in places[1:]):
            agreed += 1
            # Equal magnitudes; two written signs that differ are two conventions (a recon's shortage negative, a
            # journal's debit positive) - a quiet line, at most.
            signs = {figure_sign(snippet, value) for _, snippet, value, fact in places if fact[0] == "number"}
            if notes is not None and -1 in signs and len(signs) > 1:
                notes.append(f"{str(item.get('figure') or 'a figure')[:120]}: the same amount, shown with opposite signs "
                             "in " + " and ".join(dict.fromkeys(os.path.basename(p) for p, _, _, _ in places))
                             + " - check that each sign follows its file's own convention.")
            continue
        disagreements.append({"figure": str(item.get("figure") or "a figure")[:120], "values": [
            {"stage": texts[path][0], "file": path, "text": snippet[:200],
             "value": value} for path, snippet, value, _ in places]})
    return disagreements, agreed


def _depends(run_state, later, earlier):
    deps = {s["id"]: set(s.get("depends_on", [])) for s in _steps(run_state)}
    stack, seen = [later], set()
    while stack:
        current = stack.pop()
        for d in deps.get(current, ()):
            if d == earlier:
                return True
            if d not in seen:
                seen.add(d)
                stack.append(d)
    return False


def odd_one_out(run_state, disagreement):
    """The stage whose output disagrees with the rest, or None when code cannot tell (the person decides)."""

    values = disagreement["values"]
    groups = []
    for value in values:
        if value.get("value") is not None:
            fact = fact_value(value["value"])
            match = lambda a, b: same_fact(a, b)  # noqa: E731
        else:
            fact = parse_number(value["text"].split(":", 1)[-1]) or parse_number(value["text"])
            match = lambda a, b: same_figure(a, b)  # noqa: E731
        for group in groups:
            if fact and group[0] and type(group[0]) is type(fact) and match(group[0], fact):
                group[1].append(value)
                break
        else:
            groups.append((fact, [value]))
    groups.sort(key=lambda g: len(g[1]), reverse=True)
    if len(groups) >= 2 and len(groups[0][1]) > len(groups[1][1]):
        stages = {v["stage"] for g in groups[1:] for v in g[1]}
        return next(iter(stages)) if len(stages) == 1 else None
    stages = list(dict.fromkeys(v["stage"] for v in values))
    if len(stages) == 2:
        a, b = stages
        if _depends(run_state, a, b):
            return a
        if _depends(run_state, b, a):
            return b
    if len(stages) == 1:
        return stages[0]
    return None
