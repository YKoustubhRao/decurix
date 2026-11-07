"""
Browser actions wait for what the browser says, not for fixed times (docs/computer-use.md "Waits"):

- key events go to the page in batches (cdp.Page.call_all), in the same order, and a field that ignores keys still
  gets the text inserted;
- a typed field's read-back (did it keep the text?) is made after the action's settle wait instead of after a fixed
  400 ms inside the typing, and a field that threw the text away is still typed again with real keys;
- a click in the browser hears whether the page began a download (cdp.DownloadWatch): none heard ends without the
  quiet second, one heard is waited for until the browser says it ended, and whenever the browser cannot say (no
  watch, a window opened, a request still on its way, another tab) the download folder's old rules decide.
"""

import json
import socket
import tempfile
import threading
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

from decurix_agent.computer import cdp, core


# ------------------------------------------------------------ key events in batches

class FakeWS:
    """A page's socket: answers each command it is sent, in order, with an event between them now and then."""

    def __init__(self, fail_id=None):
        self.sent, self.queue, self.fail_id = [], [], fail_id
        self.sock = SimpleNamespace(settimeout=lambda t: None)

    def send(self, text):
        message = json.loads(text)
        self.sent.append(message)
        self.queue.append({"method": "Page.lifecycleEvent", "params": {}})
        if message["id"] == self.fail_id:
            self.queue.append({"id": message["id"], "error": {"message": "no such node"}})
        else:
            self.queue.append({"id": message["id"], "result": {"n": message["id"]}})

    def receive(self):
        return json.dumps(self.queue.pop(0))


def bare_page(ws):
    page = cdp.Page.__new__(cdp.Page)
    page.target, page.ws, page.seq, page.events = {"url": "http://x/"}, ws, 0, []
    return page


class CallAll(unittest.TestCase):

    def test_every_call_is_sent_before_an_answer_is_read_and_answers_come_back_in_order(self):
        ws = FakeWS()
        page = bare_page(ws)
        answers = page.call_all([("Input.dispatchKeyEvent", {"type": "keyDown", "key": c}) for c in "abc"])
        self.assertEqual([m["params"]["key"] for m in ws.sent], ["a", "b", "c"])
        self.assertEqual(answers, [{"n": 1}, {"n": 2}, {"n": 3}])
        self.assertEqual(len(page.events), 3)  # events met on the way are kept, as call() keeps them

    def test_an_error_is_raised_once_every_answer_is_in(self):
        ws = FakeWS(fail_id=2)
        page = bare_page(ws)
        with self.assertRaises(cdp.ComputerError) as ctx:
            page.call_all([("Input.dispatchKeyEvent", {}), ("Input.dispatchKeyEvent", {}), ("Input.insertText", {})])
        self.assertIn("no such node", str(ctx.exception))
        self.assertEqual(ws.queue, [])           # nothing left unread: the session stays in step
        self.assertEqual(page.call("Runtime.evaluate"), {"n": 4})


class KeysInBatches(unittest.TestCase):

    class KeyPage(cdp.Page):
        def __init__(self, takes_keys, url="http://x/form"):
            self.target, self.value, self.takes_keys, self.calls, self.batches = {"url": url}, "", takes_keys, [], []

        def _do(self, method, params):
            self.calls.append(method)
            if method == "Input.dispatchKeyEvent" and params.get("type") == "keyDown" and self.takes_keys:
                self.value += params["text"]
            elif method == "Input.insertText":
                self.value += params["text"]
            elif method == "Runtime.callFunctionOn":
                return {"result": {"value": self.value}}
            return {}

        def call(self, method, params=None, timeout=None):
            return self._do(method, params or {})

        def call_all(self, calls, timeout=None):
            self.batches.append(len(calls))
            return [self._do(m, p) for m, p in calls]

    def test_a_line_goes_as_one_batch_of_key_events_in_order(self):
        page = self.KeyPage(takes_keys=True)
        cdp._type_line(page, "obj", "04-10-2026")
        self.assertEqual(page.value, "04-10-2026")
        self.assertEqual(page.batches, [20])
        self.assertNotIn("Input.insertText", page.calls)

    def test_a_long_line_goes_in_several_batches(self):
        page = self.KeyPage(takes_keys=True)
        cdp._type_line(page, "obj", "x" * 100)
        self.assertEqual(page.value, "x" * 100)
        self.assertEqual(sum(page.batches), 200)
        self.assertTrue(all(n <= cdp.KEY_BATCH for n in page.batches))

    def test_a_field_that_ignores_keys_still_gets_the_text(self):
        page = self.KeyPage(takes_keys=False)
        cdp._type_line(page, "obj", "04-10-2026")
        self.assertEqual(page.value, "04-10-2026")
        self.assertIn("Input.insertText", page.calls)

    def test_a_viewer_of_an_app_elsewhere_gets_its_keys_one_at_a_time_and_nothing_inserted(self):
        page = self.KeyPage(takes_keys=False, url="http://cp/host-gui/view?x")
        cdp._type_line(page, "obj", "D20")
        self.assertEqual(page.batches, [])
        self.assertEqual(page.calls.count("Input.dispatchKeyEvent"), 6)
        self.assertNotIn("Input.insertText", page.calls)


# ------------------------------------------------------------ the read-back, made after the settle wait

class FieldPage:
    """A tab with one text field: keys go into it; `keeps` False empties it once read back later."""

    def __init__(self, field):
        self.field, self.calls = field, []

    def call(self, method, params=None, timeout=None):
        params = params or {}
        self.calls.append((method, params))
        if method == "Input.dispatchKeyEvent" and params.get("type") == "keyDown":
            self.field["value"] += params["text"]
        elif method == "Runtime.callFunctionOn":
            declaration = params.get("functionDeclaration", "")
            if "multiline" in declaration:
                return {"result": {"value": {"multiline": False}}}
            return {"result": {"value": self.field["value"]}}
        return {}

    def close(self):
        pass


class ReadBackLater(unittest.TestCase):

    def type(self, check_later):
        field = {"value": ""}
        pages = []

        def page_for(element):
            pages.append(FieldPage(field))
            return pages[-1]

        with mock.patch.object(cdp, "_page_for", side_effect=page_for), \
                mock.patch.object(cdp, "_object", return_value="obj"):
            typed = cdp.type_text({"handle": {"backend_id": 7}}, "Acme", check_later=check_later)
        return typed, field, pages, page_for

    @staticmethod
    def waited(page):
        return [p for m, p in page.calls if m == "Runtime.evaluate" and "setTimeout(r, 500)" in p.get("expression", "")]

    def test_without_it_the_field_is_read_back_after_its_wait_as_before(self):
        typed, field, pages, _ = self.type(check_later=False)
        self.assertIs(typed, True)
        self.assertTrue(self.waited(pages[0]))

    def test_with_it_typing_returns_at_once_and_the_check_reads_the_field_later(self):
        typed, field, pages, page_for = self.type(check_later=True)
        self.assertTrue(callable(typed))
        self.assertFalse(self.waited(pages[0]))            # no 400 ms inside the typing
        with mock.patch.object(cdp, "_page_for", side_effect=page_for), \
                mock.patch.object(cdp, "_object", return_value="obj"), mock.patch.object(cdp, "KEPT_WAIT_MS", 0):
            self.assertTrue(typed())                       # it kept "Acme"
            field["value"] = ""                            # the page emptied it after the text arrived
            self.assertFalse(typed())
            field["value"] = "Acme Corp"
            self.assertTrue(typed())

    def test_the_check_never_reads_sooner_than_the_old_wait(self):
        typed, field, pages, page_for = self.type(check_later=True)
        with mock.patch.object(cdp, "_page_for", side_effect=page_for), \
                mock.patch.object(cdp, "_object", return_value="obj"), \
                mock.patch.object(cdp.time, "sleep") as slept:
            typed()
        self.assertTrue(slept.called)
        self.assertLessEqual(slept.call_args[0][0], cdp.KEPT_WAIT_MS / 1000)

    def test_a_tab_gone_by_then_is_left_to_the_look_after_the_action(self):
        typed, field, pages, _ = self.type(check_later=True)
        with mock.patch.object(cdp, "_page_for", side_effect=cdp.ComputerError("closed")), \
                mock.patch.object(cdp, "KEPT_WAIT_MS", 0):
            self.assertTrue(typed())


class TypingCheckedAfterTheSettle(unittest.TestCase):
    """act: the read-back is made once the screen has settled; a field that threw the text away is typed again with
    real keys and the screen settles again."""

    def act(self, kept):
        import config_stub  # noqa: F401
        from decurix_agent.computer import approval, audit
        field = {"role": "textbox", "name": "Customer", "source": "cdp", "ref": "e9", "path": "site x > textbox Customer",
                 "box": [700, 490, 400, 30], "states": [], "value": "", "handle": {"backend_id": 3, "target_id": "T"}}
        looks = [{"elements": [dict(field)], "frame": None, "windows": [], "groups": [], "size": [1280, 720]},
                 {"elements": [dict(field, value="Acme")], "frame": None, "windows": [], "groups": [], "size": [1280, 720]}]
        order = []

        def perform(*a, later=None, **k):
            order.append("perform")
            later.append(lambda: order.append("checked") or kept)
            return "cdp"

        with mock.patch.object(core, "observe", side_effect=lambda *a, **k: looks.pop(0) if len(looks) > 1 else looks[0]), \
                mock.patch.object(core, "_perform", side_effect=perform), \
                mock.patch.object(core, "_wait_for_settle", side_effect=lambda *a: order.append("settle")), \
                mock.patch.object(core, "_type_with_keys", side_effect=lambda *a: order.append("keys") or "xdotool") as keys, \
                mock.patch.object(audit, "record"), \
                mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            result = core.act("type", ref="e9", text="Acme")
        return result, order, keys

    def test_a_kept_field_is_checked_once_after_the_settle(self):
        result, order, keys = self.act(kept=True)
        self.assertEqual(order, ["perform", "settle", "checked"])
        keys.assert_not_called()
        self.assertEqual(result["status"], "done")

    def test_a_field_that_threw_the_text_away_is_typed_with_real_keys(self):
        result, order, keys = self.act(kept=False)
        self.assertEqual(order, ["perform", "settle", "checked", "keys", "settle"])
        self.assertEqual(result["status"], "done")


class PerformLeavesTheCheck(unittest.TestCase):

    element = {"role": "textbox", "name": "Customer", "source": "cdp", "handle": {"backend_id": 3, "target_id": "T"}}

    def test_a_list_gets_the_check_and_none_waits_as_before(self):
        check = lambda: True  # noqa: E731
        later = []
        with mock.patch.object(core.cdp, "type_text", return_value=check) as typed, \
                mock.patch.object(core, "_load_state", return_value={}):
            self.assertEqual(core._perform("type", self.element, None, "Acme", None, None, None, None, False,
                                           later=later), "cdp")
            self.assertEqual(later, [check])
            self.assertTrue(typed.call_args.kwargs["check_later"])
        with mock.patch.object(core.cdp, "type_text", return_value=True) as typed, \
                mock.patch.object(core, "_load_state", return_value={}):
            core._perform("type", self.element, None, "Acme", None, None, None, None, False)
            self.assertFalse(typed.call_args.kwargs["check_later"])

    def test_text_thrown_away_at_once_is_typed_with_real_keys(self):
        with mock.patch.object(core.cdp, "type_text", return_value=False), \
                mock.patch.object(core, "_load_state", return_value={}), \
                mock.patch.object(core, "_type_with_keys", return_value="xdotool") as keys:
            self.assertEqual(core._perform("type", self.element, None, "Acme", None, None, None, None, False,
                                           later=[]), "xdotool")
        keys.assert_called_once()


# ------------------------------------------------------------ the download watch

class LineWS:
    """The watch's socket: a real socket pair, one JSON message a line."""

    def __init__(self):
        self.sock, self.other = socket.socketpair()
        self.buffer = b""
        self.closed = False

    def receive(self):
        while b"\n" not in self.buffer:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise cdp.ComputerError("closed")
            self.buffer += chunk
        line, self.buffer = self.buffer.split(b"\n", 1)
        return line.decode()

    def close(self):
        self.closed = True
        self.sock.close()

    def say(self, method, **params):
        self.other.sendall((json.dumps({"method": method, "params": params}) + "\n").encode())


def watch_with(ws, early=()):
    page = cdp.Page.__new__(cdp.Page)
    page.target, page.ws, page.seq, page.events = {}, ws, 0, list(early)
    return cdp.DownloadWatch(page, ["T"])


def until(predicate, seconds=2):
    deadline = time.time() + seconds
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(0.01)
    return False


class TheWatchHears(unittest.TestCase):

    def setUp(self):
        self.ws = LineWS()
        self.addCleanup(self.ws.other.close)
        self.watch = watch_with(self.ws, early=[{"method": "Network.requestWillBeSent", "params": {"requestId": "r0"}}])
        self.addCleanup(self.watch.close)

    def test_a_download_from_begin_to_end(self):
        self.ws.say("Page.downloadWillBegin", guid="g1", suggestedFilename="report.txt")
        self.ws.say("Page.downloadProgress", guid="g1", state="inProgress")
        self.assertTrue(until(lambda: self.watch.status()["began"] == {"g1": "report.txt"}))
        self.assertEqual(self.watch.status()["ended"], {})
        self.ws.say("Page.downloadProgress", guid="g1", state="completed")
        self.assertTrue(until(lambda: self.watch.status()["ended"] == {"g1": "completed"}))

    def test_requests_and_loads_in_flight_are_busy_until_they_end(self):
        self.assertTrue(until(lambda: self.watch.status()["busy"]))  # the one met while it was being enabled
        self.ws.say("Page.frameStartedLoading", frameId="f")
        self.ws.say("Network.loadingFinished", requestId="r0")
        time.sleep(0.1)
        self.assertTrue(self.watch.status()["busy"])
        mark = time.time()
        self.ws.say("Page.frameStoppedLoading", frameId="f")
        self.assertTrue(until(lambda: not self.watch.status()["busy"]))
        self.assertGreaterEqual(self.watch.status()["quiet_since"], mark - 0.01)

    def test_a_window_opened_is_said(self):
        self.ws.say("Page.windowOpen", url="http://x/report")
        self.assertTrue(until(lambda: self.watch.status()["opened"]))

    def test_closing_it_ends_the_reader_and_the_session(self):
        self.watch.close()
        self.assertTrue(until(lambda: not self.watch.status()["alive"]))
        self.assertTrue(self.ws.closed)

    def test_a_session_the_browser_closed_is_no_longer_alive(self):
        self.ws.other.close()
        self.assertTrue(until(lambda: not self.watch.status()["alive"]))

    def test_alone_only_with_no_frame_of_another_site_and_no_new_tab(self):
        lists = {"same": [{"id": "T", "type": "page"}],
                 "frame": [{"id": "T", "type": "page"}, {"id": "F", "type": "iframe"}],
                 "tab": [{"id": "T", "type": "page"}, {"id": "N", "type": "page"}]}
        for case, expected in (("same", True), ("frame", False), ("tab", False)):
            with mock.patch.object(cdp, "_get", return_value=lists[case]):
                self.assertEqual(self.watch.alone(), expected, case)
        with mock.patch.object(cdp, "_get", side_effect=OSError("refused")):
            self.assertFalse(self.watch.alone())


class StartingAWatch(unittest.TestCase):

    def test_none_when_the_browser_cannot_say(self):
        with mock.patch.object(cdp, "_get", side_effect=OSError("refused")):
            self.assertIsNone(cdp.watch_downloads("T"))
        with mock.patch.object(cdp, "_get", return_value=[{"id": "T", "type": "page", "webSocketDebuggerUrl": "ws://x"},
                                                          {"id": "F", "type": "iframe"}]):
            self.assertIsNone(cdp.watch_downloads("T"))                  # a frame of another site
        two = [{"id": t, "type": "page", "webSocketDebuggerUrl": "ws://x"} for t in ("T", "U")]
        with mock.patch.object(cdp, "_get", return_value=two):
            self.assertIsNone(cdp.watch_downloads(None))                 # which tab, unknown
            self.assertIsNone(cdp.watch_downloads("gone"))

    def test_a_page_that_will_not_enable_its_domains_is_no_watch(self):
        class NoPage:
            closed = False

            def __init__(self, target):
                pass

            def call(self, method, params=None, timeout=None):
                raise cdp.ComputerError("Network.enable: not allowed")

            def close(self):
                NoPage.closed = True

        with mock.patch.object(cdp, "_get", return_value=[{"id": "T", "type": "page", "webSocketDebuggerUrl": "ws://x"}]), \
                mock.patch.object(cdp, "Page", NoPage):
            self.assertIsNone(cdp.watch_downloads("T"))
        self.assertTrue(NoPage.closed)


# ------------------------------------------------------------ the note, with and without the watch

class FakeWatch:
    def __init__(self, alone=True, **status):
        self.state = {"alive": True, "opened": False, "began": {}, "ended": {}, "busy": False,
                      "quiet_since": time.time() - 5}
        self.state.update(status)
        self._alone, self.closed, self.asked = alone, False, 0

    def status(self):
        return dict(self.state)

    def alone(self):
        self.asked += 1
        return self._alone

    def close(self):
        self.closed = True


class TheNoteWithTheBrowserHeard(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        patcher = mock.patch.object(core, "_downloads_dir", return_value=self.dir)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.before = core._downloads_snapshot()

    def note(self, watch, acted_ago=0.6, since=False, **kw):
        acted = time.time() - acted_ago
        started = time.time()
        note = core._downloads_note(self.before, since=acted if since else None, watch=watch, acted=acted, **kw)
        return note, time.time() - started

    def test_nothing_heard_and_nothing_in_flight_ends_without_the_quiet_second(self):
        watch = FakeWatch()
        note, took = self.note(watch)                     # no `since`: the click that showed no result at all
        self.assertEqual(note, "")
        self.assertLess(took, 0.3)
        self.assertEqual(watch.asked, 1)
        self.assertTrue(watch.closed)

    def test_it_never_ends_sooner_than_the_signal_quiet_after_the_action(self):
        note, took = self.note(FakeWatch(), acted_ago=0.0)
        self.assertEqual(note, "")
        self.assertGreaterEqual(took, core.DOWNLOAD_SIGNAL_QUIET - 0.02)
        self.assertLess(took, 0.8)

    def test_a_request_still_on_its_way_keeps_the_old_quiet_second(self):
        note, took = self.note(FakeWatch(busy=True))
        self.assertEqual(note, "")
        self.assertGreaterEqual(took, core.DOWNLOAD_QUIET_SECONDS - 0.05)

    def test_a_window_opened_or_another_tab_leaves_it_to_the_folder(self):
        for watch in (FakeWatch(opened=True), FakeWatch(alive=False), FakeWatch(alone=False)):
            note, took = self.note(watch)
            self.assertEqual(note, "")
            self.assertGreaterEqual(took, core.DOWNLOAD_QUIET_SECONDS - 0.05, watch.state)
            self.assertTrue(watch.closed)

    def test_a_download_heard_is_waited_for_until_it_ends_and_is_in_the_folder(self):
        watch = FakeWatch(began={"g": "statement.pdf"})
        partial = self.dir / "statement.pdf.crdownload"
        partial.write_bytes(b"%PDF")

        def finish():
            time.sleep(1.3)                               # past the quiet second: the browser said it is not done
            partial.rename(self.dir / "statement.pdf")
            watch.state["ended"] = {"g": "completed"}

        threading.Thread(target=finish, daemon=True).start()
        note, took = self.note(watch, since=True)
        self.assertIn("Downloaded:", note)
        self.assertIn("statement.pdf (4 bytes)", note)
        self.assertGreaterEqual(took, 1.2)

    def test_a_download_heard_whose_file_is_not_yet_named_is_waited_for(self):
        watch = FakeWatch(began={"g": "a.txt"}, ended={"g": "completed"})
        (self.dir / "a.txt.crdownload").write_bytes(b"x")

        def rename():
            time.sleep(0.2)
            (self.dir / "a.txt.crdownload").rename(self.dir / "a.txt")

        threading.Thread(target=rename, daemon=True).start()
        note, _ = self.note(watch, since=True)
        self.assertIn("a.txt (1 bytes)", note)

    def test_the_folder_still_names_what_arrived_and_the_duplicate(self):
        (self.dir / "report.txt").write_bytes(b"same")
        self.before = core._downloads_snapshot()
        (self.dir / "report (1).txt").write_bytes(b"same")
        watch = FakeWatch(began={"g": "report.txt"}, ended={"g": "completed"})
        note, took = self.note(watch, since=True)
        self.assertIn("report (1).txt", note)
        self.assertIn("byte for byte the same file", note)
        self.assertLess(took, 0.3)

    def test_a_download_heard_that_never_arrives_is_said(self):
        watch = FakeWatch(began={"g": "bill.pdf"}, ended={"g": "canceled"})
        note, took = self.note(watch, since=True)
        self.assertIn("began downloading bill.pdf", note)
        self.assertIn("canceled", note)
        note, _ = self.note(FakeWatch(began={"g": "bill.pdf"}), since=True, wait=0.2)
        self.assertIn("began downloading bill.pdf", note)
        self.assertIn("(yet)", note)

    def test_a_download_the_browser_holds_is_still_said_with_the_watch(self):
        (self.dir / "Unconfirmed 524817.crdownload").write_bytes(b"x")
        note, _ = self.note(FakeWatch(began={"g": "x"}), since=True, wait=0.2)
        self.assertIn("holding this download until it is confirmed", note)

    def test_without_a_watch_the_rules_are_the_folders_as_before(self):
        note, took = self.note(None)
        self.assertEqual(note, "")
        self.assertGreaterEqual(took, core.DOWNLOAD_QUIET_SECONDS - 0.05)


class TheActionStartsAndEndsTheWatch(unittest.TestCase):

    def setUp(self):
        import config_stub  # noqa: F401
        self.dir = Path(tempfile.mkdtemp())

    def act(self, perform, watch):
        from decurix_agent.computer import approval, audit
        link = {"role": "link", "name": "Download statement", "source": "cdp", "ref": "e6",
                "path": "site x > link Download statement", "box": [10, 10, 40, 20], "states": [], "group": "g",
                "window_title": "Statements - Chromium", "handle": {"backend": 1, "target_id": "T"}}
        look = {"elements": [link], "frame": None, "size": [1280, 720], "active_title": link["window_title"],
                "windows": [{"title": link["window_title"], "box": (0, 0, 800, 600), "active": True}], "page_url": "",
                "groups": [{"id": "g", "source": "cdp", "window_title": link["window_title"]}]}
        with mock.patch.object(core, "observe", side_effect=lambda *a, **k: look), \
                mock.patch.object(core, "_perform", side_effect=perform), \
                mock.patch.object(core, "_wait_for_settle"), mock.patch.object(audit, "record"), \
                mock.patch.object(core, "_downloads_dir", return_value=self.dir), \
                mock.patch.object(core.cdp, "submit_form_fields", return_value=None), \
                mock.patch.object(core.cdp, "watch_downloads", return_value=watch) as started, \
                mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False)):
            return core.act("click", ref="e6"), started

    def test_the_watch_is_on_the_elements_tab_and_closed_with_the_note(self):
        watch = FakeWatch()
        started_at = time.time()
        done, started = self.act(lambda *a, **k: "cdp", watch)
        started.assert_called_once_with("T")
        self.assertTrue(watch.closed)
        self.assertNotIn("Downloaded", done["text"])
        self.assertLess(time.time() - started_at, 0.9)

    def test_a_failed_action_closes_it(self):
        watch = FakeWatch()

        def perform(*a, **k):
            raise core.ComputerError("that element has no size on the page")

        with self.assertRaises(core.ComputerError):
            self.act(perform, watch)
        self.assertTrue(watch.closed)


# ------------------------------------------------------------ a download that arrives after the action's note

class LateDownloadsAreSaidOnce(unittest.TestCase):
    """A download a page began after an action's note ended (a timer, past DOWNLOAD_SIGNAL_QUIET) is said by the next
    action or gui command, once (core._unreported_downloads)."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        state = Path(tempfile.mkdtemp())
        for patcher in (mock.patch.object(core, "_downloads_dir", return_value=self.dir),
                        mock.patch.object(core, "STATE_DIR", state),
                        mock.patch.object(core, "DOWNLOADS_SEEN_FILE", state / "downloads-seen.json")):
            patcher.start()
            self.addCleanup(patcher.stop)
        (self.dir / "old.pdf").write_bytes(b"%PDF-old")
        self.assertEqual(core.downloads_since_last(), "")   # the first time, the folder is the record

    def test_a_late_file_is_said_by_the_next_command_and_never_again(self):
        (self.dir / "late.txt").write_bytes(b"quarterly")
        said = core.downloads_since_last()
        self.assertTrue(said.startswith("Downloaded since the last action: "))
        self.assertIn("late.txt (9 bytes)", said)
        self.assertNotIn("old.pdf", said)
        self.assertEqual(core.downloads_since_last(), "")

    def test_the_same_file_again_is_called_that(self):
        (self.dir / "old (1).pdf").write_bytes(b"%PDF-old")
        said = core.downloads_since_last()
        self.assertIn("old (1).pdf", said)
        self.assertIn("byte for byte the same file", said)

    def test_a_download_still_being_written_is_said_once_it_has_arrived(self):
        partial = self.dir / "big.zip.crdownload"
        partial.write_bytes(b"x")
        self.assertEqual(core.downloads_since_last(), "")
        partial.rename(self.dir / "big.zip")
        self.assertIn("big.zip (1 bytes)", core.downloads_since_last())
        self.assertEqual(core.downloads_since_last(), "")

    def test_the_action_still_waits_for_its_own_partial_download(self):
        before = core._downloads_snapshot()
        partial = self.dir / "statement.pdf.crdownload"
        partial.write_bytes(b"%PDF")
        sleeps = []

        def sleep(seconds):
            sleeps.append(seconds)
            if len(sleeps) == 3:
                partial.rename(self.dir / "statement.pdf")

        with mock.patch.object(core.time, "sleep", side_effect=sleep):
            note = core._downloads_note(before, since=core.time.time() - 5, remember=True)
        self.assertIn("Downloaded: ", note)
        self.assertIn("statement.pdf", note)
        self.assertNotIn("since the last action", note)
        self.assertEqual(core.downloads_since_last(), "")    # said by the action: not again

    def test_an_actions_note_says_what_arrived_before_it_and_after_the_last_report(self):
        (self.dir / "early.txt").write_bytes(b"e")             # arrived while the command looked, before the action
        before = core._downloads_snapshot()
        (self.dir / "mine.txt").write_bytes(b"m")
        note = core._downloads_note(before, wait=0, remember=True)
        self.assertIn("Downloaded: ", note)
        self.assertIn("mine.txt", note.split("since the last action")[0])
        self.assertIn("Downloaded since the last action: ", note)
        self.assertIn("early.txt", note.split("since the last action")[1])
        self.assertEqual(core.downloads_since_last(), "")
        (self.dir / "later.txt").write_bytes(b"l")              # began by a timer after the note had ended
        self.assertIn("later.txt", core.downloads_since_last())

    def test_a_note_without_remember_keeps_no_record(self):
        before = core._downloads_snapshot()
        (self.dir / "x.txt").write_bytes(b"x")
        self.assertIn("x.txt", core._downloads_note(before, wait=0))
        self.assertIn("x.txt", core.downloads_since_last())      # not recorded by that note: said here

    def test_no_name_is_written_in_the_record(self):
        (self.dir / "Salary slip Jane.pdf").write_bytes(b"%PDF")
        core.downloads_since_last()
        self.assertNotIn("Salary", core.DOWNLOADS_SEEN_FILE.read_text(encoding="utf-8"))


class EveryCommandBeginsWithIt(unittest.TestCase):

    def test_the_late_download_is_the_first_line_and_a_failure_never_stops_the_command(self):
        import contextlib
        import io
        from decurix_agent.computer import cli
        out = io.StringIO()
        with mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "clicked"}),                 mock.patch.object(cli.core, "downloads_since_last",
                                  return_value="Downloaded since the last action: /d/late.txt (9 bytes)."),                 contextlib.redirect_stdout(out):
            cli.run(["click", "330,428"])
        self.assertTrue(out.getvalue().startswith("Downloaded since the last action: /d/late.txt"))
        self.assertIn("clicked", out.getvalue())
        out = io.StringIO()
        with mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "clicked"}) as act,                 mock.patch.object(cli.core, "downloads_since_last", side_effect=OSError("no folder")),                 contextlib.redirect_stdout(out):
            cli.run(["click", "330,428"])
        act.assert_called_once()


if __name__ == "__main__":
    unittest.main()
