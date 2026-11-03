import json
import os
import re
import shutil
import time
from pathlib import Path

from decurix_agent import attachments, config, shared
from decurix_agent.logging_utils import log
from decurix_agent.provenance import _squash


# ============================================================
# The files a task made or changed
#
# A task that ends in a file - a workbook, a summary, a merged PDF - used to
# be reported on the worker's word alone: the final check looked only at
# tasks that acted on the screen. Found live: asked to compare two
# workbooks, a worker's report said two entries "were correctly matched"
# while the workbook it had just written listed both as unmatched; nothing
# compared the two, and the task was recorded done.
#
# So what a task wrote is read back and judged against its report
# (skill_task._gate -> verify.judge(produced=...)). What counts is decided
# with no model: a readable file in the work folder, or one the report names,
# last written after the task began.
# ============================================================

# Pictures are described (format and size in pixels), not read: found live (2026-10-15), product photos resized
# and exported in GIMP were "not shown by the screen", and the check could not confirm a correct task.
IMAGES = {".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff"}
READABLE = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv", ".tsv", ".txt", ".md", ".json", ".xml", ".html", ".htm", ".eml",
            ".ods", ".odt", ".odp", ".svg"} | IMAGES

MAX_FILES = 4
# What a person asked for, ahead of what a worker made on the way when only MAX_FILES are read. Found live
# (2026-10-14): JSON specs and notes written after a workbook pushed it out of the four newest.
DELIVERABLES = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv", ".ods", ".odt", ".odp", ".svg"} | IMAGES
MAX_FILE_CHARS = 7000

# Folders that hold tools' own files, never a task's result (decurix-reports: the whole text of a message too long
# for the chat, messaging.REPORTS_DIR).
SKIP_DIRS = {"node_modules", "__pycache__", "venv", "site-packages", "target", "dist", "build", "decurix-reports"}

# A file a request asks for by name ("make contract_changes.docx in my work folder").
REQUESTED_FILE = re.compile(r"\b[\w-][\w.-]*\.(?:xlsx|xlsm|docx|pptx|pdf|csv|tsv|txt|md|json|xml|html?|eml|ods|odt|odp|svg|png|jpe?g|gif|webp|bmp|tiff?)\b", re.I)
NAMED_PATH = re.compile(r"(?:~|/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.[a-z0-9]{2,5}\b", re.I)
# A path written in quotes or backticks, which may hold spaces ("/home/headless/PC/Tax Returns/x.xlsx").
QUOTED_PATH = re.compile(r"[`'\"]((?:~|/|[a-z]:[\\/])[^`'\"\n]*?[\w-]\.[a-z0-9]{2,5})[`'\"]", re.I)


def since():
    """When the task being worked on began (seconds), or None when that is not known."""

    try:
        record = json.loads((config.DECURIX_DIR / "active-task.json").read_text(encoding="utf-8"))
        return float(record["since"])
    except (OSError, ValueError, KeyError, TypeError):
        return None


def _walk(root):
    """Readable files under root, a few levels deep, hidden and tool folders left out."""

    return shared.walk_files(root, SKIP_DIRS)


def files(report="", started=None, root=None, request=""):
    """
    The readable files this task wrote, newest first, at most MAX_FILES:
    those in the work folder and those the report names, last written after
    the task began. Empty when the task's start is not known - never a guess
    from the files that happen to be there.

    When the request names the files it wants (contract_changes.docx), those
    and the ones the report names are the result; a working file the worker
    made on the way - a JSON spec it fed a tool, a script - is not judged as
    one. Found live (2026-10-13): a correct document was refused because an
    earlier draft of its spec, still in the work folder, "conflicted" with it.
    """

    wanted = {name.lower() for name in REQUESTED_FILE.findall(attachments.text(request or ""))}
    named = {Path(os.path.expanduser(p)).name.lower() for p in NAMED_PATH.findall(report or "")}

    started = since() if started is None else started
    if started is None:
        return []

    candidates = set(_walk(root or config.WORK_DIR))
    if root is None:
        candidates.update(shared.walk())  # what the task made in the shared folder counts like the work folder
    # (`raw`, not `named`: reusing that name here left it holding the last path's text, and every file the report
    # named but the request did not was then dropped from what is read back - review 2026-11-02.)
    for raw in NAMED_PATH.findall(report or ""):
        candidates.add(Path(os.path.expanduser(raw)))

    made = []
    for path in candidates:
        if path.suffix.lower() not in READABLE:
            continue
        try:
            stat = path.stat()
        except OSError:
            continue
        if wanted and path.name.lower() not in wanted and path.name.lower() not in named:
            continue
        if stat.st_mtime > started and stat.st_size > 0:
            made.append((path.suffix.lower() in DELIVERABLES or path.name.lower() in named, stat.st_mtime, str(path)))

    return [path for _, _, path in sorted(made, reverse=True)[:MAX_FILES]]


def named_sources(report="", request="", made=()):
    """
    Files the report or the request names by path that exist and can be read, other than those in `made`: what the
    work read, for a judge that asks to look closer. Found live (2026-10-21): the report compared a
    portal's ledger with "Closing credit carried forward" in the workbook it named, read off the wrong month's row; the
    check could look only in files the task made, so it could neither confirm nor catch it.
    """

    seen = {str(m) for m in made or ()}
    out = []
    text = "\n".join((report or "", attachments.text(request or "")))
    for raw in QUOTED_PATH.findall(text) + NAMED_PATH.findall(text):
        path = Path(os.path.expanduser(raw.strip()))
        if str(path) in seen or path.suffix.lower() not in READABLE:
            continue
        try:
            if path.is_file() and path.stat().st_size > 0:
                seen.add(str(path))
                out.append(str(path))
        except OSError:
            continue
    return out[:MAX_FILES]


# ============================================================
# Workbooks whose formulas hold no results
#
# A formula a script writes (openpyxl) has no result in the file until a
# spreadsheet program saves it: Excel and LibreOffice work it out on open,
# but an import, a preview, a phone or a script reading the file sees a
# blank. Found live (2026-10-19): a lookup workbook written by the worker's
# own script was read back with `office read` - which calculates on the fly
# - and reported "completed and verified"; every price and total in the
# file handed in was empty. So before a task's files are judged, the
# workbooks it wrote get their results stored (office's LibreOffice
# calculation, formulas kept), whatever wrote them; one that still lacks
# them is a fact the final check decides on, with no model.
# ============================================================


def store_results(paths):
    """
    Store the formula results of the workbooks among `paths` that have formulas without them. Returns, for each one
    that still has some afterwards (LibreOffice missing or unable to calculate it), a sentence saying so.
    """

    from decurix_agent import progress
    from decurix_agent.tools import office

    left = []
    for path in paths:
        if Path(path).suffix.lower() not in office.RESULT_BOOKS:
            continue
        try:
            missing = office.formulas_without_results(path)
            if not missing:
                continue
            office.calculate_workbook(path)
            progress.emit(f"Stored the results of {len(missing)} formula(s) in {Path(path).name}: they had none "
                          "in the file.")
            log(f"produced: stored the results of {len(missing)} formula(s) in {path}")
        except Exception as exc:  # noqa: BLE001 - what is still missing is said below, whatever went wrong
            log(f"produced: could not store the formula results of {path}: {exc}")
        still = office.formulas_without_results(path)
        if still:
            left.append(f"{path} has {len(still)} formula cell(s) with no stored result "
                        f"({office._ref_sample(still)}) - a program or an import reading the file sees them blank")
    return left


# A request that asks for figures rather than formulas: then a typed total is what was asked.
VALUES_ASKED = re.compile(r"\b(?:as|in|only)\s+(?:plain\s+)?values\b|\bvalues\s+only\b|\b(?:no|without)\s+formulas?\b|"
                          r"\bplain\s+numbers\b|\bpaste[ds]?\s+(?:as\s+)?values\b|\bhard-?coded?\b", re.I)


def formula_errors(paths, failed=None):
    """A sentence for each workbook among `paths` with formula cells showing an error (office.formula_errors) - certain,
    blocking: anyone who opens the file sees #REF!. `failed`: see _per_file."""

    from decurix_agent.tools import office

    return _per_file(paths, office.formula_errors, "check the formula results of", failed=failed)


def _per_file(paths, check, what, failed=None):
    """check(path)'s sentences for each of `paths`, each headed by the file's name; a file it cannot be made for is
    logged ("could not <what> <path>") and decides nothing - and, given a `failed` list, is added to it, so a blocking
    check that could not be made leaves the work NOT checked rather than passed (FileFacts.could_not)."""

    out = []
    for path in paths:
        try:
            found = check(path)
        except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing
            log(f"produced: could not {what} {path}: {exc}")
            _not_made(failed, f"could not {what} {Path(path).name}", exc)
            continue
        out.extend(f"{Path(path).name}: {sentence}" for sentence in found)
    return out


def coverage_gaps(paths):
    """
    A sentence for each workbook among `paths` that has a column filled down a table and stopped short of it, or a Total
    that sums less than the table above it (office.coverage_gaps). Found live (2026-10-19): a lookup column filled for
    23 of 24 rows, a Total of 515127 for a true 579607, reported verified. Evidence, never a verdict (independent
    review, 2026-10-21): a bonus only for eligible rows, a lookup left blank where nothing matches and a Total that
    leaves out trailing reference rows are right, and code cannot tell them from a fill that stopped short.
    """

    from decurix_agent.tools import office

    return _per_file(paths, office.coverage_gaps, "check the coverage of")


# A request that asks for a Total row: "a Total row at the bottom", "a row labelled Total", "totals under the data".
TOTAL_ASKED = re.compile(r"\b(?:sub)?totals?\s+(?:row|line)\b|\brow\s+(?:labell?ed|called|named|titled)?\s*[\"'“]?(?:sub)?totals?\b|"
                         r"\b(?:sub)?totals?\b[^.\n]{0,40}\b(?:at the bottom|under|below|at the end)\b|"
                         r"\b(?:at the bottom|under|below|at the end)\b[^.\n]{0,40}\b(?:sub)?totals?\b", re.I)


def total_asked(request=""):
    """Whether the request asks for a Total row (then a total split over two rows is a wrong answer)."""

    return bool(TOTAL_ASKED.search(attachments.text(request or "")))


def layout_faults(paths, request=""):
    """
    What is wrong with how the workbooks among `paths` are laid out, decided from the file: typed numbers among the
    formulas of a column when the request asked for formulas (office.constants_among_formulas), and a Total whose label
    and sums are on two rows when it asked for a Total row (office.split_totals). Found live (2026-10-19, calc_gst):
    columns asked "with formulas" had rows 4 to 13 typed in and the Total split over F14 and F15 - and the report, which
    admitted the typed rows, passed.
    """

    from decurix_agent.tools import office

    check_formulas, check_total = formulas_asked(request), total_asked(request)
    return _per_file(paths, lambda path: ((office.constants_among_formulas(path) if check_formulas else [])
                                          + (office.split_totals(path) if check_total else [])),
                     "check the layout of")


# A request that asks for formulas or for figures that follow their rows: then a typed total is not what was asked.
FORMULAS_ASKED = re.compile(r"\bformulas?\b|\blive\b|\bso (?:that )?(?:it|they) (?:update|recalculate)s?\b|"
                            r"\bupdates? (?:itself|themselves|automatically)\b|\brecalculates?\b", re.I)


def formulas_asked(request=""):
    """
    Whether the request asks for formulas. Only then is a typed total a wrong answer the gate sends back; otherwise a
    correct typed total is a correct answer, and is only noted (the PM's rule, 2026-10-19).
    """

    return bool(FORMULAS_ASKED.search(attachments.text(request or "")))


# A request for rows or cells to be coloured: a highlight, a fill of a colour, conditional formatting, shading.
HIGHLIGHT_ASKED = re.compile(
    r"\bhighlight(?:s|ed|ing)?\b|\bconditional(?:ly)?[\s-]+format\w*|"
    r"\b(?:red|green|yellow|amber|orange|blue|grey|gray|pink)\s+(?:fill|background|highlight|shading)\b|"
    r"\bfill(?:ed)?\s+(?:it\s+|them\s+|the\s+\w+\s+)?(?:in\s+)?(?:red|green|yellow|amber|orange|blue|grey|gray|pink)\b|"
    r"\b(?:shade|colou?r)\s+(?:the\s+|every\s+|each\s+|all\s+)?(?:\w+\s+)?(?:rows?|cells?|lines?)\b", re.I)


def missing_highlight(paths, request=""):
    """
    For a request that asks for rows or cells to be filled or highlighted: a sentence for each workbook among `paths`
    that has no conditional format and no fill below its first row at all - the formatting was not applied. [] when
    the request asks for none, or any workbook has some (whether the right rows carry it is for the check that reads
    the file: no condition is evaluated here).
    """

    from decurix_agent.tools import office

    if not HIGHLIGHT_ASKED.search(attachments.text(request or "")):
        return []
    books = [p for p in paths if Path(p).suffix.lower() in (".xlsx", ".xlsm")]
    out = []
    for path in books:
        try:
            nothing = office.nothing_highlighted(path)
        except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing
            log(f"produced: could not read the formatting of {path}: {exc}")
            return []
        if nothing is False or nothing is None:
            return []  # one workbook has some: the check that reads it decides the rest
        out.append(f"{Path(path).name}: no row has a fill and no conditional format exists")
    return out


def typed_totals(paths, request=""):
    """
    For the workbooks among `paths`: a sentence for each whose Total row holds typed numbers that are the sums of
    the columns above them (office.typed_totals), unless the request asked for values. In a workbook a person keeps
    using, a total of cells is a formula over those cells - typed, it goes stale the day a row changes.
    """

    from decurix_agent.tools import office

    if VALUES_ASKED.search(attachments.text(request or "")):
        return []
    out = []
    for path in paths:
        try:
            found = office.typed_totals(path)
        except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing
            log(f"produced: could not look at the totals of {path}: {exc}")
            continue
        if found:
            out.append(f"{Path(path).name}: the Total row holds typed numbers, not formulas - "
                       + "; ".join(found[:8]) + (f" (+{len(found) - 8} more)" if len(found) > 8 else ""))
    return out


# ============================================================
# What a program can decide about the files a task made (2026-10-19)
#
# Found live: a picture "resized to 50%" was saved at its old size, and the report itself said so (1200 x 600) -
# the model check passed it; four letters "each on its own page" were joined by the characters "\f" typed as text.
# These are facts of the saved file, decided here with no model.
# ============================================================

# (A dot followed by a letter is a file name's, not the end of the sentence: "resize photo.jpg to 800 x 600".)
RESIZE_PERCENT = re.compile(r"\b(?:resiz|scal|shrink|reduc|enlarg|halv)\w*\b(?:[^.\n]|\.(?=\S)){0,80}?\b(\d{1,3}(?:\.\d+)?)\s*(?:%|per\s*cent)", re.I)
RESIZE_PIXELS = re.compile(r"\b(?:resiz|scal|shrink|reduc|enlarg)\w*\b(?:[^.\n]|\.(?=\S)){0,80}?\b(\d{2,5})\s*(?:x|×|by)\s*(\d{2,5})\b", re.I)
OWN_PAGE = re.compile(r"\b(?:own|separate|new|single)\s+page\b|\bpage\s+breaks?\b|\bone\s+(?:\w+\s+)?(?:per|a)\s+page\b", re.I)


def requested_names(request=""):
    """
    The file names a request asks to be made or saved: those after a verb that saves, writes, makes or names one, in
    the same sentence ("open logo.png ... and save it as logo_small.png" asks for logo_small.png; logo.png is its input).
    """

    text = attachments.text(request or "")
    out = []
    for sentence in re.split(r"(?<=[.!?])\s+|\n+", text):
        verbs = [m.start() for m in SAVE_SENTENCE.finditer(sentence)] + [m.start() for m in MAKE_SENTENCE.finditer(sentence)]
        if not verbs:
            continue
        first = min(verbs)
        out.extend(m.group(0) for m in REQUESTED_FILE.finditer(sentence) if m.start() > first)
    return list(dict.fromkeys(out))


MAKE_SENTENCE = re.compile(r"\b(?:writ\w*|creat\w*|mak\w*|produc\w*|generat\w*|export\w*)\b", re.I)


def missing_requested(request, made, root=None, only=None):
    """
    The files the request asks to be made that this task did not make, as a sentence, or "". Only for a request whose
    files go to this desktop: one that names folders of the person's computer is checked there
    (missing_on_host). Found live (2026-10-19): asked for error_summary.txt, the worker built a program and reported
    it built; nothing read the request's own file name, and the report went out unchecked. `only`: the file names
    (or paths) this work hands in - a stage of a longer request is held to its own files, never to others the request's
    sentences in its instruction name (found live, round 5 P1: the memo stage was refused for the deck).
    """

    missing = missing_names(request, made, root=root, only=only)
    if not missing:
        return ""
    return (", ".join(missing) + (" was" if len(missing) == 1 else " were") + " not made: no such file was written "
            "during this task")


def missing_names(request, made, root=None, only=None):
    """The names missing_requested reports, as a list ([] when none)."""

    text = attachments.text(request or "")
    if _host_paths(text):
        return []
    names = [n for n in requested_names(request) if Path(n).suffix.lower() in READABLE]
    if only is not None:
        own = {Path(str(o).replace("\\", "/")).name.lower() for o in only}
        names = [n for n in names if Path(n.replace("\\", "/")).name.lower() in own]
    if not names:
        return []
    have = {Path(p).name.lower() for p in made}
    root = Path(root or config.WORK_DIR)
    there = {p.name.lower() for p in _walk(root)}
    # A name that is there at all - made now, or an input the request mentions in the same sentence ("an email with
    # invoice.pdf attached") - is not called missing; only a file nowhere to be found is.
    return [name for name in names if name.lower() not in have and name.lower() not in there]


def named_not_written(request, made, root=None):
    """
    Evidence, never a block: the file names the request's words seem to ask for that no file of the task carries.
    Found (review 2026-11-03, E-10): read from the words alone, "Do not create report.pdf" asked for report.pdf, and a
    file "on the GST portal" was taken for one to make - each refused at certainty. Only a planned stage's own outputs
    (structure) can make a missing file blocking (file_facts' own_outputs).
    """

    missing = missing_names(request, made, root=root)
    if not missing:
        return []
    return [f"The request's words name {', '.join(missing)}, and no file of that name was written during this task "
            "(read from the words alone: judge whether the request asks for it to be made)"]


def source_images(request, made, root=None, fetch=None):
    """
    Local copies of the pictures the request names that are not what it asks to be made (its inputs): from this
    desktop (the work folder, the inbox), or fetched from the person's computer for a request that names its folder.
    """

    text = attachments.text(request or "")
    outputs = {n.lower() for n in requested_names(request)} | {Path(p).name.lower() for p in made}
    names = [n for n in dict.fromkeys(REQUESTED_FILE.findall(text))
             if Path(n).suffix.lower() in IMAGES and n.lower() not in outputs]
    if not names:
        return []
    root = Path(root or config.WORK_DIR)
    local = {p.name.lower(): p for p in _walk(root)}
    out, wanted_host = [], []
    for name in names:
        if name.lower() in local:
            out.append(str(local[name.lower()]))
        else:
            wanted_host.extend(f + "\\" + name for f in _host_paths(text) if not REQUESTED_FILE.fullmatch(_leaf(f)))
    if wanted_host:
        out.extend(str(p) for p in copy_from_host(wanted_host, fetch=fetch))
    return out


def _image_size(path):
    try:
        from PIL import Image
        with Image.open(path) as im:
            return im.size
    except Exception:  # noqa: BLE001 - an unreadable picture decides nothing here
        return None


def image_findings(request, made, sources):
    """
    What is wrong with the pictures a task saved, compared with the picture(s) it was given (`sources`, local paths)
    and the size the request asks for (a percentage, or W x H pixels), as sentences. A picture asked to change size
    that has its source's size, or another size than asked, is wrong. Empty when nothing can be compared.
    """

    text = attachments.text(request or "")
    percent = RESIZE_PERCENT.search(text)
    pixels = RESIZE_PIXELS.search(text)
    if not (percent or pixels):
        return []
    outputs = [p for p in made if Path(p).suffix.lower() in IMAGES]
    source_names = {Path(s).name.lower() for s in sources}
    outputs = [p for p in outputs if Path(p).name.lower() not in source_names]
    src_sizes = [s for s in (_image_size(p) for p in sources) if s]
    out = []
    for path in outputs:
        size = _image_size(path)
        if size is None:
            out.append(f"{Path(path).name} could not be opened as a picture to check its size")
            continue
        w, h = size
        if pixels:
            want = (int(pixels.group(1)), int(pixels.group(2)))
            if abs(w - want[0]) > 1 or abs(h - want[1]) > 1:
                out.append(f"{Path(path).name} is {w} x {h} pixels; the request asks for {want[0]} x {want[1]}")
            continue
        if len(src_sizes) != 1:
            continue  # no single source to measure the percentage against
        sw, sh = src_sizes[0]
        factor = float(percent.group(1)) / 100
        want = (round(sw * factor), round(sh * factor))
        if (w, h) == (sw, sh) and factor != 1:
            out.append(f"{Path(path).name} is {w} x {h} pixels - the same size as the picture it was made from; it "
                       f"was not resized (asked: {percent.group(1)}%, which is {want[0]} x {want[1]})")
        elif abs(w - want[0]) > 1 or abs(h - want[1]) > 1:
            out.append(f"{Path(path).name} is {w} x {h} pixels; {percent.group(1)}% of {sw} x {sh} is "
                       f"{want[0]} x {want[1]}")
    return out


def page_break_findings(request, made):
    """The Word documents among `made` with no page break where the request asks for each part on its own page - a
    reading of the request, so only evidence (harness audit, 2026-10-21)."""

    from decurix_agent.tools import office

    if not OWN_PAGE.search(attachments.text(request or "")):
        return []
    out = []
    for path in made:
        if Path(path).suffix.lower() != ".docx":
            continue
        try:
            if office.docx_structure(path)["page_breaks"] == 0:
                out.append(f"{Path(path).name} has no page break, and the request asks for each part on its own page")
        except Exception as exc:  # noqa: BLE001 - an unreadable file is the reader's to report
            log(f"produced: could not read the structure of {path}: {exc}")
    return out


# What `office update` filled, noted by the tool itself (review round 2, 2026-11-03): {output (real path): {"source":
# the document it was filled from, "placeholders": what that source held when it was filled, "at"}}. A template
# filled into a separately named output, or a document filled in place with no shared-folder snapshot, is then still a
# filled source: its placeholder left unfilled is a mechanical fact, as the tool's own "Placeholders still in it" says.
FILLED_MAX = 500


def _filled_file():
    return config.DECURIX_DIR / "filled-documents.json"


def note_filled(output, source, placeholders):
    """Note that `output` was filled from `source`, which then held `placeholders` (office update). Never raises: a
    note that cannot be kept must not fail the tool (the check then reads the request's sources only)."""

    try:
        path = _filled_file()
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(data, dict):
                data = {}
        except (OSError, ValueError):
            data = {}
        data[os.path.realpath(str(output))] = {"source": os.path.realpath(str(source)),
                                               "placeholders": [str(x) for x in placeholders or ()][:200],
                                               "at": time.time()}
        if len(data) > FILLED_MAX:
            data = dict(sorted(data.items(), key=lambda kv: kv[1].get("at") or 0)[-FILLED_MAX:])
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(path.name + ".tmp")
        tmp.write_text(json.dumps(data), encoding="utf-8")
        os.replace(tmp, path)
    except Exception as exc:  # noqa: BLE001
        log(f"produced: could not note what {output} was filled from: {exc}")


def filled_from(output):
    """The note office update kept for `output` ({"source", "placeholders"}), or None."""

    try:
        data = json.loads(_filled_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    found = data.get(os.path.realpath(str(output))) if isinstance(data, dict) else None
    return found if isinstance(found, dict) else None


def _filled_sources(request, made, root=None):
    """The Word documents the task worked from: the request's attached or named .docx/.dotx files that it did not
    make, and the copy kept from before the task of a made document that was there already (filled in place)."""

    from decurix_agent import source_guard

    made_set = {os.path.realpath(str(m)) for m in made}
    text = attachments.text(request or "")
    found = [str(p) for p in attachments.files(request or "") if Path(str(p)).suffix.lower() in (".docx", ".dotx")]
    names = {n.lower() for n in REQUESTED_FILE.findall(text) if Path(n).suffix.lower() in (".docx", ".dotx")}
    names |= {n.lower() for n in re.findall(r"\b[\w-][\w.-]*\.dotx\b", text, re.I)}
    if names:
        found += [str(p) for p in _walk(Path(root or config.WORK_DIR)) if p.name.lower() in names]
    for m in made:
        if Path(str(m)).suffix.lower() == ".docx":
            try:
                before = source_guard.before_copy(m)
            except Exception:  # noqa: BLE001 - no copy known: nothing to compare with
                before = None
            if before:
                found.append(str(before))
    return [p for p in dict.fromkeys(found) if os.path.realpath(p) not in made_set and Path(p).is_file()]


def _source_placeholders(request, made, root=None, failed=None):
    """Every placeholder the documents the task filled hold (a set), read with no model: the request's sources, and
    what office update noted each made document's source held when it filled it (filled_from). A source that cannot
    be read is a check not made, put in `failed` (FileFacts.could_not) - never quietly a pass (review round 2)."""

    from decurix_agent.tools import office

    held = set()
    for m in made:
        note = filled_from(m)
        if note:
            held.update(str(x) for x in note.get("placeholders") or ())
    for path in _filled_sources(request, made, root=root):
        try:
            held.update(office.docx_placeholders(path))
        except Exception as exc:  # noqa: BLE001
            log(f"produced: could not look for placeholders in {path}: {exc}")
            if failed is not None:
                failed.append(f"the placeholders of {Path(path).name}, a document the work was filled from, could not "
                              f"be read ({type(exc).__name__})")
    return held


def _left_placeholders(made):
    """{path: [placeholder]} of the Word documents among `made` that still hold one."""

    from decurix_agent.tools import office

    out = {}
    for path in made:
        if Path(path).suffix.lower() != ".docx":
            continue
        try:
            left = office.docx_placeholders(path)
        except Exception as exc:  # noqa: BLE001
            log(f"produced: could not look for placeholders in {path}: {exc}")
            continue
        if left:
            out[path] = left
    return out


def template_placeholder_notes(request, made, root=None):
    """Evidence: a placeholder left in a made document that no document the task filled held - a template or a guide
    may well be meant to keep it (review 2026-11-03, E-11: a requested mail-merge template was refused)."""

    left = _left_placeholders(made)
    if not left:
        return []
    held = _source_placeholders(request, made, root=root)
    out = []
    for path, found in left.items():
        own = [p for p in found if p not in held]
        if own:
            out.append(f"{Path(path).name} holds the placeholder(s) {', '.join(own[:6])}, which no document it was "
                       "filled from held (judge whether the request asks for them to be filled)")
    return out


def document_findings(request, made, root=None, failed=None):
    """
    What is wrong with the Word documents a task saved, decided without reading the request: a "\\f" typed as text where
    a break was meant, and a placeholder left that a document the task filled held (the template's own placeholder
    was not filled: mechanical). A placeholder no source held is evidence (template_placeholder_notes; review
    2026-11-03, E-11). (Page breaks the request asks for: page_break_findings.)
    """

    from decurix_agent.tools import office

    out = []
    held = None
    for path in made:
        if Path(path).suffix.lower() != ".docx":
            continue
        try:
            structure = office.docx_structure(path)
        except Exception as exc:  # noqa: BLE001 - an unreadable file is the reader's to report
            log(f"produced: could not read the structure of {path}: {exc}")
            continue
        if "\\f" in structure["typed_escapes"]:
            out.append(f"{Path(path).name} holds the characters \"\\f\" typed as text where a page break was meant")
        # A template's placeholder left in a document made from it ({{FULL_NAME}} where the name belongs).
        try:
            left = office.docx_placeholders(path)
        except Exception as exc:  # noqa: BLE001
            log(f"produced: could not look for placeholders in {path}: {exc}")
            left = []
        if left:
            held = _source_placeholders(request, made, root=root, failed=failed) if held is None else held
            unfilled = [p for p in left if p in held]
            if unfilled:
                out.append(f"{Path(path).name} still holds the template's placeholder(s) {', '.join(unfilled[:6])} - "
                           "fill them (office update DOC.docx with a \"replace\" spec)")
    return out


class FileFacts:
    """
    What a program decides about the files a piece of work made, with no model (see file_facts). Each field is [] or ""
    when nothing is wrong:
      unstored      workbooks whose formulas still have no stored result
      typed_totals  a Total row of typed numbers where the request asked for formulas (evidence: a reading of it)
      totals_note   the same, where it did not ask (a correct answer, only noted)
      missing_files a file a planned stage's own outputs name that was not made (structure: blocking)
      missing_named a file name the request's words seem to ask for that no file carries (evidence)
      placeholder_notes a template placeholder left in a made document that no document it was filled from held
                    (evidence)
      file_faults   hygiene: an Office file a strict reader cannot open, a template placeholder left, "\\f" as text
      request_faults a picture's size or a document's page breaks against what the request asks (evidence)
      gaps          formula cells whose stored result is an error (#REF!, #VALUE!, #DIV/0! ...): certain, blocking
      gap_notes     a column filled down a table that stops short of it, or a Total that sums less than the table
                    (evidence: an intentional blank looks the same - independent review, 2026-10-21)
      unchecked     Office files not opened: password-protected, Strict Open XML, a kind not checked (evidence)
      layout        typed numbers among a column's formulas, a Total split over two rows, as asked (evidence)
      source_mismatch a figure given as a source's own ("closing balance per bank statement") that the source does not
                    hold (evidence: which line states a balance, and its sign, are the book's conventions)
      disclosures   an email draft to one party of the inputs that names another party with figures (evidence)
      payee_mismatch a payment to a bank account / IFSC / IBAN / UPI id that is not on record for its payee (payees.py)
      unknown_recipients an email draft addressed to an address that is in no input (payees.unknown_recipients)
      draft_facts   a fact an email draft states that no source and no recorded work holds (provenance)
      draft_fields  a draft's fact against its recipient's own row of the request's table (evidence)
      duplicates    files made under different names that are byte for byte the same (evidence: a copy may be meant)
      pattern_breaks a row of a sheet of repeated blocks whose formula reads a differently labelled row than the same row
                    of the other blocks (evidence: office.reference_pattern_breaks), or formulas that moved with rows put
                    in this task while their references stayed behind (office.moved_formula_breaks)
      code_notes    a long reference code a file holds that differs in a few characters from one the task was shown
                    (evidence: sequential references and sibling ids look the same - independent review, 2026-10-22)
      could_not     the blocking checks above that could not be made (they raised): the work is NOT checked on that
                    point - never passed for it (verify.outcome's not_made; review 2026-11-02)
    notes() is the evidence: what reads the request or needs judgement. The judge weighs it and the person sees it under
    "Please check"; it never fails the work by itself (harness audit, 2026-10-21).
    """

    def __init__(self, unstored=(), typed_totals=(), totals_note=(), missing_files="", file_faults=(), gaps=(), layout=(),
                 source_mismatch=(), disclosures=(), payee_mismatch=(), unknown_recipients=(), draft_facts=(),
                 request_faults=(), draft_fields=(), gap_notes=(), unchecked=(), duplicates=(), pattern_breaks=(),
                 code_notes=(), could_not=(), missing_named=(), placeholder_notes=()):
        self.could_not = list(could_not)
        self.missing_named = list(missing_named)
        self.placeholder_notes = list(placeholder_notes)
        self.code_notes = list(code_notes)
        self.duplicates = list(duplicates)
        self.pattern_breaks = list(pattern_breaks)
        self.unknown_recipients = list(unknown_recipients)
        self.draft_facts = list(draft_facts)
        self.request_faults = list(request_faults)
        self.draft_fields = list(draft_fields)
        self.payee_mismatch = list(payee_mismatch)
        self.source_mismatch = list(source_mismatch)
        self.disclosures = list(disclosures)
        self.gaps = list(gaps)
        self.gap_notes = list(gap_notes)
        self.unchecked = list(unchecked)
        self.layout = list(layout)
        self.unstored = list(unstored)
        self.typed_totals = list(typed_totals)
        self.totals_note = list(totals_note)
        self.missing_files = missing_files
        self.file_faults = list(file_faults)

    # The facts that fail the work by themselves, in one place for every route (the final gate, a taught run's
    # read-back, a desktop tool's run, a long request's stage). Found (review 2026-11-03, E-27): four routes each listed
    # their own, and the lists had drifted.
    # source_mismatch is evidence (review 2026-11-03, E-13): which line of a source states a balance, and its sign,
    # are a book's conventions.
    BLOCKING = ("unstored", "gaps", "missing_files", "file_faults", "payee_mismatch", "unknown_recipients",
                "draft_facts")

    def blocking(self):
        """Every blocking fact, in BLOCKING's order: the work is not done while any is there."""

        out = []
        for name in self.BLOCKING:
            value = getattr(self, name)
            out.extend([value] if isinstance(value, str) else value)
        return [p for p in out if p]

    def not_made(self):
        """What the code could not check: a blocking check that raised, and an Office file it could not open
        (password-protected, Strict Open XML, a kind it does not read). The work is then not checked on that point -
        never passed for it (verify.outcome's not_made)."""

        return [p for p in list(self.could_not) + list(self.unchecked) if p]

    def notes(self):
        """The evidence among these facts (never a verdict): what reads the request, or needs judgement."""

        return (list(self.gap_notes) + list(self.unchecked) + list(self.layout) + list(self.typed_totals) + list(self.request_faults)
                + list(self.source_mismatch) + list(self.missing_named) + list(self.placeholder_notes)
                + list(self.disclosures) + list(self.draft_fields) + list(self.duplicates)
                + list(self.pattern_breaks) + list(self.code_notes))


OFFICE_KINDS = {".xlsx", ".xlsm", ".xltx", ".xltm", ".docx", ".docm", ".dotx", ".pptx", ".pptm", ".potx"}

# What the first bytes of a file of each kind are (PDF allows a little before its header).
_KIND_MAGIC = {".pdf": ("a PDF", (b"%PDF",), 1024), ".png": ("a PNG picture", (b"\x89PNG",), 0),
               ".jpg": ("a JPEG picture", (b"\xff\xd8\xff",), 0), ".jpeg": ("a JPEG picture", (b"\xff\xd8\xff",), 0),
               ".gif": ("a GIF picture", (b"GIF87a", b"GIF89a"), 0)}


# What a file whose bytes are not the kind its name says is to be done with (named_kind_faults): it is not finished
# work to correct in place - it was never the document. Found live (2026-10-24): told "the saved file is not finished,
# correct it in the file itself", the worker re-saved a sign-in page three times and never clicked the attachment.
WRONG_KIND_MARK = " is named as "
WRONG_KIND_REMEDY = ("A file that is not what its name says was never the document: delete it and get the document "
                     "again where it is offered - in the browser, from the page or message it is attached to (a link "
                     "fetched outside the browser can bring back the page that asks to sign in) - then open it to "
                     "check it is the document before you copy it where it belongs.")


def wrong_kind(faults):
    """The faults among `faults` that are files not of the kind their name says (named_kind_faults)."""

    return [f for f in faults or () if WRONG_KIND_MARK in f and " but holds " in f]


def named_kind_faults(paths):
    """
    A sentence for each file among `paths` whose name says one kind (a PDF, a picture) and whose bytes are not that
    kind - decided from the bytes, no model. Found live (2026-10-24): a browser's aborted download left a web page
    saved as "197_sales_invoice_Anil_Rego.pdf"; the report said both invoices were in the folder.
    """

    out = []
    for path in dict.fromkeys(str(p) for p in paths or ()):
        p = Path(path)
        kind = _KIND_MAGIC.get(p.suffix.lower())
        if not kind or not p.is_file():
            continue
        name, magics, window = kind
        try:
            head = p.read_bytes()[:max(window, 16) + 16]
        except OSError:
            continue
        found = head[:window + 8] if window else head[:8]
        if any((m in found) if window else head.startswith(m) for m in magics):
            continue
        text = head.lstrip()[:64].lower()
        what = ("nothing (it is empty)" if not head else "a web page" if text.startswith((b"<!doctype", b"<html", b"<"))
                else "text" if all(32 <= c < 127 or c in (9, 10, 13) for c in head[:32]) else "something else")
        out.append(f"{p.name} is named as {name} but holds {what} - not {name}: get it again and check what it holds")
    return out


def open_statuses(paths):
    """
    {path: (status, sentence)} of the Office files among `paths` (office.open_status: ok, protected, unchecked,
    broken). Each file that opens is noted as opened (opened_ok), so a later check can tell a file that broke after it
    was handed in from one that never opened here (no_longer_open).
    """

    from decurix_agent.tools import office

    out = {}
    for path in dict.fromkeys(str(p) for p in paths or ()):
        if Path(path).suffix.lower() not in OFFICE_KINDS or not Path(path).is_file():
            continue
        try:
            out[path] = office.open_status(path)
        except OSError as exc:  # (a file that cannot be read at all is the reader's to report)
            log(f"produced: could not read {path} to open it: {exc}")
    _note_opened([p for p, (status, _) in out.items() if status == "ok"])
    return out


def _opened_file():
    return config.DECURIX_DIR / "opened-files.json"


def _note_opened(paths):
    if not paths:
        return
    try:
        known = json.loads(_opened_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        known = {}
    for path in paths:
        known[str(Path(path).resolve())] = time.time()
    try:
        _opened_file().parent.mkdir(parents=True, exist_ok=True)
        _opened_file().write_text(json.dumps(dict(list(known.items())[-2000:])), encoding="utf-8")
    except OSError as exc:
        log(f"produced: could not note the files that opened: {exc}")


def opened_ok(path):
    """Whether this file opened with a strict reader when a check last looked at it (open_statuses noted it)."""

    try:
        known = json.loads(_opened_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    return str(Path(path).resolve()) in known


def no_longer_open(paths):
    """
    The run-wide re-check (independent review, 2026-10-21): the Office files among `paths` that opened when they were
    handed in (opened_ok) and no longer open, each as a sentence. A file never seen opening here is not judged by it -
    its own stage's file_facts decided it.
    """

    before = {str(p): opened_ok(p) for p in paths or ()}
    return [said for path, (status, said) in open_statuses(paths).items() if status == "broken" and before.get(path)]


def notes_text(notes):
    """Evidence lines for the judge (FileFacts.notes): what code noticed, to weigh against the sources - not a verdict."""

    notes = [" ".join(str(n).split())[:700] for n in notes or () if str(n).strip()]
    if not notes:
        return ""
    return ("--- what code checks noticed (evidence to weigh against the sources, not a verdict - each may be right or "
            "wrong)\n" + "\n".join(f"- {n}" for n in notes[:12]))


def file_facts(request, made, host_copies=(), root=None, named_in=None, drafts_made=None, own_outputs=None):
    """
    The code-decided checks of the files made for `request`, shared by every route that reports work: the final gate
    (skill_task._gate) and a planned stage's check (long_task.code_problems) call this one function, so a fix applies
    to both. Workbooks in `made` get their formula results stored first (store_results). `host_copies` are local
    copies of files saved on the person's computer: read like the others for what they hold. `named_in` is the text
    that names the files to make, their sizes and their page breaks - a stage of a longer request passes its own
    instruction, so the request's other files are not asked of it; `request` still says whether formulas were asked.
    `drafts_made` are the email drafts the work made (checked for one party's business told to another).
    """

    request = request or ""
    named = request if named_in is None else named_in
    unstored = store_results(made)
    found_totals = typed_totals(made, request) if not unstored else []
    wrong_totals = found_totals if formulas_asked(request) else []
    every = list(dict.fromkeys(list(made) + [str(c) for c in host_copies]))
    opened = open_statuses(every)
    failed = []  # the blocking checks that could not be made: FileFacts.could_not
    return FileFacts(
        # Needs the formula results stored (above): they say which empty cells a text result explains.
        gaps=formula_errors(every, failed=failed) if not unstored else [],
        gap_notes=coverage_gaps(every) if not unstored else [],
        layout=layout_faults(every, request),
        unstored=unstored,
        typed_totals=wrong_totals,
        totals_note=[] if wrong_totals else found_totals,
        # Blocking only for names from structure (a planned stage's outputs); otherwise evidence (E-10).
        missing_files=missing_requested(named, every, root=root, only=own_outputs) if own_outputs is not None else "",
        missing_named=named_not_written(named, every, root=root) if own_outputs is None else [],
        file_faults=[s for st, s in opened.values() if st == "broken"] + document_findings(named, every, root=root,
                                                                                            failed=failed)
        + named_kind_faults(every),
        placeholder_notes=template_placeholder_notes(named, every, root=root),
        unchecked=[s for st, s in opened.values() if st in ("protected", "unchecked") and s],
        request_faults=image_findings(named, every, source_images(named, every, **({"root": root} if root else {})))
        + page_break_findings(named, every),
        # Needs the formula results stored too: the figure checked is the value the file holds (sources.py).
        source_mismatch=_source_mismatch(request, every, failed) if not unstored else [],
        code_notes=miscopied_codes(every, request),
        disclosures=draft_disclosures(drafts_made, request) if drafts_made else [],
        payee_mismatch=_payee_mismatch(request, every, failed),
        duplicates=identical_files(every),
        pattern_breaks=reference_breaks(every) + moved_breaks(every) + layout_breaks(every),
        unknown_recipients=(_unknown_recipients(request, every, drafts_made, failed) if drafts_made else [])
        + _unknown_in_files(request, every, failed),
        **(_draft_facts(request, every, drafts_made, failed) if drafts_made else {}),
        # (Evaluated last: every check above has run, so `failed` holds each one that could not be made.)
        could_not=failed)


def reference_breaks(paths):
    """A sentence for each row of a workbook among `paths` whose formula reads another row than the same row of the
    sheet's other blocks (office.reference_pattern_breaks) - evidence for the judge."""

    from decurix_agent.tools import office

    return _per_file(paths, office.reference_pattern_breaks, "compare the blocks of")


def moved_breaks(paths):
    """A sentence for each workbook among `paths` whose formulas moved with rows put in or taken out this task while
    their references stayed behind (office.moved_formula_breaks, against the copy kept when the task began) - evidence."""

    from decurix_agent import source_guard
    from decurix_agent.tools import office

    def check(path):
        before = source_guard.before_copy(path)
        return office.moved_formula_breaks(before, path) if before else []

    return _per_file(paths, check, "compare with its copy from before the task")


# A reference code: twelve or more letters and digits with both in it (a filing receipt, a tax id, an IBAN, an order reference).
_LONG_CODE = re.compile(r"(?<![A-Za-z0-9])(?=[A-Za-z0-9]*\d)(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{12,}(?![A-Za-z0-9])")


def _shown_codes(request=""):
    """The long reference codes the task was shown: the request, and what the system's pages showed (the tools' page
    notes and the screen actions' results) - never what the work itself wrote or printed."""

    texts = [request or ""]
    try:
        from decurix_agent import long_task
        from decurix_agent.computer import evidence
        # Every screen action's whole result, not the last few as the judge is shown: a long task's first filing,
        # whose confirmation the code was copied from, is far back by its end (found live, 2026-10-22).
        events = evidence._task_events(None)
        texts += long_task._page_notes() + list(evidence.observed()) + [
            f"{e.get('summary') or ''} {e.get('change') or ''}" for e in events]
    except Exception as exc:  # noqa: BLE001 - nothing read is nothing held
        log(f"produced: could not gather what the screens showed: {exc}")
    return {c.upper() for t in texts for c in _LONG_CODE.findall(str(t))}


def miscopied_codes(paths, request=""):
    """
    Evidence for the judge, never a verdict: a sentence for each long reference code a file the work wrote holds that
    nothing the task was shown holds, while exactly one code it was shown differs from it in one to three characters.
    Found live (2026-10-22): the portal showed the filing's receipt code as AA2909267663712A and the workbook was
    given AA2909267663796A; the report read the workbook back and called it verified. But "a few characters apart" is
    a judgement, not a certainty (independent review, 2026-10-22): order numbers in a run (SO2026000121..125, read from
    an input, with ...124 on the screen) and sibling ids (one company's tax ids in two regions) are near twins too - so
    it is worded as a difference to check, never "copied wrong". Short codes are left out: twelve characters or more.
    """

    shown = _shown_codes(request)
    if not shown:
        return []
    asked = {c.upper() for c in _LONG_CODE.findall(request or "")}
    from decurix_agent.tools import office
    out = []
    for path in dict.fromkeys(str(p) for p in paths or ()):
        try:
            changed = changed_rows_text(path) if Path(path).suffix.lower() in (".xlsx", ".xlsm") else ""
            text = changed or office.read_document(path).get("text") or ""
        except Exception as exc:  # noqa: BLE001 - a file that cannot be read decides nothing here
            log(f"produced: could not read {path} for its codes: {exc}")
            continue
        for code in dict.fromkeys(_LONG_CODE.findall(text)):
            upper = code.upper()
            if upper in shown:
                continue
            twins = [k for k in shown if len(k) == len(upper) and 1 <= sum(a != b for a, b in zip(k, upper)) <= 3]
            if len(twins) == 1:
                apart = sum(a != b for a, b in zip(twins[0], upper))
                where = "in the request" if twins[0] in asked else "on the screen"
                out.append(f"{code} in {Path(path).name} differs in {apart} character(s) from {twins[0]} shown {where}; "
                           "check which is right.")
    return out


def identical_files(paths):
    """
    Sentences naming files among `paths` with different names and the same bytes. Found live
    (2026-10-21): two invoices downloaded from a chat were saved as 197_....pdf and 198_....pdf, and 198 was a second
    download of 197; the report said both were "verified PDFs" and the check, shown only their names, could not tell.
    Evidence for the judge, never a verdict: a copy can be what was asked.
    """

    import hashlib

    groups = {}
    for path in dict.fromkeys(str(p) for p in paths or ()):
        try:
            if not Path(path).is_file() or Path(path).stat().st_size == 0:
                continue
            digest = hashlib.sha256(Path(path).read_bytes()).hexdigest()
        except OSError:
            continue
        groups.setdefault(digest, []).append(path)
    out = []
    for same in groups.values():
        names = sorted({Path(p).name for p in same})
        if len(names) > 1:
            out.append(" and ".join(names[:4]) + " are byte for byte the same file - if they should be different "
                       "documents, one of them is a copy of the other.")
    return out


def _not_made(failed, what, exc):
    """Note in `failed` (when given) that a blocking check could not be made, and why."""

    if failed is not None:
        failed.append(f"{what} ({' '.join(str(exc).split())[:160] or type(exc).__name__})")


def _unknown_in_files(request, every, failed=None):
    from decurix_agent import payees
    try:
        return payees.unknown_addresses_in_files(request, every)
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing (and is said: `failed`)
        log(f"produced: could not check the addresses in the files: {exc}")
        _not_made(failed, "could not check the addresses in the files", exc)
        return []


def _draft_facts(request, every, drafts_made, failed=None):
    """{"draft_facts": provenance findings, "draft_fields": evidence} for a task's drafts (long_task.draft_checks; the
    final gate and the tool route)."""

    try:
        from decurix_agent import long_task
        unsourced, fields = long_task.draft_checks(drafts_made, request, every)
        return {"draft_facts": unsourced, "draft_fields": fields}
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing (and is said: `failed`)
        log(f"produced: could not check the drafts' facts against their sources: {exc}")
        _not_made(failed, "could not check the drafts' facts against their sources", exc)
        return {}


def _unknown_recipients(request, every, drafts_made, failed=None):
    from decurix_agent import payees
    try:
        return payees.unknown_recipients(request, drafts_made, made=every)
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing (and is said: `failed`)
        log(f"produced: could not check where the drafts go: {exc}")
        _not_made(failed, "could not check where the drafts go", exc)
        return []


def _payee_mismatch(request, every, failed=None):
    from decurix_agent import payees
    try:
        found = payees.mismatches(request, every)
        # Payments with no record of the payees' accounts to check them against: the check is not made, so the work
        # is not checked - never passed (payees.unchecked; review 2026-11-02).
        if failed is not None:
            failed.extend(payees.unchecked(request, every))
        return found
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing (and is said: `failed`)
        log(f"produced: could not check where payments go: {exc}")
        _not_made(failed, "could not check where payments go", exc)
        return []


def _source_mismatch(request, every, failed=None):
    from decurix_agent import sources
    try:
        return sources.mismatches(request, every)
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made decides nothing (and is said: `failed`)
        log(f"produced: could not compare figures with their sources: {exc}")
        _not_made(failed, "could not compare figures with their sources", exc)
        return []


def asked_files(paths, request=""):
    """Those of `paths` that the request asks for by name (the deliverables, as against working files)."""

    wanted = {name.lower() for name in REQUESTED_FILE.findall(attachments.text(request or ""))}
    return [p for p in paths if Path(p).name.lower() in wanted]


WORK_FOLDER = re.compile(r"\b(?:in|to|into)\s+(?:my|the|our)\s+work\s+folder\b", re.I)


def misplaced(request, root=None):
    """
    Where a file the request said to put in the work folder ended up instead,
    or "": a file it names that exists under an inbox folder (an attachment
    saved over in place) and not in the work folder itself. Decided with no
    model. Found live (2026-10-13): asked to "save it as grades.xlsx in my work
    folder", a worker pressed Ctrl+S on the attachment - twice - and reported
    "saved in the work folder"; the check took the report's word for it.
    """

    text = attachments.text(request or "")
    if not WORK_FOLDER.search(text):
        return ""
    root = Path(root or config.WORK_DIR)
    began = since()
    out = []
    # Only a file named in a sentence that says where it goes: "update last_invoice_number.txt when done" in the same
    # request as "save the invoices in my work folder" asks for the attachment itself to be updated (found live).
    placed = " ".join(s for s in re.split(r"(?<=[.!?])\s+|\n+", text) if WORK_FOLDER.search(s))
    for name in dict.fromkeys(REQUESTED_FILE.findall(placed)):
        if (root / name).exists():
            continue
        elsewhere = sorted((root / "inbox").glob(f"*/{name}")) if (root / "inbox").is_dir() else []
        # An attachment this task never wrote is one of its inputs ("... already in paid.csv"), not a
        # result saved in the wrong place. Found live (2026-10-13): a correct register was refused for
        # want of the paid list the request had only named.
        if began is not None:
            elsewhere = [p for p in elsewhere if _written_since(p, began)]
        if elsewhere:
            out.append(f"{name} is not in the work folder ({root}); it was saved at {elsewhere[-1]}")
    return "; ".join(out)


def _written_since(path, began):
    """Whether a file was last written after the task began (a second's slack for the copy that put it there)."""

    try:
        return path.stat().st_mtime > began + 1
    except OSError:
        return False


def drafts():
    """The emails this task drafted into the Outbox (tools/outbox.py keeps a note of each)."""

    from decurix_agent.tools import outbox

    try:
        return outbox.drafted()
    except Exception as exc:
        log(f"produced: could not read this task's drafts: {exc}")
        return []


# ============================================================
# A draft that tells one party about another's dealings
#
# Found live (2026-10-20, round 5 P2): asked to hold an order over a customer's credit limit "and ask me", a stage
# drafted an email TO that customer asking it to decide - listing another customer's order and dues with it. A draft
# addressed to one party of the inputs that names another party of the inputs together with figures discloses that
# party's business. Decided in code, with precision first: the parties are only names the inputs list as distinct
# parties (a column headed Customer, Vendor, Supplier, Employee ... in an attached table, with two or more names; a
# chat export "Chat with <name>"), the recipient is a party only when its address or name is that party's, and the
# recipient's own name is never a finding.
# ============================================================

_PARTY_HEADER = re.compile(r"^\s*(?:customer|client|vendor|supplier|party|company|employee|buyer|payee|account|"
                           r"debtor|creditor)(?:\s+name)?\s*$", re.I)
_CHAT_WITH = re.compile(r"(?i)\bchat with (.+?)\.(?:txt|zip)$")
_FIGURE = re.compile(r"\d[\d,]*\d(?:\.\d+)?|\d{2,}")
_MIN_PARTY_ROWS = 2


def _table_rows(path):
    """The rows of an attached table (xlsx or csv) as lists of cells, or []."""

    suffix = Path(path).suffix.lower()
    rows = []
    try:
        if suffix in {".xlsx", ".xlsm"}:
            import openpyxl
            book = openpyxl.load_workbook(path, read_only=True, data_only=True)
            try:
                for sheet in book.worksheets:
                    rows += [list(r) for r in sheet.iter_rows(values_only=True)]
                    rows.append([])  # a sheet ends a table
            finally:
                book.close()
        elif suffix in {".csv", ".tsv"}:
            import csv
            with open(path, encoding="utf-8-sig", errors="replace", newline="") as handle:
                rows = [list(r) for r in csv.reader(handle, delimiter="\t" if suffix == ".tsv" else ",")]
    except Exception as exc:  # noqa: BLE001 - an input that cannot be read names no party
        log(f"produced: could not read {path} for its parties: {exc}")
    return rows


def parties(paths):
    """
    The distinct parties the inputs list: {name: {its email addresses}} - names under a party column of a table (two or
    more of them), and the person or business a chat export is with.
    """

    found = {}
    for path in paths or ():
        match = _CHAT_WITH.search(Path(str(path)).name)
        if match:
            # "Chat with <person> (<business>)": the person and the business are one party - they share one set,
            # so writing to either is writing to both.
            inner = re.findall(r"\(([^)]+)\)", match.group(1))
            names = [n.strip() for n in inner + [re.sub(r"\s*\([^)]*\)", "", match.group(1))] if len(n.strip()) >= 3]
            _link(found, names)
            continue
        column, names = None, {}
        for row in _table_rows(path):
            cells = ["" if c is None else str(c).strip() for c in row]
            if not any(cells):
                column = None
                continue
            if column is None:
                column = next((i for i, c in enumerate(cells) if _PARTY_HEADER.match(c)), None)
                continue
            if column < len(cells) and re.search(r"[A-Za-z]{2}", cells[column]) and 3 <= len(cells[column]) <= 80:
                names.setdefault(cells[column], set()).update(c.lower() for c in cells if "@" in c and " " not in c)
        if len(names) >= _MIN_PARTY_ROWS:
            for name, mails in names.items():
                _link(found, [name])
                found[name].update(mails)
    return found


def _link(found, names):
    """Make every one of names share one set of addresses in found (merging what they had)."""

    if not names:
        return
    shared_set = set()
    for name in names:
        shared_set |= found.get(name, set())
    for name, mails in list(found.items()):
        if any(mails is found.get(n) for n in names if n in found):
            shared_set |= mails
    olds = [found[n] for n in names if n in found]
    for name in list(found):
        if any(found[name] is o for o in olds):
            found[name] = shared_set
    for name in names:
        found[name] = shared_set


def _names_in(text, name):
    return re.search(r"(?<![\w])" + re.escape(name) + r"(?![\w])", text or "", re.I) is not None


def draft_disclosures(drafts_made, request):
    """
    Drafts addressed to one party of the inputs that name another party of the inputs together with figures: one plain
    finding each ("the draft to X names Y with figures"). [] when none, or when the inputs list no parties.
    """

    known = parties(attachments.files(request or ""))
    if len(known) < 2:
        return []
    out = []
    for draft in drafts_made or ():
        to = [str(a).strip().lower() for a in (draft.get("to") or []) + (draft.get("cc") or [])]
        addresses = " ".join(to)
        body = f"{draft.get('subject') or ''}\n{draft.get('body') or ''}"
        greeting = next((line for line in str(draft.get("body") or "").splitlines() if line.strip()), "")
        recipients = {name for name, mails in known.items()
                      if any(a in mails or a.split("<")[-1].rstrip(">") in mails for a in to)
                      or (len(_squash(name)) >= 5 and _squash(name) in _squash(addresses))
                      or (re.match(r"(?i)\s*(?:dear|hi|hello)\b", greeting) and _names_in(greeting, name))}
        if not recipients:
            continue
        recipients |= {n for n, mails in known.items() if any(mails is known[r] for r in recipients)}
        others = [name for name in known if name not in recipients and _names_in(body, name)
                  and not any(_names_in(name, r) or _names_in(r, name) for r in recipients)]
        if others and _FIGURE.search(re.sub(r"(?i)\b\d{1,2}\s+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)\w*\s+\d{4}\b",
                                            " ", body)):
            out.append(f"the email draft to {', '.join(draft.get('to') or [])} (\"{str(draft.get('subject') or '')[:80]}\") "
                       f"names {', '.join(sorted(others)[:5])} with figures - another party's business told to "
                       f"{', '.join(sorted(recipients))}")
    return out


def drafts_text(made=None):
    """Those drafts as the check reads them, or ""."""

    parts = []
    for n, draft in enumerate(drafts() if made is None else made, 1):
        body = str(draft.get("body") or "")
        if len(body) > MAX_FILE_CHARS:
            body = body[:MAX_FILE_CHARS] + "\n[cut here]"
        parts.append(f"--- email draft {n} (in the Outbox, not sent)\nTo: {', '.join(draft.get('to') or [])}\n"
                     f"Cc: {', '.join(draft.get('cc') or []) or '-'}\nSubject: {draft.get('subject') or ''}\n"
                     f"Attached: {', '.join(draft.get('attachments') or []) or 'nothing'}\n\n{body}")
    return "\n".join(parts)


def _picture(path):
    """A picture as the check needs it: its format and size in pixels."""

    try:
        from PIL import Image
        with Image.open(path) as im:
            return f"a {im.format} image, {im.size[0]} x {im.size[1]} pixels, {Path(path).stat().st_size} bytes"
    except Exception as exc:
        return f"(a picture that could not be opened: {str(exc)[:120]})"


def _label(path):
    """A file's name as the check reads it: with where it is on the person's computer when it is in their shared folder."""

    host = shared.to_host(path)
    return f"{path} (= {host} on the person's computer: their shared folder)" if host else str(path)


TABLE_KINDS = {".xlsx", ".xlsm", ".csv", ".tsv", ".ods"}
_FOCUS_TOKEN = re.compile(r"\b[A-Za-z0-9]*\d[A-Za-z0-9]*(?:[-/_][A-Za-z0-9]+){0,4}\b")
HEAD_LINES = 12


def focus_tokens(findings):
    """The references and figures that findings name ("SO-5103", "1,800.00", "Orders!D25"), to find their rows by."""

    out = []
    for finding in findings or ():
        for m in _FOCUS_TOKEN.finditer(str(finding).replace(",", "")):
            token = m.group(0)
            if len(token) >= 3 and token not in out:
                out.append(token)
    return out[:60]


# A sheet's heading in office read's text ("=== Sheet 'Returns': used range ...") and a cell's address as it writes
# one ("C97=..."), on a lowered line: a whole address, never the end of a longer one ("ac97=").
_SHEET_HEADING = re.compile(r"^=== Sheet (['\"])(.*?)\1: used range")
_CELL_AT = re.compile(r"(?:^|(?<=\s))([a-z]{1,3}\d+)=")


def sample_table(body, budget, focus=(), cells=()):
    """
    A long table's text cut to `budget` characters so the judge sees what matters (independent review, 2026-10-21 -
    not the first N characters): its first lines (the header), every line naming what a finding points at, every row
    holding one of `cells` ((sheet, address) pairs, matched on that sheet only and by whole address), its total lines,
    then an even sample of the rest; each gap marked with how many lines it leaves out.
    """

    lines = body.splitlines()
    if len(body) <= budget or len(lines) <= HEAD_LINES:
        return body[:budget]
    focus = [str(f).lower() for f in focus or ()]
    wanted = {}
    for sheet, ref in cells or ():
        wanted.setdefault(str(sheet), set()).add(str(ref).lower())
    sheet_of, current = [], None
    for line in lines:
        heading = _SHEET_HEADING.match(line)
        if heading:
            current = heading.group(2)
        sheet_of.append(current)
    keep = set(range(min(HEAD_LINES, len(lines))))
    flat = [line.lower().replace(",", "") for line in lines]

    def named(i):
        """Whether line i names what a finding points at, or holds a changed cell of its own sheet."""
        if any(f in flat[i] for f in focus):
            return True
        refs = wanted.get(sheet_of[i])
        return bool(refs) and any(ref in refs for ref in _CELL_AT.findall(lines[i].lower()))

    keep |= {i for i in range(len(lines)) if named(i)}
    keep |= {i for i, line in enumerate(flat) if re.search(r"\b(?:grand\s+)?(?:sub)?totals?\b", line)}
    marker = 32  # ("[... N line(s) not shown ...]" between kept lines)
    used = sum(len(lines[i]) + 1 + marker for i in keep)
    rest = [i for i in range(len(lines)) if i not in keep]
    if rest and used < budget:
        average = max(1, sum(len(lines[i]) + 1 for i in rest) // len(rest)) + marker
        room = max(0, (budget - used) // average)
        if room:
            stride = max(1, len(rest) / room)
            keep |= {rest[int(k * stride)] for k in range(min(room, len(rest)))}
    out, last = [], -1
    # (The header, the named rows and the totals first, then the sample, each in file order, within the budget.)
    first = set(range(min(HEAD_LINES, len(lines)))) | {i for i in keep if i in range(len(lines)) and (
        named(i) or re.search(r"\b(?:grand\s+)?(?:sub)?totals?\b", flat[i]))}
    chosen, size = set(), 0
    for i in sorted(first) + sorted(keep - first):
        cost = len(lines[i]) + 1 + marker
        if size + cost > budget - marker:
            continue
        chosen.add(i)
        size += cost
    for i in sorted(chosen):
        if i != last + 1:
            out.append(f"[... {i - last - 1} line(s) not shown ...]")
        out.append(lines[i])
        last = i
    if last < len(lines) - 1:
        out.append(f"[... {len(lines) - 1 - last} line(s) not shown ...]")
    return "\n".join(out)


LOOK_CHARS = 8000
LOOK_LINES = 40


_CELL_TERM = re.compile(r"^(?:.*!)?\$?([a-z]{1,3})\$?(\d{1,6})(?::\$?([a-z]{1,3})\$?(\d{1,6}))?$")


def _col_index(col):
    """A column's number (a=1, z=26, aa=27)."""
    n = 0
    for ch in col:
        n = n * 26 + ord(ch) - 96
    return n


def _col_name(n):
    """A column's letters from its number."""
    out = ""
    while n:
        n, rem = divmod(n - 1, 26)
        out = chr(97 + rem) + out
    return out


def _cells_pattern(term):
    """A regex for the lines of an office read listing that hold a cell or range term (lower case), or None."""
    m = _CELL_TERM.match(term.replace(" ", ""))
    if not m:
        return None
    c1, r1 = m.group(1), int(m.group(2))
    c2, r2 = (m.group(3) or c1), int(m.group(4) or r1)
    r1, r2 = min(r1, r2), max(r1, r2)
    a, b = sorted((_col_index(c1), _col_index(c2)))
    if r2 - r1 > 200 or b - a > 52:
        return None
    rows = "|".join(str(r) for r in range(r1, r2 + 1))
    cols = "|".join(_col_name(n) for n in range(a, b + 1))
    return re.compile(rf"(?:^\s*(?:{rows})\s*\[|(?<![a-z0-9])(?:{cols})(?:{rows})\s*=)")


def lines_with(paths, terms, budget=LOOK_CHARS):
    """The lines of those files that hold any of `terms` (case and spacing ignored), each file's first lines with them
    (a table's header), for a judge that asked to look closer (verify.judge's `look`). "" when none holds one."""

    from decurix_agent.tools import office

    wanted = [" ".join(str(t).lower().split()) for t in terms or () if str(t).strip()]
    if not wanted:
        return ""
    # A cell or range the judge names ("A79:E81", "Sheet!B12") is found as office read prints its rows - "79 [...]" or
    # "A79=..." - never as the literal text: found live (2026-10-23), asked to see A79:E81, the look-up
    # found nothing and the right figures went unseen twice.
    cells = [_cells_pattern(t) for t in wanted]
    cells = [c for c in cells if c is not None]
    parts, size = [], 0
    for path in paths or ():
        if Path(path).suffix.lower() in IMAGES:
            continue
        try:
            lines = (office.read_document(path).get("text") or "").splitlines()
        except Exception as exc:  # noqa: BLE001 - a file it cannot read holds nothing to show
            log(f"produced: could not look in {path}: {exc}")
            continue
        flat = [" ".join(line.lower().replace(",", "").split()) for line in lines]
        hits = [i for i, line in enumerate(flat)
                if any(w in line or w.replace(",", "") in line for w in wanted)
                or any(c.search(line) for c in cells)][:LOOK_LINES]
        if not hits:
            continue
        head = [i for i in range(min(3, len(lines))) if i not in hits]
        block = f"--- {_label(path)}\n" + "\n".join(
            ([lines[i] for i in head] + ["[...]"] if head else []) + [f"(line {i + 1}) {lines[i]}" for i in hits])
        if size + len(block) > budget:
            block = block[:max(0, budget - size)]
        if block:
            parts.append(block)
            size += len(block)
        if size >= budget:
            break
    return "\n".join(parts)


MAX_CHANGED_CELLS = 150
# How much of a workbook the judge's view reads before sampling it to MAX_FILE_CHARS.
JUDGE_SHEET_ROWS = 5000
JUDGE_SHEET_CHARS = 400_000


_CHANGES = {}
_CHANGES_KEPT = 32


def _stamp(path):
    """A file's (size, modification time): what tells one saved version of it from another."""

    info = os.stat(path)
    return info.st_size, info.st_mtime_ns


def workbook_changes(path):
    """
    {sheet: {row: [cell addresses]}} of a workbook's cells that differ from the copy kept before the task
    (source_guard.before_copy); None without a copy or when either cannot be read. Read once per saved version of the
    two files: the final check asks it of the same workbook several times (changed rows, layout, codes, the judge's view).
    """

    if Path(path).suffix.lower() not in (".xlsx", ".xlsm"):
        return None
    try:
        from decurix_agent import source_guard
        before = source_guard.before_copy(path)
        if before is None:
            return None
        key = (str(path), _stamp(path), str(before), _stamp(before))
        if key in _CHANGES:
            return _CHANGES[key]
        import openpyxl
        old, new = openpyxl.load_workbook(str(before)), openpyxl.load_workbook(str(path))
    except Exception as exc:  # noqa: BLE001 - extra focus, never a reason to fail the read
        log(f"produced: could not compare {path} with its copy from before the task: {exc}")
        return None
    out = {}
    for ws in new.worksheets:
        was = old[ws.title] if ws.title in old.sheetnames else None
        for row in ws.iter_rows():
            for c in row:
                if c.value is not None and (was is None or was[c.coordinate].value != c.value):
                    out.setdefault(ws.title, {}).setdefault(c.row, []).append(c.coordinate)
    if len(_CHANGES) >= _CHANGES_KEPT:
        _CHANGES.clear()
    _CHANGES[key] = out
    return out


def changed_cells(path):
    """
    [(sheet, "C97"), ...]: the cells of a workbook that differ from the copy kept before the task, each with its sheet
    - so a long sheet the judge is shown a sample of shows what the task changed (sample_table's `cells`). Found live
    (2026-10-22): the report gave a credit carried forward the saved workbook contradicted, and the judge,
    shown a sample of the sheet without the new month's rows, passed it. [] when there is no copy.
    """

    out = []
    for sheet, rows in (workbook_changes(path) or {}).items():
        for r in sorted(rows):
            for ref in rows[r]:
                out.append((sheet, ref))
                if len(out) >= MAX_CHANGED_CELLS:
                    return out
    return out


def changed_rows_by_sheet(path):
    """{sheet: {row numbers}} of a workbook's rows that differ from its copy from before the task; None without a copy."""

    changes = workbook_changes(path)
    if changes is None:
        return None
    return {sheet: set(rows) for sheet, rows in changes.items()}


def layout_breaks(paths):
    """A sentence for each block a workbook's task added or changed that does not follow the layout of the sheet's other
    blocks (office.block_layout_breaks) - evidence for the judge."""

    from decurix_agent.tools import office

    def check(path):
        if Path(path).suffix.lower() not in (".xlsx", ".xlsm"):
            return []
        return office.block_layout_breaks(path, rows=changed_rows_by_sheet(path))

    return _per_file(paths, check, "compare the layout of the blocks of")


MAX_CHANGED_ROWS_CHARS = 5000


def changed_rows_text(path):
    """
    The rows of a workbook that differ from the copy kept before the task, each as its saved values (no formulas) with
    the block it belongs to - what the task changed, compact enough to be shown whole beside a long sheet's sample. ""
    when there is no copy or nothing changed.
    """

    # Each sheet's own changed rows: a row number alone matched the same row of every sheet, and the judge was shown
    # other sheets' untouched rows as "what this task changed" (independent review, 2026-10-22).
    by_sheet = changed_rows_by_sheet(path)
    if not by_sheet:
        return ""
    try:
        import openpyxl
        from decurix_agent.tools import office
        values = openpyxl.load_workbook(str(path), data_only=True)
        formulas = openpyxl.load_workbook(str(path))
    except Exception as exc:  # noqa: BLE001
        log(f"produced: could not read the changed rows of {path}: {exc}")
        return ""
    lines = []
    for ws in formulas.worksheets:
        if not by_sheet.get(ws.title):
            continue
        try:
            blocks = office.sheet_blocks(ws)
        except Exception:  # noqa: BLE001
            blocks = []
        wv = values[ws.title]
        for r in sorted(by_sheet[ws.title]):
            tag = next((t for a, b, t in blocks if a <= r <= b), "")
            shown = [f"{wv.cell(r, c).coordinate}={office._cell_shown(wv.cell(r, c).value)}"
                     for c in range(1, ws.max_column + 1) if wv.cell(r, c).value is not None]
            if shown:
                lines.append(f"{ws.title}{(' [' + tag + ']') if tag else ''}  " + "  ".join(shown))
    text = "\n".join(lines)
    if len(text) > MAX_CHANGED_ROWS_CHARS:
        text = text[:MAX_CHANGED_ROWS_CHARS] + "\n[... more changed rows not shown]"
    return text


def text(paths, focus=()):
    """What those files say now, read the way the worker's own tools read them; unreadable ones say so. A long table is
    sampled (sample_table): its header, the rows `focus` (what findings name) points at, its totals, an even sample."""

    from decurix_agent.tools import office

    tokens = focus_tokens(focus)
    parts = []
    for path in paths:
        label = _label(path)
        if Path(path).suffix.lower() in IMAGES:
            parts.append(f"--- {label}\n{_picture(path)}")
            continue
        try:
            if Path(path).suffix.lower() in (".xlsx", ".xlsm"):
                # Every row, then sampled below: office read's own view stops at its first rows, and what a task adds
                # to a sheet is usually at its end (2026-10-22: the new month, rows 86-99, was never shown).
                body = (office._data_check_workbook(Path(path)) + office.read_workbook(
                    path, max_rows=JUDGE_SHEET_ROWS, limit=JUDGE_SHEET_CHARS)).strip()
            else:
                body = (office.read_document(path).get("text") or "").strip()
        except Exception as exc:  # a file the reader cannot open is reported as such, not left out
            log(f"produced: could not read {path}: {exc}")
            parts.append(f"--- {label}\n(could not be read: {str(exc)[:200]})")
            continue
        if len(body) > MAX_FILE_CHARS and Path(path).suffix.lower() in TABLE_KINDS:
            body = sample_table(body, MAX_FILE_CHARS, tokens, cells=changed_cells(path))
        elif len(body) > MAX_FILE_CHARS:
            body = body[:MAX_FILE_CHARS] + f"\n[cut here: {len(body) - MAX_FILE_CHARS} more characters not shown]"
        # The end of a sheet is where a fill that stopped short and a Total row show, and the text above is cut before
        # it: a workbook's last rows are always shown, cell by cell, the empty ones named.
        try:
            tail = office.workbook_tail(path)
        except Exception as exc:  # noqa: BLE001 - extra evidence, never a reason to fail the read
            log(f"produced: no tail of {path}: {exc}")
            tail = ""
        changed = changed_rows_text(path) if Path(path).suffix.lower() in (".xlsx", ".xlsm") else ""
        if changed:
            # Every row the task changed, with its saved values: what a report about the file is compared with.
            changed = "\n[What this task changed in it - every row that differs from before the task, as saved now:]\n" + changed
        parts.append(f"--- {label}\n{body or '(empty)'}" + (f"\n{tail}" if tail else "") + changed)
    return "\n".join(parts)


# ============================================================
# A file a task was asked to save on the person's computer
#
# A host app (Excel, Word, PowerPoint on the person's computer) saves to
# C:\..., which this desktop cannot see, so the check above never read the
# result. Found live (2026-10-16): asked to save sales_q3_report.xlsx, Excel
# kept the file's CSV type and saved sales_q3_report.xlsx.csv - and the
# screen, where Excel hides the extension, showed the name asked for; the task
# was reported done. The control plane can see that disk (its host mount) and
# answers, for a path the desktop's own open request names, whether the file
# is there - never its content. Decided with no model.
# ============================================================

# A folder or file on the host's C: drive as a request writes it; a folder may hold spaces, so it runs to a quote,
# a bracket or a line's end, and is then cut at a sentence's end.
HOST_PATH = re.compile(r"\b[cC]:[\\/][^\n\"'()<>|*?]*")
SAVE_SENTENCE = re.compile(r"\b(?:save[ds]?|saving|export(?:s|ed)?|store|named|call(?:ed)?)\b", re.I)
WRITTEN_SLACK = 5


def _host_paths(text):
    """The C:\\ paths a request names, each cut at a sentence's end."""

    out = []
    for match in HOST_PATH.finditer(text):
        found = re.split(r"[.,;:](?:\s|$)", match.group(0))[0].rstrip(" .,;:\\/")
        if len(found) > 3:
            out.append(found)
    return list(dict.fromkeys(out))


def _leaf(path):
    return path.replace("\\", "/").rsplit("/", 1)[-1]


def _ask_host(path):
    """What the control plane says of a host path: a dict, or None when it could not say."""

    # A path under the worker's shared folder is right here: answered from the file itself, never through the host.
    local = shared.to_local(path)
    if local is not None:
        return shared.stat(local)

    import urllib.parse
    import urllib.request

    url = (config.CONTROL_PLANE_URL.rstrip("/") + "/api/host-files/stat?"
           + urllib.parse.urlencode({"desktop": config.DESKTOP_ID, "path": path}))
    try:
        with urllib.request.urlopen(url, timeout=10) as answer:
            found = json.loads(answer.read().decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - not knowing is never a verdict
        log(f"produced: could not ask about {path}: {exc}")
        return None
    return None if not isinstance(found, dict) or found.get("unknown") else found


def _host_findings(request, ask=None):
    """
    The files a request said to save on the person's computer: what is wrong
    with them (sentences) and which are there as asked (host paths). A file
    named in a sentence that saves something, looked for in the C:\\ folders the
    request names that do exist (or at the whole path the request gives it). A
    folder that is not there, or a control plane that cannot be asked, decides
    nothing.
    """

    ask = ask or _ask_host
    text = attachments.text(request or "")
    paths = _host_paths(text)
    if not paths:
        return [], []
    saved = " ".join(s for s in re.split(r"(?<=[.!?])\s+|\n+", text) if SAVE_SENTENCE.search(s))
    wanted = list(dict.fromkeys(REQUESTED_FILE.findall(saved)))
    if not wanted:
        return [], []
    whole = [p for p in paths if REQUESTED_FILE.fullmatch(_leaf(p))]
    folders = []
    for path in paths:
        if path in whole:
            continue
        found = ask(path)
        if found is None:
            return [], []
        if found.get("exists") and found.get("folder"):
            folders.append(path)
    began = since()
    out, good = [], []
    for name in wanted:
        places = [p for p in whole if _leaf(p).lower() == name.lower()] + [f + "\\" + name for f in folders]
        if not places:
            continue
        there = []
        for place in places:
            found = ask(place)
            if found is None:
                return [], []
            if found.get("exists") and not found.get("folder"):
                there.append((place, found))
        if not there:
            folder = places[0].rsplit("\\", 1)[0]
            out.append(f"{name} is not in {folder} on the person's computer")
        elif any(_wrong_kind(name, found) for _, found in there):
            kind = next(found.get("kind") for _, found in there if _wrong_kind(name, found))
            out.append(f"{there[0][0]} is there, but it is not a real {name.rsplit('.', 1)[-1]} file (its bytes are "
                       f"{KIND_WORDS.get(kind, kind)}) - it was saved with another file type")
        elif began is not None and all(_older(found, began) for _, found in there):
            out.append(f"{there[0][0]} is there but was last written before this task began - it was not saved now")
        else:
            good.append(there[0][0])
    return out, good


def missing_on_host(request, ask=None):
    """What of the files a request said to save on the person's computer is not there as asked, or ""."""

    return "; ".join(_host_findings(request, ask)[0])


def saved_on_host(request, ask=None):
    """The files a request said to save on the person's computer that are there as asked (host paths)."""

    return _host_findings(request, ask)[1]


HOST_COPY_TASKS = 5  # the check copies of the last few tasks are kept; older ones are removed


def _host_copy_root():
    return config.DECURIX_DIR / "computer" / "host-copies"


def _prune_host_copies(keep):
    """Remove the check copies of all but the newest HOST_COPY_TASKS tasks (never `keep`'s), and any copy left loose in
    the folder by an older version of this code (review 2026-11-03, E-43: nothing was ever removed)."""

    root = _host_copy_root()
    try:
        entries = list(root.iterdir())
    except OSError:
        return
    for entry in entries:
        if entry.is_file():
            try:
                entry.unlink()
            except OSError:
                pass
    folders = sorted((e for e in entries if e.is_dir()), key=lambda d: d.stat().st_mtime, reverse=True)
    for old in folders[HOST_COPY_TASKS:]:
        if old.name != keep:
            shutil.rmtree(old, ignore_errors=True)


def host_copies(paths, fetch=None):
    """
    {host path: local copy} of files of the person's computer, for the final check to read (the control plane hands
    over only files the task's own request names). A file in the shared folder is read where it is. A fetched copy
    keeps the file's own name, in a folder of its own per full path, under a folder per task
    (~/.decurix/computer/host-copies/<task>/<hash of the path>/<name>): found (review 2026-11-03, E-31), copies kept by
    name alone made two report.xlsx files of two client folders one file, so one was checked twice and the other
    never. A file
    that cannot be fetched is left out.
    """

    import hashlib
    import urllib.parse
    import urllib.request

    from decurix_agent import task_context

    task = re.sub(r"[^\w-]", "_", task_context.current() or "current")
    folder = _host_copy_root() / task
    out = {}
    fetched = False
    for path in dict.fromkeys(paths or ()):
        if Path(_leaf(path)).suffix.lower() not in READABLE:
            continue
        local = shared.to_local(path)
        if local is not None and Path(local).is_file():
            out[path] = Path(local)  # in the shared folder: read where it is, not fetched
            continue
        try:
            if fetch is not None:
                data = fetch(path)
            else:
                url = (config.CONTROL_PLANE_URL.rstrip("/") + "/api/host-files/read?"
                       + urllib.parse.urlencode({"desktop": config.DESKTOP_ID, "path": path}))
                with urllib.request.urlopen(url, timeout=60) as answer:
                    data = answer.read()
            place = folder / hashlib.sha256(path.encode("utf-8")).hexdigest()[:16]
            place.mkdir(parents=True, exist_ok=True)
            local = place / _leaf(path)
            local.write_bytes(data)
            out[path] = local
            fetched = True
        except Exception as exc:  # noqa: BLE001 - a copy that cannot be made leaves the check to the rest
            log(f"produced: could not copy {path} from the person's computer: {exc}")
    if fetched:
        _prune_host_copies(task)
    return out


def copy_from_host(paths, fetch=None):
    """The local copies host_copies makes, as a list in the order of `paths` (callers that need to know which copy is
    which host file use host_copies)."""

    return list(host_copies(paths, fetch=fetch).values())


# What a file's name says its first bytes must be (the control plane reads eight of them, never more).
KIND_OF = {".xlsx": "zip", ".xlsm": "zip", ".docx": "zip", ".pptx": "zip", ".ods": "zip", ".odt": "zip",
           ".odp": "zip", ".pdf": "pdf"}
KIND_WORDS = {"text": "plain text, as a CSV or a text file is", "zip": "a zip package", "pdf": "a PDF",
              "ole": "an old Office file", "other": "something else"}


def _wrong_kind(name, found):
    """Whether a host file's bytes are not what its extension says (a CSV saved under a workbook's name)."""

    want = KIND_OF.get(os.path.splitext(name)[1].lower())
    kind = found.get("kind")
    return bool(want and kind and kind != want)


def _older(found, began):
    """Whether a host file the control plane described was last written before the task began."""

    from datetime import datetime

    try:
        written = datetime.fromisoformat(str(found.get("modified", "")).replace("Z", "+00:00")).timestamp()
    except ValueError:
        return False
    return written < began - WRITTEN_SLACK
