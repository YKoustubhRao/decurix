"""
A field with a <datalist>: the browser draws its list outside the page, so a search sees nothing appear. Found live
(2026-10-26): `gui select` on such a field searched "Sales@18%", then its shorter searches, and left "Sales@18" in the
field; the application refused the save. The entries are now read through the page and the one asked for typed exactly.
"""

import unittest
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent.computer import cdp, core
from decurix_agent.computer.errors import ComputerError

FIELD = {"ref": "e839", "role": "combobox", "name": "Particulars 1", "source": "cdp", "handle": {"backend_id": 5}, "path": "p"}
ENTRIES = ["Sales@18%", "Sales@12%", "CGST@9%", "SGST@9%", "IGST@18%", "Coastal Builders", "Hebri Agro Industries"]


class TheDatalist(unittest.TestCase):
    def setUp(self):
        self.typed = []
        for p in (mock.patch.object(cdp, "datalist_options", lambda element: list(ENTRIES)),
                  mock.patch.object(cdp, "type_text", lambda element, text, replace=True: self.typed.append(text)),
                  mock.patch("decurix_agent.computer.resolve.note", lambda line: None)):
            p.start()
            self.addCleanup(p.stop)

    def test_the_exact_entry_is_typed_whole(self):
        self.assertEqual(core._select_datalist(FIELD, "Sales@18%"), "cdp")
        self.assertEqual(self.typed, ["Sales@18%"])
        self.assertEqual(core._selected["name"], "Sales@18%")

    def test_an_entry_whose_name_lies_within_the_value(self):
        self.assertEqual(core._select_datalist(FIELD, "Hebri Agro Industries (Udupi)"), "cdp")
        self.assertEqual(self.typed, ["Hebri Agro Industries"])

    def test_nothing_fits_says_what_the_list_offers(self):
        with self.assertRaises(ComputerError) as caught:
            core._select_datalist(FIELD, "Sales@28%")
        self.assertIn("offers no entry that is", str(caught.exception))
        self.assertIn("Sales@18%", str(caught.exception))
        self.assertEqual(self.typed, [])

    def test_a_field_without_a_list_is_not_this(self):
        with mock.patch.object(cdp, "datalist_options", lambda element: None):
            self.assertIsNone(core._select_datalist(FIELD, "Sales@18%"))

    def test_an_identifier_picks_the_entry_whatever_its_name(self):
        with mock.patch.object(cdp, "datalist_options", lambda element: ["Coastal Builders (29AAGFC5512K1ZC)", "Hebri Agro"]):
            self.assertEqual(core._select_datalist(FIELD, "Coastal Infra Projects", ids=["29AAGFC5512K1ZC"]), "cdp")
        self.assertEqual(self.typed, ["Coastal Builders (29AAGFC5512K1ZC)"])
        self.assertIn("matched", core._selected["why"])


if __name__ == "__main__":
    unittest.main()
