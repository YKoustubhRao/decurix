"""
Broker mode's state and files (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "the screen side's own state"): what of
the worker's state the screen side is told with a command and where it keeps it; what it writes of that state coming
back for the caller to write; the files a command names handed over as descriptors; the commands that run as the worker;
the worker's accessibility bus read by the screen side, and an action on one of its widgets done only over a window of
the worker's; the browser launcher run as the worker handing its pages to the broker.

Most run anywhere; the ones that need Unix uids and socket pairs with descriptors skip on Windows.

    cd agent && PYTHONPATH=".:tests" python3 -m unittest tests.test_screen_state
"""

import os
import shutil
import socket
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

TESTS = os.path.dirname(os.path.abspath(__file__))
AGENT = os.path.dirname(TESTS)
sys.path.insert(0, AGENT)

from decurix_agent.computer import broker, daemon, gui_client, screen_call  # noqa: E402

POSIX = os.name == "posix" and hasattr(os, "getuid") and hasattr(socket, "AF_UNIX")
TASK = "t0123456789abcdef"


def _frames(sock):
    """The O frames a socket pair's other end received: [(flags, path, data)]."""

    data = b""
    while True:
        chunk = sock.recv(1 << 16)
        if not chunk:
            break
        data += chunk
    out = []
    while data:
        assert data[:1] == b"O", data[:10]
        n = int.from_bytes(data[1:5], "big")
        flags, path, body = gui_client.unpack(data[5:5 + n])
        out.append((int(flags), path, body))
        data = data[5 + n:]
    return out


class _Child:
    """A broker's child's caller connection (broker.CALLER_MODULE), for as long as the with-block runs."""

    def __init__(self, inputs=None):
        self.ours, self.theirs = socket.socketpair()
        self.holder = types.ModuleType(broker.CALLER_MODULE)
        self.holder.conn = self.ours
        self.holder.inputs = dict(inputs or {})
        self.patch = mock.patch.dict(sys.modules, {broker.CALLER_MODULE: self.holder})

    def __enter__(self):
        self.patch.start()
        return self

    def __exit__(self, *exc):
        self.patch.stop()
        self.ours.close()
        self.theirs.close()

    def frames(self):
        """What was sent so far (the child's end is closed first, so the read ends)."""

        self.ours.close()
        try:
            return _frames(self.theirs)
        finally:
            self.theirs.close()


class Tmp(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="screen-state-")
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def path(self, *parts):
        return os.path.join(self.tmp, *parts)

    def write(self, rel, data):
        path = self.path(*rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)
        return path


# ------------------------------------------------------------
# The worker's state, told to the screen side
# ------------------------------------------------------------

class TheCallerTellsItsState(Tmp):
    def test_the_context_is_the_task_its_secrets_and_its_evidence(self):
        task = self.path("task")
        state = self.path("state")
        self.write("task", TASK.encode())
        self.write("task.secrets", b'{"t0123456789abcdef": ["hunter2x"]}')
        self.write("state/withheld-secrets", b"abc\n")
        self.write(f"state/computer/evidence/{TASK}.jsonl", b'{"note": 1}\n')
        self.write(f"state/recordings/task-{TASK}/meta.json", b"{}")
        self.write("state/computer/evidence/t1111111111111111.jsonl", b"another task's")
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": task, "DECURIX_DIR": state}):
            fields = gui_client.caller_context()
        sent = {f[2:].split(b"\0", 1)[0]: f[2:].split(b"\0", 1)[1] for f in fields if f[:2] == b"\0c"}
        absent = {f[2:] for f in fields if f[:2] == b"\0x"}
        self.assertEqual(sent[b"t"], TASK.encode())
        self.assertEqual(sent[b"t.secrets"], b'{"t0123456789abcdef": ["hunter2x"]}')
        self.assertEqual(sent[b"swithheld-secrets"], b"abc\n")
        self.assertEqual(sent[b"scomputer/evidence/%s.jsonl" % TASK.encode()], b'{"note": 1}\n')
        self.assertIn(b"srecordings/task-%s/meta.json" % TASK.encode(), sent)
        self.assertIn(b"t.stage", absent)
        self.assertNotIn(b"scomputer/evidence/t1111111111111111.jsonl", sent, "only the current task's evidence")

    def test_no_task_no_evidence(self):
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": self.path("none"), "DECURIX_DIR": self.path("s")}):
            names = [f[2:].split(b"\0", 1)[0] for f in gui_client.caller_context()]
        self.assertFalse(any(b"evidence" in n for n in names))

    @unittest.skipUnless(POSIX, "the request carries the environment as bytes (os.environb)")
    def test_the_request_carries_the_context_and_the_daemon_parses_it(self):
        fields = [b"\0ct\0" + TASK.encode(), b"\0xt.stage", b"\0f" + b"a.html\0/w/a.html"]
        data = gui_client.request(["open", "a.html"], 0b111, fields)
        req = daemon.parse_request(data, [10, 11, 12, 13, 14])
        self.assertEqual(req.context, {b"t": TASK.encode(), b"t.stage": None})
        self.assertEqual(req.inputs, [(b"a.html", b"/w/a.html", 14)])
        self.assertEqual(req.cwd_fd, 13)
        self.assertEqual(req.fds, {0: 10, 1: 11, 2: 12})
        self.assertNotIn(b"", req.env)
        with self.assertRaises(ValueError):
            daemon.parse_request(data, [10, 11, 12, 13])  # a handed file's descriptor missing

    @unittest.skipUnless(POSIX, "needs Unix uids")
    def test_the_screen_side_keeps_it_in_its_own_folders_only(self):
        caller = self.path("caller")
        os.mkdir(caller, 0o700)
        state = self.path("screen-state")
        env = {"DECURIX_TASK_FILE": os.path.join(caller, "task"), "DECURIX_DIR": state}
        self.write("caller/task.stage", b"old")
        context = {b"t": TASK.encode(), b"t.stage": None, b"t.bogus": b"x",
                   b"scomputer/evidence/%s.records.json" % TASK.encode(): b"[]",
                   b"scomputer/evidence/../../escape": b"x", b"s/etc/passwd": b"x", b"sskills/x.json": b"x"}
        with mock.patch.dict(os.environ, env), _Child():
            broker.take_context(context)
        with open(os.path.join(caller, "task"), "rb") as f:
            self.assertEqual(f.read(), TASK.encode())
        self.assertEqual(os.stat(os.path.join(caller, "task")).st_mode & 0o777, 0o600)
        self.assertFalse(os.path.exists(os.path.join(caller, "task.stage")), "a part the caller lacks is removed")
        self.assertFalse(os.path.exists(os.path.join(caller, "task.bogus")))
        self.assertTrue(os.path.exists(os.path.join(state, "computer", "evidence", f"{TASK}.records.json")))
        self.assertFalse(os.path.exists(self.path("escape")))
        self.assertFalse(os.path.exists(os.path.join(state, "skills")))

    @unittest.skipUnless(POSIX, "needs Unix uids")
    def test_never_a_task_file_in_a_folder_others_may_enter(self):
        shared = self.path("shared")
        os.mkdir(shared, 0o755)
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": os.path.join(shared, "task")}), _Child():
            broker.take_context({b"t": TASK.encode()})
        self.assertFalse(os.path.exists(os.path.join(shared, "task")))

    def test_outside_a_brokers_child_nothing_is_taken(self):
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": self.path("task")}):
            broker.take_context({b"t": TASK.encode()})
        self.assertFalse(os.path.exists(self.path("task")))


class TheWorkersLaunchersAndDocuments(Tmp):
    def test_the_client_sends_its_launchers_and_hands_over_the_tasks_documents(self):
        home = self.path("home")
        self.write("home/Desktop/excel.desktop", b"[Desktop Entry]\nName=Excel (Host)\nExec=host-excel\n")
        self.write("home/.local/share/applications/pad.desktop", b"[Desktop Entry]\nName=Pad\nExec=mousepad\n")
        self.write("home/Desktop/notes.txt", b"not a launcher")
        doc = self.write("home/work/bill.pdf", b"%PDF")
        self.write("task.files", (doc + "\n").encode())
        with mock.patch.dict(os.environ, {"HOME": home, "USERPROFILE": home, "DECURIX_TASK_FILE": self.path("task"),
                                          "DECURIX_DIR": self.path("state")}):
            fields = gui_client.caller_context()
            documents = gui_client.task_documents()
        names = [f[2:].split(b"\0", 1)[0] for f in fields if f[:2] == b"\0c"]
        self.assertIn(b"l", names, "the marker that the whole set follows")
        self.assertIn(b"lDesktop/excel.desktop", names)
        self.assertIn(b"lapplications/pad.desktop", names)
        self.assertNotIn(b"lDesktop/notes.txt", names)
        self.assertEqual(documents, [doc])

    @unittest.skipUnless(POSIX, "needs a private home folder (Unix modes)")
    def test_the_screen_side_keeps_the_whole_set_and_lists_it(self):
        from decurix_agent.computer import launchers
        home = self.path("screen-home")
        os.mkdir(home, 0o700)
        stale = os.path.join(home, broker.LAUNCHERS_DIR, "Desktop", "gone.desktop")
        os.makedirs(os.path.dirname(stale))
        open(stale, "w").close()
        context = {b"l": b"", b"lDesktop/excel.desktop": b"[Desktop Entry]\nName=Excel (Host)\nExec=host-excel\n",
                   b"lDesktop/../../x.desktop": b"x", b"lother/x.desktop": b"x"}
        with mock.patch.dict(os.environ, {"HOME": home}), _Child():
            broker.take_context(context)
        self.assertFalse(os.path.exists(stale), "a launcher the worker no longer has goes")
        self.assertFalse(os.path.exists(self.path("x.desktop")))
        found = launchers.entries(broker.launcher_places(home))
        self.assertEqual([(e["name"], e["exec"]) for e in found], [("Excel (Host)", "host-excel")])

    @unittest.skipUnless(POSIX, "needs a private home folder (Unix modes)")
    def test_a_tasks_document_is_read_only_as_handed_over(self):
        from decurix_agent.computer import core
        from decurix_agent.tools import office
        home = self.path("screen-home")
        os.mkdir(home, 0o700)
        doc = self.write("work/remittance.txt", b"Paid by Northwind Traders")
        fd = os.open(doc, os.O_RDONLY)
        self.addCleanup(os.close, fd)
        read = []

        def read_document(path):
            read.append(path)
            with open(path, encoding="utf-8") as f:
                return {"text": f.read()}

        with mock.patch.dict(os.environ, {"HOME": home}), _Child({doc: fd}), \
                mock.patch("decurix_agent.task_context.current_files", return_value=[doc, "/w/other.pdf"]), \
                mock.patch.object(office, "read_document", side_effect=read_document), \
                mock.patch.dict(core._DOCUMENT_WORDS, clear=True):
            self.assertTrue(core._in_documents("Northwind Traders"))
            self.assertFalse(core._in_documents("Contoso"))
        self.assertTrue(read and all(p.startswith(os.path.join(home, broker.COPIES_DIR)) for p in read),
                        "the screen side read a document of the worker's by its name")


# ------------------------------------------------------------
# The worker's state, written back
# ------------------------------------------------------------

class TheScreenSideHandsItsWritesBack(Tmp):
    def test_a_recording_and_an_evidence_note_go_back_and_nothing_else(self):
        state = self.path("screen-state")
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), _Child() as child:
            broker.mirror(os.path.join(state, "recordings", "task-x", "events.jsonl"), b"line\n", append=True)
            broker.mirror(os.path.join(state, "computer", "evidence", f"{TASK}.jsonl"), b"{}")
            broker.mirror(os.path.join(state, "computer", "shown-refs.json"), b"{}")
            broker.mirror(os.path.join(state, "recordings"), b"{}")
            broker.mirror(self.path("elsewhere", "recordings", "x"), b"{}")
            frames = child.frames()
        self.assertEqual(frames, [
            (gui_client.OUTPUT_STATE | gui_client.OUTPUT_APPEND, b"recordings/task-x/events.jsonl", b"line\n"),
            (gui_client.OUTPUT_STATE, b"computer/evidence/%s.jsonl" % TASK.encode(), b"{}")])

    def test_write_scrubbed_hands_back_what_it_wrote_withheld(self):
        from decurix_agent.computer import privacy
        state = self.path("screen-state")
        path = Path(state) / "computer" / "evidence" / f"{TASK}.jsonl"
        path.parent.mkdir(parents=True)
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), \
                mock.patch.object(privacy, "_withhold_known", side_effect=lambda t, shapes=True: t.replace("hunter2x", "[withheld]")), \
                _Child() as child:
            privacy.write_scrubbed(path, "typed hunter2x\n", append=True)
            privacy.write_scrubbed(path.with_name(f"{TASK}.records.json"), {"v": "hunter2x"}, atomic=True)
            frames = child.frames()
        self.assertEqual(path.read_text(encoding="utf-8"), "typed [withheld]\n")
        self.assertEqual(frames[0][2], b"typed [withheld]\n")
        self.assertTrue(frames[0][0] & gui_client.OUTPUT_APPEND)
        self.assertEqual(frames[1][1], b"computer/evidence/%s.records.json" % TASK.encode())
        self.assertNotIn(b"hunter2x", frames[1][2])

    def test_nothing_goes_back_outside_a_brokers_child(self):
        from decurix_agent.computer import privacy
        with mock.patch.object(broker, "_send") as send:
            privacy.write_scrubbed(Path(self.tmp) / "x.txt", "text")
            broker.mirror(self.path("recordings", "x"), b"x")
        send.assert_not_called()

    def test_the_caller_writes_it_under_its_own_state_folder_only(self):
        state = self.path("worker-state")
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), mock.patch("os.write", wraps=os.write):
            self.assertTrue(gui_client.save_state(gui_client.OUTPUT_STATE, b"recordings/task-x/meta.json", b"{1}"))
            self.assertTrue(gui_client.save_state(gui_client.OUTPUT_STATE | gui_client.OUTPUT_APPEND,
                                                  b"recordings/task-x/events.jsonl", b"a\n"))
            self.assertTrue(gui_client.save_state(gui_client.OUTPUT_STATE | gui_client.OUTPUT_APPEND,
                                                  b"recordings/task-x/events.jsonl", b"b\n"))
            for bad in (b"../x", b"/etc/x", b"recordings/../../x", b"computer/shown-refs.json", b"recordings",
                        b"recordings//x", b"computer/evidence/./x"):
                self.assertFalse(gui_client.save_state(gui_client.OUTPUT_STATE, bad, b"x"), bad)
        with open(os.path.join(state, "recordings", "task-x", "meta.json"), "rb") as f:
            self.assertEqual(f.read(), b"{1}")
        with open(os.path.join(state, "recordings", "task-x", "events.jsonl"), "rb") as f:
            self.assertEqual(f.read(), b"a\nb\n")
        self.assertFalse(os.path.exists(self.path("x")))

    def test_a_state_frame_in_the_outcome_is_written_and_does_not_fail_the_command(self):
        state = self.path("worker-state")
        a, b = socket.socketpair()
        payload = gui_client.pack([b"%d" % gui_client.OUTPUT_STATE, b"computer/evidence/n.jsonl", b"note"])
        b.sendall(b"S\0O" + len(payload).to_bytes(4, "big") + payload + b"C\0E\0")
        b.close()
        try:
            with mock.patch.dict(os.environ, {"DECURIX_DIR": state}):
                self.assertEqual(gui_client._outcome(a, set()), ("exit", 0))
        finally:
            a.close()
        with open(os.path.join(state, "computer", "evidence", "n.jsonl"), "rb") as f:
            self.assertEqual(f.read(), b"note")

    def test_a_demonstration_is_handed_back_whole_at_its_end(self):
        state = self.path("screen-state")
        session = os.path.join(state, "recordings", "demo-1")
        os.makedirs(os.path.join(session, "frames"))
        for rel, data in (("meta.json", b"{}"), ("events.jsonl", b"e\n"), ("frames/a.png", b"PNG"), (".lock", b"")):
            with open(os.path.join(session, *rel.split("/")), "wb") as f:
                f.write(data)
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), _Child() as child:
            broker.mirror_tree(session)
            frames = child.frames()
        self.assertEqual(sorted(p for _, p, _ in frames),
                         [b"recordings/demo-1/events.jsonl", b"recordings/demo-1/frames/a.png",
                          b"recordings/demo-1/meta.json"])


class TheTasksRunContextGoesBothWays(Tmp):
    """The task's run context (runs/<task>/context.json): the save path reads it on the screen side and keeps there the
    question it waits on (a field's unit, a date's order), which the agent - the worker - asks the person from. Found
    live (2026-11-08): it stayed in the screen user's copy, and the person was asked the engine's question instead."""

    def test_the_caller_sends_its_tasks_context(self):
        task, state = self.path("task"), self.path("state")
        self.write("task", TASK.encode())
        self.write(f"state/runs/{TASK}/context.json", b'{"reply_judgement": "j_abcdef12"}')
        self.write(f"state/runs/{TASK}/run.json", b"{}")
        self.write("state/runs/t1111111111111111/context.json", b"another task's")
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": task, "DECURIX_DIR": state}):
            fields = gui_client.caller_context()
        sent = {f[2:].split(b"\0", 1)[0]: f[2:].split(b"\0", 1)[1] for f in fields if f[:2] == b"\0c"}
        self.assertEqual(sent[b"sruns/%s/context.json" % TASK.encode()], b'{"reply_judgement": "j_abcdef12"}')
        self.assertFalse(any(name.startswith(b"sruns/") and not name.endswith(b"/context.json") for name in sent))
        self.assertNotIn(b"sruns/t1111111111111111/context.json", sent, "only the current task's context")

    @unittest.skipUnless(POSIX, "needs Unix uids")
    def test_the_screen_side_keeps_it_in_its_own_folder(self):
        caller = self.path("caller")
        os.mkdir(caller, 0o700)
        state = self.path("screen-state")
        with mock.patch.dict(os.environ, {"DECURIX_TASK_FILE": os.path.join(caller, "task"), "DECURIX_DIR": state}),                 _Child():
            broker.take_context({b"sruns/%s/context.json" % TASK.encode(): b"{}",
                                 b"sruns/%s/run.json" % TASK.encode(): b"{}"})
        self.assertTrue(os.path.exists(os.path.join(state, "runs", TASK, "context.json")))
        self.assertFalse(os.path.exists(os.path.join(state, "runs", TASK, "run.json")))

    def test_a_question_the_save_keeps_goes_back_to_the_worker(self):
        from decurix_agent import config, task_context
        state = self.path("screen-state")
        question = {"label": "Rate", "host": "127.0.0.1:8765", "form": "/taxes", "unit": "%", "shown": "18",
                    "wanted": "18%"}
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), mock.patch.object(config, "DECURIX_DIR", Path(state)),                 _Child() as child:
            task_context._keep_context(TASK, unit_question=question)
            broker.mirror(os.path.join(state, "runs", TASK, "run.json"), b"{}")
            broker.mirror(os.path.join(state, "runs", TASK, "LABELLED"), b"x")
            frames = child.frames()
        self.assertEqual([(flags, rel) for flags, rel, _ in frames],
                         [(gui_client.OUTPUT_STATE, b"runs/%s/context.json" % TASK.encode())])
        import json
        self.assertEqual(json.loads(frames[0][2])["unit_question"], question)

    def test_the_worker_writes_its_context_and_nothing_else_of_the_run_folder(self):
        state = self.path("worker-state")
        rel = b"runs/%s/context.json" % TASK.encode()
        with mock.patch.dict(os.environ, {"DECURIX_DIR": state}), mock.patch("os.write", wraps=os.write):
            self.assertTrue(gui_client.save_state(gui_client.OUTPUT_STATE, rel, b'{"a": 1}'))
            for bad in (b"runs/%s/run.json" % TASK.encode(), b"runs/%s/LABELLED" % TASK.encode(),
                        b"runs/t0123/context.json", b"runs/../context.json", b"runs/%s/x/context.json" % TASK.encode()):
                self.assertFalse(gui_client.save_state(gui_client.OUTPUT_STATE, bad, b"x"), bad)
        target = os.path.join(state, "runs", TASK, "context.json")
        with open(target, "rb") as f:
            self.assertEqual(f.read(), b'{"a": 1}')
        if POSIX:
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o600)


# ------------------------------------------------------------
# Files a command names, handed over
# ------------------------------------------------------------

class TheFilesACommandNames(Tmp):
    def test_the_client_hands_over_the_regular_files_it_can_read(self):
        page = self.write("work/page.html", b"<p>hi</p>")
        bill = self.write("work/bill.txt", b"bill")
        found = gui_client.input_files(["upload", "e12", page, "--file=" + bill, "file://" + page, self.tmp,
                                        self.path("missing.txt"), "e5"])
        try:
            given = [(g, p) for g, p, _ in found]
            self.assertIn((os.fsencode(page), os.fsencode(os.path.abspath(page))), given)
            self.assertIn((os.fsencode(bill), os.fsencode(os.path.abspath(bill))), given)
            self.assertEqual(len(found), 2, "a file named twice (a file:// URL of it) is handed over once; a folder, "
                                            "a missing file and a ref are not")
        finally:
            for _, _, fd in found:
                os.close(fd)
        found = gui_client.input_files(["open", "file://" + page])
        try:
            self.assertEqual([(g, p) for g, p, _ in found],
                             [(os.fsencode("file://" + page), os.fsencode(os.path.abspath(page)))])
        finally:
            for _, _, fd in found:
                os.close(fd)

    def test_at_most_so_many(self):
        paths = [self.write(f"f{i}", b"x") for i in range(gui_client.MAX_INPUTS + 4)]
        found = gui_client.input_files(["upload"] + paths)
        for _, _, fd in found:
            os.close(fd)
        self.assertEqual(len(found), gui_client.MAX_INPUTS)

    @unittest.skipUnless(POSIX, "needs a private home folder (Unix modes)")
    def test_the_screen_side_reads_a_handed_file_only_from_its_descriptor(self):
        home = self.path("screen-home")
        os.mkdir(home, 0o700)
        bill = self.write("work/bill.txt", b"Customer: Acme\n")
        fd = os.open(bill, os.O_RDONLY)
        self.addCleanup(os.close, fd)
        os.unlink(bill)  # gone by its name: only the descriptor reads it now
        with mock.patch.dict(os.environ, {"HOME": home}), _Child({bill: fd}):
            self.assertTrue(broker.handed(bill))
            copy = broker.local_copy(bill)
            self.assertEqual(broker.local_copy(copy), copy, "a copy is not copied again")
            with self.assertRaises(broker.NotHanded):
                broker.local_copy(self.path("work", "other.txt"))
        self.assertEqual(os.path.basename(copy), "bill.txt")
        self.assertTrue(copy.startswith(os.path.join(home, broker.COPIES_DIR)))
        with open(copy, "rb") as f:
            self.assertEqual(f.read(), b"Customer: Acme\n")
        self.assertEqual(os.stat(copy).st_mode & 0o777, 0o600)

    @unittest.skipUnless(POSIX, "needs a private home folder (Unix modes)")
    def test_a_copys_address_reads_back_as_the_workers_own_file(self):
        home = self.path("screen-home")
        os.mkdir(home, 0o700)
        page = self.write("work/my form.html", b"<title>Customers</title>")
        fd = os.open(page, os.O_RDONLY)
        self.addCleanup(os.close, fd)
        with mock.patch.dict(os.environ, {"HOME": home, "DECURIX_SCREEN_USER": "1"}), _Child({page: fd}):
            copy = broker.local_copy(page)
            import urllib.parse
            url = "file://" + urllib.parse.quote(copy)
            mine = "file://" + urllib.parse.quote(page)
            self.assertEqual(broker.shown_url(url), mine)
            self.assertEqual(broker.shown_url(url + "#top"), mine + "#top")
            self.assertEqual(broker.shown_url("https://example.com/a"), "https://example.com/a")
            other = "file://" + urllib.parse.quote(os.path.join(home, broker.COPIES_DIR, "abc", "x.html"))
            self.assertEqual(broker.shown_url(other), other, "a folder that is no copy of ours stays as it is")
            origin = "file://" + urllib.parse.quote(os.path.join(os.path.dirname(copy), broker.ORIGIN_FILE))
            self.assertEqual(broker.shown_url(origin), origin)
            with mock.patch.dict(os.environ, {"DECURIX_SCREEN_USER": "0"}):
                self.assertEqual(broker.shown_url(url), url, "outside broker mode an address is itself")

    def test_outside_a_brokers_child_a_path_is_itself(self):
        self.assertEqual(broker.local_copy("/some/file.html"), "/some/file.html")

    def test_an_agent_call_hands_over_the_file_it_opens(self):
        self.assertEqual(screen_call.named_files("core.open_target", ("/home/headless/work/a.ods",), {}),
                         ["/home/headless/work/a.ods"])
        self.assertEqual(screen_call.named_files("core.open_target", (), {"target": "x.html", "new_tab": True}),
                         ["x.html"])
        self.assertEqual(screen_call.named_files("cdp.page_list", (), {}), [])
        command = screen_call.command("core.open_target", ["/w/a.html"])
        self.assertEqual(command[-3:], [screen_call.CALL_COMMAND, "core.open_target", "/w/a.html"])

    def test_a_replay_hands_over_the_files_its_steps_open_and_upload(self):
        program = {"steps": [
            {"n": 1, "kind": "open", "open": "file:///home/headless/work/old.html"},
            {"n": 2, "kind": "open", "open": "file:///home/headless/work/form.html"},
            {"n": 3, "kind": "type", "param": "name", "target": {"name": "Name"}},
            {"n": 4, "kind": "upload", "files": ["/home/headless/work/recorded.pdf"]},
            {"n": 5, "kind": "upload", "param": "scan", "files": ["/home/headless/work/demo.pdf"]},
            {"n": 6, "kind": "open", "open": "file:///home/headless/work/form.html"},
        ]}
        self.assertEqual(screen_call.program_files(program, {"scan": "/w/a.pdf; /w/b.pdf", "name": "x"}, start=2),
                         ["file:///home/headless/work/form.html", "/home/headless/work/recorded.pdf",
                          "/w/a.pdf", "/w/b.pdf"])
        self.assertEqual(screen_call.program_files(program, {"scan": ["/w/c.pdf"]}, start=5),
                         ["/w/c.pdf", "file:///home/headless/work/form.html"])
        self.assertEqual(screen_call.program_files({"steps": "nonsense"}), [])
        self.assertEqual(screen_call.program_files(None), [])

    def test_a_replay_through_the_broker_names_those_files_to_the_client(self):
        program = {"title": "t", "steps": [{"n": 1, "kind": "open", "open": "file:///w/form.html"}]}
        with mock.patch.object(screen_call, "exchange", return_value={"result": {"outcome": "verified"}}) as sent:
            screen_call.run_replay(program, params={}, start=1)
        self.assertEqual(sent.call_args.kwargs["files"], ["file:///w/form.html"])


# ------------------------------------------------------------
# The commands the worker runs itself
# ------------------------------------------------------------

class TheWorkersOwnStores(unittest.TestCase):
    def test_store_commands_run_as_the_worker_and_screen_commands_go_to_the_broker(self):
        env = {"DECURIX_SCREEN_USER": "1"}
        for command in ("skills", "recordings", "recording", "record", "records", "recall", "skill", "do", "replay",
                        "guide"):
            with self.subTest(command=command), mock.patch.dict(os.environ, env), \
                    mock.patch.object(gui_client, "in_process", side_effect=SystemExit(0)) as here, \
                    mock.patch.object(gui_client, "hand_over") as over:
                with self.assertRaises(SystemExit):
                    gui_client.main([command, "x"])
                here.assert_called_once_with([command, "x"])
                over.assert_not_called()
        for command in ("observe", "click", "type", "open", "upload", "ask", "verify", "key"):
            with self.subTest(command=command), mock.patch.dict(os.environ, env), \
                    mock.patch.object(gui_client, "in_process") as here, \
                    mock.patch.object(gui_client, "hand_over", return_value=("absent", None)), \
                    mock.patch.object(gui_client.os, "_exit", side_effect=SystemExit(1)), \
                    mock.patch.object(gui_client.os, "write"):
                with self.assertRaises(SystemExit):
                    gui_client.main([command, "x"])
                here.assert_not_called()

    def test_the_screen_calls_they_make_are_routed(self):
        self.assertIn("screen.grab", screen_call.SERVED)
        self.assertIn("core.forget_failed_search", screen_call.SERVED)
        from decurix_agent.computer import core, screen
        for fn, name in ((screen.grab, "screen.grab"), (core.forget_failed_search, "core.forget_failed_search")):
            with self.subTest(name=name), mock.patch.object(screen_call, "remote", return_value=True), \
                    mock.patch.object(screen_call, "call", return_value="R") as call:
                self.assertEqual(fn(), "R")
                self.assertEqual(call.call_args[0][0], name)

    def test_the_agent_waits_for_the_broker_and_gives_up(self):
        with mock.patch.object(gui_client, "broker_ready", side_effect=[False, False, True]):
            self.assertTrue(screen_call.wait_for_broker(seconds=5, pause=0))
        with mock.patch.object(gui_client, "broker_ready", return_value=False):
            self.assertFalse(screen_call.wait_for_broker(seconds=0, pause=0))

    def test_at_boot_recover_waits_for_the_broker(self):
        from decurix_agent import app
        from decurix_agent.computer import demonstration, recording
        with mock.patch.object(recording, "import_legacy_audit", return_value=0), \
                mock.patch.object(screen_call, "remote", return_value=True), \
                mock.patch.object(screen_call, "wait_for_broker", return_value=True) as wait, \
                mock.patch.object(demonstration, "recover") as recover:
            app.tidy_recordings()
        wait.assert_called_once()
        recover.assert_called_once()
        with mock.patch.object(recording, "import_legacy_audit", return_value=0), \
                mock.patch.object(screen_call, "remote", return_value=True), \
                mock.patch.object(screen_call, "wait_for_broker", return_value=False), \
                mock.patch.object(demonstration, "recover") as recover:
            app.tidy_recordings()
        recover.assert_not_called()


# ------------------------------------------------------------
# The worker's accessibility bus
# ------------------------------------------------------------

@unittest.skipUnless(POSIX, "needs Unix uids")
class TheWorkersAccessibilityBus(Tmp):
    def setUp(self):
        super().setUp()
        self.launch = self.path("launch")
        os.mkdir(self.launch, 0o711)
        self.env = mock.patch.dict(os.environ, {"DECURIX_LAUNCH_DIR": self.launch,
                                                "DECURIX_SCREEN_WORKER_UID": str(os.getuid())})
        self.env.start()
        self.addCleanup(self.env.stop)

    def publish(self, text):
        with open(os.path.join(self.launch, broker.A11Y_BUS_FILE), "w") as f:
            f.write(text)

    def test_a_unix_socket_address_is_taken(self):
        self.publish("unix:path=/tmp/decurix-launch-1001/at-spi/bus,guid=" + "a" * 32 + "\n")
        self.assertEqual(broker.worker_a11y_bus(), "unix:path=/tmp/decurix-launch-1001/at-spi/bus,guid=" + "a" * 32)

    def test_an_address_that_runs_a_program_is_not(self):
        for bad in ("unixexec:path=/bin/sh,argv1=-c,argv2=id", "tcp:host=127.0.0.1,port=1",
                    "unix:path=/tmp/x;unixexec:path=/bin/sh", "unix:path=/tmp/x,guid=zz"):
            self.publish(bad)
            self.assertIsNone(broker.worker_a11y_bus(), bad)

    def test_not_from_a_folder_others_may_write_or_a_link(self):
        self.publish("unix:path=/tmp/bus")
        os.chmod(self.launch, 0o777)
        self.assertIsNone(broker.worker_a11y_bus())
        os.chmod(self.launch, 0o711)
        target = self.path("elsewhere")
        with open(target, "w") as f:
            f.write("unix:path=/tmp/bus")
        os.unlink(os.path.join(self.launch, broker.A11Y_BUS_FILE))
        os.symlink(target, os.path.join(self.launch, broker.A11Y_BUS_FILE))
        self.assertIsNone(broker.worker_a11y_bus())

    def test_the_screen_side_reads_it_and_the_worker_side_does_not(self):
        from decurix_agent.computer import atspi
        self.publish("unix:path=/tmp/bus")
        with mock.patch.dict(os.environ, {"AT_SPI_BUS_ADDRESS": "unix:path=/elsewhere"}), \
                mock.patch.object(broker, "screen_side", return_value=True):
            atspi._broker_bus()
            self.assertEqual(os.environ["AT_SPI_BUS_ADDRESS"], "unix:path=/tmp/bus")
        with mock.patch.dict(os.environ, {"AT_SPI_BUS_ADDRESS": "unix:path=/own"}), \
                mock.patch.object(broker, "screen_side", return_value=False):
            atspi._broker_bus()
            self.assertEqual(os.environ["AT_SPI_BUS_ADDRESS"], "unix:path=/own")


class TheBrowserNeverGetsTheWorkersBus(unittest.TestCase):
    @unittest.skipUnless(POSIX, "the session's environment is found by uid")
    def test_the_screen_sides_browser_start_drops_it(self):
        from decurix_agent.computer import session
        with mock.patch.object(broker, "screen_side", return_value=True), \
                mock.patch.dict(os.environ, {"AT_SPI_BUS_ADDRESS": "unix:path=/tmp/worker-bus"}), \
                mock.patch.object(session.subprocess, "Popen") as popen:
            session.launch(["chromium-browser", "https://example.com"])
        self.assertNotIn("AT_SPI_BUS_ADDRESS", popen.call_args.kwargs["env"])

    def test_the_relay_drops_it_too(self):
        source = Path(AGENT, "decurix_agent", "computer", "cdp_relay.py").read_text(encoding="utf-8")
        self.assertIn('if k != "AT_SPI_BUS_ADDRESS"', source)


class AWidgetOfTheWorkersIsPressedOnlyOverItsWindow(unittest.TestCase):
    def test_refused_over_another_users_window_and_done_over_the_workers(self):
        from decurix_agent.computer import core, screen
        from decurix_agent.computer.errors import ComputerError
        widget = {"source": "atspi", "role": "button", "name": "Cancel", "box": [600, 380, 80, 40]}
        with mock.patch.object(broker, "screen_side", return_value=True), \
                mock.patch.object(broker, "worker_uid", return_value=1001):
            with mock.patch.object(screen, "owner_uid_at", return_value=998), \
                    mock.patch.object(core, "_focus_window") as focus:
                with self.assertRaises(ComputerError):
                    core._worker_point("click", widget, None)
                focus.assert_called_once()  # brought forward first, as an action on it would be
            with mock.patch.object(screen, "owner_uid_at", return_value=None):
                with self.assertRaises(ComputerError):
                    core._worker_point("click", widget, (640, 400))
            with mock.patch.object(screen, "owner_uid_at", return_value=1001):
                core._worker_point("click", widget, (640, 400))
            with mock.patch.object(screen, "owner_uid_at", side_effect=AssertionError("not asked")):
                core._worker_point("click", dict(widget, source="cdp"), None)

    def test_outside_broker_mode_nothing_is_asked(self):
        from decurix_agent.computer import core, screen
        with mock.patch.object(broker, "screen_side", return_value=False), \
                mock.patch.object(screen, "owner_uid_at", side_effect=AssertionError("not asked")):
            core._worker_point("click", {"source": "atspi", "box": [0, 0, 5, 5]}, None)


# ------------------------------------------------------------
# The scripts
# ------------------------------------------------------------

def _code(path):
    return "\n".join(line for line in Path(path).read_text(encoding="utf-8").splitlines()
                     if not line.lstrip().startswith("#"))


class TheSessionsAndTheLauncher(unittest.TestCase):
    def test_the_browser_launcher_run_as_the_worker_hands_its_pages_to_the_broker(self):
        launcher = _code(Path(AGENT, "chromium-browser"))
        guard = launcher.index('if [ "${DECURIX_SCREEN_USER:-}" = "1" ] && [ "$(/usr/bin/id -u)" = '
                               '"${DECURIX_SCREEN_WORKER_UID:-1001}" ]; then')
        block = launcher[guard:launcher.index("\nfi\n", guard)]
        self.assertIn('/usr/local/bin/gui open "$arg"', block)
        self.assertIn("/usr/local/bin/gui open browser", block)
        self.assertIn('exit "$status"', block)
        self.assertLess(guard, launcher.index("exec \"$chrome\""), "it never reaches the browser itself")

    def test_the_worker_session_has_no_xfdesktop_and_publishes_its_accessibility_bus(self):
        worker = _code(Path(AGENT, "decurix-worker-session.sh"))
        self.assertNotIn("xfdesktop", worker)
        self.assertIn("env -u DISPLAY -u XAUTHORITY XDG_RUNTIME_DIR=\"$LAUNCH_DIR\" dbus-launch", worker)
        self.assertIn("org.a11y.Bus.GetAddress", worker)
        self.assertIn('"$LAUNCH_DIR/a11y-bus"', worker)
        self.assertLess(worker.index("org.a11y.Bus.GetAddress"), worker.index("dbus-update-activation-environment"))

    def test_the_screen_session_draws_the_wallpaper_and_keeps_the_callers_task_in_its_own_folder(self):
        screen = _code(Path(AGENT, "decurix-screen-session.sh"))
        self.assertIn("decurix_agent.computer.wallpaper /opt/decurix-wallpaper.svg", screen)
        self.assertIn('DECURIX_TASK_FILE="$SHARED/caller/task"', screen)
        self.assertIn('chmod 700 "$SHARED/caller"', screen)

    @unittest.skipUnless(Path(AGENT).parent.joinpath("docker", "desktop.basic.Dockerfile").is_file(),
                         "the repository's docker/ folder is not beside this agent/ copy")
    def test_the_workers_accessibility_bus_admits_the_screen_user(self):
        dockerfile = Path(AGENT).parent.joinpath("docker", "desktop.basic.Dockerfile").read_text(encoding="utf-8")
        self.assertIn('<allow user="decurixscreen"/>', dockerfile)
        self.assertIn("/usr/share/defaults/at-spi2/accessibility.conf", dockerfile)


if __name__ == "__main__":
    unittest.main()
