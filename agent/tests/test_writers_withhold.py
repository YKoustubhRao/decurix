"""
Every writer of desktop state withholds secrets, or says why it holds none (review round 2 of 2026-11-04, B2; CLAUDE.md:
"every writer of desktop state ends with privacy.scrub").

Three writers kept a marked request verbatim after its task (the shared folder's snapshot manifest, a planned run's
ledger, the follow-up file). This walks the agent's code for every function that writes a file (write_text,
write_bytes, json.dump, open(..., "w"/"a"/"x"), os.fdopen for writing) and fails for one that neither withholds (a call
of privacy.scrub, withhold_known, task_context.withhold, messaging's _withheld ...) nor is listed below with the
reason it holds no secret. A new writer must do one or the other: this is the test that catches the next one.
"""

import ast
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "decurix_agent"
WITHHOLDS = {"scrub", "withhold_known", "withhold", "_withheld", "withheld", "safe_text", "_withhold_derived",
             "withhold_text", "shown_by_gate"}

# file:function -> why what it writes holds no secret (or where it is withheld before it gets there).
REGISTRY = {
    "app_skills.py:learn": "an app guide the engine wrote; kept pending, promoted only after checked work (follow-up: "
                           "withhold the guide's text)",
    "attachments.py:_extract": "the person's own files, unpacked from their archive",
    "computer/approval.py:_locked": "the local approval store's lock file and state: fingerprints and the gate's words",
    "computer/audit.py:keep_observation": "an observation blob: privacy.scrub'd in _observation_doc before it is hashed",
    "computer/cli.py:_look": "a screenshot (pixels; password fields are painted over by the caller)",
    "computer/cli.py:main": "stderr of the gui command (its own error line)",
    "computer/cli.py:run": "a screenshot file the command was asked for",
    "computer/cli.py:save_screenshot": "a screenshot file the command was asked for",
    "computer/core.py:_form_check": "the pending-submit note: a hash of the form, a ref and a time",
    "computer/core.py:_note_failed_search": "the failed search: a field's label and time (the typed text is not kept)",
    "computer/core.py:_note_front": "the front window's id and title",
    "computer/core.py:_save_state": "the last observation's refs; a secret field's value is withheld at observe",
    "computer/core.py:_save_task_windows": "window ids and titles",
    "computer/core.py:_state_lock": "a lock file",
    "computer/core.py:_unsearched_record": "a once-per-task mark ('1')",
    "computer/demonstration.py:run": "the recorder's pid",
    "computer/demonstration.py:start": "the recorder's session id, pid and times",
    "computer/evidence.py:note_file_read": "a path the tools read",
    "computer/modes.py:set_stage": "a stage name",
    "computer/privacy.py:_keep_hashes": "SHA-256 hashes of secrets, never the text",
    "computer/privacy.py:_with_literals": "the task's secret values themselves, in tmpfs, mode 0600, cleared at the "
                                          "task's end: what every withholding reads",
    "computer/privacy.py:_write_literals": "as _with_literals",
    "computer/recording.py:_locked": "a recording's lock file",
    "computer/recording.py:write_whole": "a recording's files, each event and meta privacy.scrub'd in append",
    "computer/records.py:_save": "the record ledger: field values as the documents give them, for the save gate to "
                                 "compare (follow-up: withhold the task's literal secrets without shape matching)",
    "computer/records.py:stop_once": "a once-per-task mark ('1')",
    "computer/shown.py:note": "refs and element names the gui command printed",
    "computer/skill_engine.py:_note_replayed": "which skill step was replayed",
    "computer/skill_engine.py:_write_note": "which skill step was replayed",
    "computer/skill_engine.py:run": "the skill engine's own progress note",
    "gate_server.py:__enter__": "the gate's lock file, in its own 0700 directory",
    "gate_server.py:_write_private": "the gate's own store, mode 0600, in its own 0700 directory",
    "guidance.py:_keep": "the guided run in hand: a skill's name, step and values, read by the next gui command",
    "guidance.py:taught_values_check": "parameter names told",
    "long_task.py:_hold": "a new request held aside word for word until the person says what to do: a transport, "
                          "like the command file",
    "long_task.py:_pending_request_reply": "the held request, put back as a command file: a transport",
    "long_task.py:_snapshot_sheets": "cells of the workbooks the work made (the person's own outputs)",
    "messaging.py:_keep_unsent": "an outcome already withheld by send_query",
    "messaging.py:_queue_learned": "a learned row already withheld and scrubbed by post_learned",
    "messaging.py:resend_learned": "rows already withheld when queued",
    "messaging.py:resend_unsent": "outcomes already withheld when kept",
    "messaging.py:save_whole": "a report's whole text, from a payload send_query already withheld",
    "methods.py:_save": "a learned method: steps and commands (follow-up: withhold its text)",
    "procedures.py:_save": "a learned procedure (follow-up: withhold its text)",
    "produced.py:_note_opened": "paths of files opened",
    "produced.py:host_copies": "local copies of the person's own files",
    "produced.py:note_filled": "paths and placeholder names",
    "retention.py:_write_manifest": "the run manifest: hashes, paths, names",
    "retention.py:_write_label": "an execution id and a label number",
    "task_context.py:_keep_context": "ids, date orders and the date question's readings",
    "task_context.py:_write": "the task file's parts; the request copy is withheld by start() before it is written",
    "tool_made.py:remember": "a document the tools saved in the shared folder: its path and words (the person's file)",
    "tool_task.py:_run": "a learned tool's script, written to its sandbox",
    "tools/office.py:_image_to_pdf": "a PDF made from the person's picture",
    "tools/office.py:_read_eml": "an email's attachment, unpacked to read it",
    "tools/office.py:_recalculated_values": "a workbook copy, to read its computed values",
    "tools/office.py:combine_pdf": "a PDF the tools were asked to make",
    "tools/outbox.py:_note": "an email draft's record: the draft the work made (follow-up: withhold its text)",
    "tools/table_quality.py:overlap_lines": "a scratch file of a table's rows",
    "workflow_runner.py:_save": "a run's checkpoint; its request input is replaced withheld at the run's final report "
                                "(long_task._withhold_request)",
}


def _name(call):
    f = call.func
    return f.attr if isinstance(f, ast.Attribute) else getattr(f, "id", "")


def _mode_writes(call, at=1):
    mode = None
    if len(call.args) > at and isinstance(call.args[at], ast.Constant):
        mode = call.args[at].value
    for k in call.keywords:
        if k.arg == "mode" and isinstance(k.value, ast.Constant):
            mode = k.value.value
    return isinstance(mode, str) and any(c in mode for c in "wax")


def _writes(func):
    for node in ast.walk(func):
        if isinstance(node, ast.Call):
            name = _name(node)
            if name in ("write_text", "write_bytes", "dump") or (name == "open" and _mode_writes(node)) \
                    or (name == "fdopen" and _mode_writes(node)):
                return True
    return False


def _withholds(func):
    return any(isinstance(n, ast.Call) and _name(n) in WITHHOLDS for n in ast.walk(func))


def writers_left_bare():
    found = set()
    for path in sorted(ROOT.rglob("*.py")):
        if "thebot" in path.relative_to(ROOT).parts:
            continue
        for func in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
            if isinstance(func, (ast.FunctionDef, ast.AsyncFunctionDef)) and _writes(func) and not _withholds(func):
                found.add(f"{path.relative_to(ROOT).as_posix()}:{func.name}")
    return found


class EveryWriterWithholds(unittest.TestCase):
    def test_no_writer_is_left_without_withholding_or_a_reason(self):
        found = writers_left_bare()
        self.assertEqual(sorted(found - set(REGISTRY)), [], "a writer of desktop state that withholds nothing: "
                         "withhold (privacy.scrub / task_context.withhold) or list it with why it holds no secret")

    def test_the_registry_holds_only_writers_that_are_there(self):
        self.assertEqual(sorted(set(REGISTRY) - writers_left_bare()), [], "a listed writer withholds now, or is gone: "
                         "take it out of REGISTRY")

    def test_the_three_writers_the_review_found_withhold(self):
        for writer in ("source_guard.py:take", "long_task.py:_remember_reported"):
            self.assertNotIn(writer, writers_left_bare())
