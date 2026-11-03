"""
A task that ends in a file is checked against the file: what the worker wrote
is read back and judged beside its report, before anyone is told it is done.
"""

from needs import needs
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
                      mock.patch.object(skill_task.produced, "text", lambda paths, focus=(): "A7='only in the first list'"),
                      mock.patch.object(skill_task, "_engine_can_drive", lambda: False)):
            patch.start()
            self.addCleanup(patch.stop)
        state.current_task = "compare the two lists into result.xlsx"

    @needs("openpyxl")
    def test_the_file_is_what_the_check_is_shown_and_no_screen(self):
        self.verdicts = [verdict(verify.DONE, seen="result.xlsx lists 8 entries")]
        self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.judged[0]["produced"], "A7='only in the first list'")
        self.assertFalse(self.judged[0]["screen"])
        self.assertIsNone(self.judged[0]["live_png"])
        self.assertIn("Checked against the files I wrote: result.xlsx lists 8 entries", self.finals[-1][0])

    @needs("openpyxl")
    def test_a_report_its_own_file_contradicts_is_not_sent_as_done(self):
        self.verdicts = [verdict(verify.NOT_DONE, seen="the file lists REF-077 as only in the first list")]
        self.assertTrue(skill_task._gate("Matched REF-077 with REF-77.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("What I wrote shows: the file lists REF-077 as only in the first list", self.asked[-1][0])
        self.assertIn("Look at the files", self.asked[-1][0])

    @needs("openpyxl")
    def test_it_is_sent_back_once_when_the_engine_can_correct_it(self):
        self.verdicts = [verdict(verify.NOT_DONE, seen="the summary sheet has no difference row")]
        sent = []
        with mock.patch.object(skill_task, "_engine_can_drive", lambda: True), \
                mock.patch.object(skill_task, "_engine", lambda task, task_file: sent.append(guidance.get()["text"])):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("the summary sheet has no difference row", sent[0])
        self.assertIn("correct what is wrong where it is", sent[0])

    @needs("openpyxl")
    def test_a_wrong_sum_in_the_report_is_not_done_without_asking_a_model(self):
        self.verdicts = []
        self.assertTrue(skill_task._gate("Atlas total matches: 750.00 + 300.00 + 84.00 = 1143.00, so it is OK.",
                                         "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("it comes to 1,134.00", self.asked[-1][0])

    @needs("openpyxl")
    def test_a_number_written_as_a_program_prints_it_is_not_done_without_asking_a_model(self):
        self.verdicts = []
        with mock.patch.object(skill_task.produced, "text", lambda paths, focus=(): "Hours: 3E+1  Net: 1,672.74"):
            self.assertTrue(skill_task._gate("Payslips made.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("3E+1", self.asked[-1][0])

    @needs("openpyxl")
    def test_a_report_that_says_it_could_not_be_done_goes_to_the_person(self):
        self.verdicts = [verdict(verify.CANNOT, seen="the workbook has no November targets")]
        self.assertTrue(skill_task._gate("The workbook has no November targets, so the deck cannot be completed.",
                                         "final", "thebot"))
        self.assertEqual(self.finals, [])
        self.assertIn("I could not finish this", self.asked[-1][0])
        self.assertIn("no November targets", self.asked[-1][0])

    def test_what_the_check_cannot_settle_goes_out_said_not_checked_not_as_a_question(self):
        # Review 2026-11-02: it went out through the ordinary path and was recorded COMPLETED. It still goes out as
        # reported, not as a question - said first to be not checked, and recorded so (verify.outcome).
        self.verdicts = [verdict(verify.UNSURE)]
        said = []
        with mock.patch.object(skill_task, "write_final_reply", lambda text, **kw: said.append((text, kw))):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(self.asked, [])
        self.assertIsNone(guidance.get())
        text, kw = said[-1]
        self.assertTrue(text.startswith("Not checked - "), text)
        self.assertIn("Made result.xlsx.", text)
        self.assertIs(kw.get("checked"), False)
        self.assertTrue(kw.get("gated"))

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


class NoModelAtTheGate(ts.TeachingBase):
    """
    The final gate with NO hosted model (verify.available() False - every test above forces it True). Review
    2026-11-02: the gate returned before even the checks a program decides, and before the person's changed files
    were put back; the report went out unchecked. Code-decided checks always run; what needs a judgement nobody can
    make is "not checked", never passed.
    """

    def setUp(self):
        super().setUp()
        guidance.end()
        self.addCleanup(guidance.end)
        self.said = []
        for patch in (mock.patch.object(skill_task.verify, "available", lambda: False),
                      mock.patch.object(skill_task, "_did_screen_work", lambda: False),
                      mock.patch.object(skill_task.produced, "files", lambda report="", **kw: ["/work/result.xlsx"]),
                      mock.patch.object(skill_task.produced, "text", lambda paths, focus=(): "A7='Total 1,134.00'"),
                      mock.patch.object(skill_task, "_engine_can_drive", lambda: False),
                      mock.patch.object(skill_task, "write_final_reply",
                                        lambda text, **kw: self.said.append((text, kw)))):
            patch.start()
            self.addCleanup(patch.stop)
        state.current_task = "compare the two lists into result.xlsx"

    @needs("openpyxl")
    def test_a_wrong_sum_is_still_not_done(self):
        self.assertTrue(skill_task._gate("Atlas total: 750.00 + 300.00 + 84.00 = 1143.00, so it is OK.",
                                         "final", "thebot"))
        self.assertEqual(self.said, [])
        self.assertIn("it comes to 1,134.00", self.asked[-1][0])

    def test_the_persons_changed_file_is_still_put_back(self):
        restored = []

        def check_and_restore(task_id, request):
            restored.append(request)
            return "list_a.xlsx is the person's and was changed; it has been put back"
        with mock.patch.object(skill_task.source_guard, "check_and_restore", check_and_restore):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(restored, ["compare the two lists into result.xlsx"])
        self.assertIn("list_a.xlsx is the person's and was changed", self.asked[-1][0])

    @needs("openpyxl")
    def test_file_work_no_one_could_judge_is_said_not_checked_never_done(self):
        self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        text, kw = self.said[-1]
        self.assertTrue(text.startswith("Not checked - there is no model here to check the screen with"), text)
        self.assertIs(kw.get("checked"), False)
        self.assertTrue(kw.get("gated"))

    def test_a_blocking_check_that_raised_is_not_a_pass(self):
        # produced.py: a check that crashed was treated as passed (review 2026-11-02).
        with mock.patch.object(skill_task.verify, "available", lambda: True), \
                mock.patch.object(skill_task.verify, "judge", lambda *a, **k: self.fail("judged past a check not made")), \
                mock.patch("decurix_agent.payees.mismatches", side_effect=RuntimeError("bad account table")):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        text, kw = self.said[-1]
        self.assertIn("could not check where payments go (bad account table)", text)
        self.assertIs(kw.get("checked"), False)

    def test_a_refusal_is_never_reported_checked(self):
        guidance.begin(skill=None, task=state.current_task, text="", mode="engine", run=None)
        with mock.patch.object(skill_task.guidance, "refused", lambda: "no, do not send it"):
            self.assertTrue(skill_task._gate("Stopped before sending.", "final", "thebot"))
        text, kw = self.said[-1]
        self.assertTrue(text.startswith("Stopped there, as you said"))
        self.assertNotIn("checked", kw)
        self.assertTrue(kw.get("gated"))


class OneDecision(unittest.TestCase):
    """verify.outcome: the one place a check's result becomes what a report may say - failing closed."""

    def test_only_a_confident_done_is_done(self):
        self.assertEqual(verify.outcome({"verdict": "done", "confidence": 0.9}),
                         {"status": "done", "checked": True, "kind": "final", "why": ""})
        for unsure in ({"verdict": "done", "confidence": 0.5}, {"verdict": "unsure"}, {"verdict": "maybe"}, None, {}):
            decided = verify.outcome(unsure)
            self.assertEqual((decided["status"], decided["checked"]), (verify.UNCHECKED, False), unsure)

    def test_a_problem_or_a_refusal_by_the_judge_is_not_done_and_checked(self):
        self.assertEqual(verify.outcome({"verdict": "done", "confidence": 1.0}, problems=["a sum is wrong"])["status"],
                         verify.NOT_DONE)
        for kind in (verify.NOT_DONE, verify.CANNOT):
            decided = verify.outcome({"verdict": kind, "why": "no record saved"})
            self.assertEqual((decided["status"], decided["checked"], decided["kind"]), (verify.NOT_DONE, True, "error"))

    def test_an_error_or_a_check_not_made_is_never_done(self):
        sure = {"verdict": "done", "confidence": 1.0}
        self.assertEqual(verify.outcome(sure, error=RuntimeError("429"))["status"], verify.UNCHECKED)
        decided = verify.outcome(sure, not_made=["could not check where payments go"])
        self.assertEqual((decided["status"], decided["checked"]), (verify.UNCHECKED, False))
        self.assertIn("could not check where payments go", decided["why"])

    def test_nothing_left_is_the_whole_item_not_its_first_word(self):
        # Review 2026-11-02: "None of 3 bills saved" read as nothing left undone.
        self.assertEqual(verify.left_undone("Done.\nLeft undone: None of 3 bills saved"), "None of 3 bills saved")
        for nothing in ("None", "None.", "**None**", "Nothing left undone.", "N/A", "none - all done"):
            self.assertEqual(verify.left_undone(f"Done.\nLeft undone: {nothing}"), "", nothing)


class ReportNamedFilesAreReadBack(unittest.TestCase):
    def test_a_file_the_report_names_and_the_request_does_not_is_kept(self):
        # Review 2026-11-02: `named` was reused as the loop variable, and a report-named file was dropped.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            asked = write(root / "summary.xlsx")
            named = write(root / "elsewhere" / "notes.csv")
            other = write(root / "elsewhere" / "other.csv")
            with mock.patch.object(produced.shared, "walk", lambda: []):
                found = produced.files(f"Made summary.xlsx, {named} and {other}.", started=time.time() - 60,
                                       root=root / "w", request="Make summary.xlsx")
        self.assertIn(named, found)
        self.assertIn(other, found)
        self.assertNotIn(asked, found)  # (not in the work folder given, nor named by path)


try:
    import openpyxl
except ImportError:  # pragma: no cover
    openpyxl = None


def script_workbook(path):
    """A workbook as a script writes one: formulas with no results in the file (openpyxl stores none)."""

    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Orders"
    ws.append(["SKU", "Qty", "Price", "Total"])
    ws.append(["A-1", 2, 5, "=B2*C2"])
    ws["D3"] = "=SUM(D2:D2)"
    wb.save(path)
    return str(path)


def calculated_orders(path, _openpyxl):
    book = openpyxl.Workbook()
    book.active.title = "Orders"
    book["Orders"]["D2"], book["Orders"]["D3"] = 10, 10
    return book


@unittest.skipIf(openpyxl is None, "openpyxl is not installed")
class FormulaResultsAreStored(unittest.TestCase):
    """A workbook the task wrote never leaves it with formulas whose results are not in the file."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.emitted = []
        patch = mock.patch("decurix_agent.progress.emit", lambda text: self.emitted.append(text))
        patch.start()
        self.addCleanup(patch.stop)

    def test_a_script_written_workbook_gets_its_results_stored(self):
        from decurix_agent.tools import office

        book = script_workbook(self.root / "priced.xlsx")
        with mock.patch.object(office, "_recalculated_values", calculated_orders):
            self.assertEqual(produced.store_results([book]), [])
        self.assertEqual(openpyxl.load_workbook(book, data_only=True)["Orders"]["D3"].value, 10)
        self.assertEqual(openpyxl.load_workbook(book)["Orders"]["D3"].value, "=SUM(D2:D2)")
        self.assertIn("Stored the results of 2 formula(s) in priced.xlsx", self.emitted[0])

    def test_one_that_cannot_be_calculated_is_said_to_be_blank(self):
        from decurix_agent.tools import office

        book = script_workbook(self.root / "priced.xlsx")
        with mock.patch.object(office.shutil, "which", lambda name: None):
            left = produced.store_results([book])
        self.assertEqual(len(left), 1)
        self.assertIn("2 formula cell(s) with no stored result (Orders!D2, Orders!D3)", left[0])

    def test_other_files_and_complete_workbooks_are_left_alone(self):
        from decurix_agent.tools import office

        notes = write(self.root / "notes.txt", "=SUM(A1)")
        book = script_workbook(self.root / "priced.xlsx")
        with mock.patch.object(office, "_recalculated_values", calculated_orders):
            produced.store_results([book])
        stamp = os.stat(book).st_mtime_ns
        with mock.patch.object(office, "calculate_workbook", lambda p: self.fail("calculated again")):
            self.assertEqual(produced.store_results([notes, book, str(self.root / "gone.xlsx")]), [])
        self.assertEqual(os.stat(book).st_mtime_ns, stamp)


class FormulaResultsAtTheGate(FileWorkIsChecked):
    """The final gate, for a task whose workbook's formulas have no results in the file."""

    def test_a_workbook_with_formulas_but_no_results_is_not_done_without_asking_a_model(self):
        self.verdicts = []
        left = ["/work/priced.xlsx has 50 formula cell(s) with no stored result (Orders!D2, Orders!E2 and 48 more)"]
        with mock.patch.object(skill_task.produced, "store_results", lambda paths: list(left)):
            self.assertTrue(skill_task._gate("Completed and verified: priced.xlsx has the lookups.", "final", "thebot"))
        self.assertEqual(self.judged, [])
        self.assertEqual(self.finals, [])
        self.assertIn("50 formula cell(s) with no stored result", self.asked[-1][0])

    @needs("openpyxl")
    def test_the_results_are_stored_before_the_file_is_judged(self):
        stored = []
        self.verdicts = [verdict(verify.DONE, seen="priced.xlsx prices every line")]
        with mock.patch.object(skill_task.produced, "store_results",
                               lambda paths: stored.append(list(paths)) or []):
            self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
        self.assertEqual(stored, [["/work/result.xlsx"]])
        self.assertEqual(len(self.judged), 1)

    @unittest.skipIf(openpyxl is None, "openpyxl is not installed")
    def test_a_real_script_workbook_is_stored_and_then_judged(self):
        from decurix_agent.tools import office

        with tempfile.TemporaryDirectory() as tmp:
            book = script_workbook(Path(tmp) / "result.xlsx")
            self.made = [book]
            self.verdicts = [verdict(verify.DONE, seen="every line priced")]
            with mock.patch.object(office, "_recalculated_values", calculated_orders):
                self.assertTrue(skill_task._gate("Made result.xlsx.", "final", "thebot"))
            self.assertEqual(office.formulas_without_results(book), [])
        self.assertEqual(len(self.judged), 1)


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
