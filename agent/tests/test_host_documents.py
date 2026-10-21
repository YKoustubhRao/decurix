"""
Documents a planned run opened in apps of the person's computer (round 5 decision, 2026-10-20): a stage that is done
closes what it opened (Don't Save when the app asks, its file check having confirmed the saved file); a stage not
done leaves them open and they are listed; the run's end closes what the run opened without saving, listing anything
with unsaved changes; the person's own documents are never touched. Stand-ins only, never a browser.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first, and holds the LongRun harness

from decurix_agent import host_documents, long_task


class Apps:
    """Stand-in viewers: {target id: title}; a closed document's viewer disappears; `unsaved` ones ask first."""

    def __init__(self, viewers, unsaved=()):
        self.viewers, self.unsaved, self.front, self.pressed, self.asking = dict(viewers), set(unsaved), None, [], False

    def install(self, case):
        for name, fn in (("_viewers", lambda: dict(self.viewers)), ("_activate", self.activate),
                         ("_press_keys", self.keys), ("_buttons", self.buttons), ("_press", self.press)):
            patch = mock.patch.object(host_documents, name, fn)
            patch.start()
            case.addCleanup(patch.stop)
        patch = mock.patch.object(host_documents, "SETTLE_SECONDS", 0)
        patch.start()
        case.addCleanup(patch.stop)

    def activate(self, tid):
        self.front = tid
        return True

    def keys(self, keys):
        assert keys == "ctrl+w"
        if self.front in self.unsaved:
            self.asking = True
        else:
            self.viewers.pop(self.front, None)

    def buttons(self):
        return [("e1", "Save"), ("e2", "Don't Save"), ("e3", "Cancel")] if self.asking else []

    def press(self, ref):
        self.pressed.append(ref)
        if ref == "e2":
            self.viewers.pop(self.front, None)
        self.asking = False


class OnlyWhatTheRunOpened(unittest.TestCase):
    def test_new_viewers_and_new_documents_count_the_persons_do_not(self):
        before = {"v1": "Budget.xlsx - Excel", "v2": "Notes.docx - Word"}
        now = {"v1": "Budget.xlsx - Excel", "v2": "MIS_Oct2026.pptx - PowerPoint - Chromium",
               "v3": "Exceptions_memo_Oct2026.docx - Word"}
        self.assertEqual(sorted(host_documents.opened_since(before, now)), ["v2", "v3"])
        self.assertEqual(host_documents.document("MIS_Oct2026.pptx - PowerPoint - Chromium"), "MIS_Oct2026.pptx")

    def test_a_done_stage_closes_its_documents_and_answers_dont_save(self):
        apps = Apps({"mine": "Budget.xlsx - Excel", "deck": "MIS_Oct2026.pptx - PowerPoint",
                     "memo": "Exceptions_memo_Oct2026.docx - Word"}, unsaved={"deck"})
        apps.install(self)
        result = host_documents.close_opened({"mine": "Budget.xlsx - Excel"}, discard=True)
        self.assertEqual(sorted(result["closed"]), ["Exceptions_memo_Oct2026.docx", "MIS_Oct2026.pptx"])
        self.assertEqual(result["left_open"], [])
        self.assertEqual(apps.pressed, ["e2"])
        self.assertEqual(apps.viewers, {"mine": "Budget.xlsx - Excel"})  # the person's own stays

    def test_the_runs_end_never_discards_unsaved_changes(self):
        apps = Apps({"deck": "MIS_Oct2026.pptx - PowerPoint", "memo": "Memo.docx - Word"}, unsaved={"deck"})
        apps.install(self)
        result = host_documents.close_opened({}, discard=False)
        self.assertEqual(result["closed"], ["Memo.docx"])
        self.assertEqual(result["left_open"], ["MIS_Oct2026.pptx (it has changes that were not saved)"])
        self.assertEqual(apps.pressed, ["e3"])  # Cancel: it stays open
        self.assertIn("deck", apps.viewers)

    def test_still_open_names_the_document_and_its_app(self):
        apps = Apps({"deck": "MIS_Oct2026.pptx - PowerPoint"})
        apps.install(self)
        self.assertEqual(host_documents.still_open({}), ["MIS_Oct2026.pptx in PowerPoint"])


class InAPlannedRun(base.LongRunBase):
    def test_a_question_leaves_the_stage_documents_open_and_says_so_and_the_end_lists_unsaved_ones(self):
        apps = Apps({"mine": "Budget.xlsx - Excel"}, unsaved={"deck"})
        apps.install(self)
        engine_perform = self.engine.perform_task

        def perform(prompt, task_file):
            if prompt.startswith("Stage 1 of 3") and "deck" not in apps.viewers:
                apps.viewers["deck"] = "MIS_Oct2026.pptx - PowerPoint"  # the stage opened the deck in the app
                base.messaging.ask_for_query("Which month's figures go on slide 2?", kind="query")
                return
            engine_perform(prompt, task_file)
        self.engine.perform_task = perform
        base.sys.modules["decurix_agent.harness"].perform_task = perform
        long_task.perform(base.LONG, None)

        asked = [s["reply"] for s in self.sent if s["kind"] == "query"][-1]
        self.assertIn("Left open for you in the apps on your computer: MIS_Oct2026.pptx in PowerPoint", asked)
        self.assertIn("deck", apps.viewers)

        # Answered, the stage runs again and is done: what it opened (in its first run too) is closed, Don't Save.
        q = long_task.ledger(self.run_id())["questions"][-1]
        self.message_verdict = {"kind": "answer", "answers": [{"question": q["id"], "answer": "October"}]}
        self.message("October")
        self.assertNotIn("deck", apps.viewers)
        self.assertEqual(apps.pressed, ["e2"])
        self.assertTrue(any("Closed in the apps: MIS_Oct2026.pptx" in line for line in self.emitted))
        self.assertEqual(apps.viewers.get("mine"), "Budget.xlsx - Excel")  # the person's own, untouched
        self.assertNotIn("Still open in the apps", self.finals()[-1]["reply"])

    def test_the_runs_end_lists_what_it_could_not_close_unsaved(self):
        apps = Apps({}, unsaved={"memo"})
        apps.install(self)
        engine_perform = self.engine.perform_task

        def perform(prompt, task_file):
            if prompt.startswith("Stage 3 of 3"):
                apps.viewers["memo"] = "Memo.docx - Word"  # opened by the last stage, which then fails its check
            engine_perform(prompt, task_file)
        self.engine.perform_task = perform
        base.sys.modules["decurix_agent.harness"].perform_task = perform
        with mock.patch.object(long_task, "_tidy_host_documents") as tidy:  # the stage leaves it open (not done)
            long_task.perform(base.LONG, None)
        tidy.assert_called()
        report = self.finals()[-1]["reply"]
        self.assertIn("Still open in the apps on your computer (not closed, so nothing unsaved is lost): "
                      "Memo.docx (it has changes that were not saved)", report)


if __name__ == "__main__":
    unittest.main()
