"""
A task that ends in a file is checked against the file: what the worker wrote
is read back and judged beside its report, before anyone is told it is done.
"""

import os
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

import test_skills as ts
from test_apprentice import verdict

from decurix_agent import guidance, produced, skill_task, state, verify


def write(path, text="x", age=0):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    when = time.time() - age
    os.utime(path, (when, when))
    return str(path)


class WhatATaskWrote(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.started = time.time() - 60

    def files(self, report=""):
        return produced.files(report, started=self.started, root=self.root)

    def test_only_what_was_written_after_the_task_began(self):
        new = write(self.root / "summary.csv")
        write(self.root / "last_week.csv", age=3600)
        self.assertEqual(self.files(), [new])

    def test_what_the_person_attached_is_not_the_tasks_own_work(self):
        write(self.root / "inbox" / "up-1" / "register.csv", age=90)
        self.assertEqual(self.files(), [])

    def test_a_file_the_task_changed_in_place_counts_wherever_it_is(self):
        changed = write(self.root / "inbox" / "up-1" / "register.csv")
        self.assertEqual(self.files(), [changed])

    def test_tools_own_folders_hidden_files_and_unreadable_kinds_are_left_out(self):
        write(self.root / "node_modules" / "pkg" / "readme.md")
        write(self.root / ".cache" / "notes.txt")
        write(self.root / "photo.bin")
        self.assertEqual(self.files(), [])

    def test_a_file_the_report_names_outside_the_work_folder(self):
        with tempfile.TemporaryDirectory() as elsewhere:
            named = write(Path(elsewhere) / "out.txt")
            self.assertEqual(self.files(f"Saved it as {named}."), [named])

    def test_newest_first_and_no_more_than_a_few(self):
        for n in range(produced.MAX_FILES + 3):
            write(self.root / f"part{n}.txt", age=n)
        got = self.files()
        self.assertEqual(len(got), produced.MAX_FILES)
        self.assertTrue(got[0].endswith("part0.txt"))

    def test_a_deliverable_is_read_before_working_files_written_after_it(self):
        deck = write(self.root / "mbr.pptx", age=30)
        book = write(self.root / "model.xlsx", age=20)
        for n in range(produced.MAX_FILES + 2):
            write(self.root / f"spec{n}.json", age=n)
        got = self.files()
        self.assertIn(deck, got)
        self.assertIn(book, got)
        self.assertEqual(len(got), produced.MAX_FILES)

    def test_with_no_known_start_nothing_is_guessed(self):
        write(self.root / "summary.csv")
        with mock.patch.object(produced, "since", lambda: None):
            self.assertEqual(produced.files("", root=self.root), [])

    def test_the_text_is_what_the_file_says_and_an_unreadable_one_says_so(self):
        good = write(self.root / "a.txt", "total 720")
        text = produced.text([good, str(self.root / "gone.txt")])
        self.assertIn("total 720", text)
        self.assertIn("could not be read", text)

    def test_a_long_file_is_cut_and_says_so(self):
        long = write(self.root / "long.txt", "9" * (produced.MAX_FILE_CHARS + 500))
        self.assertIn("500 more characters not shown", produced.text([long]))


class FileWorkIsChecked(ts.TeachingBase):
    """The final gate, for a task that touched no screen and wrote a file."""

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.verdicts, self.judged = [], []

        def judge(*args, **kwargs):
            self.judged.append(kwargs)
            return self.verdicts.pop(0)

        self.made = ["/work/result.xlsx"]
        for patch in (mock.patch.object(skill_task.verify, "available", lambda: True),
                      mock.patch.object(skill_task.verify, "judge", judge),
                      mock.patch.object(skill_task, "_did_screen_work", lambda: False),
                      mock.patch.object(skill_task.produced, "files", lambda report="", **kw: list(self.made)),
                      mock.patch.object(skill_task.produced, "text", lambda paths: "A7='only in the first list'"),
                      mock.patch.object(skill_task, "_engine_can_drive", lambda: False)):
            patch.start()
            self.addCleanup(patch.stop)
        state.current_task = "compare the two lists into result.xlsx"

    def test_the_file_is_what_the_check_is_shown_and_no_screen(self):
        self.verdicts = [verdict(verify.DONE, seen="result.xlsx lists 8 entries")]
        self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.judged[0]["produced"], "A7='only in the first list'")
        self.assertFalse(self.judged[0]["screen"])
        self.assertIsNone(self.judged[0]["live_png"])
        self.assertIn("Checked against the files I wrote: result.xlsx lists 8 entries", self.finals[-1][0])

    def test_a_report_its_own_file_contradicts_is_not_sent_as_done(self):
        self.verdicts = [verdict(verify.NOT_DONE, seen="the file lists REF-077 as only in the first list")]
        self.assertTrue(skill_task._gate("Matched REF-077 with REF-77.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("What I wrote shows: the file lists REF-077 as only in the first list", self.asked[-1][0])
        self.assertIn("Look at the files", self.asked[-1][0])

    def test_it_is_sent_back_once_when_the_engine_can_correct_it(self):
        self.verdicts = [verdict(verify.NOT_DONE, seen="the summary sheet has no difference row")]
        sent = []
        with mock.patch.object(skill_task, "_engine_can_drive", lambda: True), \
                mock.patch.object(skill_task, "_engine", lambda task, task_file: sent.append(guidance.get()["text"])):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("the summary sheet has no difference row", sent[0])
        self.assertIn("correct what is wrong where it is", sent[0])

    def test_a_wrong_sum_in_the_report_is_not_done_without_asking_a_model(self):
        self.verdicts = []
        self.assertTrue(skill_task._gate("Atlas total matches: 750.00 + 300.00 + 84.00 = 1143.00, so it is OK.",
                                         "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("it comes to 1,134.00", self.asked[-1][0])

    def test_a_number_written_as_a_program_prints_it_is_not_done_without_asking_a_model(self):
        self.verdicts = []
        with mock.patch.object(skill_task.produced, "text", lambda paths: "Hours: 3E+1  Net: 1,672.74"):
            self.assertTrue(skill_task._gate("Payslips made.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("3E+1", self.asked[-1][0])

    def test_a_report_that_says_it_could_not_be_done_goes_to_the_person(self):
        self.verdicts = [verdict(verify.CANNOT, seen="the workbook has no November targets")]
        self.assertTrue(skill_task._gate("The workbook has no November targets, so the deck cannot be completed.",
                                         "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("I could not finish this", self.asked[-1][0])
        self.assertIn("no November targets", self.asked[-1][0])

    def test_what_the_check_cannot_settle_goes_out_as_reported_not_as_a_question(self):
        self.verdicts = [verdict(verify.UNSURE)]
        self.assertFalse(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.asked, [])
        self.assertIsNone(guidance.get())

    def test_a_task_that_wrote_nothing_is_not_checked(self):
        self.made = []
        self.assertFalse(skill_task._gate("The kernel is 6.6.", "final", "thebot"))
        self.assertEqual(self.judged, [])

    def test_a_tickets_or_a_replays_own_report_is_not_an_engines(self):
        for engine in ("human", "replay", "web", skill_task.SKILL_ENGINE, skill_task.LEARN_ENGINE):
            self.assertFalse(skill_task._gate("Recording saved.", "final", engine))
        self.assertEqual(self.judged, [])

    def test_a_person_s_recording_is_not_judged_even_though_it_is_all_screen_work(self):
        # Found live: a ticket ended with the form not yet saved was judged "not done" and handed to the engine,
        # which started typing into the person's form.
        with mock.patch.object(skill_task, "_did_screen_work", lambda: True), \
                mock.patch.object(skill_task, "_engine", lambda *a, **k: self.fail("handed to the engine")):
            self.assertFalse(skill_task._gate("Recorded 20 steps of \"Enter this order\".", "final", "human"))
        self.assertEqual(self.judged, [])


class ANewRequestIsNotAnAnswer(ts.TeachingBase):
    """A message with files, arriving while a doubt about the last task is open, is not kept as what was wrong with it."""

    def test_it_closes_the_doubt_unconfirmed_and_remembers_nothing(self):
        from decurix_agent import attachments

        corrected = []
        state.paused_skill = None
        paused = {"mode": "confirm", "run": None, "claim": "Entered it.", "engine": "thebot", "task": "enter the document",
                  "verdict": verdict(verify.UNSURE, seen="the form is still open")}
        message = "Enter this one too\n\n" + attachments.MARK + "\n  /home/headless/work/inbox/up-1/document_2.pdf"
        with mock.patch.object(skill_task, "_correct", lambda run, text: corrected.append(text)), \
                mock.patch.object(skill_task, "_retry_engine", lambda *a, **k: corrected.append("retried")):
            skill_task._confirm_reply(paused, message)
        self.assertEqual(corrected, [])
        self.assertEqual(self.finals[-1][1], "error")
        self.assertIn("NOT confirmed", self.finals[-1][0])
        self.assertIn("Send the new request again", self.finals[-1][0])


if __name__ == "__main__":
    unittest.main()


class AYesThatSaysWhatIsMissing(ts.TeachingBase):
    """"Right, Rae's ticket still needs its reply" is what is wrong, not a bare yes (found live, 2026-10-14)."""

    def doubt(self, message):
        handed = []
        state.paused_skill = None
        paused = {"mode": "confirm", "run": None, "claim": "Triaged all six.", "engine": "thebot",
                  "task": "triage the inbox", "verdict": verdict(verify.NOT_DONE, seen="no reply to Rae")}
        with mock.patch.object(skill_task, "_engine_can_drive", lambda: True), \
                mock.patch.object(skill_task, "_retry_engine", lambda *a, **k: handed.append(a)):
            skill_task._confirm_reply(paused, message)
        return handed

    def test_a_long_reply_starting_with_right_goes_back_to_the_engine(self):
        self.assertTrue(self.doubt("Right, Rae's ticket still needs its reply - draft it as the playbook says."))

    def test_a_bare_right_still_asks_for_done(self):
        self.assertFalse(self.doubt("right"))
        self.assertIn("reply `done`", self.asked[-1][0])


class PicturesAreDescribed(unittest.TestCase):
    def test_an_exported_picture_is_read_back_as_its_format_and_size(self):
        # Found live (2026-10-15): JPEGs exported from GIMP were not read back, and the check could not confirm them.
        import tempfile
        from PIL import Image
        from decurix_agent import produced
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "lamp.jpg")
            Image.new("RGB", (800, 600)).save(path, "JPEG")
            said = produced.text([path])
        self.assertIn("JPEG image, 800 x 600 pixels", said)
