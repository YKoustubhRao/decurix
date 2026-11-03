"""
A secret field's value is withheld at the look itself (review 2026-11-03, D-18): no render of an observation, and no
form listing, holds it. The raw value stays only in the handle acting reads.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent.computer import cdp, core, elements as el


def field(name, value, **handle):
    return {"ref": "e3", "role": "textbox", "name": name, "value": value, "path": "p", "group": "g", "states": [],
            "source": "cdp", "handle": dict(handle)}


class TheLookWithholdsSecretValues(unittest.TestCase):
    def test_a_code_field_typed_as_text_is_withheld_where_it_is_read(self):
        found = [field("One-time code", "482913", input_type="text", autocomplete="one-time-code"),
                 field("Customer", "Vendor One")]
        core._withhold_secret_values(found)
        rendered = el.render({"size": [800, 600], "active_title": "x", "windows": [], "elements": found,
                              "groups": [{"id": "g", "title": "W"}]})
        self.assertNotIn("482913", rendered)
        self.assertIn("Vendor One", rendered)
        self.assertEqual(found[0]["value"], core.SECRET_VALUE)
        self.assertEqual(found[0]["handle"]["value"], "482913")

    def test_a_password_labelled_field_is_withheld(self):
        found = [field("Password", "plain-text-value")]
        core._withhold_secret_values(found)
        self.assertNotIn("plain-text-value", el.element_line(found[0]))


class TheFormListingLeavesSecretsOut(unittest.TestCase):
    def test_a_text_type_code_field_is_not_listed(self):
        page = mock.Mock()
        page.call.return_value = {"result": {"value": {
            "form": "Pay", "submit": "Pay",
            "fields": [["Amount", "500", "text", ""], ["One-time code", "482913", "text", "one-time-code"],
                       ["Payee", "Vendor One", "text", ""]]}}}
        with mock.patch.object(cdp, "_page_for", return_value=page), mock.patch.object(cdp, "_object", return_value="o"):
            found = cdp.submit_form_fields({"handle": {"backend_id": 1}}, min_fields=1)
        self.assertEqual(found["fields"], [["Amount", "500"], ["Payee", "Vendor One"]])


if __name__ == "__main__":
    unittest.main()
