"""
What teaching and running a PO -> sales order skill on ERPNext (usecases/po-entry) found.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import json
import unittest
from unittest import mock

from test_recordings import FakeScreen  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent.computer import cdp, core, replay
from decurix_agent.computer.errors import ComputerError


class MadeUpNamesInTitles(unittest.TestCase):
    """ERPNext titles a new form with a name it makes up for it, every time."""

    def test_a_generated_name_is_not_something_to_wait_for(self):
        self.assertEqual(replay._stable_title("New Sales Order - new-sales-order-gwaniocbxa - Chromium"),
                         "New Sales Order - Chromium")

    def test_real_titles_are_kept_whole(self):
        title = "Premier League - Scores & Fixtures - Football - BBC Sport - Chromium"
        self.assertEqual(replay._stable_title(title), title)
        self.assertEqual(replay._stable_title("Rockford Tool & Die - SAL-ORD-2026-00001 - Chromium"),
                         "Rockford Tool & Die - Chromium")

    def test_the_check_mined_from_an_open_waits_for_what_comes_back(self):
        checks = replay.mine_checks({"url": "http://erpnext:8080/desk/sales-order/new-sales-order-gwaniocbxa",
                                     "active_window": "New Sales Order - new-sales-order-gwaniocbxa - Chromium"})
        self.assertIn({"type": "url", "value": "http://erpnext:8080/desk/sales-order/*"}, checks)
        self.assertIn({"type": "active_window", "value": "New Sales Order - Chromium"}, checks)


class FormattedValues(unittest.TestCase):

    def test_a_number_shown_formatted_was_kept(self):
        self.assertTrue(cdp._formatted("12", "12.000"))
        self.assertTrue(cdp._formatted("1200", "1,200"))
        self.assertFalse(cdp._formatted("10-12-2026", ""))
        self.assertFalse(cdp._formatted("12", "13.000"))


class FakeSocket:
    def __init__(self, messages):
        self.messages = list(messages)
        self.sent = []
        self.sock = mock.Mock()

    def send(self, text):
        self.sent.append(json.loads(text))

    def receive(self):
        if self.messages:
            return json.dumps(self.messages.pop(0))
        # Answer whatever was asked last, as Chromium does.
        return json.dumps({"id": self.sent[-1]["id"], "result": {}})


class FakePage(cdp.Page):
    def __init__(self, messages):
        self.target = {"id": "T1"}
        self.ws = FakeSocket(messages)
        self.seq = 0


class LeavingAPageWithChanges(unittest.TestCase):
    """Found on ERPNext: Page.navigate does not answer while "Leave site?" is open."""

    def test_the_question_is_answered_stay_and_the_open_says_why(self):
        page = FakePage([
            {"id": 1, "result": {}},                                     # Page.enable
            {"method": "Page.javascriptDialogOpening", "params": {"type": "beforeunload", "message": ""}},
        ])

        with self.assertRaises(ComputerError) as raised:
            cdp._navigate_or_refuse_to_leave(page, "http://erpnext:8080/app/sales-order/new")

        self.assertIn("unsaved changes", str(raised.exception))
        dialog = [m for m in page.ws.sent if m["method"] == "Page.handleJavaScriptDialog"]
        self.assertEqual(dialog[0]["params"], {"accept": False})   # stayed: nothing thrown away

    def test_a_page_with_nothing_to_lose_is_left(self):
        page = FakePage([{"id": 1, "result": {}}, {"method": "Page.frameNavigated", "params": {}},
                         {"id": 2, "result": {"frameId": "F"}}])

        self.assertEqual(cdp._navigate_or_refuse_to_leave(page, "http://erpnext:8080/app"), {"frameId": "F"})


class TextAFieldThrewAway(unittest.TestCase):
    """ERPNext's date field empties itself after text inserted in one piece; real keys stay."""

    def element(self):
        return {"source": "cdp", "role": "textbox", "name": "Delivery Date", "box": [610, 370, 270, 28],
                "handle": {"backend_id": 7}}

    def test_text_the_page_kept_is_typed_once(self):
        with mock.patch.object(cdp, "type_text", return_value=True) as typed, \
                mock.patch.object(core.xinput, "type_text") as keys:
            self.assertEqual(core._perform("type", self.element(), None, "10-12-2026", None, None, None, None, False),
                             "cdp")
        typed.assert_called_once()
        keys.assert_not_called()

    def test_text_the_page_threw_away_is_typed_again_with_real_keys(self):
        with mock.patch.object(cdp, "type_text", return_value=False), \
                mock.patch.object(cdp, "click") as clicked, \
                mock.patch.object(core, "_focus_window"), \
                mock.patch.object(core.time, "sleep"), \
                mock.patch.object(core.xinput, "key") as pressed, \
                mock.patch.object(core.xinput, "type_text") as keys:
            self.assertEqual(core._perform("type", self.element(), None, "10-12-2026", None, None, None, None, False),
                             "xdotool")
        clicked.assert_called_once()
        pressed.assert_called_once_with("ctrl+a")
        keys.assert_called_once_with("10-12-2026")


if __name__ == "__main__":
    unittest.main()
