"""
The "azure" harness: a tool-calling loop on the hosted model the control
plane forwards to (llm_client -> /api/llm/chat -> internal/llm).

It is a general-purpose engine for work that needs a model to read documents
and screenshots, operate applications, research information, write code,
and create or update files.

Same contract as the other three harnesses (harness.py): perform_task,
continue_after_query, wait_for_backend, ask_once. What it can do:

  shell            a command in the work dir (the dangerous ones wait for a yes)
  observe/act/...  the screen, through computer/mcp_server's own tools, so the
                   approval gate and the recordings apply exactly as they do
                   for Claude
  read_document    PDFs (scans as pictures), images, workbooks, Word, email
  read_workbook / create_workbook / update_workbook (charts survive) /
  combine_pdf      tools/office.py
  draft_email      puts an email in the control plane's Outbox; a person sends
                   it. The agent can never send mail itself.
  ask_human        pause the task with a question, or for approval
  finish           the answer

A question or an approval pauses the task (ask_for_query) with the whole
conversation kept here, and continue_after_query carries on from it with the
human's reply as the paused tool call's result.
"""

import base64
import json
import os
import time
from pathlib import Path

from decurix_agent import config, guidance, llm_client, progress, state, task_context, usage
from decurix_agent.commands import dangerous_reason, run_command
from decurix_agent.computer import approval
from decurix_agent.identity import get_identity_context
from decurix_agent.logging_utils import log
from decurix_agent.memory_context import get_memory_context
from decurix_agent.messaging import ask_for_query, remember, write_final_reply
from decurix_agent.tools import office, outbox

ENGINE = "azure"

MAX_STEPS = int(os.environ.get("DECURIX_AZURE_MAX_STEPS", "120"))
# What one tool result may put into the conversation.
MAX_RESULT_CHARS = 14000
# Pictures kept in the conversation, counted apart: a document's pages are the
# task's source data and must outlive the screenshots of working a system,
# where only the latest screen matters. Live, a scanned invoice was pushed out
# by four screenshots of the application and the model asked a person to type in what
# it had already read. Older ones become a line saying how to see them again.
KEEP_SCREENS = 2
KEEP_DOCUMENT_PAGES = 6
SCREEN_LABEL = "The screen after that call:"
SYSTEM_PROMPT = """You are a careful general-purpose worker operating your own Linux desktop (XFCE, a Chrome browser, LibreOffice) through tools. Carry out the user's task in the applications and files they specify. You can research, write code, work with documents and data, and operate desktop and web applications.

When to act, and when to ask - the rule everything else follows:
- Do, without asking, everything the request asks for that can be undone: create drafts and editable records, fill in and correct their fields, upload the documents that belong to them, navigate, read, write files, prepare emails with draft_email. Leaving out a document or value that must not be used is part of doing the task, not a question: leave it out, do the rest, and say so in your report.
- Ask (ask_human) only for one of these, and say which:
  1. Approval (kind "approval"): a step that cannot be taken back or that reaches outside - submitting, confirming, posting, approving, paying, sending, deleting or cancelling a document. Emails are never sent by you; a person sends drafts from the Outbox.
  2. Real ambiguity: the documents and the system leave two answers open - no record fits, or several fit everything the document says (two entries of that name; configurations when the request specifies none of their attributes) - with the candidates and what tells them apart. An option described by the request or source is the candidate with all those attributes, whatever order the application writes them in. Other options existing is not itself ambiguity.
  3. Missing information: a value neither the documents nor the system have, or a sign-in with no credentials in your notes (ask the person to sign in on the screen; never ask for a password in the chat).
- Never ask to confirm something the request already told you to do.

Reading and matching:
- Work from what is there. Read every document with read_document before using it; observe the screen before acting on it. Never invent a value.
- Documents belong to someone: check the name and number on a document against the person, organization or task it is for, before you use or upload it.
- Finding a record: match identifiers and relevant attributes from the request and sources. Differences in capitalization, punctuation or spacing alone do not make a different record. Do not assume two records are the same merely because their names are similar; if several candidates fit, ask with the evidence that distinguishes them.
- Checking documents against each other is not finding a record: there "matches" means the same characters (ignoring only capitals and spacing). Report every other difference in a name, number or address, however small, with both spellings. Say which documents you compared.
- "Make sure X matches Y": compare field by field and change what differs; report what you changed.
- A document's own figures go on the draft made from it: when the system proposes something else (a default value or a template setting) and the document states a value, enter the document's value and report the difference with both figures - a draft is reviewed before anyone confirms it, so this is not a question.
- Numbers: every figure has a unit and a basis. Read which one the source quotes (per unit or per group, bytes or megabytes, seconds or minutes), convert to what was asked, say the conversion, and check it is plausible next to the others - off by a round factor usually means the wrong unit.

Working in systems and files:
- Desktop and web apps: use observe/act/open. Prefer element refs from the latest observe; type one field per action. Fields further down a page need a scroll - look before concluding something is not there. After each important step, verify it worked before moving on.
- Saving: a form is saved only when the page says so. After Save, read what appeared: a success message, or field errors ("Required", "Should be a valid date ...") - an error means nothing on that form was saved; fix the field and save again. A page with several forms (a record's sections, a dialog over the page) has a Save in each: use the one inside the form you filled, by its ref, not by position.
- Fields that suggest entries as you type (a name, category or configured option): the value is set only by choosing a suggestion - typing and moving on leaves it empty. Use act's select action with the value; it types and clicks the match. If the list only offers a general entry without the required configuration, select that and complete what the page asks next: in a variant or configuration dialog, set every attribute the document names (colour, material, size) before its Confirm - the dialog opens on defaults, which are not the document's - then check the line shows the document's variant.
- Dates: type a date in the format its field shows (the placeholder: "yyyy-dd-mm" is year-day-month, so 14 March 1998 is 1998-14-03) instead of picking it in a calendar, then check the field holds it.
- Files into a web page: use act's upload action on the control that opens the file dialog (an Upload / Browse / Attach button or the file field) with the file's path. Never click it to work the file dialog yourself.
- Files: your work directory is {work_dir}; files that came with an email are listed in the request. Put what you produce in the work directory with clear names, and format values for their meaning: units labelled, percentages as percentages, dates as dates.
- Spreadsheets: read_workbook shows cells, formulas and the ranges charts use. Change an existing model with update_workbook (it keeps charts and formatting); when you add rows, extend what charts and tables read with replace_refs. Check totals yourself (python3 in the shell is fine) and say whether they tie out.
- Shell: python3 (with openpyxl, pypdf), pdftotext, LibreOffice (soffice), curl and the usual tools. Each shell call is a fresh shell in the work directory.

What this desktop was taught:
- Taught procedures come first. They were demonstrated by a person here and confirmed by them. If one fits, run it with run_skill, giving every value it needs from the task's documents - a value's recorded example shows which thing to read (a name, reference or date), never what to type; a list value takes one entry per line item, separated by ';'. If it halts, look at the screen, finish the rest yourself, and say which step stopped and why. A recording of a person doing a similar task (the recordings tool) shows how it is done here when no procedure fits.
- Learn from the work: when a task succeeded and you found out something the next task will need that is not a screen step - how to reach a system, a mapping between two codes, a person's correction - save it with remember (one short, specific note), never values that change each time.

Finishing:
- Call finish with a short report (what you did, where the outputs are, what a person must check or approve, what you could not do) and a checklist of every part of the request - each record, field, document, file and email it asked for - done or not, with how you know. If something failed and you cannot fix it, finish with status "failed" and say why.

{identity}
{memory}
{procedures}"""

EXTRA_TOOLS = [
    {
        "name": "shell",
        "description": "Run a shell command (sh) in the work directory and get its output and exit code.",
        "parameters": {
            "type": "object",
            "properties": {
                "command": {"type": "string"},
                "timeout": {"type": "integer", "description": "Seconds, default 120, at most 900."},
            },
            "required": ["command"],
        },
    },
    {
        "name": "read_document",
        "description": (
            "Read a file: PDF (a scan is shown as page pictures), image, Excel workbook, Word document, "
            "email (.eml, saving its attachments), text/CSV, or a folder listing."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "pages": {"type": "string", "description": "For a PDF: '2' or '1-3'."},
                "as_images": {"type": "boolean", "description": "For a PDF: also show its pages as pictures (layout, stamps, tables)."},
            },
            "required": ["path"],
        },
    },
    {
        "name": "read_workbook",
        "description": "Read an Excel workbook: cells with addresses, formulas with their last values, and the ranges its charts and tables use.",
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "sheet": {"type": "string"},
                "cells": {"type": "string", "description": "A range like A1:H200."},
                "max_rows": {"type": "integer"},
            },
            "required": ["path"],
        },
    },
    {
        "name": "create_workbook",
        "description": (
            "Create a new .xlsx. sheets: [{name, rows: [[...]], header: true, widths: {A: 20}, "
            "number_format: {C: '#,##0.00'}}]. Strings starting with = are formulas."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "sheets": {"type": "array", "items": {"type": "object"}},
            },
            "required": ["path", "sheets"],
        },
    },
    {
        "name": "update_workbook",
        "description": (
            "Change cells of an existing workbook, keeping its charts, formatting and everything else. "
            "edits: [{sheet, cell, value}] (number, text, null to empty, or a formula '=SUM(B2:B14)'). "
            "replace_refs: [{from: 'Data!$B$2:$B$13', to: 'Data!$B$2:$B$14'}] extends what charts/tables/names read. "
            "Writes output_path, or the file itself (keeping <name>.orig.xlsx the first time)."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "edits": {"type": "array", "items": {"type": "object"}},
                "replace_refs": {"type": "array", "items": {"type": "object"}},
                "output_path": {"type": "string"},
            },
            "required": ["path"],
        },
    },
    {
        "name": "combine_pdf",
        "description": "Make one PDF from PDFs, images and office documents (Word, Excel - converted as LibreOffice prints them), in the order given.",
        "parameters": {
            "type": "object",
            "properties": {
                "output_path": {"type": "string"},
                "inputs": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["output_path", "inputs"],
        },
    },
    {
        "name": "draft_email",
        "description": (
            "Prepare an email for a person to review and send from the Outbox. It is NOT sent. "
            "attachments are file paths on this desktop."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "to": {"type": "array", "items": {"type": "string"}},
                "cc": {"type": "array", "items": {"type": "string"}},
                "subject": {"type": "string"},
                "body": {"type": "string"},
                "attachments": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["to", "subject", "body"],
        },
    },
    {
        "name": "remember",
        "description": (
            "Save a short note this desktop will know on every later task (its worker memory, shown to people "
            "on its card): how to reach a system, a code mapping, a person's correction. Not for one-off values."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "title": {"type": "string", "description": "A few words, e.g. 'Project portal login route'."},
                "note": {"type": "string", "description": "The lesson, specific enough to act on."},
            },
            "required": ["title", "note"],
        },
    },
    {
        "name": "ask_human",
        "description": (
            "Pause and ask the person. kind 'question' when you need information or a decision; "
            "kind 'approval' before anything consequential (submitting/posting a document, paying, "
            "sending, deleting). Their reply comes back as this call's result."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "question": {"type": "string"},
                "kind": {"type": "string", "enum": ["question", "approval"]},
            },
            "required": ["question"],
        },
    },
    {
        "name": "finish",
        "description": (
            "End the task with a report for the person. checklist: one line for every part of the request "
            "(each record to create, field to fill, document to file or leave out, file to write, email to draft), "
            "with done true/false and a note - the evidence it is done, or exactly why it is not."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "report": {"type": "string"},
                "status": {"type": "string", "enum": ["done", "failed"]},
                "checklist": {"type": "array", "items": {"type": "object", "properties": {
                    "item": {"type": "string"}, "done": {"type": "boolean"}, "note": {"type": "string"}},
                    "required": ["item", "done"]}},
            },
            "required": ["report", "checklist"],
        },
    },
]

# How many times this task's finish was sent back (see _finish_check).
_finish_pushbacks = 0

# Kept between a pause and the human's reply.
_conversation = None
_paused = None  # {"call_id", "tool", "command" (for a dangerous shell command)}


# ------------------------------------------------------------------
# Tools
# ------------------------------------------------------------------

def _tool_specs():
    from decurix_agent.computer import mcp_server

    specs = [{"type": "function", "function": {"name": t["name"], "description": t["description"], "parameters": t["parameters"]}}
             for t in EXTRA_TOOLS]

    for t in mcp_server.TOOLS:
        specs.append({"type": "function", "function": {
            "name": t["name"], "description": t["description"], "parameters": t["inputSchema"],
        }})

    return specs


def _screen_tool_names():
    from decurix_agent.computer import mcp_server
    return {t["name"] for t in mcp_server.TOOLS}


class Pause(Exception):
    """A tool that stopped the task for a person."""

    def __init__(self, text, kind="query", command=None):
        super().__init__(text)
        self.text = text
        self.kind = kind
        self.command = command


class Finish(Exception):
    def __init__(self, report, status="done"):
        super().__init__(report)
        self.report = report
        self.status = status


def _run_tool(name, args):
    """Run one tool. Returns (text, [image paths or png bytes]). Raises Pause / Finish."""

    if name == "finish":
        pushback = _finish_check(args)
        if pushback:
            return pushback, []
        raise Finish(_with_checklist((args.get("report") or "").strip(), args.get("checklist")),
                     args.get("status") or "done")

    if name == "ask_human":
        kind = "approval" if args.get("kind") == "approval" else "query"
        raise Pause((args.get("question") or "").strip(), kind=kind)

    if name == "remember":
        title, note = (args.get("title") or "").strip(), (args.get("note") or "").strip()
        if not title or not note:
            return "Not saved: give a title and a note.", []
        saved = remember(title[:120], note[:1500], category="procedure")
        return ("Saved to this desktop's memory." if saved else "Could not save it (the control plane did not take it)."), []

    if name == "shell":
        command = args.get("command") or ""
        why = dangerous_reason(command)
        if why:
            raise Pause(
                f"I want to run this command, and it needs your yes first because {why}:\n\n{command}\n\n"
                "Reply yes to run exactly this, or tell me what to do instead.",
                kind="approval", command=command,
            )
        return _shell(command, args.get("timeout")), []

    if name == "read_document":
        r = office.read_document(args.get("path"), pages=args.get("pages"), as_images=bool(args.get("as_images")))
        return r["text"], r["images"]

    if name == "read_workbook":
        return office.read_workbook(args.get("path"), sheet=args.get("sheet"), cells=args.get("cells"),
                                    max_rows=int(args.get("max_rows") or office.DEFAULT_ROWS)), []

    if name == "create_workbook":
        return office.create_workbook(args.get("path"), args.get("sheets") or []), []

    if name == "update_workbook":
        return office.update_workbook(args.get("path"), args.get("edits"), args.get("replace_refs"), args.get("output_path")), []

    if name == "combine_pdf":
        return office.combine_pdf(args.get("output_path"), args.get("inputs") or []), []

    if name == "draft_email":
        return _draft_email(args), []

    if name in _screen_tool_names():
        return _screen_tool(name, args)

    return f"There is no tool {name!r}.", []


def _finish_check(args):
    """
    Why a finish is sent back once more, or None. Live, a long screen session
    ended "done" having uploaded a document it had itself found to be someone
    else's, and with four of the requested fields left empty; a checklist of
    every part of the request, each done or said why not, is what makes such a
    gap visible to the engine before it reaches a person.
    """

    global _finish_pushbacks

    if _finish_pushbacks >= 2:
        return None

    checklist = [c for c in args.get("checklist") or [] if isinstance(c, dict) and c.get("item")]
    if not checklist:
        _finish_pushbacks += 1
        return ("Not finished yet: call finish again with a checklist - one line for every part of the request "
                "(each record, field, document, file, email), with done true/false and a note saying how you know.")

    undone = [c for c in checklist if not c.get("done")]
    if undone and (args.get("status") or "done") == "done" and _finish_pushbacks == 0:
        _finish_pushbacks += 1
        listed = "; ".join(f"{c['item']} ({c.get('note') or 'no reason given'})" for c in undone)
        return ("Not finished yet - these parts of the request are not done: " + listed + ". Do them now (look again: "
                "fields further down a page need a scroll, a document that must not be used must also not be "
                "uploaded or entered). If one truly cannot be done, call finish again with its note saying exactly "
                "why, in words a person can act on.")

    return None


def _with_checklist(report, checklist):
    """The report, with every part of the request marked done or not - what a person checks it against."""

    lines = [f"- {'done' if c.get('done') else 'NOT DONE'}: {c['item']}" + (f" - {c['note']}" if c.get("note") else "")
             for c in checklist or [] if isinstance(c, dict) and c.get("item")]
    return report + ("\n\nChecklist:\n" + "\n".join(lines) if lines else "")


def _shell(command, timeout=None):
    try:
        timeout = max(5, min(int(timeout or 120), 900))
    except (TypeError, ValueError):
        timeout = 120

    result = run_command(command, timeout=timeout)
    output = result["output"] or "(no output)"
    return f"exit code {result['exit_code']}\n{output}"


def _screen_tool(name, args):
    from decurix_agent.computer import mcp_server

    blocks, is_error = mcp_server.call_tool(name, args)

    blocked = approval.pending()
    if blocked is not None:
        raise Pause(approval.request_text(blocked), kind="approval")

    texts, images = [], []
    for block in blocks:
        if block.get("type") == "text":
            texts.append(block["text"])
        elif block.get("type") == "image":
            images.append(base64.b64decode(block["data"]))

    text = "\n".join(texts)
    if is_error:
        text = "(failed) " + text
    return text, images


def _draft_email(args):
    try:
        return outbox.draft(args.get("to"), args.get("subject"), args.get("body"),
                            cc=args.get("cc"), attachments=args.get("attachments"))
    except outbox.DraftError as exc:
        return f"Not drafted: {exc}"


# ------------------------------------------------------------------
# The loop
# ------------------------------------------------------------------

def _describe(name, args):
    """What the chat's live steps show for one tool call."""

    if name == "shell":
        return "Running: " + (args.get("command") or "")[:300]
    if name in ("read_document", "read_workbook"):
        return f"Reading {args.get('path')}"
    if name in ("create_workbook", "update_workbook"):
        return f"{'Creating' if name == 'create_workbook' else 'Updating'} {args.get('path')}"
    if name == "combine_pdf":
        return f"Combining {len(args.get('inputs') or [])} files into {args.get('output_path')}"
    if name == "draft_email":
        return f"Drafting an email: {args.get('subject')}"
    if name == "observe":
        return "Looking at the screen"
    if name == "open":
        return f"Opening {args.get('target')}"
    if name == "act":
        target = args.get("name") or args.get("ref") or (f"{args.get('x')},{args.get('y')}" if args.get("x") is not None else "")
        extra = f" {args.get('text')!r}" if args.get("action") == "type" and args.get("text") else ""
        return f"{args.get('action')} {target}{extra}".strip()
    if name == "verify":
        return "Checking the screen"
    return name


def _image_parts(images):
    parts = []
    for img in images:
        if isinstance(img, (bytes, bytearray)):
            parts.append(llm_client.image_part(bytes(img)))
        else:
            parts.append(llm_client.image_part(path=img))
    return parts


def _trim_images(messages):
    """Keep the newest KEEP_SCREENS screenshots and KEEP_DOCUMENT_PAGES document pictures."""

    kept = {"screen": 0, "document": 0}
    limit = {"screen": KEEP_SCREENS, "document": KEEP_DOCUMENT_PAGES}

    for msg in reversed(messages):
        content = msg.get("content")
        if not isinstance(content, list):
            continue

        # Each picture belongs to the label before it in the same message.
        kinds, kind = [], "document"
        for part in content:
            if part.get("type") == "text":
                kind = "screen" if part.get("text", "").startswith(SCREEN_LABEL) else "document"
            kinds.append(kind)

        for i in range(len(content) - 1, -1, -1):
            if content[i].get("type") != "image_url":
                continue
            kept[kinds[i]] += 1
            if kept[kinds[i]] > limit[kinds[i]]:
                content[i] = {"type": "text", "text": (
                    "[an earlier screenshot, no longer shown - observe to see the screen now]" if kinds[i] == "screen"
                    else "[an earlier document picture, no longer shown - read_document it again to see it]")}


# How many taught procedures and untaught demonstrations the prompt lists.
MAX_PROCEDURES = 8
MAX_DEMONSTRATIONS = 5


def _procedures(task):
    """
    The taught skills that may fit this task, and the demonstrations nobody
    has taught yet, as the engine reads them. Ranked by the task's words but
    never filtered by them: a request rarely words itself like the skill's
    title ("enter this document" / "Add a record in the portal"), and the model
    is the better judge of which fits.
    """

    from decurix_agent.computer import recording, replay, skill_engine, skills

    lines = []

    try:
        ranked = skills.search(task, limit=MAX_PROCEDURES)
        rest = [s for s in skills.search("", limit=100) if s["name"] not in {r["name"] for r in ranked}]
        chosen = (ranked + rest)[:MAX_PROCEDURES]
    except (OSError, ValueError) as exc:
        log(f"azure harness: could not list skills: {exc}")
        chosen = []

    for skill in chosen:
        lines.append("- " + skill_engine.summary_line(skill))
        said = skills.explanation(skill)
        if said:
            lines.append(f"    the person who taught it said (follow it): {said[:600]}")
        for param in skills.required(skill):
            kind = "a list, one per item" if param.get("list") else "one value"
            lines.append(f"    {param['name']}: typed into {param.get('label') or param['name']} ({kind}); "
                         f"recorded example {json.dumps(param.get('example') or '')}")

    try:
        demos = [m for m in replay.search(task, limit=20)
                 if m.get("kind") == "demonstration" and m.get("status") in (None, "complete")][:MAX_DEMONSTRATIONS]
    except (OSError, ValueError):
        demos = []

    if not lines and not demos:
        return ""

    out = ["Procedures taught on this desktop (run one with run_skill; read one's steps with skills):"]
    out += lines or ["- (none yet)"]
    if demos:
        out.append("Demonstrations recorded here (read one with the recordings tool, id first):")
        out += [f"- {m['id']} {json.dumps(m.get('title') or '(untitled)')} - {m.get('event_count', 0)} actions" for m in demos]
    return "\n".join(out)


def _system_message(task=""):
    memory = get_memory_context()
    prompt = SYSTEM_PROMPT.format(
        work_dir=config.WORK_DIR,
        identity=get_identity_context(f"hosted model via the control plane ({ENGINE})"),
        memory=("Notes this desktop has been taught (use them):\n" + memory) if memory else "",
        procedures=_procedures(task),
    )
    # A taught task this request is (skill_task._by_model): its procedure,
    # this run's values and how to look at the demonstration, in the
    # engine's own shell terms (`gui skill NAME --look N` through run_shell).
    guided = guidance.text()
    return f"{prompt}\n\n{guided}" if guided else prompt


def _step(messages, tools):
    _trim_images(messages)
    message, used = llm_client.chat(messages, tools=tools, max_tokens=6000)
    usage.add(input_tokens=used.get("prompt_tokens"), output_tokens=used.get("completion_tokens"))

    assistant = {"role": "assistant", "content": message.get("content") or ""}
    if message.get("tool_calls"):
        assistant["tool_calls"] = message["tool_calls"]
    messages.append(assistant)
    return message


def _run(messages):
    """
    Drive the model until it finishes or pauses. Every exit goes through
    write_final_reply or ask_for_query - the task is never left hanging.
    """

    global _conversation, _paused

    tools = _tool_specs()

    for step in range(MAX_STEPS):
        try:
            message = _step(messages, tools)
        except llm_client.LLMError as exc:
            _conversation = None
            write_final_reply(
                "The hosted model could not be reached, so the task stopped. Send the request again in a minute; "
                f"if it keeps happening, check Azure OpenAI's key: New desktop > Model > Azure OpenAI > Change, on the dashboard.\n\n({exc})",
                kind="error", engine=ENGINE,
            )
            return

        calls = message.get("tool_calls") or []

        if not calls:
            text = (message.get("content") or "").strip()
            if text:
                _conversation = None
                write_final_reply(text, engine=ENGINE)
                return
            messages.append({"role": "user", "content": "Carry on: use a tool, or call finish with your report."})
            continue

        pictures = []

        for i, call in enumerate(calls):
            name = call.get("function", {}).get("name", "")
            try:
                args = json.loads(call.get("function", {}).get("arguments") or "{}")
            except json.JSONDecodeError:
                args = {}

            progress.emit(_describe(name, args))

            try:
                text, images = _run_tool(name, args)

            except Finish as done:
                _answer_rest(messages, calls[i + 1:])
                _conversation = None
                write_final_reply(done.report or "(no report)", kind="error" if done.status == "failed" else "final", engine=ENGINE)
                return

            except Pause as pause:
                _answer_rest(messages, calls[i + 1:])
                _conversation = messages
                _paused = {"call_id": call["id"], "tool": name, "command": pause.command}
                ask_for_query(pause.text, kind=pause.kind, engine=ENGINE)
                return

            except (office.OfficeError, OSError, ValueError, TypeError, KeyError) as exc:
                text, images = f"(failed) {exc}", []

            except Exception as exc:  # a tool must never kill the task
                log(f"azure harness: tool {name} raised {exc!r}")
                text, images = f"(failed) {type(exc).__name__}: {exc}", []

            # What the engine was told, for whoever debugs a run later: the chat
            # shows only progress lines, and a model's judgement ("the page is
            # unreadable") cannot be checked without what it read.
            log(f"azure harness: {name} {json.dumps(args)[:300]} -> {len(images)} picture(s), "
                f"{(text or '').strip()[:600]!r}")

            if len(text) > MAX_RESULT_CHARS:
                text = text[:MAX_RESULT_CHARS] + f"\n[... {len(text) - MAX_RESULT_CHARS} more characters not shown]"

            messages.append({"role": "tool", "tool_call_id": call["id"], "content": text or "(no output)"})

            if images:
                label = SCREEN_LABEL if name in _screen_tool_names() else f"Pictures from {name} ({args.get('path') or ''}):"
                pictures.append({"type": "text", "text": label})
                pictures.extend(_image_parts(images))

        # Pictures go after every result of this turn: a message between two
        # tool results is refused ("tool_calls must be followed by tool
        # messages responding to each tool_call_id").
        if pictures:
            messages.append({"role": "user", "content": pictures})

    _conversation = None
    write_final_reply(
        f"The task used all {MAX_STEPS} steps without finishing. What was done so far is on the desktop; "
        "send a narrower request, or tell me where to pick up.",
        kind="error", engine=ENGINE,
    )


def _answer_rest(messages, rest):
    """
    Every tool call in an assistant message must get a result before the
    conversation goes on. The paused call's own result is the human's reply
    (added by continue_after_query); calls after it never ran.
    """

    for call in rest:
        messages.append({"role": "tool", "tool_call_id": call["id"],
                         "content": "Not run: the task paused before this call. Call it again if it is still needed."})


def perform_task(task, task_file):
    global _conversation, _paused, _finish_pushbacks

    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_query = False
    _conversation, _paused = None, None
    _finish_pushbacks = 0
    usage.set_model(os.environ.get("AZURE_MODEL") or "hosted model")

    log("========================================")
    log("STARTING TASK (azure harness)")
    log(task)
    log("========================================")

    messages = [
        {"role": "system", "content": _system_message(task)},
        {"role": "user", "content": task},
    ]

    _run(messages)


def continue_after_query(query_response):
    global _conversation, _paused

    if not state.current_task or _conversation is None or _paused is None:
        log("azure harness: a reply arrived but no task of this harness is paused")
        write_final_reply(
            "Your reply arrived, but there was no paused task here to continue (the desktop may have restarted). "
            "Send the request again.", kind="error", engine=ENGINE,
        )
        return

    state.waiting_for_query = False
    messages, paused = _conversation, _paused
    _paused = None

    reply = (query_response or "").strip()

    if paused.get("command"):
        if approval.is_approval(reply) and not _qualified(reply):
            result = "The person said yes. " + _shell(paused["command"])
        else:
            result = f"The person did not approve running it. Their reply: {reply}"
    elif paused["tool"] in _screen_tool_names():
        # watchers.command_watcher already turned a yes into a one-shot grant.
        if approval.has_grant():
            result = f"The person approved: {reply}\nRepeat exactly the same action now and it will go through."
        else:
            result = f"The person did not approve that action. Their reply: {reply}"
    else:
        result = f"The person replied: {reply}"

    messages.append({"role": "tool", "tool_call_id": paused["call_id"], "content": result})

    log("========================================")
    log("RECEIVED HUMAN REPLY (azure harness)")
    log(reply)
    log("========================================")

    _run(messages)


def _qualified(reply):
    lowered = f" {reply.lower()} "
    return any(w in lowered for w in (" but ", " except ", " not ", " instead ", " don't ", " dont "))


def wait_for_backend():
    """The hosted model is behind the control plane; it is ready when that answers."""

    from decurix_agent.readiness import wait_for_http_ok
    wait_for_http_ok(config.CONTROL_PLANE_URL.rstrip("/") + "/api/llm", "the control plane's hosted model")


def warm_after_ready():
    return


def ask_once(system, prompt):
    return llm_client.ask_once(system, prompt)
