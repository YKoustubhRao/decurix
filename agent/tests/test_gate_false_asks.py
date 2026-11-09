"""
The approval gate's false asks found on ERPNext bills (2026-11-08, O-11), each fixed by structure, never by a word
taken off a list, and the real commits beside them still asked:

- a button that opens a new, empty record ("+ Add Purchase Invoice") is let through only when the page says the press
  submits no form holding values (approval.opens_new_record, context "submits");
- a press found by eye is judged by the control the page names at the point, and its yes is bound to that control, not
  to the pixel (core._gate_spot) - three looks at one list row asked three times;
- after Ctrl+L and an address typed, Return is Return in the browser's address bar (core._location_bar), not a press
  "that cannot be told" on the page.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_gate_false_asks.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import approval, core


def button(name):
    return {"role": "button", "name": name, "path": "", "source": "cdp", "handle": {"backend_id": 7}}


class ANewRecordButton(unittest.TestCase):

    def test_opening_an_empty_record_is_let_through_only_when_the_page_says_it_sends_nothing(self):
        add = button("+ Add Purchase Invoice")
        self.assertTrue(approval.opens_new_record(add))
        self.assertIsNone(approval.assess("click", add, context={"submits": False}))
        self.assertIsNotNone(approval.assess("click", add, context={"submits": True}))
        self.assertIsNotNone(approval.assess("click", add, context={}))  # not read from the page: asked

    def test_the_label_shape_is_narrow(self):
        for name in ("New Purchase Invoice", "Create Purchase Order", "Make Payment Entry", "Add Purchase Receipt"):
            self.assertTrue(approval.opens_new_record(button(name)), name)
        for name in ("Purchase", "Buy now", "Pay", "Create Payment", "Add and pay", "Create and send invoice",
                     "Place order", "New payment - pay now", "Confirm Purchase Order", "Purchase Invoice"):
            self.assertFalse(approval.opens_new_record(button(name)), name)
        self.assertFalse(approval.opens_new_record({"role": "link", "name": "Add Purchase Invoice"}))

    def test_real_commits_still_ask(self):
        for name in ("Pay", "Buy now", "Purchase", "Place order", "Confirm Purchase Order",
                     "Send invoice", "Submit and pay", "+ Pay"):
            self.assertIsNotNone(approval.assess("click", button(name), context={"submits": False}), name)

    def test_the_page_is_asked_only_for_such_a_button(self):
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=None) as asked:
            self.assertFalse(core._press_submits("click", button("Add Purchase Invoice")))
        asked.assert_called_once()
        with mock.patch.object(core.cdp, "submit_form_fields", return_value=core.cdp.UNREADABLE):
            self.assertTrue(core._press_submits("click", button("Add Purchase Invoice")))
        with mock.patch.object(core.cdp, "submit_form_fields", return_value={"fields": [["Amount", "500"]]}):
            self.assertTrue(core._press_submits("click", button("Add Purchase Invoice")))
        self.assertIsNone(core._press_submits("click", {"role": "button", "name": "Add Purchase Invoice"}))


class APressFoundByEye(unittest.TestCase):

    BEFORE = {"page_url": "http://erp.test/desk/purchase-invoice", "active_title": "Purchase Invoice - Chromium",
              "elements": []}
    ROW = {"role": "link", "name": "Bharti Airtel Ltd - Enterprise Business", "source": "cdp",
           "handle": {"backend_id": 42}, "window_title": "Purchase Invoice - Chromium"}

    def test_the_page_names_the_control_and_the_yes_is_bound_to_it(self):
        described = "Bharti Airtel Ltd - Enterprise Business row in Purchase Invoice list"
        with mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "hit", return_value=(self.ROW, {"url": "x"})):
            spots = [core._gate_spot("click", None, p, described, self.BEFORE) for p in ((500, 320), (300, 300))]
        for spot, why, bound in spots:
            self.assertEqual(spot["name"], self.ROW["name"])
            self.assertIs(bound, spot)
            self.assertIsNone(approval.assess("click", spot))  # a link to a record: not a purchase
        prints = {approval._fingerprint("click", bound, None, None, p, context={"url": "u"})
                  for (_, _, bound), p in zip(spots, ((500, 320), (300, 300)))}
        self.assertEqual(len(prints), 1)  # one control, one yes - wherever the look put the point on it

    def test_with_nothing_named_there_the_description_is_judged_and_the_pixel_kept(self):
        with mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "hit", return_value=(None, {"url": "x"})), \
                mock.patch("decurix_agent.computer.ocr.available", return_value=False):
            spot, why, bound = core._gate_spot("click", None, (10, 10), "the Pay button", self.BEFORE)
        self.assertEqual(spot["name"], "the Pay button")
        self.assertIsNone(bound)
        self.assertIsNotNone(approval.assess("click", spot))


class TheAddressBar(unittest.TestCase):

    BEFORE = {"active_title": "Purchase Invoice - Chromium", "elements": []}

    def events(self, *events):
        return [dict(e, window=e.get("window", "Purchase Invoice - Chromium"), outcome="done") for e in events]

    def located(self, events, page_has=False):
        with mock.patch.object(core.browser, "is_browser_title", return_value=True), \
                mock.patch.object(core.cdp, "available", return_value=True), \
                mock.patch.object(core.cdp, "page_has_keyboard", return_value=page_has), \
                mock.patch("decurix_agent.computer.evidence._task_events", return_value=events):
            return core._location_bar(self.BEFORE)

    def test_after_ctrl_l_and_an_address_return_is_returns_in_the_address_bar(self):
        bar = self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                       {"kind": "type", "element": None, "target": "", "text": "erp.test/app/x"}))
        self.assertTrue(bar and bar["location_bar"])
        self.assertIsNone(approval.press_after_focus_move(["Delete", "Return"], bar))
        context = {"focused": bar, "window": "Purchase Invoice - Chromium", "url": "http://erp.test/app/x",
                   "controls": [{"role": "button", "name": "Pay", "path": ""}]}
        self.assertIsNone(approval.assess("key", None, keys=["Delete", "Return"], context=context))
        self.assertIsNone(core._pressed_by_key("key", None, ["Return"], self.BEFORE, focus=bar))

    def test_not_without_both_facts(self):
        typed = {"kind": "type", "element": None, "target": "", "text": "x"}
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]}, typed), page_has=True))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]}, typed), page_has=None))
        self.assertIsNone(self.located(self.events(typed)))  # nothing put it there
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                                   {"kind": "click", "element": {"role": "button"}}, typed)))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"]},
                                                   {"kind": "key", "keys": ["Tab"]}, typed)))
        self.assertIsNone(self.located(self.events({"kind": "key", "keys": ["ctrl+l"], "window": "Other - Chromium"},
                                                   typed)))

    def test_an_unknown_keyboard_still_asks(self):
        context = {"focused": None, "window": "Purchase Invoice - Chromium", "url": "http://erp.test/app/x",
                   "controls": [{"role": "button", "name": "Pay", "path": ""}]}
        self.assertIsNotNone(approval.assess("key", None, keys=["Tab", "Return"], context=context))


if __name__ == "__main__":
    unittest.main()
