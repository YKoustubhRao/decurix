"""
Two faults of the save check found in a live run (2026-11-08):

- A cut read judged as the whole value. The form readers (cdp.FORM_FIELDS, core._page_fields, the Enter path) kept a
  value's first 80 characters with no mark, and records.presave compared that part with the declared value: a declared
  126-character Notes field was refused at every save until the token budget ran out. Now a value is read whole up to
  cdp.FORM_VALUE_CHARS, a longer one is cut at a word and marked cut, and presave compares a cut value only as far as
  it was read.
- A choice left in its "no selection" state was invisible: FORM_FIELDS skips empty fields, so a select left at its
  empty first option never reached presave, and the record that left it out was saved. Now such a choice (told by
  structure: no option selected, or the option whose value is empty) that the record does not declare is a point -
  ADVISORY (records.UNSET_CHOICE_BLOCKS) until it passes the replay gate and a clean live round.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp, core, records
from decurix_agent.computer.errors import ComputerError

NOTES = ("Parcel pickups, 18 runs: 2,160.00; Toll charges: 115.00; Packing: 60.00; Taxable value: 2,335.00; "
         "CGST 9%: 210.15; SGST 9%: 210.15")
LONG = " ".join(f"consignment {i} to depot {i + 40} weighed {i * 3}.5 kg;" for i in range(1, 30))
BILL = f"""TAX INVOICE
Invoice No: KT-50833      Date: 03/04/2026
From: Kestrel Freightways
Notes: {NOTES}
Detail: {LONG}
Total 2,755.30
"""


class Ledger(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bill = Path(self.tmp.name) / "bill.txt"
        self.bill.write_text(BILL, encoding="utf-8")
        for p in (mock.patch.object(records, "LEDGER_DIR", Path(self.tmp.name) / "ledger"),
                  mock.patch("decurix_agent.task_context.current", lambda: "t0123456789abcdef"),
                  mock.patch("decurix_agent.task_context.current_request", lambda: "Enter the courier bill"),
                  mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: ""),
                  mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []),
                  mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day")):
            p.start()
            self.addCleanup(p.stop)

    def declare(self, **more):
        fields = {"Bill number": "KT-50833", "Vendor": "Kestrel Freightways", "Total": "2,755.30", "Notes": NOTES}
        fields.update(more)
        return records.declare("KT-50833", str(self.bill), fields)[0]


class NeverJudgeACutRead(Ledger):
    def test_a_declared_126_character_field_saves(self):
        self.assertGreater(len(NOTES), 120)
        rec = self.declare()
        self.assertEqual(rec["fields"]["Notes"]["intent"], NOTES)
        # Read the way the page-fields reader reads it: whole, not cut.
        page = {"elements": [{"ref": "e9", "role": "textbox", "name": "Bill number", "value": "KT-50833", "group": "g"},
                             {"ref": "e10", "role": "textbox", "name": "Vendor", "value": "Kestrel Freightways", "group": "g"},
                             {"ref": "e13", "role": "textbox", "name": "Total", "value": "2,755.30", "group": "g"},
                             {"ref": "e15", "role": "textbox", "name": "Notes", "value": NOTES, "group": "g"}]}
        found = core._page_fields(page, {"group": "g"})
        self.assertIn(["Notes", NOTES], found["fields"])
        self.assertEqual(found["cut"], [])
        held, clean, _ = core._ledger_check(found["fields"], cut=found["cut"], unset=found["unset"])
        self.assertEqual(held, "")
        self.assertTrue(clean)

    def test_the_part_an_old_reader_kept_would_have_been_refused(self):
        # (What the run met: the first 80 characters, unmarked, against the whole declared value.)
        rec = self.declare()
        blocking, _, _ = records.presave(rec, [("Notes", NOTES[:80])])
        self.assertTrue(blocking)
        # Marked as read in part, the same part is undecided - never called different.
        blocking, advisory, _ = records.presave(rec, [("Notes", NOTES[:80])], cut=["Notes"])
        self.assertEqual(blocking, [])
        self.assertTrue(any("read only in part" in a for a in advisory), advisory)

    def test_a_value_longer_than_the_cap_is_undecided_not_different(self):
        self.assertGreater(len(LONG), cdp.FORM_VALUE_CHARS)
        rec = self.declare(Detail=LONG)
        part, cut = cdp.cut_value(LONG)
        self.assertTrue(cut)
        self.assertLessEqual(len(part), cdp.FORM_VALUE_CHARS)
        self.assertTrue(LONG.startswith(part) and LONG[len(part)] == " ")  # cut at a word, never inside one
        blocking, advisory, _ = records.presave(rec, [("Notes", NOTES), ("Detail", part)], cut=["Detail"])
        self.assertEqual(blocking, [])
        self.assertTrue(any(a.startswith("Detail was read only in part") for a in advisory), advisory)
        # A part that already reads otherwise than the record's start is not the record's value.
        other = "consignment 9 to depot 99" + part[25:]
        blocking, _, _ = records.presave(rec, [("Detail", other)], cut=["Detail"])
        self.assertTrue(blocking and "read in part" in blocking[0], blocking)

    def test_the_readers_keep_values_whole_up_to_the_cap_and_mark_a_cut(self):
        self.assertEqual(cdp.cut_value(NOTES), (NOTES, False))
        self.assertNotIn("v.slice(0, 80)", cdp.FORM_FIELDS)
        self.assertIn(f"v.length > {cdp.FORM_VALUE_CHARS}", cdp.FORM_FIELDS)
        found = cdp._without_secrets({"form": "Bill", "fields": [["Notes", "x" * 10, "textarea", "", True],
                                                                 ["Vendor", "Kestrel Freightways", "text", "", False]],
                                      "unset": ["Cost centre"]})
        self.assertEqual(found["fields"], [["Notes", "x" * 10], ["Vendor", "Kestrel Freightways"]])
        self.assertEqual(found["cut"], ["Notes"])
        self.assertEqual(found["unset"], ["Cost centre"])
        page = {"elements": [{"ref": "e15", "role": "textbox", "name": "Detail", "value": LONG, "group": "g"}]}
        found = core._page_fields(page, {"group": "g"})
        self.assertEqual(found["cut"], ["Detail"])

    def test_the_enter_path_reads_the_typed_line_whole(self):
        box = {"ref": "e15", "role": "textbox", "name": "Notes", "value": "", "source": "cdp", "handle": {"backend_id": 1}}
        seen = {}

        def check(element, checked=False, found=None, listing=True, confirm=None):
            seen.update(found)
            return ""
        with mock.patch.object(cdp, "enter_form_fields", return_value={"form": "Bill", "fields": [], "submit": "Save"}), \
                mock.patch.object(core, "_form_check", check):
            core._commit_check("type", box, None, {"elements": []}, text=NOTES + "\n")
        self.assertEqual(seen["fields"], [["Notes", NOTES]])
        self.assertEqual(seen["cut"], [])

    def test_a_declared_value_is_never_kept_cut(self):
        with self.assertRaises(ComputerError) as caught:
            records.declare("KT-50833", str(self.bill), {"Notes": "word " * (records.MAX_VALUE // 4)})
        self.assertIn("longer than", str(caught.exception))


class AnUnsetChoiceIsDeclaredOrAsked(Ledger):
    def test_an_unset_select_the_record_leaves_out_is_a_point(self):
        rec = self.declare()
        form = [("Bill number", "KT-50833"), ("Vendor", "Kestrel Freightways"), ("Total", "2,755.30"), ("Notes", NOTES)]
        blocking, advisory, _ = records.presave(rec, form, unset=["Cost centre"])
        self.assertEqual(blocking, [])  # ADVISORY until it passes the replay gate (UNSET_CHOICE_BLOCKS)
        point = [a for a in advisory if "Cost centre is left unset" in a]
        self.assertTrue(point, advisory)
        self.assertIn("ask the person (HELP) which Cost centre", point[0])
        with mock.patch.object(records, "UNSET_CHOICE_BLOCKS", True):
            blocking, _, _ = records.presave(rec, form, unset=["Cost centre"])
        self.assertTrue(any("Cost centre is left unset" in b for b in blocking), blocking)

    def test_a_declared_choice_or_one_with_a_value_is_no_point(self):
        rec = self.declare(**{"Cost centre": "Kestrel Freightways"})
        _, advisory, _ = records.presave(rec, [("Cost centre", "Kestrel Freightways")], unset=["Cost centre"])
        self.assertFalse(any("left unset" in a for a in advisory), advisory)
        rec = self.declare()
        _, advisory, _ = records.presave(rec, [("Cost centre", "Head office")], unset=[])
        self.assertFalse(any("left unset" in a for a in advisory), advisory)

    def test_a_required_looking_label_plays_no_part(self):
        rec = self.declare()
        _, advisory, _ = records.presave(rec, [], unset=["Branch", "Cost centre *", "Project (optional)"])
        point = next(a for a in advisory if "left unset" in a)
        for label in ("Branch", "Cost centre *", "Project (optional)"):
            self.assertIn(label, point)

    def test_the_readers_report_an_unset_choice_by_structure(self):
        self.assertIn("o.value === ''", cdp.FORM_FIELDS)
        self.assertIn("unset.push", cdp.FORM_FIELDS)
        page = {"elements": [{"ref": "e14", "role": "combobox", "name": "Cost centre", "value": "", "group": "g"},
                             {"ref": "e15", "role": "textbox", "name": "Reference", "value": "", "group": "g"},
                             {"ref": "e16", "role": "combobox", "name": "Vendor", "value": "Kestrel Freightways",
                              "group": "g"}]}
        self.assertEqual(core._page_fields(page, {"group": "g"})["unset"], ["Cost centre"])

    def test_the_point_is_shown_with_the_save_and_kept_in_the_ledger(self):
        self.declare()
        form = [("Bill number", "KT-50833"), ("Vendor", "Kestrel Freightways"), ("Total", "2,755.30"), ("Notes", NOTES)]
        held, clean, lines = core._ledger_check(form, unset=["Cost centre"])
        self.assertEqual(held, "")
        self.assertTrue(any("Cost centre is left unset" in line for line in lines), lines)
        rec = records.open_record(records.load())
        self.assertEqual(rec["unset"], ["Cost centre"])
        shown = records.readback(rec, "Bill KT-50833", "KT-50833 Kestrel Freightways 2,755.30 " + NOTES, "saved")
        self.assertTrue(any(s.startswith("Left unset at this save: Cost centre") for s in shown), shown)
        listed = [line for line in records.listing() if "left unset" in line]
        self.assertEqual(len(listed), 1)
        self.assertNotIn(": ", listed[0])  # (the soul reads "    Label: value" lines as declared fields)


if __name__ == "__main__":
    unittest.main()
