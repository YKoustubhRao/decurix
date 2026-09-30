"""
Teaching and running a workflow that is driven by a document each time: a
demonstration types several values (a customer, a PO number, ...), one
question settles which of them change, and a run fills the ones it was not
given from the run's documents (binding.py) - asking only for what they do
not show. No display, no model: the model's answer is scripted.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import tempfile
import unittest
from pathlib import Path
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
from test_recordings import event, field
from test_skills import TeachingBase

from decurix_agent import binding, skill_task
from decurix_agent.computer import learning, skills


def po_demo(session="demo-20260920T090000-cccccc", customer="Deco Addict", po="DA-PO-7731", warehouse="Main"):
    """A demonstration: open Sales, type the customer, the PO number and the warehouse, save."""

    events = [
        event(0, "click", field("link", "Sales"), expect={"appeared": [{"role": "heading", "name": "Quotations"}]}),
        event(1, "type", field("textbox", "Customer"), text=customer,
              expect={"values": [{"role": "textbox", "name": "Customer", "value": customer}]}),
        event(2, "type", field("textbox", "Customer Reference"), text=po,
              expect={"values": [{"role": "textbox", "name": "Customer Reference", "value": po}]}),
        event(3, "type", field("textbox", "Warehouse"), text=warehouse,
              expect={"values": [{"role": "textbox", "name": "Warehouse", "value": warehouse}]}),
        event(4, "click", field("button", "Save"), expect={"appeared": [{"role": "alert", "name": "Saved"}]}),
    ]
    meta = {"id": session, "title": "Enter a customer PO", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "actors": ["human"], "screen": [1280, 720]}
    return meta, events


def answer_until(skill, kind):
    """Answer the questions before the first of `kind` as a person would; return that question."""

    replies = {"goal": "the quotation is saved", "start": "no"}
    while (question := learning.next_question(skill)) is not None and question["kind"] != kind:
        learning.answer(skill, question, replies[question["kind"]])
    return question


class GroupedValuesQuestion(unittest.TestCase):
    def setUp(self):
        self.skill = learning.build([po_demo()])
        answer_until(self.skill, "variables")

    def test_every_typed_value_is_asked_about_in_one_question(self):
        question = learning.next_question(self.skill)
        self.assertEqual(question["kind"], "variables")
        self.assertEqual(len(question["values"]), 3)
        self.assertIn("Deco Addict", question["text"])
        self.assertIn("DA-PO-7731", question["text"])

    def test_all_change_except_one(self):
        question = learning.next_question(self.skill)
        result = learning.answer(self.skill, question, "all change except 3")
        self.assertTrue(result["understood"])

        by_label = {p["label"]: p for p in self.skill["params"]}
        self.assertTrue(by_label["Customer"]["varies"])
        self.assertTrue(by_label["Customer Reference"]["varies"])
        self.assertFalse(by_label["Warehouse"]["varies"])
        self.assertEqual(by_label["Warehouse"]["default"], "Main")

        # Kept per step, so a lesson resumed later asks only about the rest.
        self.assertEqual(sum(1 for k in self.skill["answers"] if k.startswith("variable:")), 3)
        self.assertEqual(learning.next_question(self.skill)["kind"], "confirm")

    def test_named_ones_change_and_the_rest_stay(self):
        question = learning.next_question(self.skill)
        choices = learning._variable_choices(question, "1 and 2 change")
        ids = [v["id"] for v in question["values"]]
        self.assertEqual([choices[i] for i in ids], ["changes", "changes", "same"])

    def test_plain_answers_and_unreadable_ones(self):
        question = learning.next_question(self.skill)
        ids = [v["id"] for v in question["values"]]
        self.assertEqual(set(learning._variable_choices(question, "all change").values()), {"changes"})
        self.assertEqual(set(learning._variable_choices(question, "they all vary").values()), {"changes"})
        self.assertEqual(set(learning._variable_choices(question, "all the same").values()), {"same"})
        self.assertEqual(learning._variable_choices(question, "all change except 2 same")[ids[1]], "same")
        self.assertIsNone(learning._variable_choices(question, "banana"))
        self.assertIsNone(learning._variable_choices(question, "all change except 7"))

    def test_an_unclear_reply_twice_takes_the_cautious_answer(self):
        question = learning.next_question(self.skill)
        self.assertFalse(learning.answer(self.skill, question, "hmm")["understood"])
        result = learning.answer(self.skill, question, "hmm")
        self.assertTrue(result["understood"])
        self.assertTrue(all(p["varies"] for p in self.skill["params"]))

    def test_a_single_typed_value_is_still_asked_on_its_own(self):
        meta, events = po_demo()
        events = [e for e in events if (e.get("element") or {}).get("name") not in ("Customer Reference", "Warehouse")]
        skill = learning.build([(meta, events)])
        self.assertIsNotNone(answer_until(skill, "variable"))


class ValuesFromDocuments(unittest.TestCase):
    def test_only_asked_params_in_the_shape_each_takes(self):
        params = [{"name": "customer", "label": "Customer"}, {"name": "qty", "label": "Qty", "list": True}]
        values, evidence = binding._clean(params, {
            "values": {"customer": " Deco Addict ", "qty": [2, "5", " 1"], "extra": "x"},
            "evidence": {"customer": "PO header, 'Bill to'"},
        })
        self.assertEqual(values, {"customer": "Deco Addict", "qty": "2; 5; 1"})
        self.assertEqual(evidence, {"customer": "PO header, 'Bill to'"})

    def test_a_list_item_holding_the_separator_is_left_out(self):
        params = [{"name": "item", "label": "Item", "list": True}]
        self.assertEqual(binding._clean(params, {"values": {"item": ["a;b", "c"]}})[0], {})

    def test_files_are_the_existing_ones_the_request_names(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as tmp:
            po = Path(tmp) / "PO.pdf"
            po.write_bytes(b"%PDF")
            text = f"Files attached to the email:\n  {po}\n  {tmp}/missing.pdf\nsee {po} again."
            self.assertEqual(binding.referenced_files(text), [str(po)])

    def test_no_model_fills_nothing(self):
        with mock.patch.object(binding.llm_client, "available", return_value=False):
            self.assertEqual(binding.fill({"name": "x"}, [{"name": "a"}], "context"), ({}, {}))


class RunFilledFromTheDocument(TeachingBase):
    """/skill with an email's worth of context: the missing value is read, the run goes on, the report says where from."""

    def teach_po_skill(self):
        self.record(session=self.SESSION, made=po_demo(session=self.SESSION))
        skill_task.perform(f"/learn {self.SESSION}")
        for reply in ("the quotation is saved", "no", "all change except 3", "yes"):
            skill_task.resume(reply)
        return skills.find("enter a customer po")[0]

    def test_values_come_from_the_documents_and_the_report_says_where(self):
        skill = self.teach_po_skill()
        self.assertIsNotNone(skill)
        self.finals.clear()
        self.asked.clear()

        from test_recordings import FakeScreen
        from decurix_agent.computer import replay
        elements = [field("link", "Sales"), field("textbox", "Customer"), field("textbox", "Customer Reference"),
                    field("textbox", "Warehouse"), field("button", "Save")]
        fake = FakeScreen(elements, after={"adds": [field("heading", "Quotations"), field("alert", "Saved")]})
        replay.core = skill_task.core = fake

        names = {p["label"]: p["name"] for p in skill["params"]}
        read = ({names["Customer"]: "Wood Corner", names["Customer Reference"]: "WC-PO-118"},
                {names["Customer"]: "PO.pdf, Bill To", names["Customer Reference"]: "PO.pdf, 'PO Number'"})

        with mock.patch.object(binding, "available", return_value=True), \
                mock.patch.object(binding, "fill", return_value=read) as fill:
            skill_task.perform(f"/skill {skill['name']}\n\nFiles attached to the email:\n  /home/headless/work/inbox/mjob_1/PO.pdf")

        fill.assert_called_once()
        self.assertIn("PO.pdf", fill.call_args.args[2])
        self.assertFalse([q for q in self.asked if "I need" in q[0]], "a value was asked for")
        typed = [kwargs.get("text") for kind, kwargs in fake.acted if kind == "type"]
        self.assertIn("Wood Corner", typed)
        self.assertIn("WC-PO-118", typed)
        self.assertIn("Main", typed)  # the value that stays, as taught
        self.assertIn("Values read from the documents", self.finals[-1][0])
        self.assertIn("Bill To", self.finals[-1][0])

    def test_what_the_documents_do_not_show_is_asked_for(self):
        skill = self.teach_po_skill()
        names = {p["label"]: p["name"] for p in skill["params"]}
        self.asked.clear()

        with mock.patch.object(binding, "available", return_value=True), \
                mock.patch.object(binding, "fill", return_value=({names["Customer"]: "Wood Corner"}, {})):
            skill_task.perform(f"/skill {skill['name']}\nsee ~/work/PO.pdf")

        self.assertTrue(self.asked, "the missing PO number was not asked for")
        self.assertIn(names["Customer Reference"], self.asked[-1][0])
        self.assertNotIn(names["Customer"] + " (", self.asked[-1][0])


class ValuesAreSpelledAsTheDocumentWritesThem(unittest.TestCase):
    PO = "WOOD CORNER\nPURCHASE ORDER\nPO Number: WC-PO-118\nAuthorised by Purchasing, Wood Corner"

    def test_a_letterhead_in_capitals_is_spelled_as_the_document_writes_it(self):
        # Found live: the quotation was saved for "WOOD CORNER".
        self.assertEqual(binding.as_written("WOOD CORNER", "Deco Addict", [self.PO]), "Wood Corner")

    def test_a_value_only_ever_written_in_capitals_is_kept(self):
        self.assertEqual(binding.as_written("WC-PO-118", "DA-PO-7731", [self.PO]), "WC-PO-118")
        self.assertEqual(binding.as_written("PURCHASE ORDER", "Deco Addict", [self.PO]), "PURCHASE ORDER")

    def test_capitals_the_example_also_had_are_kept(self):
        self.assertEqual(binding.as_written("WOOD CORNER", "DECO ADDICT", [self.PO]), "WOOD CORNER")

    def test_part_of_a_longer_word_is_not_a_spelling(self):
        self.assertEqual(binding.as_written("WOOD", "Oak", ["Woodland Ltd\nWOOD"]), "WOOD")

    def test_the_clean_step_applies_it_to_single_and_list_values(self):
        params = [{"name": "customer", "example": "Deco Addict"},
                  {"name": "product", "example": "Large Cabinet", "list": True}]
        values, _ = binding._clean(params, {"values": {"customer": "WOOD CORNER",
                                                        "product": ["LARGE DESK", "Office Chair"]}},
                                   [self.PO + "\n1 Large Desk 4\n2 Office Chair 10"])
        self.assertEqual(values, {"customer": "Wood Corner", "product": "Large Desk; Office Chair"})


if __name__ == "__main__":
    unittest.main()


def onboarding_demo(session, offer="/home/headless/work/new/Offer_Ananya.pdf", name="Ananya"):
    """A demonstration: type the new joiner's name, upload her offer letter, save."""

    events = [
        event(0, "type", field("textbox", "First Name"), text=name,
              expect={"values": [{"role": "textbox", "name": "First Name", "value": name}]}),
        dict(event(1, "upload", field("button", "Browse")), files=[offer]),
        event(2, "click", field("button", "Save"), expect={"appeared": [{"role": "alert", "name": "Saved"}]}),
    ]
    meta = {"id": session, "title": "Onboard an employee", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "actors": ["human"], "screen": [1280, 720]}
    return meta, events


class UploadedFiles(unittest.TestCase):
    def test_one_demonstrations_file_is_asked_about_with_the_typed_values(self):
        skill = learning.build([onboarding_demo("demo-20260921T090000-dddddd")])
        question = answer_until(skill, "variables")
        self.assertIsNotNone(question)
        self.assertIn("the file", question["text"])
        self.assertIn("Offer_Ananya.pdf", question["text"])

        learning.answer(skill, question, "all change")
        upload = next(p for p in skill["params"] if p.get("file"))
        self.assertTrue(upload["varies"])
        self.assertIsNone(upload["default"])

    def test_a_file_the_same_every_time_stays(self):
        skill = learning.build([onboarding_demo("demo-20260921T090000-dddddd")])
        question = answer_until(skill, "variables")
        learning.answer(skill, question, "1 changes")
        upload = next(p for p in skill["params"] if p.get("file"))
        self.assertFalse(upload["varies"])
        self.assertEqual(upload["default"], "/home/headless/work/new/Offer_Ananya.pdf")

    def test_two_demonstrations_with_different_files_learn_that_it_changes(self):
        skill = learning.build([onboarding_demo("demo-20260921T090000-dddddd"),
                                onboarding_demo("demo-20260921T091500-eeeeee", offer="/home/headless/work/new/Offer_Ravi.pdf",
                                                name="Ravi")])
        upload = next(p for p in skill["params"] if p.get("file"))
        self.assertTrue(upload["varies"])

    def test_several_files_given_as_one_value_are_split(self):
        from test_recordings import FakeScreen
        from decurix_agent.computer import replay

        fake = FakeScreen([field("button", "Browse")])
        saved = replay.core
        replay.core = fake
        try:
            step = {"n": 1, "kind": "upload", "param": "docs", "target": {"role": "button", "name": "Browse",
                    "path": field("button", "Browse")["path"]}, "expect": []}
            replay._run_step(step, {"docs": "/w/a.pdf; /w/b.png"})
        finally:
            replay.core = saved
        self.assertEqual(fake.acted[-1][1]["files"], ["/w/a.pdf", "/w/b.png"])


class ListValuesAgainstTheirField(unittest.TestCase):
    def test_each_item_of_a_list_is_judged_not_the_whole(self):
        from decurix_agent.computer import constraints

        constraint = constraints.infer("Quantity", "spinbutton", "2")  # what learning records for the demo
        self.assertTrue(constraint and constraint["strict"])
        qty = {"name": "quantity", "label": "Quantity", "list": True, "constraint": constraint}
        self.assertEqual(constraints.violations([qty], {"quantity": "1; 4; 10"}, split=skills.split_items), [])
        wrong = constraints.violations([qty], {"quantity": "1; four; 10"}, split=skills.split_items)
        self.assertEqual(len(wrong), 1)
        self.assertIn("item 2", wrong[0][1])
