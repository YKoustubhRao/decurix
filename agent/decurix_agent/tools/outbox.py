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

    names = ", ".join(f["name"] for f in files) or "no attachments"
    progress.emit(f"Drafted an email to {', '.join(to)}: {payload['subject']}")
    return (f"Drafted (id {answer.get('id', '?')}) to {', '.join(to)}, subject {payload['subject']!r}, with {names}. "
            "It waits in the Outbox for a person to send it - it has NOT been sent.")
