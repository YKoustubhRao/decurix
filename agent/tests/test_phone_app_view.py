import json
import unittest
from unittest.mock import MagicMock, patch

from decurix_agent.computer import approval, cdp


# A phone app reaches a desktop as the control plane's viewer page
# (web/templates/phoneapp.html): the video, with the app's own controls -
# read from Android's accessibility tree - laid over it as invisible buttons
# and fields. These tests are the desktop's half: that such a page is read
# only once it has caught up with the last input, and that what it lists is
# judged by the approval gate like any web page.

METRICS = json.dumps({
    "sx": 0, "sy": 0, "ow": 420, "oh": 860, "iw": 420, "ih": 816, "dpr": 1,
    "scx": 0, "scy": 0, "title": "WhatsApp", "url": "http://decurix:8080/phone-app/view?id=x",
})


def viewer_page(ready_answers):
    """A page whose readiness goes through ready_answers, holding one Send button."""

    answers = list(ready_answers)
    page = MagicMock()
    page.target = {"id": "t1", "title": "WhatsApp"}

    def evaluate(expression):
        if expression == cdp._READY:
            return answers.pop(0) if len(answers) > 1 else answers[0]
        return METRICS

    def call(method, params=None):
        if method == "Accessibility.getFullAXTree":
            return {"nodes": [
                {"nodeId": "1", "role": {"value": "RootWebArea"}, "name": {"value": "WhatsApp"}, "childIds": ["2"], "backendDOMNodeId": 1},
                {"nodeId": "2", "parentId": "1", "role": {"value": "button"}, "name": {"value": "Send"}, "childIds": [], "backendDOMNodeId": 5},
            ]}
        if method == "DOMSnapshot.captureSnapshot":
            return {"strings": [], "documents": [{
                "nodes": {"backendNodeId": [1, 5], "attributes": [[], []]},
                "layout": {"nodeIndex": [0, 1], "bounds": [[0, 0, 420, 816], [350, 700, 60, 60]]},
            }]}
        return {}

    page.evaluate.side_effect = evaluate
    page.call.side_effect = call
    return page


class TestPhoneAppViewer(unittest.TestCase):

    @patch("decurix_agent.computer.cdp.time.sleep")
    @patch("decurix_agent.computer.cdp.active_page")
    def test_the_page_is_read_after_it_caught_up_with_the_last_tap(self, mock_active, _sleep):
        page = viewer_page(["busy", "busy", "complete"])
        mock_active.return_value = page

        elements, group = cdp.observe()

        ready_checks = [c for c in page.evaluate.call_args_list if c[0][0] == cdp._READY]
        self.assertEqual(len(ready_checks), 3, "observe did not wait for the page to stop being busy")
        self.assertIn("data-decurix-busy", cdp._READY)

        send = next(e for e in elements if e["name"] == "Send")
        self.assertEqual(send["role"], "button")
        self.assertEqual(group["window_title"], "WhatsApp - Chromium")

    @patch("decurix_agent.computer.cdp.time.sleep")
    @patch("decurix_agent.computer.cdp.active_page")
    def test_a_phone_apps_send_waits_for_approval(self, mock_active, _sleep):
        mock_active.return_value = viewer_page(["complete"])
        elements, _ = cdp.observe()
        send = next(e for e in elements if e["name"] == "Send")

        self.assertIsNotNone(approval.assess("click", send))
        # Enter in the app's window sends what was typed: stopped too.
        self.assertIsNotNone(approval.assess("key", None, keys=["Enter"], context={"window": "WhatsApp - Chromium"}))


if __name__ == "__main__":
    unittest.main()
