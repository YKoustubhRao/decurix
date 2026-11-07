import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent.computer import core


# ============================================================
# A click that downloads says what it downloaded (2026-10-21):
# the result only said the link was focused, and a second click on the same
# attachment went unnoticed until its copy was saved under another name.
# ============================================================

class TestWhatAClickDownloaded(unittest.TestCase):

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        patcher = mock.patch.object(core, "_downloads_dir", return_value=self.dir)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_a_new_file_is_named_with_its_size(self):
        before = core._downloads_snapshot()
        (self.dir / "198_invoice.pdf").write_bytes(b"%PDF-198")
        note = core._downloads_note(before, wait=0)
        self.assertIn("Downloaded:", note)
        self.assertIn("198_invoice.pdf (8 bytes)", note)

    def test_the_same_attachment_downloaded_again_is_called_that(self):
        (self.dir / "197_invoice.pdf").write_bytes(b"%PDF-197")
        before = core._downloads_snapshot()
        (self.dir / "197_invoice (1).pdf").write_bytes(b"%PDF-197")
        note = core._downloads_note(before, wait=0)
        self.assertIn("197_invoice (1).pdf", note)
        self.assertIn("byte for byte the same file", note)

    def test_a_different_file_with_the_same_name_is_not_called_a_repeat(self):
        (self.dir / "report.pdf").write_bytes(b"%PDF-old")
        before = core._downloads_snapshot()
        (self.dir / "report (1).pdf").write_bytes(b"%PDF-new!")
        note = core._downloads_note(before, wait=0)
        self.assertIn("its content differs", note)

    def test_nothing_downloaded_says_nothing(self):
        (self.dir / "old.pdf").write_bytes(b"x")
        before = core._downloads_snapshot()
        self.assertEqual(core._downloads_note(before, wait=0), "")

    def test_a_download_still_running_is_reported_as_such(self):
        before = core._downloads_snapshot()
        (self.dir / "big.zip.crdownload").write_bytes(b"x")
        self.assertIn("Still downloading", core._downloads_note(before, wait=0))

    # Found live (2026-10-24): a download from a site without https sat as "Unconfirmed 123.crdownload"; the worker
    # fetched the address itself and saved a web page under the bill's name.
    def test_a_download_the_browser_holds_says_how_to_release_it(self):
        before = core._downloads_snapshot()
        (self.dir / "Unconfirmed 524817.crdownload").write_bytes(b"x")
        note = core._downloads_note(before, wait=0)
        self.assertIn("holding this download until it is confirmed", note)
        self.assertIn("chrome://downloads", note)
        self.assertIn("Download insecure file", note)

    def test_a_file_that_is_not_what_its_name_says_is_said_at_once(self):
        before = core._downloads_snapshot()
        (self.dir / "bill.pdf").write_bytes(b"<!DOCTYPE html><html>sign in</html>")
        note = core._downloads_note(before, wait=0)
        self.assertIn("bill.pdf", note)
        self.assertIn(" But ", note)

    def test_no_download_folder_is_no_note(self):
        with mock.patch.object(core, "_downloads_dir", return_value=self.dir / "missing"):
            self.assertEqual(core._downloads_note(core._downloads_snapshot(), wait=0), "")


class OnlyTheBrowserDownloads(unittest.TestCase):
    """A download can only come from the browser: an action anywhere else is not followed by a wait for one
    (2026-11-07: every click paid a second for "nothing new within a second")."""

    LOOK = {"windows": [{"title": "notes.txt - Mousepad", "box": (0, 0, 600, 400), "active": False},
                        {"title": "Invoices - Chromium", "box": (300, 0, 900, 700), "active": True}],
            "active_title": "Invoices - Chromium", "page_url": "https://site.example/inv"}

    def test_by_the_elements_own_source_and_application(self):
        self.assertTrue(core._may_download("click", {"source": "cdp"}, None, self.LOOK))
        self.assertTrue(core._may_download("click", {"source": "atspi", "app": "Chromium"}, None, self.LOOK))
        self.assertTrue(core._may_download("click", {"source": "atspi", "app": "x",
                                                     "window_title": "Save File - Google Chrome"}, None, self.LOOK))
        self.assertFalse(core._may_download("click", {"source": "atspi", "app": "mousepad",
                                                      "window_title": "notes.txt - Mousepad"}, None, self.LOOK))

    def test_a_click_at_a_point_by_the_window_on_top_there(self):
        self.assertTrue(core._may_download("click", None, (500, 100), self.LOOK))     # the browser is on top there
        self.assertFalse(core._may_download("click", None, (100, 100), self.LOOK))    # only the editor is there

    def test_keys_by_the_window_in_front(self):
        self.assertTrue(core._may_download("key", None, None, self.LOOK))
        self.assertFalse(core._may_download("key", None, None, dict(self.LOOK, active_title="notes.txt - Mousepad")))
        self.assertTrue(core._may_download("key", None, None, dict(self.LOOK, active_title="")))   # unknown: watched


class TheSecondStartsAtTheAction(unittest.TestCase):
    """The second in which a download must show runs from the action, while the screen settles and is looked at."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        patcher = mock.patch.object(core, "_downloads_dir", return_value=self.dir)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_nothing_downloaded_costs_no_second_once_one_has_passed(self):
        before = core._downloads_snapshot()
        with mock.patch.object(core.time, "sleep") as slept:
            self.assertEqual(core._downloads_note(before, since=core.time.time() - 1.1), "")
        slept.assert_not_called()

    def test_a_download_the_browser_began_is_still_waited_for(self):
        before = core._downloads_snapshot()
        partial = self.dir / "statement.pdf.crdownload"
        partial.write_bytes(b"%PDF")
        sleeps = []

        def sleep(seconds):
            sleeps.append(seconds)
            if len(sleeps) == 3:        # the slow server's file arrives a while later
                partial.rename(self.dir / "statement.pdf")

        with mock.patch.object(core.time, "sleep", side_effect=sleep):
            note = core._downloads_note(before, since=core.time.time() - 5)
        self.assertIn("Downloaded:", note)
        self.assertIn("statement.pdf", note)


def _act_patches(look, perform, downloads):
    from types import SimpleNamespace
    from decurix_agent.computer import approval, audit
    return [mock.patch.object(core, "observe", side_effect=lambda *a, **k: look),
            mock.patch.object(core, "_perform", side_effect=perform),
            mock.patch.object(core, "_wait_for_settle"),
            mock.patch.object(audit, "record"),
            mock.patch.object(core, "_downloads_dir", return_value=downloads),
            mock.patch.object(core.cdp, "submit_form_fields", return_value=None),
            mock.patch.object(approval, "check", return_value=SimpleNamespace(allowed=True, approved=False))]


class AnActionDownloadsOrNot(unittest.TestCase):
    """The whole action: a click outside the browser never looks for a download; one in the browser still says what
    it downloaded."""

    def setUp(self):
        import config_stub  # noqa: F401
        self.dir = Path(tempfile.mkdtemp())

    def act(self, element, perform):
        look = {"elements": [element], "frame": None, "windows": [{"title": element["window_title"],
                                                                   "box": (0, 0, 800, 600), "active": True}],
                "size": [1280, 720], "active_title": element["window_title"], "page_url": "",
                "groups": [{"id": "g", "source": element["source"], "window_title": element["window_title"]}]}
        patches = _act_patches(look, perform, self.dir)
        for p in patches:
            p.start()
        try:
            return core.act("click", ref=element["ref"])
        finally:
            for p in reversed(patches):
                p.stop()

    def test_a_click_in_a_desktop_app_does_not_wait_for_a_download(self):
        button = {"role": "push button", "name": "Bold", "source": "atspi", "app": "libreoffice", "ref": "e5",
                  "path": "app libreoffice > push button Bold", "box": [10, 10, 40, 20], "states": [], "group": "g",
                  "window_title": "report.odt - LibreOffice Writer"}
        with mock.patch.object(core, "_downloads_note") as note,                 mock.patch.object(core, "_downloads_snapshot") as snapshot:
            self.act(button, lambda *a, **k: "atspi")
        note.assert_not_called()
        snapshot.assert_not_called()

    def test_a_click_in_the_browser_that_downloads_says_so(self):
        link = {"role": "link", "name": "Download statement", "source": "cdp", "ref": "e6",
                "path": "site x > link Download statement", "box": [10, 10, 40, 20], "states": [], "group": "g",
                "window_title": "Statements - Chromium", "handle": {"backend": 1}}

        def perform(*a, **k):
            (self.dir / "statement.pdf").write_bytes(b"%PDF-1.4 statement")
            return "cdp"

        done = self.act(link, perform)
        self.assertIn("Downloaded:", done["text"])
        self.assertIn("statement.pdf", done["text"])


if __name__ == "__main__":
    unittest.main()
