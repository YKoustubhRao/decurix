"""
One comparator for a field's values (decurix_agent/values.py, W0.2 and W0.3 of the plan of 2026-11-03).

Found in review: the screen's comparator read every digit date day first, so 04/13/2026 became the 13th month of 2026,
and compared numbers by their last 10 digits, so two account numbers that shared them were "the same"; a taught run's
comparator went through float(), which loses every digit past the 15th. The ledger, the screen and a taught run now
share one comparator, which answers None where what is known cannot decide.
"""

import datetime
import json
import tempfile
import unittest
from decimal import Decimal
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, skill_task, state, task_context, values
from decurix_agent.computer import core, records

TASK = "t00000000000000aa"


class Dates(unittest.TestCase):
    def test_a_thirteenth_month_is_no_date(self):
        self.assertIsNone(values.parse_date("2026-13-04"))
        self.assertEqual(values.parse_date("04/13/2026"), ("date", (2026, 4, 13)))
        self.assertIs(values.same_value("04/13/2026", "2026-13-04"), False)
        for order in ("day", "month", None):
            found = values.parse_date("04/13/2026", order)
            self.assertFalse(found and found[0] == "date" and found[1][1] == 13, order)

    def test_under_day_order_a_month_over_12_is_not_a_date(self):
        self.assertIsNone(values.parse_date("04/13/2026", "day"))
        self.assertIsNone(values.parse_date("13/04/2026", "month"))
        self.assertIsNone(values.parse_date("32/01/2026", "day"))
        self.assertIsNone(values.parse_date("31/02/2026", "day"))

    def test_the_known_order_decides(self):
        self.assertIs(values.same_value("03/04/2026", "2026-03-04", "month"), True)
        self.assertIs(values.same_value("03/04/2026", "2026-04-03", "day"), True)
        self.assertIs(values.same_value("03/04/2026", "2026-04-03", "month"), False)
        self.assertIs(values.same_value("03/04/2026", "2026-03-04", "day"), False)

    def test_with_no_order_an_ambiguous_date_is_not_decided(self):
        self.assertIsNone(values.same_value("03/04/2026", "2026-04-03"))
        self.assertIsNone(values.same_value("03/04/2026", "2026-03-04"))
        self.assertEqual(values.parse_date("03/04/2026"), ("ambiguous", (3, 4, 2026), "p/p/y"))

    def test_an_ambiguous_date_that_is_neither_reading_is_decided_different(self):
        self.assertIs(values.same_value("03/04/2026", "2026-07-15"), False)

    def test_the_same_layout_compares_figure_by_figure(self):
        self.assertIs(values.same_value("03/04/2026", "03-04-2026"), True)
        self.assertIs(values.same_value("03/04/2026", "3.4.26"), True)
        self.assertIs(values.same_value("03/04/2026", "04/03/2026"), False)

    def test_figures_that_cannot_be_read_the_other_way_decide_alone(self):
        self.assertIs(values.same_value("28/09/2026", "2026-09-28"), True)
        self.assertIs(values.same_value("09/28/2026", "2026-09-28"), True)
        self.assertIs(values.same_value("05/05/2026", "2026-05-05"), True)

    def test_named_months_iso_and_date_objects(self):
        self.assertIs(values.same_value("1-Oct-26", "2026-10-01"), True)
        self.assertIs(values.same_value("Oct 1, 2026", "01 October 2026"), True)
        self.assertIs(values.same_value(datetime.date(2026, 4, 3), "2026-04-03T10:00"), True)
        self.assertIs(values.same_value(datetime.datetime(2026, 4, 3, 9), "3rd Apr 2026"), True)
        self.assertIs(values.same_value("2026-02-30", "2026-03-02"), False)

    def test_a_date_is_never_a_number_or_text(self):
        self.assertIs(values.same_value("03/04/2026", "03042026"), False)
        self.assertIs(values.same_value("2026-04-03", "20260403"), False)

    def test_as_iso_only_for_a_decided_day(self):
        self.assertEqual(values.as_iso("28/09/2026"), "2026-09-28")
        self.assertIsNone(values.as_iso("03/04/2026"))
        self.assertEqual(values.as_iso("03/04/2026", "day"), "2026-04-03")


class Numbers(unittest.TestCase):
    def test_every_digit_counts(self):
        a, b = "4111223344556677", "9900223344556677"
        self.assertEqual(a[-10:], b[-10:])
        self.assertIs(values.same_value(a, b), False)
        self.assertIs(values.same_value("12345678901234567", "12345678901234568"), False)  # float's trap
        self.assertEqual(float("12345678901234567"), float("12345678901234568"))
        self.assertIs(values.same_value("4111 2233 4455 6677", "4111223344556677"), True)

    def test_a_country_code_in_the_e164_shape(self):
        self.assertIs(values.same_value("+91 98765 43210", "9876543210"), True)
        self.assertIs(values.same_value("9876543210", "+91-98765-43210"), True)
        self.assertIs(values.same_value("+91 98765 43210", "+1 98765 43210"), False)
        self.assertIs(values.same_value("+91 98765 43210", "+91 98765 43210"), True)
        self.assertIs(values.same_value("+1 500", "500"), False)  # an amount, not a phone

    def test_numbers_by_value(self):
        self.assertIs(values.same_value("76,700.00", "76700"), True)
        self.assertIs(values.same_value("Rs. 5,850.00", "5850"), True)
        self.assertIs(values.same_value("₹1,25,000", "125000.00"), True)
        self.assertIs(values.same_value("1", "1188"), False)
        self.assertEqual(values.parse_number("1,25,000/-"), Decimal(125000))

    def test_identifiers_are_their_letters_and_digits(self):
        self.assertIs(values.same_value("INV-0042", "0042"), False)
        self.assertIs(values.same_value("INV-0042", "inv0042"), True)
        self.assertIs(values.same_value("INV-1234", "PO-1234"), False)
        self.assertIs(values.same_value("0012345", "12345"), False)
        self.assertIsNone(values.parse_number("INV-1234"))

    def test_text(self):
        self.assertIs(values.same_value("Coastal  Infra Projects", "coastal infra projects"), True)
        self.assertIs(values.same_value("Karnataka (29)", "Karnataka"), True)
        self.assertIs(values.same_value("", ""), False)
        self.assertIs(values.same_value(None, None), False)


class TheTasksDateOrder(unittest.TestCase):
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

    def test_the_control_planes_order_comes_first(self):
        with mock.patch.object(state, "current_task", "Dates are DD/MM/YYYY."):
            task_context.set_date_order(TASK, "month")
            self.assertEqual(task_context.date_order(), "month")

    def test_the_requests_marker(self):
        with mock.patch.object(state, "current_task", "Dates are MM/DD/YYYY here."):
            self.assertEqual(task_context.date_order(), "month")
        with mock.patch.object(state, "current_task", "Dates are dd-mm-yyyy here."):
            self.assertEqual(task_context.date_order(), "day")

    def test_unambiguous_dates_in_the_request(self):
        with mock.patch.object(state, "current_task", "Bill of 03/04/2026, due 28/09/2026."):
            self.assertEqual(task_context.date_order(), "day")

    def test_unambiguous_dates_in_a_file_the_request_brought(self):
        bill = self.root / "bill.txt"
        bill.write_text("Bill date 03/04/2026\nDue 28/09/2026\n", encoding="utf-8")
        with mock.patch.object(task_context, "current_files", lambda: [str(bill)]):
            self.assertEqual(task_context.date_order(), "day")
        kept = json.loads((self.root / "runs" / TASK / "context.json").read_text(encoding="utf-8"))
        self.assertEqual(kept["derived_date_order"]["order"], "day")

    def test_nothing_says(self):
        self.assertIsNone(task_context.date_order())

    def test_another_request_under_the_same_id_is_read_again(self):
        with mock.patch.object(state, "current_task", "Due 28/09/2026."):
            self.assertEqual(task_context.date_order(), "day")
        with mock.patch.object(state, "current_task", "Due 09/28/2026."):
            self.assertEqual(task_context.date_order(), "month")


class TheCallersShareIt(unittest.TestCase):
    def test_the_ledger(self):
        with mock.patch.object(task_context, "date_order", lambda task_id=None: None):
            self.assertIsNone(records.same_value("03/04/2026", "2026-04-03"))
            self.assertTrue(records.same_value("01-10-2026", "1/10/2026"))
            self.assertFalse(records.same_value("01-10-2026", "10-01-2026"))
        with mock.patch.object(task_context, "date_order", lambda task_id=None: "day"):
            self.assertIs(records.same_value("03/04/2026", "2026-04-03"), True)
            self.assertEqual(records.as_date("03/04/2026"), "2026-04-03")
            self.assertTrue(records.stands_in("2026-04-03", "Bill date: 03/04/2026."))
        with mock.patch.object(task_context, "date_order", lambda task_id=None: "month"):
            self.assertFalse(records.stands_in("2026-04-03", "Bill date: 03/04/2026."))

    def test_the_save_gate_waits_on_a_date_it_cannot_compare(self):
        rec = {"fields": {"Bill date": {"intent": "03/04/2026", "from": "source"}}}
        with mock.patch.object(task_context, "date_order", lambda task_id=None: None):
            blocking, _, _ = records.presave(rec, [("Bill date", "2026-04-03")])
        self.assertEqual(len(blocking), 1)
        self.assertIn("which order are dates written in here?", blocking[0])
        with mock.patch.object(task_context, "date_order", lambda task_id=None: "day"):
            blocking, _, _ = records.presave(rec, [("Bill date", "2026-04-03")])
        self.assertEqual(blocking, [])

    def test_a_taught_runs_check(self):
        with mock.patch.object(task_context, "date_order", lambda task_id=None: None):
            self.assertFalse(skill_task._same_value("12345678901234567", "12345678901234568"))
            self.assertTrue(skill_task._same_value("5,900.00", "5900"))
            self.assertFalse(skill_task._same_value("03/04/2026", "2026-04-03"))
        with mock.patch.object(task_context, "date_order", lambda task_id=None: "day"):
            self.assertTrue(skill_task._same_value("03/04/2026", "2026-04-03"))


class CrossedValues(unittest.TestCase):
    """Two payees in the request's table whose 16-digit accounts share their last 10 digits. Compared by those 10, a
    payment form holding the second payee's account in the first payee's record was "its own" value: the crossing was
    never seen. Every digit counts now, and the right account still passes."""

    HEADER = ["Payee name", "Email", "Account number"]
    ROWS = [["Asha Rao", "asha@example.test", "4111223344556677"],
            ["Ravi Kumar", "ravi@example.test", "9900223344556677"]]

    def setUp(self):
        p = mock.patch.object(task_context, "date_order", lambda task_id=None: None)
        p.start()
        self.addCleanup(p.stop)

    def test_the_right_account_passes(self):
        form = [("Email", "asha@example.test"), ("Account number", "4111223344556677")]
        self.assertEqual(core.crossed_values(form, [(self.HEADER, self.ROWS)]), [])

    def test_the_other_payees_account_is_named(self):
        form = [("Email", "asha@example.test"), ("Account number", "9900223344556677")]
        self.assertEqual(core.crossed_values(form, [(self.HEADER, self.ROWS)]),
                         [("Account number", "9900223344556677", "Ravi Kumar", "4111223344556677")])


if __name__ == "__main__":
    unittest.main()
