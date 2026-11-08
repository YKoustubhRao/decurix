"""
One comparator for a field's values (decurix_agent/values.py, W0.2 and W0.3 of the plan of 2026-11-03, and review
round 2 of 2026-11-04).

Found in review: the screen's comparator read every digit date day first and compared numbers by their last 10 digits;
a taught run's went through float(). Round 2 found the one comparator itself dropping signs (brackets, a Unicode minus,
a trailing minus), units (Cr/Dr, kg/lb) and every non-Latin letter, matching a number against punctuation-stripped
text, and reading the source and the application under one date order. Each of those is a case below.
"""

import json
import tempfile
import unittest
from decimal import Decimal
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, skill_task, state, task_context, values, watchers
from decurix_agent.computer import core, records

TASK = "t00000000000000aa"


def same(a, b, a_order=None, b_order=None):
    return values.compare(a, b, a_order, b_order).same


class SignsUnitsAndFormats(unittest.TestCase):
    """Review round 2, B1: every row was True."""

    def test_signs_are_kept(self):
        for a in ("(1,234.00)", "−1,234", "1,234-", "-1,234"):
            self.assertIs(same(a, "1234"), False, a)
        self.assertIs(same("-₹500", "₹500"), False)
        self.assertIs(same("₹-500", "-500"), True)
        self.assertIs(same("(500)", "-500"), True)
        self.assertIs(same("1,25,000/-", "125000"), True)  # "/-" is "only", not a minus

    def test_units_must_be_the_same_unit(self):
        self.assertIs(same("500 Cr", "500 Dr"), False)
        self.assertIs(same("15 kg", "15 lb"), False)
        self.assertIs(same("15 kg", "15 KG"), True)
        self.assertIsNone(same("15 kg", "15"))  # a unit on one side only is not decided
        # A label that names more than one unit names none (review round 4).
        for label in ("Weight in kg or lb", "Weight (kg, lb)", "Weight (kg / lb)", "Rate % (kg)",
                      "Weight (kg) or volume (l)"):
            self.assertIsNone(values.compare("15 lb", "15", units=values.field_units(label)).same, label)
            self.assertIsNone(values.compare("15 kg", "15", units=values.field_units(label)).same, label)
        self.assertIs(values.compare("15 kg", "15", units=values.field_units("Weight (kg)")).same, True)
        self.assertIs(values.compare("18%", "18", units=values.field_units("GST Rate (%)")).same, True)
        self.assertIs(same("$100", "₹100"), False)
        self.assertIs(same("5900.00 INR", "₹5,900"), True)

    def test_a_number_against_what_is_no_number_is_undecided(self):
        self.assertIsNone(same("12.5", "12-5"))
        self.assertIsNone(same("1.250,00", "125000"))  # separators that read two ways
        self.assertIsNone(same("12,5", "12.5"))
        self.assertIsNone(same("+1 500000", "500000"))  # a signed amount is not a phone

    def test_every_digit_counts(self):
        a, b = "4111223344556677", "9900223344556677"
        self.assertEqual(a[-10:], b[-10:])
        self.assertIs(same(a, b), False)
        self.assertIs(same("12345678901234567", "12345678901234568"), False)  # float's trap
        self.assertEqual(float("12345678901234567"), float("12345678901234568"))
        self.assertIs(same("4111 2233 4455 6677", "4111223344556677"), True)
        self.assertIs(same("98765 43210", "9876543210"), True)

    def test_numbers_by_value(self):
        self.assertIs(same("76,700.00", "76700"), True)
        self.assertIs(same("Rs. 5,850.00", "5850"), True)
        self.assertIs(same("₹1,25,000", "125000.00"), True)
        self.assertIs(same("1", "1188"), False)
        self.assertIs(same("१२३", "123"), True)        # Devanagari digits
        self.assertIs(same("１２３", "123"), True)        # full-width digits
        self.assertIs(same("1 250", "1250"), True)                # a narrow no-break space groups
        self.assertEqual(values.parse_number("1,25,000/-"), Decimal(125000))
        self.assertEqual(values.parse_number("(1,234.50)"), Decimal("-1234.50"))

    def test_a_country_code_in_the_e164_shape(self):
        self.assertIs(same("+91 98765 43210", "9876543210"), True)
        self.assertIs(same("9876543210", "+91-98765-43210"), True)
        self.assertIs(same("+91 98765 43210", "+1 98765 43210"), False)
        self.assertIs(same("+91 98765 43210", "+91 98765 43210"), True)

    def test_identifiers_are_their_letters_and_digits(self):
        self.assertIs(same("INV-0042", "0042"), False)
        self.assertIs(same("INV-0042", "inv0042"), True)
        self.assertIs(same("INV-1234", "PO-1234"), False)
        self.assertIs(same("0012345", "12345"), False)
        self.assertIsNone(values.parse_number("INV-1234"))


class ARangeAndItsCount(unittest.TestCase):
    """Review round 2 leftover: false_sums read "Rows 2 - 10 = 9." as a wrong subtraction. A range of numbered things
    and how many it holds is no sum - decided by its structure, never by which word names the things."""

    def test_a_range_named_in_its_clause_and_its_count(self):
        self.assertTrue(values.range_count("Rows ", "2 - 10", "9"))
        self.assertTrue(values.range_count("I checked pages ", "3-7", "5"))
        self.assertTrue(values.range_count("Zeilen ", "2 – 10", "9"))  # any script's word, an en dash

    def test_a_sum_stays_a_sum(self):
        self.assertFalse(values.range_count("Balance ", "200 - 350", "150"))  # a reversed difference is no count
        self.assertFalse(values.range_count("Rows ", "2 - 10", "8"))         # not its count
        self.assertFalse(values.range_count("Rows ", "10 - 2", "9"))         # not ascending
        self.assertFalse(values.range_count("", "2 - 10", "9"))              # nothing named
        self.assertFalse(values.range_count("Rows: ", "2 - 10", "9"))        # another clause names it
        self.assertFalse(values.range_count("Total 4 + ", "2 - 10", "9"))    # an operator, not a word, before it
        self.assertFalse(values.range_count("Rows ", "2.5 - 10", "9"))
        self.assertFalse(values.range_count("Rows ", "2 - 10", "9.0"))
        self.assertFalse(values.range_count("Rows ", "2 + 10", "11"))
        self.assertFalse(values.range_count("Rows ", "02 - 10", "9"))


class TextOfEveryScript(unittest.TestCase):
    """Review round 2, B2: every letter outside a-z was dropped."""

    def test_letters_are_kept(self):
        self.assertIs(same("Müller", "Möller"), False)
        self.assertIs(same("Ram राम", "Ram श्याम"), False)
        self.assertIs(same("राम", "राम"), True)
        self.assertIs(same("José", "JOSÉ"), True)
        self.assertIs(same("राम", "रम"), False)  # a vowel sign is a mark, and it counts

    def test_text(self):
        self.assertIs(same("Coastal  Infra Projects", "coastal infra projects"), True)
        self.assertIs(same("Karnataka (29)", "Karnataka"), True)
        self.assertIs(same("", ""), False)
        self.assertIs(same(None, None), False)

    def test_punctuation_and_spacing_variants_of_one_name(self):
        """Review round 3, R4: dropping punctuation without a gap while keeping word gaps made each row "differs", so
        presave blocked correct saves of these names."""
        for a, b in (("ABC Pvt.Ltd.", "ABC Pvt. Ltd."), ("M/s.ABC Traders", "M/s ABC Traders"),
                     ("Saint-Gobain", "Saint Gobain"), ("A. B. Enterprises", "A.B. Enterprises"),
                     ("ACME-LTD", "acme ltd")):
            self.assertIs(same(a, b), True, (a, b))
        self.assertEqual(values.text_key("ABC Pvt.Ltd."), "abc pvt ltd")  # punctuation is a gap, gaps collapse
        self.assertIs(same("Müller", "Möller"), False)
        self.assertIs(same("Ram राम", "Ram श्याम"), False)

    def test_digits_grouped_otherwise_are_another_value(self):
        """Review round 4: with every gap removed, "12-345" was "123-45" and "PO 12/3" was "PO 1/23"."""
        # Round 5 (N4): undecided, not "differs" - it may be one code an application regrouped - so the save asks.
        for a, b in (("12-345", "123-45"), ("PO 12/3", "PO 1/23"), ("Unit 1-23", "Unit 12-3"),
                     ("Flat 12, 34th Cross", "Flat 1, 234th Cross"), ("HSN 8471 30", "HSN 847130")):
            self.assertIsNone(same(a, b), (a, b))
        self.assertIs(same("PO 12/3", "PO 12/4"), False)  # other digits still differ
        for a, b in (("INV-0042", "inv0042"), ("ABC Pvt.Ltd.", "ABC Pvt. Ltd."), ("INV-2024-0042", "INV 2024 0042"),
                     ("PO 12/3", "po 12-3")):
            self.assertIs(same(a, b), True, (a, b))
        self.assertIs(same("1234 5678", "12345678"), True)  # digits and spaces only: by its digits

    def test_a_leading_point(self):
        """Review round 3, nit 5."""
        self.assertIs(same(".50", "0.50"), True)
        self.assertIs(same(".50", "0.05"), False)


class Dates(unittest.TestCase):
    def test_a_thirteenth_month_is_no_date(self):
        self.assertIsNone(values.parse_date("2026-13-04"))
        self.assertEqual(values.parse_date("04/13/2026").parts, (2026, 4, 13))
        self.assertIsNot(same("04/13/2026", "2026-13-04"), True)
        for order in ("day", "month", None):
            found = values.parse_date("04/13/2026", order)
            self.assertFalse(found and found.kind == "date" and found.parts[1] == 13, order)

    def test_a_date_the_known_order_cannot_read_is_asked_about_not_different(self):
        # Review round 2, B4: under "day", 04/13/2026 is no date of that order - the order may not be this one's.
        self.assertEqual(values.parse_date("04/13/2026", "day").kind, "unreadable")
        result = values.compare("04/13/2026", "2026-04-13", "day", None)
        self.assertIsNone(result.same)
        self.assertEqual(result.why, "date_order:a")

    def test_each_side_under_its_own_order(self):
        # A US vendor's bill (month first) entered in a day-first org's books.
        self.assertIs(same("03-04-2026", "03/04/2026", "day", "month"), False)
        self.assertIs(same("04-03-2026", "03/04/2026", "day", "month"), True)
        self.assertIs(same("03/04/2026", "2026-04-03", "day", None), True)
        self.assertIs(same("03/04/2026", "2026-03-04", "month", None), True)

    def test_an_ambiguous_side_with_no_order_is_undecided_whatever_its_layout(self):
        self.assertIsNone(same("03/04/2026", "2026-04-03"))
        self.assertIsNone(same("03/04/2026", "03-04-2026"))  # the same layout decides nothing (review round 2, B3/B4)
        self.assertEqual(values.compare("2026-04-03", "03/04/2026").why, "date_order:b")

    def test_an_ambiguous_date_that_is_neither_reading_is_different(self):
        self.assertIs(same("03/04/2026", "2026-07-15"), False)

    def test_figures_that_cannot_be_read_the_other_way_decide_alone(self):
        self.assertIs(same("28/09/2026", "2026-09-28"), True)
        self.assertIs(same("09/28/2026", "2026-09-28"), True)
        self.assertIs(same("05/05/2026", "2026-05-05"), True)

    def test_named_months_iso_times_and_objects(self):
        import datetime
        self.assertIs(same("1-Oct-26", "2026-10-01"), True)
        self.assertIs(same("Oct 1, 2026", "01 October 2026"), True)
        self.assertIs(same(datetime.date(2026, 4, 3), "2026-04-03"), True)
        self.assertIs(same(datetime.datetime(2026, 4, 3, 9), "3rd Apr 2026"), True)
        self.assertIsNot(same("2026-02-30", "2026-03-02"), True)  # (30 February is no date)
        self.assertIsNone(values.parse_date("3 Junk 2026"))      # a month by its name or its abbreviation only
        self.assertIsNone(values.parse_date("03/04/2026 INV-55"))  # only a time may follow
        self.assertIs(same("2026-04-03 10:00", "2026-04-03 23:59"), False)
        self.assertEqual(values.parse_date("31/12/99").parts, (1999, 12, 31))

    def test_a_date_against_a_number_is_undecided(self):
        self.assertIsNone(same("03/04/2026", "03042026"))
        self.assertIsNone(same("2026-04-03", "20260403"))
        self.assertIs(same("2026-04-03", "hello"), False)

    def test_as_iso_only_for_a_decided_day(self):
        self.assertEqual(values.as_iso("28/09/2026"), "2026-09-28")
        self.assertIsNone(values.as_iso("03/04/2026"))
        self.assertEqual(values.as_iso("03/04/2026", "day"), "2026-04-03")


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        task_context._derived.clear()
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(state, "current_task_id", TASK),
                  mock.patch.object(state, "current_task", "Enter the bill.")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(task_context._derived.clear)


class TheTasksDateOrders(Base):
    def test_the_control_planes_order_is_the_applications(self):
        task_context.set_date_order(TASK, "month")
        self.assertEqual(task_context.app_date_order(), "month")
        self.assertIsNone(task_context.source_date_order())  # the org's app is not its vendors' documents

    def test_the_requests_marker_is_the_sources(self):
        with mock.patch.object(state, "current_task", "Dates are MM/DD/YYYY here."):
            self.assertEqual(task_context.source_date_order(), "month")
        with mock.patch.object(state, "current_task", "Dates are dd-mm-yyyy here."):
            self.assertEqual(task_context.source_date_order(), "day")

    def test_unambiguous_dates_in_the_request(self):
        with mock.patch.object(state, "current_task", "Bill of 03/04/2026, due 28/09/2026."):
            self.assertEqual(task_context.source_date_order(), "day")

    def test_unambiguous_dates_in_a_file_the_request_brought(self):
        bill = self.root / "bill.txt"
        bill.write_text("Bill date 03/04/2026\nDue 28/09/2026\n", encoding="utf-8")
        with mock.patch.object(task_context, "current_files", lambda: [str(bill)]):
            self.assertEqual(task_context.source_date_order(), "day")
        kept = json.loads((self.root / "runs" / TASK / "context.json").read_text(encoding="utf-8"))
        self.assertEqual(kept["derived_date_order"]["order"], "day")

    def test_nothing_says(self):
        self.assertIsNone(task_context.source_date_order())
        self.assertIsNone(task_context.app_date_order())

    def test_another_request_under_the_same_id_is_read_again(self):
        with mock.patch.object(state, "current_task", "Due 28/09/2026."):
            self.assertEqual(task_context.source_date_order(), "day")
        with mock.patch.object(state, "current_task", "Due 09/28/2026."):
            self.assertEqual(task_context.source_date_order(), "month")

    def test_an_answer_needs_a_judgement(self):
        self.assertFalse(task_context.set_answered_order(TASK, "source", "day", ""))
        self.assertTrue(task_context.set_answered_order(TASK, "source", "day", "j_0123456789abcdef"))
        self.assertEqual(task_context.source_date_order(), "day")


class TheSaveAsksThePerson(Base):
    """Review round 2, B3: the save waits on a real question, and the person's numbered answer decides it. Round 3, R3:
    it was added to the engine's own question and any "1" or "2" answered it - one reply to an engine question with
    numbered choices set the date's order too. It goes out alone now, and only the reply to its own query answers it."""

    JUDGED = "j_0123456789abcdef"

    def setUp(self):
        super().setUp()
        from decurix_agent import messaging
        self.messaging = messaging
        self.sent = []
        for p in (mock.patch.object(messaging, "send_query", lambda *a, **k: self.sent.append((a, k)) or True),
                  mock.patch.object(messaging, "_query_gate", lambda *a: True),
                  mock.patch.object(state, "asked_query_id", "")):
            p.start()
            self.addCleanup(p.stop)

    def rec(self):
        return {"key": "B1", "source": "bill.pdf", "source_order": None,
                "fields": {"Bill date": {"intent": "03/04/2026", "from": "source"}}}

    def ask(self, question="I could not save the bill. What should I do?"):
        return self.messaging.ask_for_query(question)

    def reply(self, text, judgement=JUDGED):
        """The person's reply, through the watcher; what the engine is handed."""
        path = self.root / "reply.txt"
        path.write_text(text, encoding="utf-8")
        state.message_facts[path.name] = {"secret": False, "judgement": judgement}
        handed = []

        class Started:
            """The thread the watcher hands the reply on with: what it would run, not run."""
            def __init__(self, target=None, args=(), **kw):
                handed.append(args[0] if args else None)

            def start(self):
                pass

        with mock.patch.object(watchers, "is_reply_file", lambda name: True), \
                mock.patch.object(watchers.threading, "Thread", Started), \
                mock.patch.object(watchers.lessons, "note", lambda *a, **k: None), \
                mock.patch.object(watchers.skill_task, "learn_answer", lambda *a, **k: None), \
                mock.patch.object(watchers, "_mark_handled", lambda p: None):
            watchers.answer_waiting(path, text, already_marked=True)
        return handed[-1] if handed else None

    def test_an_undecided_date_asks_with_both_readings_and_the_answer_decides(self):
        blocking, _, _ = records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.assertEqual(len(blocking), 1)
        question = task_context.date_question(TASK)
        self.assertEqual(question["side"], "source")
        text = task_context.date_question_text(question)
        self.assertIn("3 April 2026", text)
        self.assertIn("4 March 2026", text)
        self.assertIn("this needs the person", blocking[0])
        query = self.ask()
        self.assertIsNone(task_context.take_date_answer(TASK, "maybe the first?", self.JUDGED, query))
        self.assertIsNotNone(task_context.date_question(TASK))
        self.assertEqual(task_context.take_date_answer(TASK, "1", self.JUDGED, query), "day")
        self.assertIsNone(task_context.date_question(TASK))
        blocking, _, _ = records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.assertEqual(blocking, [])

    def test_only_a_reply_that_is_the_number_answers(self):
        # Review round 3 (the "number anywhere" mutation): a reply that holds an option's number among other words
        # answers nothing.
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        query = self.ask()
        for said in ("I think 1", "1 or 2, not sure", "option 2 maybe", "12", "21"):
            self.assertIsNone(task_context.take_date_answer(TASK, said, self.JUDGED, query), said)
        self.assertIsNone(task_context.source_date_order())
        self.assertEqual(task_context.take_date_answer(TASK, " 1. ", self.JUDGED, query), "day")

    def test_the_other_answer_makes_the_same_save_differ(self):
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        query = self.ask()
        self.assertEqual(task_context.take_date_answer(TASK, "2", self.JUDGED, query), "month")
        blocking, _, _ = records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.assertEqual(len(blocking), 1)
        self.assertIn("holds '2026-04-03' where the record says '03/04/2026'", blocking[0])

    def test_typing_the_sources_own_digits_is_no_answer(self):
        blocking, _, _ = records.presave(self.rec(), [("Bill date", "03/04/2026")])
        self.assertEqual(len(blocking), 1)

    def test_the_question_goes_out_alone_in_place_of_the_engines(self):
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.ask("Which ledger? 1. Sales ledger 2. Purchase ledger")
        self.assertEqual(len(self.sent), 1, "the date question is never answered for the person by the query gate")
        body = self.sent[-1][0][1]
        self.assertIn("Reply 1 if it is 3 April 2026", body)
        self.assertNotIn("Sales ledger", body)  # never added to the engine's question
        self.assertTrue(task_context.date_question(TASK)["query_id"])

    def test_a_1_to_an_engine_question_with_numbered_choices_sets_no_order(self):
        # The engine's question with its own numbered choices is the one out; the date question is kept but not yet
        # asked. The reply "1" is the engine's answer, and the date's order is not set by it.
        self.ask("Which ledger? 1. Sales ledger 2. Purchase ledger")
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        handed = self.reply("1")
        self.assertEqual(handed, "1")  # the engine gets its own answer, untouched
        self.assertIsNone(task_context.source_date_order())
        self.assertIsNotNone(task_context.date_question(TASK))
        self.assertIsNone(task_context.take_date_answer(TASK, "1", self.JUDGED, state.asked_query_id))

    def test_a_reply_to_the_date_query_through_the_watcher_takes_the_order(self):
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.ask("Which ledger? 1. Sales ledger 2. Purchase ledger")
        handed = self.reply("1")
        self.assertEqual(task_context.source_date_order(), "day")
        self.assertIn("Not your question", handed)  # the engine is told what was asked, never handed a bare "1"
        self.assertIn("day first", handed)
        labelled = json.loads((self.root / "runs" / TASK / "run.json").read_text(encoding="utf-8"))
        self.assertIn("reply", labelled["labelled_by"])  # (review round 2: the reply labels the run)

    def test_a_reply_to_the_date_query_without_a_judgement_sets_nothing(self):
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.ask()
        self.reply("1", judgement="")
        self.assertIsNone(task_context.source_date_order())

    def test_a_reply_that_picks_neither_leaves_it_to_be_asked_again(self):
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        self.ask()
        handed = self.reply("the third of April")
        self.assertIn("picked neither reading", handed)
        self.assertIsNone(task_context.source_date_order())
        self.assertNotIn("query_id", task_context.date_question(TASK))
        self.ask()
        self.assertEqual(len(self.sent), 2)
        self.assertIn("Reply 1 if it is 3 April 2026", self.sent[-1][0][1])

    def test_a_task_that_ends_with_it_unanswered_is_not_checked(self):
        from decurix_agent import verdict as verdicts
        records.presave(self.rec(), [("Bill date", "2026-04-03")])
        posted = []
        with mock.patch.object(self.messaging, "send_query", lambda *a, **k: posted.append(k) or True), \
                mock.patch.object(self.messaging, "_final_gate", None), \
                mock.patch.object(self.messaging.progress, "flush", lambda: None):
            self.messaging.write_final_reply("Saved the bill.", checked=True,
                                             verdict=verdicts.make(verdicts.DONE, "verify",
                                                                   checks=[verdicts.check("c", "pass", "code")]))
        self.assertTrue(posted)
        self.assertFalse(posted[-1].get("checked"))
        self.assertEqual(posted[-1]["verdict"]["verdict"], verdicts.UNCHECKED)
        self.assertIn("Reply 1 if it is 3 April 2026", posted[-1]["reply"])


class TheCallersShareIt(Base):
    def test_the_ledger(self):
        self.assertIsNone(records.same_value("03/04/2026", "2026-04-03"))
        self.assertTrue(records.same_value("01-10-2026", "1/10/2026", order="day"))
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "day"), \
                mock.patch.object(task_context, "app_date_order", lambda task_id=None: "day"):
            self.assertIs(records.same_value("2026-04-03", "03/04/2026"), True)
            self.assertEqual(records.as_date("03/04/2026"), "2026-04-03")
            self.assertTrue(records.stands_in("2026-04-03", "Bill date: 03/04/2026.", "day", "day"))
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "month"):
            self.assertFalse(records.stands_in("2026-04-03", "Bill date: 03/04/2026.", text_order="month"))

    def test_a_taught_runs_check(self):
        self.assertFalse(skill_task._same_value("12345678901234567", "12345678901234568"))
        self.assertTrue(skill_task._same_value("5,900.00", "5900"))
        self.assertFalse(skill_task._same_value("03/04/2026", "2026-04-03"))
        with mock.patch.object(task_context, "source_date_order", lambda task_id=None: "day"):
            self.assertTrue(skill_task._same_value("03/04/2026", "2026-04-03"))


class CrossedValues(Base):
    """Two payees in the request's table whose 16-digit accounts share their last 10 digits. Compared by those 10, a
    payment form holding the second payee's account in the first payee's record was "its own" value: the crossing was
    never seen. Every digit counts now, and the right account still passes."""

    HEADER = ["Payee name", "Email", "Account number"]
    ROWS = [["Asha Rao", "asha@example.test", "4111223344556677"],
            ["Ravi Kumar", "ravi@example.test", "9900223344556677"]]

    def test_the_right_account_passes(self):
        form = [("Email", "asha@example.test"), ("Account number", "4111223344556677")]
        self.assertEqual(core.crossed_values(form, [(self.HEADER, self.ROWS)]), [])

    def test_the_other_payees_account_is_named(self):
        form = [("Email", "asha@example.test"), ("Account number", "9900223344556677")]
        self.assertEqual(core.crossed_values(form, [(self.HEADER, self.ROWS)]),
                         [("Account number", "9900223344556677", "Ravi Kumar", "4111223344556677")])

    def test_an_undecided_date_is_never_a_crossing(self):
        # Review round 2 (mutation M16): another row's date that the typed one may be is not refused - it is not
        # decided either way.
        header = ["Payee name", "Email", "Due date"]
        rows = [["Asha Rao", "asha@example.test", "2026-03-04"], ["Ravi Kumar", "ravi@example.test", "2026-04-03"]]
        form = [("Email", "asha@example.test"), ("Due date", "03/04/2026")]
        self.assertEqual(core.crossed_values(form, [(header, rows)]), [])
        # Round 3 (nit 1): the own row decidedly different and the other row undecided - the case that separates "is
        # True" from "is not False". The other row's date is not decided to be the typed one, so it is no crossing.
        rows = [["Asha Rao", "asha@example.test", "2026-05-05"], ["Ravi Kumar", "ravi@example.test", "2026-04-03"]]
        self.assertEqual(core.crossed_values(form, [(header, rows)]), [])
        # ... and the other row decidedly the typed date is one.
        form = [("Email", "asha@example.test"), ("Due date", "2026-04-03")]
        self.assertEqual(core.crossed_values(form, [(header, rows)]),
                         [("Due date", "2026-04-03", "Ravi Kumar", "2026-05-05")])


if __name__ == "__main__":
    unittest.main()


class TheTypingPathToo(unittest.TestCase):
    """Review round 2: the number field's own check compared through float(), W0.3's trap on the typing path."""

    def test_a_reformatted_long_number_is_not_kept(self):
        from decurix_agent.computer import cdp
        self.assertFalse(cdp.same_number("12345678901234568", "12345678901234567"))
        self.assertFalse(cdp._formatted("12345678901234567", "12345678901234568"))
        self.assertTrue(cdp.same_number("24", "24.0"))
        self.assertTrue(cdp._formatted("1200", "1,200"))
