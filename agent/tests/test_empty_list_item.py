"""
A field a document shows with its place empty - an inter-state bill's CGST and SGST rows, beside an intra-state bill
that fills them - takes the list item "(empty)" for that document, held by the document's own text
(binding.left_blank), and the replay leaves that field untyped for that item only (skills.expand). Found live
(2026-11-09): the empty rows of one bill sent a whole month of vouchers to the model by hand.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_empty_list_item.py
"""

import unittest

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import binding
from decurix_agent.computer import skills

INTRA = ("TAX INVOICE\nInvoice No : 1049\n[Table 3]\nBASIC PRICE | 12,000.00\nCGST @ 9% | 1,080.00\nSGST@9% | 1,080.00\n"
         "IGST @ 18% | \nGRAND TOTAL | 14,160.00\nTotal Invoice Amount In Words: Rupees Fourteen Thousand")
INTER = ("TAX INVOICE\nInvoice No : 1052\n[Table 3]\nBASIC PRICE | 50,000.00\nCGST @ 9% | \nSGST@9% | \n"
         "IGST @ 18% | 9,000.00\nGRAND TOTAL | 59,000.00\nTotal Invoice Amount In Words: Rupees Fifty Nine Thousand")


class ADocumentThatLeavesAFieldEmpty(unittest.TestCase):

    def test_the_empty_place_is_held_and_a_filled_one_is_not(self):
        self.assertTrue(binding.left_blank("CGST amount", INTER))
        self.assertTrue(binding.left_blank("IGST amount", INTRA))
        self.assertFalse(binding.left_blank("CGST amount", INTRA))
        self.assertFalse(binding.left_blank("IGST amount", INTER))

    def test_a_field_the_document_has_no_place_for_is_not_empty(self):
        self.assertFalse(binding.left_blank("Cess amount", INTRA))

    def test_only_a_row_with_marked_cells_proves_an_empty_place(self):
        # Review of freeze 2 (A11): a line whose cells the text does not mark is no proof - the value is read.
        laid_out = INTER.replace(" | ", "   ").replace(" |", "")
        self.assertFalse(binding.left_blank("CGST amount", laid_out))
        self.assertFalse(binding.left_blank("CGST", "| Tax | CGST @ 9%"))  # no cell after the word's own
        self.assertTrue(binding.left_blank("CGST", "| CGST @ 9% | |"))


def voucher_skill():
    steps = [{"n": 1, "id": "s1", "kind": "type", "param": "number", "target": {"role": "textbox", "name": "Voucher No."}},
             {"n": 2, "id": "s2", "kind": "type", "param": "cgst", "target": {"role": "textbox", "name": "CGST amount"}},
             {"n": 3, "id": "s3", "kind": "type", "param": "igst", "target": {"role": "textbox", "name": "IGST amount"}},
             {"n": 4, "id": "s4", "kind": "click", "target": {"role": "button", "name": "Save voucher"}}]
    params = [{"name": "number", "label": "Voucher No.", "list": True, "varies": True, "step": 1},
              {"name": "cgst", "label": "CGST amount", "list": True, "varies": True, "step": 2},
              {"name": "igst", "label": "IGST amount", "list": True, "varies": True, "step": 3}]
    return {"name": "vouchers", "steps": steps, "params": params,
            "loops": [{"id": "l1", "enabled": True, "blocks": [["s1", "s2", "s3", "s4"]]}]}


class TheReplayOfAnEmptyItem(unittest.TestCase):

    def test_types_nothing_into_that_field_for_that_document_only(self):
        program, values = skills.expand(voucher_skill(), {"number": "1049; 1052", "cgst": "1,080.00; (empty)",
                                                          "igst": "(empty); 9,000.00"})
        typed = [(s.get("param"), values.get(s.get("param"))) for s in program["steps"] if s["kind"] == "type"]
        self.assertEqual(typed, [("number__1", "1049"), ("cgst__1", "1,080.00"),
                                 ("number__2", "1052"), ("igst__2", "9,000.00")])
        self.assertEqual(len([s for s in program["steps"] if s["kind"] == "click"]), 2, "both vouchers are saved")
        self.assertEqual(skills.EMPTY_ITEM, binding.EMPTY_ITEM)

    def test_an_empty_item_breaks_no_number_constraint(self):
        # Found live (2026-11-09): the run read "(empty)" as "not a number", asked a person for CGST and SGST, and a
        # correct month was refused before its first step.
        from decurix_agent.computer import constraints
        param = {"name": "cgst", "label": "CGST amount", "list": True,
                 "constraint": {"type": "number", "strict": True}}
        self.assertEqual(constraints.violations([param], {"cgst": "1,080.00; (empty)"}, split=skills.split_items), [])
        self.assertTrue(constraints.violations([param], {"cgst": "1,080.00; none"}, split=skills.split_items))


if __name__ == "__main__":
    unittest.main()
