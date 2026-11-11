"""
Every action that commits what a form or a page holds goes through the same pre-save checks (core._commit_check ->
_form_check: a record entered twice, a placeholder, an invented value, the declared record's ledger, the listing of a
full form), whichever way it is pressed. Found in review (2026-11-02): they ran only for a single `gui click` on a CDP
<form type=submit>, so a double-click, --see, the act tool (the MCP server, removed 2026-11-07), an AT-SPI press, an Enter on a filled form and every button
an application draws with divs went past them.
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp, cli, core

PLACEHOLDER = [["Customer", "Cafe Bloom"], ["Customer PO number", "TEST"], ["Quantity", "2"]]
FULL = [["Customer", "Cafe Bloom"], ["Customer PO number", "WA-1810"], ["Quantity", "2"], ["Rate", "149"]]


def field(name, value, group="g", source="cdp", role="textbox"):
    return {"role": role, "name": name, "value": value, "group": group, "source": source, "states": [],
            "window_title": "New order - Chromium", "path": f"form > {role} {name}"}


def page(fields, group="g", source="cdp"):
    return {"elements": [field(n, v, group=group, source=source) for n, v in fields], "page_url": "https://app.test/o/new",
            "groups": [{"id": group, "window_title": "New order - Chromium"}], "active_title": "New order - Chromium"}


class Base(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(core, "FORM_CHECK_FILE", Path(tmp.name) / "form-check.json"),
                  mock.patch.object(core, "_unsourced_fields", lambda fields: []),
                  mock.patch.object(core, "_ledger_check", lambda fields, **_: None),
                  mock.patch.object(core, "_relayed_page", lambda before: False)):
            p.start()
            self.addCleanup(p.stop)


class EveryPressThatCommits(Base):
    def test_a_double_click_on_a_forms_submit_is_checked(self):
        save = {"ref": "e2", "role": "button", "name": "Save order", "path": "/form/button", "source": "cdp", "group": "g"}
        with mock.patch.object(cdp, "submit_form_fields", return_value={"form": "New order", "fields": PLACEHOLDER}):
            said = core._commit_check("double_click", save, None, page([]))
        self.assertIn("Customer PO number: TEST", said)

    def test_a_button_drawn_with_divs_is_checked_against_what_the_window_holds(self):
        # No <form> around it (an application drawn with divs and spans): its label says it saves.
        save = {"ref": "e2", "role": "button", "name": "Save", "path": "div > button Save", "source": "cdp", "group": "g"}
        with mock.patch.object(cdp, "submit_form_fields", return_value=None):
            self.assertIn("Customer PO number: TEST", core._commit_check("click", save, None, page(PLACEHOLDER)))
            # A full form is listed first, and sent with --checked.
            listed = core._commit_check("click", save, None, page(FULL))
            self.assertIn("Not pressed yet", listed)
            self.assertEqual(core._commit_check("click", save, None, page(FULL), checked=True), "")
            # A control that commits nothing is not checked.
            nxt = dict(save, name="Next")
            self.assertEqual(core._commit_check("click", nxt, None, page(PLACEHOLDER)), "")

    def test_an_atspi_press_is_checked(self):
        submit = {"ref": "e7", "role": "button", "name": "Submit", "source": "atspi", "group": "atspi:app:0:Order",
                  "window_title": "Order"}
        said = core._commit_check("click", submit, None, page(PLACEHOLDER, group="atspi:app:0:Order", source="atspi"))
        self.assertIn("Customer PO number: TEST", said)

    def test_secrets_and_file_names_are_not_what_a_page_saves(self):
        fields = page([["Name", "report_jan.xlsx"], ["Customer", "Cafe Bloom"]])
        fields["elements"].append(dict(field("Password", "hunter22"), role="textbox", states=["protected"]))
        held = core._page_fields(fields, {"group": "g"})["fields"]
        self.assertEqual(held, [["Customer", "Cafe Bloom"]])

    def test_enter_in_a_forms_field_is_checked(self):
        box = field("Customer PO number", "TEST")
        with mock.patch.object(cdp, "enter_form_fields", return_value={"form": "New order", "fields": PLACEHOLDER,
                                                                       "submit": "Save order"}):
            self.assertIn("Customer PO number: TEST", core._commit_check("key", box, ["Return"], page([])))
        # A full form is never sent by an Enter: its own submit control lists it.
        with mock.patch.object(cdp, "enter_form_fields", return_value={"form": "New order", "fields": FULL,
                                                                       "submit": "Save order"}):
            said = core._commit_check("key", box, ["Return"], page([]))
            self.assertIn("press the form's own submit control", said)
        # Enter outside a form, or a small form (a search, a sign-in): sent.
        with mock.patch.object(cdp, "enter_form_fields", return_value=None):
            self.assertEqual(core._commit_check("key", box, ["Return"], page([])), "")
        with mock.patch.object(cdp, "enter_form_fields", return_value={"form": "Search", "fields": [["q", "mugs"]]}):
            self.assertEqual(core._commit_check("key", box, ["Return"], page([])), "")

    def test_a_new_line_typed_into_a_forms_field_is_an_enter_with_what_it_typed(self):
        box = field("Customer PO number", "")
        with mock.patch.object(cdp, "enter_form_fields", return_value={"form": "New order",
                                                                       "fields": [["Customer", "Cafe Bloom"]]}):
            said = core._commit_check("type", box, None, page([]), text="TEST\n")
        self.assertIn("Customer PO number: TEST", said)

    def test_ctrl_s_on_a_web_page_is_checked_without_a_listing(self):
        self.assertIn("Customer PO number: TEST", core._commit_check("key", None, ["ctrl+s"], page(PLACEHOLDER)))
        self.assertEqual(core._commit_check("key", None, ["ctrl+s"], page(FULL)), "")


class EveryWayOfPressing(unittest.TestCase):
    """The gui command asks for the checks on every press but a right-click."""

    def run_cli(self, argv):
        with mock.patch.object(cli.core, "act", return_value={"status": "done", "text": "ok"}) as act, \
                mock.patch.object(cli, "_refuse_native_dropdown", lambda ref: None):
            cli.run(argv)
        return act.call_args.kwargs

    def test_the_gui_command(self):
        self.assertIs(self.run_cli(["double-click", "e5"])["check_form"], True)
        self.assertEqual(self.run_cli(["double-click", "e5", "--checked"])["check_form"], "checked")
        self.assertIs(self.run_cli(["click", "--see", "the Save button"])["check_form"], True)
        self.assertIs(self.run_cli(["key", "Return"])["check_form"], True)
        self.assertIs(self.run_cli(["type", "e5", "hello"])["check_form"], True)
        self.assertIs(self.run_cli(["right-click", "e5"])["check_form"], False)


if __name__ == "__main__":
    unittest.main()
