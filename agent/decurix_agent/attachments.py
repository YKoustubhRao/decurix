"""
Files a person attached to a request, or to a ticket they are recording.

The control plane writes them into this desktop's ~/work/inbox/<id>/ before
it hands the request over, and names them at the end of the request, one path
a line under MARK (httpapi/attachments.go's attachmentsMark builds the same
block: change one and the other's test says so). The paths are in the
request's own text on purpose - that is where every engine, the check of a
finished task (verify.py) and a taught skill filling its values (binding.py)
already look for a run's documents.

What needs the request *without* them is anything that compares its words: a
ticket's title, a recorded task matched against a new request (mimic.py,
request_reading.py), a question for a web app (ask_task.py).
"""

import os

from decurix_agent.logging_utils import log

MARK = "Files attached to this request, saved on this desktop:"

# How many of a lesson's documents the model is shown, and how much of each.
MAX_DOCUMENTS = 4
MAX_DOCUMENT_CHARS = 6000
MAX_DOCUMENT_PICTURES = 4


def split(content):
    """(the request as the person wrote it, [paths of the files it brought])."""

    text, found, rest = (content or "").partition(MARK)
    if not found:
        return (content or ""), []

    return text.rstrip(), [line.strip() for line in rest.splitlines() if line.strip()]


def text(content):
    """A request without the files it brought."""

    return split(content)[0]


def files(content):
    """The paths of the files a request brought."""

    return split(content)[1]


def block(paths):
    """The lines that name `paths` in a request ("" for none)."""

    if not paths:
        return ""

    return MARK + "".join(f"\n  {path}" for path in paths)


def names(paths):
    """The files' own names, for a line a person reads."""

    return ", ".join(os.path.basename(str(path).replace("\\", "/")) for path in paths)


def parts(paths, heading):
    """
    The documents at `paths` as content parts for a hosted model: each one's
    text, and the pages of a scan as pictures - what a person working from
    them had in front of them. Best-effort: a file that is gone or cannot be
    read is said to be so, never left out silently.
    """

    # Imported here: office.py pulls in the document tools, and this module is
    # read by the watcher for every request.
    from decurix_agent import llm_client
    from decurix_agent.tools import office

    out, pictures = [], 0

    for path in list(paths or [])[:MAX_DOCUMENTS]:
        if not os.path.isfile(path):
            out.append({"type": "text", "text": f"\n[{path}: no longer on this desktop]"})
            continue

        try:
            doc = office.read_document(path)
        except (office.OfficeError, OSError, ValueError) as exc:
            log(f"attachments: could not read {path}: {exc}")
            out.append({"type": "text", "text": f"\n[{path}: could not be read - {exc}]"})
            continue

        out.append({"type": "text", "text": f"\n--- Document {path} ---\n{(doc.get('text') or '')[:MAX_DOCUMENT_CHARS]}"})
        for image in doc.get("images") or []:
            if pictures == MAX_DOCUMENT_PICTURES:
                break
            out.append(llm_client.image_part(path=image))
            pictures += 1

    if out:
        out.insert(0, {"type": "text", "text": heading})

    return out
