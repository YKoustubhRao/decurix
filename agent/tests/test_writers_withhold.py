"""
Every write of desktop state goes through one function that withholds secrets first (privacy.write_scrubbed); any
other way of writing a file is on a short list, each with its reason (review rounds 2 and 3 of 2026-11-04; CLAUDE.md:
"every writer of desktop state ends with privacy.scrub").

Round 2 walked each function for a write and passed it when the function called any withholding function anywhere -
so a function that scrubbed one field and wrote another raw passed, and so did a write through a helper
(recording.write_whole, which skills.save writes through), Path.open("w") and shutil.copyfile, none of which it saw.
Round 3 makes it structural: the walk forbids every form of writing a file in the agent's package - write_text,
write_bytes, open / Path.open / os.fdopen for writing, json.dump, os.replace / os.rename and a path's own
replace / rename, the shutil copies - except inside privacy.write_scrubbed and the writers listed below. A new writer
either goes through write_scrubbed or is listed here with why what it writes holds no secret.
"""

import ast
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "decurix_agent"

# The function every write of desktop state goes through.
THE_WRITER = "computer/privacy.py:write_scrubbed"

# file:function -> why it writes another way. Kept short: bytes nothing can scrub, the person's own files, lock files,
# the secret stores themselves, and transports that must carry what they carry.
ALLOWED = {
    # The secret stores themselves: what every withholding reads (scrubbing them would erase what they hold).
    "computer/privacy.py:_keep_hashes": "the secrets file: SHA-256 hashes, never text",
    "computer/privacy.py:_write_literals": "the tasks' secret values themselves, tmpfs, 0600, let go at the task's end",
    "computer/privacy.py:_with_literals": "the literal store's lock file",
    "computer/privacy.py:_with_resume": "a planned run's resume copy of its request, beside the tasks' secret values: "
                                        "tmpfs, 0600, let go at the task's end",
    "gate_server.py:__enter__": "the gate's lock file, in its own 0700 directory",
    "gate_server.py:_write_private": "the gate's own stores (the signed request, the tasks' secret values, the seen "
                                     "nonces), 0600, in "
                                     "the gate's 0700 directory, which the worker cannot read",
    # Lock files: an empty handle to lock, no content.
    "computer/approval.py:_locked": "the approval store's lock, and its state in the gate only (fingerprints and "
                                    "the gate's own words, withheld by shown_by_gate; the gate's user cannot read "
                                    "the worker's values) - a desktop with no gate writes it through write_scrubbed",
    "computer/core.py:_state_lock": "the observation lock file",
    "computer/recording.py:_locked": "a recording's lock file",
    "computer/skill_engine.py:run": "the skill engine's lock file",
    "computer/skill_engine.py:_note_replayed": "the replay notes' lock file",
    "retention.py:_write_label": "the labels' lock file",
    "workflow_runner.py:_lock": "a workflow run's lock file",
    # Bytes, which no text withholding can read: pictures (secret fields painted over by their callers) and blobs.
    "computer/audit.py:keep_observation": "an observation blob, privacy.scrub'd in _observation_doc before it is "
                                          "encoded",
    "computer/broker.py:deliver_to_caller": "a screenshot file the command was asked for (pixels; password fields "
                                            "painted over), written here or handed to the caller in broker mode",
    "computer/broker.py:_keep_copy": "broker mode: the caller's own state as the caller sent it (its task file, its "
                                     "secrets, its evidence: already withheld where the worker wrote it), or a file "
                                     "it handed over, kept 0600 in the screen user's private folders",
    "computer/broker.py:worker_a11y_bus": "opens the worker's published accessibility bus address read-only "
                                          "(writes nothing)",
    "computer/gui_client.py:_read_own": "opens the worker's own state read-only, to tell the screen broker (writes "
                                        "nothing)",
    "computer/gui_client.py:input_files": "opens the files a command names read-only, to hand them to the screen "
                                          "broker (writes nothing)",
    "computer/gui_client.py:save_state": "broker mode: a write of the worker's state the screen side made through "
                                         "privacy.write_scrubbed (or a recording's pixels) and handed back, written "
                                         "as the worker under its own state folder",
    "computer/gui_client.py:save_output": "a screenshot the screen broker handed back, written as the caller at a "
                                          "path the caller named; and a fixed line to its own stderr",
    "computer/recording.py:write_bytes_whole": "a recording's frames (pixels; password fields painted over)",
    # Not a file this code writes content to.
    "computer/cli.py:main": "stdout pointed at os.devnull",
    "computer/demonstration.py:start": "the recorder subprocess's own output handle",
    # The gui daemon (computer/daemon.py, gui_client.py): transport, not a file this code writes content to.
    "computer/daemon.py:bind": "renames the daemon's own freshly bound socket into place (no content)",
    "computer/daemon.py:claim": "the daemon's lock file in its own 0700 folder (no content)",
    "computer/gui_client.py:may_start_daemon": "the daemon's lock file, opened to see whether a daemon holds it (no "
                                               "content)",
    "computer/gui_client.py:_daemon_log": "the startup log, opened for a started daemon's own event lines (it logs no "
                                          "command's arguments, environment or output)",
    "computer/gui_client.py:start_daemon": "/dev/null, a started daemon's stdin (writes nothing)",
    "computer/cdp_relay.py:bind": "renames the DevTools relay's own freshly bound socket into place (no content)",
    "computer/cdp_relay.py:send": "a DevTools command to the browser's pipe (a transport, not a file)",
    "computer/screen_call.py:send": "one protocol line of an agent's screen call to its caller's pipe, in the broker's "
                                    "child (a transport, not a file: the caller holds what it says as it would have "
                                    "in-process)",
    "computer/screen_call.py:_isolate_stdio": "opens /dev/null as the call's stdin (writes nothing)",
    "computer/launch_helper.py:bind": "renames the launch helper's own freshly bound socket into place (no content)",
    "computer/gui_client.py:_descriptors": "opens the working folder O_PATH, to hand it to the daemon (writes nothing)",
    "computer/gui_client.py:_end": "a fixed line to the command's own stderr (fd 2): 'what it did was not seen'",
    "computer/gui_client.py:main": "a fixed line to the command's own stderr (fd 2): why the daemon did not take it",
    "http_api.py:_frame": "a recording frame streamed to the HTTP client, not a file",
    "logging_utils.py:log": "the log itself: every line is withheld by _withheld first (write_scrubbed logs, so it "
                            "cannot be its own writer)",
    # The person's own files: their documents and copies of them, which the work is about.
    "attachments.py:keep_originals": "copies of the person's own attachments",
    "attachments.py:_extract": "the person's own files, unpacked from their archive",
    "playbooks.py:_run_in_copy": "a learned tool's script copied into its sandbox",
    "produced.py:host_copies": "local copies of the person's own files",
    "script_sandbox.py:copy_work": "the person's work files copied into a sandbox",
    "script_sandbox.py:copy_back": "the person's work files copied back out of a sandbox",
    "shared.py:copy_file": "a copy of the person's file to their shared folder",
    "source_guard.py:take": "a snapshot copy of the person's source file (its manifest goes through write_scrubbed)",
    "tool_made.py:remember": "a copy of a document the tools saved for the person (its record goes through "
                             "write_scrubbed)",
    "tools/office.py:_read_eml": "an email's attachment, unpacked to read it",
    "tools/office.py:_write_zip": "an Office document the person asked for",
    "tools/office.py:combine_pdf": "a PDF the person asked for",
    "tools/office.py:create_pdf": "a PDF the person asked for",
    "tools/office.py:_recalculated_values": "a copy of the person's workbook, to read its computed values",
    "tools/office.py:_image_to_pdf": "a PDF made from the person's picture",
    "tools/office.py:create_presentation": "a presentation the person asked for",
    "tools/office.py:update_presentation": "the person's presentation, updated as asked",
    # Transports: the request itself, read by the watcher and replaced by its withheld copy once read
    # (watchers._mark_handled).
    "messaging.py:save_request": "the command file: the request the worker must read, renamed into place",
    "long_task.py:_hold": "a new request held aside word for word until the person says what to do, like the command "
                          "file",
    "long_task.py:_pending_request_reply": "the held request, put back as a command file",
    "computer/recording.py:import_legacy_audit": "renames an imported audit index (no new content)",
    "long_task.py:_release": "puts a held request back as a command file (a transport, renamed)",
    "long_task.py:ledger": "moves an unreadable ledger aside (no new content)",
    "tools/office.py:update_document": "the person's document, updated as asked",
    "watchers.py:_mark_handled": "renames a command file it found holding no secret (its withheld copy goes "
                                 "through write_scrubbed)",
}

# The forms of writing each listed writer uses, and no other: a listed function that writes some other way (a
# field scrubbed, another written raw) fails too.
FORMS = {
    "computer/privacy.py:_keep_hashes": {"write_text"},
    "computer/privacy.py:_write_literals": {"os.fdopen", "os.replace"},
    "computer/privacy.py:_with_literals": {"open"},
    "computer/privacy.py:_with_resume": {"open", "os.fdopen", "os.replace"},
    "gate_server.py:__enter__": {"open"},
    "gate_server.py:_write_private": {"os.fdopen", "os.replace"},
    "computer/approval.py:_locked": {"open", "os.fdopen", "os.replace"},
    "computer/core.py:_state_lock": {"open"},
    "computer/recording.py:_locked": {"open"},
    "computer/skill_engine.py:run": {"open"},
    "computer/skill_engine.py:_note_replayed": {"open"},
    "retention.py:_write_label": {"open"},
    "workflow_runner.py:_lock": {"Path.open"},
    "computer/audit.py:keep_observation": {"os.replace", "write_bytes"},
    "computer/broker.py:deliver_to_caller": {"open", "os.replace"},
    "computer/gui_client.py:save_output": {"os.open", "os.write", "os.replace"},
    "computer/broker.py:_keep_copy": {"os.open", "os.write", "os.replace"},
    "computer/gui_client.py:save_state": {"os.open", "os.write", "os.replace"},
    "computer/gui_client.py:_read_own": {"os.open"},
    "computer/broker.py:worker_a11y_bus": {"os.open"},
    "computer/gui_client.py:input_files": {"os.open"},
    "computer/recording.py:write_bytes_whole": {"path.replace", "write_bytes"},
    "computer/cli.py:main": {"open"},
    "computer/demonstration.py:start": {"open"},
    "computer/daemon.py:bind": {"os.rename"},
    "computer/daemon.py:claim": {"os.open"},
    "computer/gui_client.py:may_start_daemon": {"os.open"},
    "computer/gui_client.py:_daemon_log": {"os.open"},
    "computer/gui_client.py:start_daemon": {"os.open"},
    "computer/cdp_relay.py:bind": {"os.rename"},
    "computer/cdp_relay.py:send": {"os.write"},
    "computer/screen_call.py:send": {"os.write"},
    "computer/screen_call.py:_isolate_stdio": {"os.open"},
    "computer/launch_helper.py:bind": {"os.rename"},
    "computer/gui_client.py:_descriptors": {"os.open"},
    "computer/gui_client.py:_end": {"os.write"},
    "computer/gui_client.py:main": {"os.write"},
    "http_api.py:_frame": {"shutil.copyfileobj"},
    "logging_utils.py:log": {"open"},
    "attachments.py:keep_originals": {"shutil.copy2"},
    "attachments.py:_extract": {"open"},
    "playbooks.py:_run_in_copy": {"shutil.copy2"},
    "produced.py:host_copies": {"write_bytes"},
    "script_sandbox.py:copy_work": {"shutil.copy2"},
    "script_sandbox.py:copy_back": {"os.replace", "shutil.copy2"},
    "shared.py:copy_file": {"shutil.copyfile"},
    "source_guard.py:take": {"shutil.copy2"},
    "tool_made.py:remember": {"shutil.copy2"},
    "tools/office.py:_read_eml": {"write_bytes"},
    "tools/office.py:_write_zip": {"os.replace"},
    "tools/office.py:combine_pdf": {"open"},
    "tools/office.py:create_pdf": {"shutil.copyfile"},
    "tools/office.py:_recalculated_values": {"shutil.copyfile", "write_text"},
    "tools/office.py:_image_to_pdf": {"write_bytes"},
    "tools/office.py:create_presentation": {"os.replace"},
    "tools/office.py:update_presentation": {"os.replace"},
    "messaging.py:save_request": {"path.replace", "write_text"},
    "long_task.py:_hold": {"path.replace", "write_text"},
    "long_task.py:_pending_request_reply": {"write_text"},
    "computer/recording.py:import_legacy_audit": {"path.rename"},
    "long_task.py:_release": {"path.replace"},
    "long_task.py:ledger": {"path.replace"},
    "tools/office.py:update_document": {"path.replace"},
    "watchers.py:_mark_handled": {"path.rename"},
}

COPIES = {"copy", "copy2", "copyfile", "copyfileobj", "copytree", "move"}
TEMP_FILES = {"NamedTemporaryFile", "TemporaryFile", "SpooledTemporaryFile"}

# The listed writers that write what their caller hands them, raw, and every function that may call them: a new
# caller is a new raw writer (review round 3: `recording.write_bytes_whole(path, text.encode())` wrote through
# unchecked). Each is a transport, a secret store or bytes.
RAW_HELPERS = {
    "write_bytes_whole": {"computer/recording.py:write_whole"},
    "_hold": {"long_task.py:_take_message", "long_task.py:_take_new_request"},
    # (_fresh: the seen nonces and their expiry - random ids the control plane made, no words of anyone's)
    "_write_private": {"gate_server.py:_fresh", "gate_server.py:end_task", "gate_server.py:keep_request",
                       "gate_server.py:keep_secrets"},
    "_keep_copy": {"computer/broker.py:take_context", "computer/broker.py:local_copy",
                   "computer/broker.py:_take_launchers"},
    "save_request": {"http_api.py:_learn", "http_api.py:_replay", "http_api.py:_request", "http_api.py:_run_skill"},
    "_write_literals": {"computer/privacy.py:_with_literals"},
    "_with_literals": {"computer/privacy.py:clear_literals", "computer/privacy.py:remember_literals"},
    "_with_resume": {"computer/privacy.py:clear_literals", "computer/privacy.py:keep_resume_copy"},
    "keep_observation": {"computer/audit.py:record"},
    "save_screenshot": {"computer/cli.py:main"},
    "deliver_to_caller": {"computer/cli.py:_look", "computer/cli.py:run", "computer/cli.py:save_screenshot"},
    "_pending_request_reply": {"long_task.py:_take_message"},
    "_release": {"long_task.py:_pending_request_reply"},
}


def _mode(call, at):
    for k in call.keywords:
        if k.arg == "mode" and isinstance(k.value, ast.Constant):
            return str(k.value.value)
    if len(call.args) > at and isinstance(call.args[at], ast.Constant):
        return str(call.args[at].value)
    return ""


def _writing(mode):
    return any(c in mode for c in "wax+")


def write_form(call):
    """The form of writing a file this call is, or "" - by structure, whatever it is named."""

    f = call.func
    if isinstance(f, ast.Name):
        if f.id == "open" and _writing(_mode(call, 1)):
            return "open"
        return "tempfile." + f.id if f.id in TEMP_FILES else ""
    if not isinstance(f, ast.Attribute):
        return ""
    owner = f.value.id if isinstance(f.value, ast.Name) else ""
    if f.attr in ("write_text", "write_bytes"):
        return f.attr
    if owner in ("io", "codecs") and f.attr == "open" and _writing(_mode(call, 1)):
        return owner + ".open"
    if owner == "os" and f.attr in ("open", "write"):
        return "os." + f.attr  # (a raw descriptor: whatever its flags, nothing withholds what goes through it)
    if owner == "tempfile" and f.attr in TEMP_FILES:
        return "tempfile." + f.attr  # (opened for writing by default: "w+b")
    if f.attr == "open" and owner not in ("os", "io", "codecs", "tarfile", "zipfile", "gzip") and _writing(
            _mode(call, 0)):
        return "Path.open"
    if owner == "os" and f.attr in ("replace", "rename"):
        return "os." + f.attr
    if owner == "os" and f.attr == "fdopen" and _writing(_mode(call, 1)):
        return "os.fdopen"
    if owner == "shutil" and f.attr in COPIES:
        return "shutil." + f.attr
    if owner == "json" and f.attr == "dump":
        return "json.dump"
    # A path's own replace(target) / rename(target): one argument (str.replace takes two).
    if f.attr in ("replace", "rename") and len(call.args) == 1 and not call.keywords:
        return "path." + f.attr
    return ""


def writes():
    """{"file:function": [(form, line)]} of every write in the agent's package outside THE_WRITER."""

    found = {}
    for path in sorted(ROOT.rglob("*.py")):
        rel = path.relative_to(ROOT).as_posix()
        if "thebot" in path.relative_to(ROOT).parts:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"))

        def visit(node, where):
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and where == "<module>":
                where = node.name
            elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                pass  # a nested function belongs to the function it is in
            if isinstance(node, ast.Call):
                form = write_form(node)
                if form:
                    found.setdefault(f"{rel}:{where}", []).append((form, node.lineno))
            for child in ast.iter_child_nodes(node):
                visit(child, where)

        for top in tree.body:
            if isinstance(top, ast.ClassDef):
                for item in top.body:
                    visit(item, "<module>")
            else:
                visit(top, "<module>")
    found.pop(THE_WRITER, None)
    return found


def raw_helper_callers():
    """{helper: {"file:function"}} of every call to a RAW_HELPERS function in the agent's package."""

    found = {name: set() for name in RAW_HELPERS}
    for path in sorted(ROOT.rglob("*.py")):
        rel = path.relative_to(ROOT).as_posix()
        if "thebot" in path.relative_to(ROOT).parts:
            continue
        for top in ast.parse(path.read_text(encoding="utf-8")).body:
            items = top.body if isinstance(top, ast.ClassDef) else [top]
            for item in items:
                where = item.name if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)) else "<module>"
                for node in ast.walk(item):
                    if isinstance(node, ast.Call):
                        name = node.func.attr if isinstance(node.func, ast.Attribute) else getattr(node.func, "id", "")
                        if name in found:
                            found[name].add(f"{rel}:{where}")
    return found


class EveryWriteGoesThroughTheWriter(unittest.TestCase):
    def test_no_other_write_in_the_agents_code(self):
        bare = {k: v for k, v in writes().items()
                if k not in ALLOWED or any(form not in FORMS.get(k, ()) for form, _ in v)}
        self.assertEqual(bare, {}, "a write of a file that does not go through privacy.write_scrubbed: write through "
                         "it, or list the writer in ALLOWED with why what it writes holds no secret")

    def test_the_list_holds_only_writers_that_are_there(self):
        self.assertEqual(sorted(set(ALLOWED) - set(writes())), [], "a listed writer writes through write_scrubbed "
                         "now, or is gone: take it off ALLOWED")
        self.assertEqual(set(ALLOWED), set(FORMS))

    def test_no_new_caller_of_a_raw_helper(self):
        self.assertEqual({k: sorted(v) for k, v in raw_helper_callers().items() if v != RAW_HELPERS[k]}, {},
                         "a raw writer has a caller it did not have: write through privacy.write_scrubbed, or list "
                         "the caller in RAW_HELPERS with why what it hands over holds no secret")

    def test_the_walk_sees_each_form(self):
        source = '''
def a(p):
    p.open("w").write("x")
def b(p):
    shutil.copyfile(p, p)
def c(p, t):
    t.replace(p)
def d(p):
    with open(p, "a") as f:
        json.dump({}, f)
def e(p):
    os.fdopen(3, "w")
def f(p):
    p.write_bytes(b"")
def g(s):
    return s.replace("a", "b"), open(s).read(), s.open().read(), io.open(s).read()
def h(p):
    io.open(p, "w"); codecs.open(p, "a"); tempfile.NamedTemporaryFile("w"); os.write(os.open(p, 1), b"")
'''
        tree = ast.parse(source)
        forms = sorted(write_form(n) for n in ast.walk(tree) if isinstance(n, ast.Call) and write_form(n))
        self.assertEqual(forms, ["Path.open", "codecs.open", "io.open", "json.dump", "open", "os.fdopen", "os.open",
                                 "os.write", "path.replace", "shutil.copyfile", "tempfile.NamedTemporaryFile",
                                 "write_bytes"])

    def test_skills_save_goes_through_the_writer(self):
        # Review round 2 (R2-N1): skills.save keeps people's run answers and corrections, through recording's helper.
        self.assertNotIn("computer/skills.py:save", writes())
        self.assertNotIn("computer/recording.py:write_whole", writes())


if __name__ == "__main__":
    unittest.main()
