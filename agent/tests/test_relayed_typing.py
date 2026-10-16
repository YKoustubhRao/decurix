"""A field in a viewer of an app running elsewhere is not judged by its value right after typing."""

import unittest

from decurix_agent.computer import core


class AViewerOfAnotherApp(unittest.TestCase):
    def test_a_host_app_field_is_relayed(self):
        # Found live (2026-10-15): real Excel's cells were typed twice ("TotalTotal").
        obs = {"groups": [{"id": "cdp:1", "url": "http://decurix:8080/host-gui/view?id=office-excel"}]}
        self.assertTrue(core._relayed({"source": "cdp", "group": "cdp:1"}, obs))

    def test_a_web_page_field_is_not(self):
        obs = {"groups": [{"id": "cdp:1", "url": "http://host.docker.internal:8000/app/sales-order/new"}]}
        self.assertFalse(core._relayed({"source": "cdp", "group": "cdp:1"}, obs))
        self.assertFalse(core._relayed({"source": "atspi", "group": "x"}, obs))


if __name__ == "__main__":
    unittest.main()


class KeysToAnotherAppAreNotInsertedAgain(unittest.TestCase):
    def test_a_viewer_line_is_typed_once(self):
        from unittest import mock
        from decurix_agent.computer import cdp

        calls = []

        class FakePage:
            target = {"url": "http://decurix:8080/host-gui/view?id=office-excel"}

            def call(self, method, params=None):
                calls.append(method)
                return {}

        with mock.patch.object(cdp, "_field_value", return_value=""):
            cdp._type_line(FakePage(), "obj", "Total")
        self.assertNotIn("Input.insertText", calls)
        self.assertEqual(calls.count("Input.dispatchKeyEvent"), 10)

    def test_a_web_field_that_ignored_its_keys_still_gets_the_text(self):
        from unittest import mock
        from decurix_agent.computer import cdp

        calls = []

        class FakePage:
            target = {"url": "http://host.docker.internal:8000/app/sales-order/new"}

            def call(self, method, params=None):
                calls.append(method)
                return {}

        with mock.patch.object(cdp, "_field_value", return_value=""):
            cdp._type_line(FakePage(), "obj", "Total")
        self.assertIn("Input.insertText", calls)
