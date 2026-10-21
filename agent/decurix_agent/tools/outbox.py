"""
Drafting an email into the control plane's Outbox.

An agent never sends email: sending in a person's name is exactly the action
every engine is told to stop before. So a draft goes to the control plane
(POST /api/outbox, internal/outbox), the Triggers page lists it with its
attachments, and only a person's Send sends it. Used by TheBot through the
`office email` shell command, and by the older Office (GPT) harness's
draft_email tool - one implementation, so both put the same thing there.
"""

import base64
import json
import mimetypes
import os
import re
import urllib.error
import urllib.request
from pathlib import Path

from decurix_agent import config, progress, task_context

OUTBOX_URL = config.CONTROL_PLANE_URL.rstrip("/") + "/api/outbox"
MAX_ATTACHMENT_BYTES = 20 * 1024 * 1024

# What each task drafted, kept on the desktop (no attachment data): the
# drafts themselves go to the control plane, and what a task did has to be
# readable here - by the final check, which reads back what a task made, and
# by the proof of a script the worker wrote from such a task (playbooks.py).
DRAFTS_DIR = config.DECURIX_DIR / "computer" / "drafts"
KEEP_DRAFT_LOGS = 60
# Set (to a file) while such a script is being proven: a draft is written
# there and goes nowhere else, so trying the script leaves no second email in
# a person's Outbox.
DRY_RUN = "DECURIX_OUTBOX_DRY"


def _log_path(task_id):
    return DRAFTS_DIR / f"{task_id}.jsonl"


def _note(path, record):
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(record, ensure_ascii=False) + "\n")


def _trim_logs():
    try:
        logs = sorted(DRAFTS_DIR.glob("*.jsonl"), key=lambda p: p.stat().st_mtime, reverse=True)
        for old in logs[KEEP_DRAFT_LOGS:]:
            old.unlink()
    except OSError:
        pass


def read_log(path):
    """The drafts noted in one log file, oldest first; [] when there is none."""

    try:
        lines = Path(path).read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    out = []
    for line in lines:
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if isinstance(record, dict):
            out.append(record)
    return out


def drafted(task_id=None):
    """The emails a task drafted (the current one's when none is named): {"to", "cc", "subject", "body", "attachments"} each."""

    task_id = task_id if task_id is not None else task_context.current()
    # A draft made outside any task (a command run by hand) is no task's.
    records = read_log(_log_path(task_id)) if task_id else []
    # A draft a later one of the same task replaced is no longer in the Outbox, and no longer this task's work.
    gone = {r.get("replaces") for r in records if r.get("replaces")}
    return [r for r in records if not r.get("id") or r.get("id") not in gone]


def _subject_words(subject):
    return re.findall(r"[a-z0-9]+", (subject or "").lower())


def _same_recipients(record, to):
    return sorted(a.lower() for a in record.get("to") or []) == sorted(a.lower() for a in to)


def _same_email(record, to, subject, attached=None):
    """The same letter: the same recipients, a subject with the same words (found live: a second attempt
    wrote "Final notice - overdue invoices" where the first had "Final notice: overdue invoices"), and the same
    files attached. Found in review (2026-10-14): without the files, one task's separate letters to one address
    under one subject (an invoice each, "Invoice" every time) replaced each other and only the last was left."""

    return (_same_recipients(record, to)
            and _subject_words(record.get("subject")) == _subject_words(subject)
            and sorted(record.get("attachments") or []) == sorted(attached or []))


def _stage():
    """(run:stage, attempt) of the long request's stage running now, or ("", 0)."""

    try:
        from decurix_agent import workflow_task
        active = workflow_task._active
        if active:
            attempt = workflow_task.runner().status(active[0])["steps"][active[1]]["attempts"]
            return f"{active[0]}:{active[1]}", attempt
    except Exception:  # noqa: BLE001 - a draft outside a run is simply a draft
        pass
    # Another process (`office email`, run by the stage's engine): the stage the agent marked (task_context.set_stage).
    return task_context.current_stage()


class DraftError(Exception):
    """A draft that was not made, and why, in words for the model."""


def _reason(exc):
    try:
        return json.loads(exc.read().decode("utf-8") or "{}").get("error") or str(exc)
    except (ValueError, OSError):
        return str(exc)


ADDRESS = re.compile(r"[^@\s<>,;]+@[^@\s<>,;]+\.[^@\s<>,;]+")


def _address_of(recipient):
    """The address in a recipient written plainly or as "Name <address>"."""

    m = re.search(r"<([^<>]+)>\s*$", recipient)
    return (m.group(1) if m else recipient).strip()


def _recipients(values):
    out = []
    for value in values or []:
        for part in str(value).replace(";", ",").split(","):
            if part.strip():
                out.append(part.strip())
    return out


def draft(to, subject, body, cc=None, attachments=None, replaces=None):
    """
    Put one email in the Outbox. Returns a sentence saying what was drafted;
    raises DraftError (nothing drafted) when a recipient or an attachment is
    missing, the attachments are too large, or the control plane refused it.

    `replaces` names an earlier draft of this same task that the new one takes
    the place of (the control plane discards it; it was never sent). A draft to
    the same recipients with the same subject as one this task already made
    replaces it on its own: found live (2026-10-13), a worker sent back to fix
    two figures drafted all seven of its letters a second time.
    """

    to = _recipients(to)
    if not to:
        raise DraftError("give at least one recipient.")
    # A recipient is an email address. Found live (2026-10-14): drafts went to "grant", "holt" and "wei" - a rep's
    # surname where the address was meant - and waited in the Outbox beside the right ones.
    bad = [r for r in to + _recipients(cc) if not ADDRESS.fullmatch(_address_of(r))]
    if bad:
        raise DraftError(f"{', '.join(repr(b) for b in bad)} is not an email address - give each recipient's full "
                         "address (name@domain). Nothing was drafted.")
    task_id = task_context.current() or ""
    explicit = bool(replaces)
    stage, attempt = _stage()
    if not replaces and task_id:
        attached = [Path(os.path.expanduser(str(path))).name for path in attachments or []]
        earlier = [r for r in drafted(task_id) if r.get("id") and _same_email(r, to, subject, attached)]
        if not earlier and stage:
            # A long request's stage run again (a retry, a re-check after a change) writes again to whom it wrote
            # before: that letter takes the earlier one's place, whatever its subject. Found live (round 5 P1,
            # 2026-10-20): a re-check drafted a second reminder to the same customer beside the first.
            earlier = [r for r in drafted(task_id) if r.get("id") and r.get("stage") == stage
                       and r.get("attempt") != attempt and _same_recipients(r, to)]
        if not earlier and task_context.continues():
            # A follow-up (a change the person sent after the task was reported) rewrites the letters that task drafted:
            # the new one to the same recipients takes the old one's place (series 16 H3, 2026-10-21: "leave Ishaan
            # out" redid the drafts, and the first run's Rao draft, still billing him, stayed beside the new one).
            earlier = [r for r in drafted(task_context.continues()) if r.get("id") and _same_recipients(r, to)]
        if earlier:
            replaces = earlier[-1]["id"]

    files = []
    total = 0
    for path in attachments or []:
        p = Path(os.path.expanduser(str(path)))
        if not p.is_absolute():
            p = config.WORK_DIR / p
        if not p.is_file():
            raise DraftError(f"attachment {path} does not exist.")
        data = p.read_bytes()
        total += len(data)
        if total > MAX_ATTACHMENT_BYTES:
            raise DraftError("the attachments are larger than 20 MB together.")
        files.append({
            "name": p.name,
            "content_type": mimetypes.guess_type(p.name)[0] or "application/octet-stream",
            "data": base64.b64encode(data).decode("ascii"),
        })

    record = {"to": to, "cc": _recipients(cc), "subject": (subject or "").strip(), "body": body or "",
              "attachments": [f["name"] for f in files]}
    if stage:
        record.update(stage=stage, attempt=attempt)
    names = ", ".join(f["name"] for f in files) or "no attachments"

    dry = os.environ.get(DRY_RUN)
    if dry:
        _note(Path(dry), record)
        return (f"Drafted (id trial) to {', '.join(to)}, subject {record['subject']!r}, with {names}. "
                "It waits in the Outbox for a person to send it - it has NOT been sent.")

    payload = {
        "desktop_id": config.DESKTOP_ID,
        "task_id": task_id,
        "replaces": replaces or "",
        "continues": task_context.continues(),
        "to": to,
        "cc": _recipients(cc),
        "subject": (subject or "").strip(),
        "body": body or "",
        "attachments": files,
    }

    def post(body):
        request = urllib.request.Request(
            OUTBOX_URL, data=json.dumps(body).encode("utf-8"), method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.loads(response.read().decode("utf-8") or "{}")

    try:
        try:
            answer = post(payload)
        except urllib.error.HTTPError as exc:
            if not payload["replaces"] or exc.code not in (404, 409):
                raise
            reason = _reason(exc)
            if explicit:
                raise DraftError(f"it cannot replace {payload['replaces']}: {reason}. Nothing was drafted.") from exc
            # The earlier draft is gone already (a person discarded or sent it): this one is simply a new draft.
            payload["replaces"] = ""
            answer = post(payload)
    except DraftError:
        raise
    except Exception as exc:  # the control plane is down, or refused it
        raise DraftError(f"the control plane did not take the draft ({exc}).") from exc

    record["id"] = answer.get("id") or ""
    if answer.get("replaced"):
        record["replaces"] = answer["replaced"]
    try:
        if task_id:
            _note(_log_path(task_id), record)
            _trim_logs()
    except OSError:
        pass   # the draft is made; only this desktop's own note of it is missing

    progress.emit(f"Drafted an email to {', '.join(to)}: {payload['subject']}")
    replaced = f" It replaces the earlier draft {answer['replaced']}, which is no longer in the Outbox." if answer.get("replaced") else ""
    if payload["replaces"] and "replaced" not in answer:
        # A control plane from before drafts could be replaced takes the draft and ignores `replaces`. Found live
        # (2026-10-14): a worker newer than its control plane left two copies of every corrected letter, each
        # reported as a replacement. Say it, so the report does not claim one letter where there are two.
        replaced = (f" The Outbox did not take it as a replacement: the earlier draft {payload['replaces']} is still "
                    "there too, and a person has to discard it.")
    return (f"Drafted (id {answer.get('id', '?')}) to {', '.join(to)}, subject {payload['subject']!r}, with {names}. "
            "It waits in the Outbox for a person to send it - it has NOT been sent." + replaced)
