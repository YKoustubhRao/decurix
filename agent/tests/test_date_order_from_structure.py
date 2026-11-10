"""
Which way a two-way digit date is written, read from structure before the person is asked - and, when it still cannot
be told, asked at once.

Found live (2026-11-10): "Save voucher" was refused - "Date (DD-MM-YYYY) holds '02-04-2027' and the record says
'02-04-2027', and which day '02-04-2027' is cannot be told from here - this needs the person". Both sides were
decidable: the field's own label wrote the application's format, and the other bills in the folder the request named
wrote 15-04-2027, 22-04-2027 and 28-04-2027 (a day above 12: day first). And the question waited for the worker's next
HELP, which never came: the worker went round the save, and the unsettled date failed the read-back round after round.
A two-way date is still never decided without an order (test_values, "the source's own digits").
"""

import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, state, task_context, values
from decurix_agent.computer import cdp, core, records

TASK = "t00000000000000bb"


class AFormatInTheFieldsLabel(unittest.TestCase):
    """values.stated_date_order: three runs of D, M and Y in the label, read by their letters - no list of formats."""

    def test_the_label_states_the_order_for_a_value_laid_out_as_it_says(self):
        for label, value, order in (("Date (DD-MM-YYYY)", "02-04-2027", "day"),
                                    ("Invoice date dd/mm/yyyy", "02/04/2027", "day"),
                                    ("Posting date (MM/DD/YYYY)", "02/04/2027", "month"),
                                    ("Due D.M.YYYY", "2.4.2027", "day"),
                                    ("Date (mm-dd-yy)", "02-04-27", "month"),
                                    ("Date (M/D/Y)", "02/04/2027", "month"),
                                    ("Fecha (DD-MM-YYYY)", "02-04-2027 10:30", "day")):
            self.assertEqual(values.stated_date_order(label, value), order, label)

    def test_nothing_is_stated_otherwise(self):
        for label, value in (("Date", "02-04-2027"),                       # no format
                             ("Date (YYYY-MM-DD)", "02-04-2027"),          # the year is not where the value has it
                             ("Date (DD-MM-YY)", "02-04-2027"),            # two year digits, the value four
                             ("Date (DD-MMM-YYYY)", "02-04-2027"),         # a month's name, the value digits
                             ("Date (DD-MM-YYYY or MM/DD/YYYY)", "02-04-2027"),  # two formats that disagree
                             ("Day/Month/Year", "02-04-2027"),             # words, not runs of one letter
                             ("Date (DD-DD-YYYY)", "02-04-2027"),          # no month
                             ("ADD-MM-YYYY", "02-04-2027"),                # inside a longer word
                             ("Date (DD-MM-YYYY)", "2027-04-02"),          # year first: decided without an order
                             ("Date (DD-MM-YYYY)", "")):
            self.assertIsNone(values.stated_date_order(label, value), label)


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        task_context._derived.clear()
        self.addCleanup(task_context._derived.clear)
        self.addCleanup(task_context._app_host.update, task="", host="")
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(state, "current_task_id", TASK),
                  mock.patch.object(state, "current_task", "Enter the bill."),
                  mock.patch.object(state, "asked_query_id", ""),
                  mock.patch.object(task_context, "sent_date_order", lambda task_id=None: None),
                  mock.patch.object(task_context, "kept_app_order", lambda host: None),
                  mock.patch.object(records, "field_place", lambda *a, **k: ("books.example.test", "/voucher/new"))):
            p.start()
            self.addCleanup(p.stop)

    @staticmethod
    def rec():
        return {"key": "B-2", "source": "bill-2.pdf", "source_order": None,
                "fields": {"Date": {"intent": "02-04-2027", "from": "source"}}}


class TheSaveReadsTheLabel(Base):
    def test_the_labels_format_decides_the_applications_side(self):
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "day"):
            blocking, _, matched = records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
            self.assertEqual((blocking, matched), ([], 1))
            # A label that states nothing: the application's side is still the person's to say.
            blocking, _, _ = records.presave(self.rec(), [("Date", "02-04-2027")])
            self.assertEqual(len(blocking), 1)
            self.assertEqual(task_context.date_question(TASK)["side"], "app")

    def test_a_placeholder_that_writes_the_format_decides_it_too(self):
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "day"):
            blocking, _, _ = records.presave(self.rec(), [("Date", "02-04-2027")], formats={"Date": "dd-mm-yyyy"})
            self.assertEqual(blocking, [])

    def test_the_label_states_what_it_says_and_no_more(self):
        # The application takes 02-04-2027 as 2 April; a document that writes month first means 4 February: differs.
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "month"):
            blocking, _, _ = records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        self.assertEqual(len(blocking), 1)
        self.assertIn("where the record says", blocking[0])

    def test_the_form_reader_passes_a_format_placeholder_on(self):
        self.assertIn("placeholder", cdp.FORM_FIELDS)
        found = cdp._without_secrets({"form": "Voucher", "fields": [
            ["Date", "02-04-2027", "text", "", False, "DD-MM-YYYY"],
            ["Narration", "Freight", "text", "", False, ""],
            ["Amount", "100", "text", "", False, "e.g. 100"]]})
        self.assertEqual(found["formats"], {"Date": "DD-MM-YYYY"})
        self.assertEqual(found["fields"][0], ["Date", "02-04-2027"])


class TheSourcesOrderFromTheRequestsFolder(Base):
    """task_context._derived_order reads the files already in a folder the request names (records._request_folder_files)
    when the request and its own files say nothing."""

    def setUp(self):
        super().setUp()
        self.home = self.root / "home"
        self.folder = self.home / "work" / "batch-4"
        self.folder.mkdir(parents=True)
        for name, date in (("bill-1.txt", "15-04-2027"), ("bill-2.txt", "02-04-2027"), ("bill-3.txt", "22-04-2027")):
            (self.folder / name).write_text(f"TAX INVOICE\nInvoice date: {date}\nAmount: 1,000.00\n", encoding="utf-8")
        for p in (mock.patch.dict(os.environ, {"HOME": str(self.home), "USERPROFILE": str(self.home)}),
                  mock.patch.object(state, "current_task", "Enter the April bills in ~/work/batch-4/ into the books."),
                  mock.patch.object(task_context, "current_files", lambda: [])):
            p.start()
            self.addCleanup(p.stop)

    def started(self, at):
        return mock.patch.object(task_context, "since", return_value=at)

    def test_the_other_bills_of_the_folder_decide_the_sources_side(self):
        with self.started(time.time() + 60):  # (the bills were there before the task began)
            self.assertEqual(task_context.source_date_order(TASK), "day")
        kept = json.loads((self.root / "runs" / TASK / "context.json").read_text(encoding="utf-8"))
        self.assertEqual(kept["derived_date_order"]["order"], "day")

    def test_bills_that_disagree_decide_nothing(self):
        (self.folder / "bill-9.txt").write_text("Invoice date: 04/28/2027\n", encoding="utf-8")
        with self.started(time.time() + 60):
            self.assertIsNone(task_context.source_date_order(TASK))

    def test_a_file_the_task_wrote_there_says_nothing(self):
        with self.started(time.time() - 3600):  # (the task began before the files were written: it made them)
            self.assertIsNone(task_context.source_date_order(TASK))

    def test_with_the_tasks_start_unknown_the_none_is_not_kept_for_the_task(self):
        with self.started(None):
            self.assertIsNone(task_context.source_date_order(TASK))
        self.assertNotIn("derived_date_order", task_context._context(TASK))
        task_context._derived.clear()  # (another process: the `gui` command that checks the save)
        with self.started(time.time() + 60):
            self.assertEqual(task_context.source_date_order(TASK), "day")

    def test_the_whole_save_goes_through(self):
        with self.started(time.time() + 60):
            blocking, _, _ = records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        self.assertEqual(blocking, [])
        self.assertIsNone(task_context.date_question(TASK))


class AQuestionStillOpenIsAskedAtOnce(Base):
    """core.person_ask_line: a refused save that waits on the agent's own date question ends with it, so the engine asks
    the person now (soul's save_refusal.go), not at the worker's next HELP."""

    def test_the_refusal_ends_with_the_question_until_it_is_put(self):
        self.assertEqual(core.person_ask_line(TASK), "")
        blocking, _, _ = records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        self.assertEqual(len(blocking), 1)
        self.assertIn("this needs the person", blocking[0])
        question = task_context.date_question(TASK)
        self.assertEqual(question["side"], "source")  # (the label settled the application's side)
        line = core.person_ask_line(TASK)
        self.assertTrue(line.startswith("\n" + core.PERSON_ASK_MARKER + " Before I save: the document writes"), line)
        self.assertIn("which can be read two ways. Reply 1 if it is", line)  # (the eval expert's DateOrderMark)
        self.assertEqual(line.count("\n"), 1)
        # Put to the person (its query kept): the next refusal does not ask it again.
        task_context.mark_date_asked(TASK, "q_0001")
        self.assertEqual(core.person_ask_line(TASK), "")

    def test_a_question_the_person_could_not_settle_is_not_asked_again(self):
        records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        task_context.mark_date_asked(TASK, "q_0001")
        task_context.unmark_date_asked(TASK)
        blocking, _, _ = records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        self.assertTrue(any("could not say which day" in b for b in blocking), blocking)
        self.assertEqual(core.person_ask_line(TASK), "")

    def test_the_engines_question_is_replaced_by_the_agents_own(self):
        from decurix_agent import messaging
        records.presave(self.rec(), [("Date (DD-MM-YYYY)", "02-04-2027")])
        sent = []
        with mock.patch.object(messaging, "send_query", lambda *a, **k: sent.append(a) or True), \
                mock.patch.object(messaging, "_query_gate", lambda *a: True):
            messaging.ask_for_query(core.person_ask_line(TASK).split(core.PERSON_ASK_MARKER, 1)[1].strip())
        self.assertEqual(len(sent), 1)
        self.assertIn("Reply 1 if it is 2 April 2027 (day first)", sent[0][1])
        self.assertTrue(task_context.date_question(TASK)["query_id"])

    def test_the_ledger_check_ends_its_refusal_with_the_question(self):
        rec = dict(self.rec(), declared_at=1.0)
        with mock.patch.object(records, "load", lambda *a, **k: [rec]), \
                mock.patch.object(records, "open_record", lambda *a, **k: rec), \
                mock.patch.object(records, "note_presave", lambda *a, **k: None), \
                mock.patch.object(records, "undeclared_task_fields", lambda *a, **k: []), \
                mock.patch.object(core, "_task_events_now", lambda: []):
            held, clean, _ = core._ledger_check([("Date (DD-MM-YYYY)", "02-04-2027"), ("Amount", "1000")])
        self.assertFalse(clean)
        first, _, last = held.partition("\n")
        self.assertTrue(first.startswith("Not pressed: the form does not hold what record B-2 says"), first)
        self.assertTrue(last.startswith(core.PERSON_ASK_MARKER + " Before I save:"), last)


if __name__ == "__main__":
    unittest.main()
