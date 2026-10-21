"""
Finding AL (2026-10-20, P2 on a portal): a unit price typed into one line of an order form "and Enter" submitted the
whole form - an order saved with one line of several. Enter in a form's single-line input is held back while the form
still has empty fields; the operator is told to move on with Tab or a click. Stand-in pages only, never a browser.
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp, core


class FormPage:
    """A stand-in page: one focused line field of a form; `submits` is what the form check answers."""

    def __init__(self, submits):
        self.submits, self.value, self.keys = submits, "", []

    def call(self, method, params=None):
        params = params or {}
        if method == "Input.dispatchKeyEvent" and params.get("type") == "keyDown":
            if params.get("key") == "Enter":
                self.keys.append("Enter")
            elif params.get("text"):
                self.value += params["text"]
        elif method == "Input.insertText":
            self.value += params["text"]
        elif method == "Runtime.callFunctionOn":
            declaration = params.get("functionDeclaration", "")
            if declaration == cdp.ENTER_SUBMITS:
                return {"result": {"value": self.submits}}
            if "multiline" in declaration:
                return {"result": {"value": {"multiline": False, "input": "text"}}}
            return {"result": {"value": self.value}}
        elif method == "Runtime.evaluate":
            return {"result": {"objectId": "focused"}}
        return {}

    def close(self):
        pass


def type_into(page, text):
    with mock.patch.object(cdp, "_page_for", return_value=page), mock.patch.object(cdp, "_object", return_value="obj"):
        cdp.type_text({"handle": {"backend_id": 7}}, text)


class EnterInAForm(unittest.TestCase):
    def test_the_live_case_types_the_price_and_holds_the_enter(self):
        page = FormPage({"form": "New sales order", "empty": 6})
        with self.assertRaises(cdp.EnterWouldSubmit) as caught:
            type_into(page, "4.20\n")
        self.assertEqual(page.value, "4.20")
        self.assertEqual(page.keys, [])
        said = str(caught.exception)
        for want in ("Typed '4.20' without the Enter", "would submit the form 'New sales order' now",
                     "6 other field(s) still empty", "Tab or click the next field",
                     "press the form's submit button when every field is filled"):
            self.assertIn(want, said)

    def test_enter_goes_through_where_it_submits_nothing_early(self):
        # Outside a form, a search box, a textarea, or a form with every field filled: the check answers None.
        page = FormPage(None)
        type_into(page, "Cafe Bloom\n")
        self.assertEqual((page.value, page.keys), ("Cafe Bloom", ["Enter"]))

    def test_text_without_enter_is_never_checked(self):
        page = FormPage({"form": "x", "empty": 1})
        type_into(page, "4.20")
        self.assertEqual((page.value, page.keys), ("4.20", []))

    def test_gui_key_enter_in_such_a_field_presses_nothing(self):
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "focused_enter_would_submit", return_value={"form": "New sales order", "empty": 3}), \
                mock.patch.object(core.xinput, "key") as key:
            with self.assertRaises(cdp.EnterWouldSubmit) as caught:
                core._perform("key", None, None, None, ["enter"], None, None, None, False)
        key.assert_not_called()
        self.assertIn("Enter was not pressed: Enter would submit the form 'New sales order'", str(caught.exception))
        # Any other key, or Enter where it submits nothing early, is pressed as always.
        with mock.patch.object(cdp, "available", return_value=True), \
                mock.patch.object(cdp, "focused_enter_would_submit", return_value=None), \
                mock.patch.object(core.xinput, "key") as key:
            core._perform("key", None, None, None, ["enter"], None, None, None, False)
            core._perform("key", None, None, None, ["tab"], None, None, None, False)
        self.assertEqual(key.call_count, 2)

    def test_the_page_check_is_precise(self):
        js = cdp.ENTER_SUBMITS
        for rule in ("el.tagName !== 'INPUT'", "'search'", "searchbox", "if (!form) return null", "if (!submit) return null",
                     "if (!empty) return null", "f.disabled || f.readOnly", "visible(f)"):
            self.assertIn(rule, js)


if __name__ == "__main__":
    unittest.main()
