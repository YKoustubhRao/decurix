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
    return read_log(_log_path(task_id)) if task_id else []


class DraftError(Exception):
    """A draft that was not made, and why, in words for the model."""


def _recipients(values):
    out = []
    for value in values or []:
        for part in str(value).replace(";", ",").split(","):
            if part.strip():
                out.append(part.strip())
    return out


def draft(to, subject, body, cc=None, attachments=None):
    """
    Put one email in the Outbox. Returns a sentence saying what was drafted;
    raises DraftError (nothing drafted) when a recipient or an attachment is
    missing, the attachments are too large, or the control plane refused it.
    """

    to = _recipients(to)
    if not to:
        raise DraftError("give at least one recipient.")

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
    names = ", ".join(f["name"] for f in files) or "no attachments"

    dry = os.environ.get(DRY_RUN)
    if dry:
        _note(Path(dry), record)
        return (f"Drafted (id trial) to {', '.join(to)}, subject {record['subject']!r}, with {names}. "
                "It waits in the Outbox for a person to send it - it has NOT been sent.")

    payload = {
        "desktop_id": config.DESKTOP_ID,
        "task_id": task_context.current() or "",
        "to": to,
        "cc": _recipients(cc),
        "subject": (subject or "").strip(),
        "body": body or "",
        "attachments": files,
    }

    request = urllib.request.Request(
        OUTBOX_URL, data=json.dumps(payload).encode("utf-8"), method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            answer = json.loads(response.read().decode("utf-8") or "{}")
    except Exception as exc:  # the control plane is down, or refused it
        raise DraftError(f"the control plane did not take the draft ({exc}).") from exc

    try:
        if task_context.current():
            _note(_log_path(task_context.current()), record)
            _trim_logs()
    except OSError:
        pass   # the draft is made; only this desktop's own note of it is missing

    progress.emit(f"Drafted an email to {', '.join(to)}: {payload['subject']}")
    return (f"Drafted (id {answer.get('id', '?')}) to {', '.join(to)}, subject {payload['subject']!r}, with {names}. "
            "It waits in the Outbox for a person to send it - it has NOT been sent.")
