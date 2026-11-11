"""
A save whose answer comes after the action's first look is still seen, and its record read back (core._await_save_answer,
core.pending_save_readback). Found live (2026-11-11, ERPNext, a bill's task): the look after Save caught only
`button "Save" [-] -> [disabled]` - the application disabled Save while its server worked, and the screen was quiet - and
the page moved to the saved record's own page (.../PINV-26-00002) a few seconds later. No read-back ran, the ledger kept
saved_at null, and the review said the bill was "declared from its document but never saved".

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_async_save_seen.py
"""

import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import config_stub  # noqa: F401

from decurix_agent.computer import approval, audit, core, records

TASK = "t7455c3b2178a35ee"
ALLOW = SimpleNamespace(allowed=True, approved=False)
NEW_URL = "http://localhost:8000/desk/purchase-invoice/new-purchase-invoice-opqhvrqdop"
RECORD_URL = "http://localhost:8000/desk/purchase-invoice/PINV-26-00002"
NEW_TITLE = "New Purchase Invoice - new-purchase-invoice-opqhvrqdop - Chromium"
RECORD_TITLE = "TechCorp Cloud Solutions India Pvt Ltd - PINV-26-00002 - Chromium"
SUPPLIER = "TechCorp Cloud Solutions India Pvt Ltd"


def look(url, title, save_states=(), tab="cdp:T1", saved_fields=True, extra=()):
    """One look at the bill's page: its Save button and the fields the bill was entered into."""

    def element(ref, role, name, value=None, states=()):
        return {"ref": ref, "role": role, "name": name, "value": value, "source": "cdp", "group": tab,
                "path": f"page > {role} {name}", "box": [10, 10, 60, 20], "states": list(states),
                "window_title": title, "handle": {"backend": int(ref[1:]), "target_id": tab[4:]}}

    elements = [element("e753", "button", "Save", states=save_states)]
    if saved_fields:
        elements += [element("e760", "combobox", "Supplier", SUPPLIER),
                     element("e761", "textbox", "Supplier Invoice No", "TC/2026/0455"),
                     element("e762", "textbox", "Supplier Invoice Date", "09-10-2026")]
    elements += list(extra)
    return {"elements": elements, "frame": None, "windows": [{"id": 1, "title": title, "box": [0, 0, 1280, 720],
                                                              "active": True}],
            "size": [1280, 720], "active_title": title, "page_url": url, "popups": [], "notes": [], "gone": {},
            "groups": [{"id": tab, "source": "cdp", "url": url, "window_title": title}]}


FORM = look(NEW_URL, NEW_TITLE)
IN_FLIGHT = look(NEW_URL, NEW_TITLE, save_states=["disabled"])
SAVED = look(RECORD_URL, RECORD_TITLE)


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        for p in (patch.object(records, "LEDGER_DIR", root / "ledger"),
                  patch.object(core, "PENDING_SAVE_FILE", root / "pending_save.json"),
                  patch.object(core, "STATE_DIR", root),
                  patch("decurix_agent.task_context.current", lambda: TASK),
                  patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"),
                  patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day"),
                  patch.object(core, "SAVE_ANSWER_POLL", 0), patch.object(core, "SAVE_SETTLE_SECONDS", 0.2)):
            p.start()
            self.addCleanup(p.stop)
        # Declared from the document while the list page was open, before the form existed (its window "").
        records._save(TASK, [{"key": "TC/2026/0455", "source": "bill09.pdf", "source_order": "day",
                              "declared_at": 1.0, "saved_at": None, "saved": "", "window": "", "readback": {},
                              "checks": 0, "fields": {"Supplier": {"intent": SUPPLIER},
                                                      "Supplier Invoice No": {"intent": "TC/2026/0455"},
                                                      "Supplier Invoice Date": {"intent": "09-10-2026"}}}])

    def act(self, looks, **kw):
        looks = list(looks)
        patches = [patch.object(core, "observe", side_effect=lambda *a, **k: looks.pop(0) if len(looks) > 1 else looks[0]),
                   patch.object(core, "_perform", return_value="cdp"),
                   patch.object(core, "_wait_for_settle"), patch.object(core, "RETYPE_PAUSE", 0),
                   patch.object(audit, "record"), patch.object(core, "_downloads_snapshot", return_value=None),
                   patch.object(core.cdp, "submit_form_fields", return_value=None),
                   patch.object(approval, "check", return_value=ALLOW)]
        for p in patches:
            p.start()
        try:
            return core.act(kw.pop("kind", "click"), ref=kw.pop("ref", "e753"), **kw)
        finally:
            for p in reversed(patches):
                p.stop()

    def saved_at(self):
        return records.load(TASK)[0].get("saved_at")


class ASaveAnsweredAfterItsFirstLook(Base):

    def test_the_live_shape_reads_the_record_back(self):
        # The first look: Save only turned disabled. The next: the record's own page, its values shown.
        result = self.act([FORM, IN_FLIGHT, SAVED, SAVED])
        self.assertIn("page is now " + RECORD_URL, result["text"])
        self.assertIn("Record TC/2026/0455: saved", result["text"])
        self.assertIn("3 of 3 declared value(s)", result["text"])
        self.assertTrue(self.saved_at(), "the ledger has the save")
        self.assertEqual(records.load(TASK)[0]["window"], RECORD_TITLE[:120])
        self.assertFalse(core.PENDING_SAVE_FILE.exists(), "an answered save is not left waiting")

    def test_a_save_that_only_disables_its_button_is_not_saved(self):
        result = self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        self.assertNotIn("Record TC/2026/0455: saved", result["text"])
        self.assertIsNone(self.saved_at())

    def test_a_refusal_that_comes_late_is_no_save(self):
        refused = look(NEW_URL, NEW_TITLE, extra=[{"ref": "e900", "role": "alert", "name": "Error: Supplier Invoice "
                                                   "Date is required", "source": "cdp", "group": "cdp:T1",
                                                   "path": "page > alert", "states": []}])
        result = self.act([FORM, IN_FLIGHT, refused, refused])
        self.assertIn("is required", result["text"])
        self.assertIsNone(self.saved_at())
        self.assertFalse(core.PENDING_SAVE_FILE.exists(), "a refused save waits for nothing")

    def test_a_press_that_is_not_a_save_is_not_waited_on(self):
        calls = []
        other = look(NEW_URL, NEW_TITLE, extra=[{"ref": "e800", "role": "button", "name": "Add Row", "source": "cdp",
                                                 "group": "cdp:T1", "path": "page > button Add Row", "states": []}])

        def seen(*a, **k):
            calls.append(k.get("remember", True))
            return other
        with patch.object(core, "observe", side_effect=seen), patch.object(core, "_perform", return_value="cdp"), \
                patch.object(core, "_wait_for_settle"), patch.object(audit, "record"), \
                patch.object(core, "_downloads_snapshot", return_value=None), \
                patch.object(approval, "check", return_value=ALLOW):
            core.act("click", ref="e800")
        self.assertEqual(calls, [True, True], "a look before and one after; no watching")


class ALaterLookAnswersIt(Base):

    def test_the_next_look_of_the_tab_on_the_record_reads_it_back(self):
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        self.assertIsNone(self.saved_at())
        self.assertEqual(core.pending_save_readback(IN_FLIGHT), "", "still on the form: it waits")
        note = core.pending_save_readback(SAVED)
        self.assertIn("is now " + RECORD_URL, note)
        self.assertIn("Record TC/2026/0455: saved", note)
        self.assertTrue(self.saved_at())
        self.assertEqual(core.pending_save_readback(SAVED), "", "read back once")

    def test_a_look_of_another_tab_keeps_it_waiting(self):
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        self.assertEqual(core.pending_save_readback(look(RECORD_URL, RECORD_TITLE, tab="cdp:T2")), "")
        self.assertIsNone(self.saved_at())
        self.assertIn("Record TC/2026/0455: saved", core.pending_save_readback(SAVED))

    def test_the_next_actions_look_reads_it_back(self):
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        result = self.act([SAVED, SAVED], ref="e761", kind="click")
        self.assertIn("Record TC/2026/0455: saved", result["text"])
        self.assertTrue(self.saved_at())

    def test_another_press_or_page_opened_in_between_lets_it_go(self):
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        self.act([IN_FLIGHT, IN_FLIGHT], ref="e761")  # a press on the form, still unanswered
        self.assertEqual(core.pending_save_readback(SAVED), "")
        self.assertIsNone(self.saved_at())
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        with patch.object(core.session, "ensure", side_effect=RuntimeError("stop here")):
            with self.assertRaises(RuntimeError):
                core.open_target(RECORD_URL)
        self.assertEqual(core.pending_save_readback(SAVED), "")
        self.assertIsNone(self.saved_at())

    def test_a_record_page_never_seen_on_the_form_is_not_this_save(self):
        # Moved to a page that names no record (the list): the save is let go, nothing read back.
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        listing = look("http://localhost:8000/desk/purchase-invoice", "Purchase Invoice - Chromium", saved_fields=False)
        self.assertEqual(core.pending_save_readback(listing), "")
        self.assertEqual(core.pending_save_readback(SAVED), "")
        self.assertIsNone(self.saved_at())


LIST_URL = "http://localhost:8000/desk/purchase-invoice?bill_no=SC/TAX/26-27/104"
LIVE_LIST_URL = ("http://localhost:8000/desk/purchase-invoice?bill_no=%5B%22like%22%2C%22%25SC%2FTAX%2F26-27%2F104"
                 "%25%22%5D")
LIST_TITLE = "Purchase Invoice - Chromium"
SAVED_FILTERS = {"ref": "e1753", "role": "button", "name": "Saved Filters", "source": "cdp", "group": "cdp:T1",
                 "path": "page > button Saved Filters", "box": [300, 40, 60, 20], "states": []}
BREADCRUMB = {"ref": "e1600", "role": "link", "name": "Purchase Invoice", "source": "cdp", "group": "cdp:T1",
              "path": "page > link Purchase Invoice", "box": [10, 40, 60, 20], "states": [], "handle": {"backend": 1600}}


class ANavigationIsNoSave(Base):
    """Found live (2026-11-11, ERPNext, bill 10): a click on the breadcrumb link "Purchase Invoice" on the unsaved bill's
    form went to the list ".../purchase-invoice?bill_no=SC/TAX/26-27/104", and the step said "Record ...: saved; the
    page now shows 0 of 3 declared value(s)" - the query's digits were taken for a record's id."""

    def ledger(self):
        rec = records.load(TASK)[0]
        return rec.get("saved_at"), rec.get("window"), rec.get("readback")

    def test_the_live_step_a_link_from_the_form_to_the_list_is_not_a_save(self):
        # As it was: the list's address with its filter encoded, and its "Saved Filters" button among what appeared.
        form = look(NEW_URL, NEW_TITLE, extra=[BREADCRUMB])
        listing = look(LIVE_LIST_URL, LIST_TITLE, saved_fields=False, extra=[SAVED_FILTERS])
        result = self.act([form, listing, listing], ref="e1600")
        self.assertIn('button "Saved Filters"', result["text"])
        self.assertNotIn("Record TC/2026/0455", result["text"])
        self.assertEqual(self.ledger(), (None, "", {}), "the ledger is unchanged")
        self.assertFalse(core.PENDING_SAVE_FILE.exists())

    def test_a_controls_label_is_no_saved_answer(self):
        self.assertFalse(core._saved('page is now ' + LIVE_LIST_URL + '; 48 element(s) appeared: button "List View" '
                                     '(e1751), button "Saved Filters" (e1753)'))
        self.assertTrue(core._saved('1 element(s) appeared: alert "Saved"'))
        self.assertTrue(core._saved('1 element(s) appeared: text "Purchase Invoice PINV-26-00002 saved"'))
        # ...whatever was pressed: a button that only shows the list is not a save either.
        form = look(NEW_URL, NEW_TITLE, extra=[dict(BREADCRUMB, role="button", ref="e1601", path="page > button PI")])
        listing = look(LIVE_LIST_URL, LIST_TITLE, saved_fields=False, extra=[SAVED_FILTERS])
        result = self.act([form, listing, listing], ref="e1601")
        self.assertNotIn("Record TC/2026/0455", result["text"])
        self.assertEqual(self.ledger(), (None, "", {}))

    def test_a_list_page_or_a_query_is_never_a_records_page(self):
        new = {"page_url": NEW_URL}
        self.assertFalse(core._became_a_record(new, "page is now " + LIST_URL))
        self.assertFalse(core._became_a_record(new, "", {"page_url": LIST_URL}))
        self.assertFalse(core._became_a_record(new, "", {"page_url": NEW_URL + "?bill_no=PINV-26-00002"}))
        self.assertFalse(core._became_a_record(new, "", {"page_url": "http://localhost:8000/desk/supplier/SUP-0001"}))
        self.assertFalse(core._became_a_record(new, "", {"page_url": "http://other.test/desk/purchase-invoice/PINV-26-00002"}))
        self.assertTrue(core._became_a_record(new, "", {"page_url": RECORD_URL}))
        self.assertTrue(core._became_a_record({"page_url": "http://books.test/#/v/new"}, "",
                                              {"page_url": "http://books.test/#/v/1054"}))

    def test_only_a_press_that_may_save_is_judged_by_the_page_it_left(self):
        self.assertFalse(core._commit_press("click", BREADCRUMB, []))
        self.assertFalse(core._commit_press("click", None, []))
        self.assertFalse(core._commit_press("key", None, ["alt+Left"]))
        self.assertTrue(core._commit_press("click", {"role": "button", "name": "Save"}, []))
        self.assertTrue(core._commit_press("click", {"role": "button", "name": "Create Invoice"}, []))
        self.assertTrue(core._commit_press("key", None, ["ctrl+s"]))
        self.assertTrue(core._commit_press("key", None, ["Return"]))

    def test_a_save_to_the_list_page_is_no_save_and_not_left_waiting_as_one(self):
        listing = look(LIST_URL, LIST_TITLE, saved_fields=False)
        result = self.act([FORM, listing, listing])
        self.assertNotIn("Record TC/2026/0455: saved", result["text"])
        self.assertEqual(self.ledger(), (None, "", {}))

    def test_a_records_page_that_shows_nothing_of_the_record_is_not_its_save(self):
        empty = look(RECORD_URL, RECORD_TITLE, saved_fields=False)
        result = self.act([FORM, empty, empty])
        self.assertIn("Record TC/2026/0455: NOT saved", result["text"])
        self.assertEqual(self.ledger(), (None, "", {}))

    def test_the_backstop_keeps_the_same_rules(self):
        self.act([FORM, IN_FLIGHT, IN_FLIGHT])
        self.assertEqual(core.pending_save_readback(look(LIST_URL, LIST_TITLE, saved_fields=False)), "")
        self.assertEqual(self.ledger(), (None, "", {}))

    def test_a_save_to_the_records_page_still_reads_back(self):
        result = self.act([FORM, SAVED, SAVED])
        self.assertIn("Record TC/2026/0455: saved; the page now shows 3 of 3", result["text"])
        self.assertTrue(self.ledger()[0])


if __name__ == "__main__":
    unittest.main()
