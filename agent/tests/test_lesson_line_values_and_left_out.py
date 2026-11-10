"""
Two lesson bugs found live (2026-11-10, a supplier's bill entered as a draft purchase invoice, taught from one
demonstration of a bill of two lines):

1. A value typed alike on every line of one demonstration was kept as a constant, never asked. Both lines had a
   quantity of 1, so the quantity became "typed the same every time", the "which of these change?" question listed
   only the values outside the lines, and every later bill would have been entered with a quantity of 1. Now such a
   value is asked in that question ("same for every line, or read from each line?"); unanswered it stays a value read
   from each line, and only the person's explicit "same" makes it a constant (learning._apply_loop, _variables).

2. The person said "Step 20 (clicking 'Add multiple') and step 29 (the drag) were glitches ... skip them". It became a
   rule ("do not click the 'Add multiple' button") while both steps stayed in the program, so a replay still clicked
   them. Now the steps a reply says to leave out are mapped onto steps by number or recorded label (_steps_named), put
   back in one line, and taken out of the steps on a yes; the words stay as a note (learning._skips).

The live question, reply and steps 1-39 below are word for word from the lesson; steps 40-49 (not shown in the
lesson's first question) are the recording's own, in its order.

    cd agent/tests && PYTHONPATH=.. python3 -m unittest test_lesson_line_values_and_left_out
"""

import json
import unittest
from unittest import mock

import test_skills  # noqa: F401 - stubs decurix_agent.config first
from test_recordings import event, field

from decurix_agent import llm_client, study
from decurix_agent.computer import learning, skills

NO_MODEL = mock.patch.object(llm_client, "available", return_value=False)


# ---------------------------------------------------------------------------------------------------------------------
# 1. A value typed alike on every line
# ---------------------------------------------------------------------------------------------------------------------

SESSION = "demo-20261110T075458-17af9c"


def one_bill(lines=(("Cloud Compute", "1", "40000"), ("Managed Database", "1", "10000")), supplier="TechCorp"):
    """One demonstration: the supplier (when given), then each line's item, quantity and rate, then save."""

    events, i = [], 0
    if supplier:
        events.append(event(i, "type", field("combobox", "Supplier"), text=supplier)); i += 1
    for item, qty, rate in lines:
        events.append(event(i, "type", field("combobox", "Item"), text=item)); i += 1
        events.append(event(i, "type", field("textbox", "Accepted Qty"), text=qty)); i += 1
        events.append(event(i, "type", field("textbox", "Rate (INR)"), text=rate)); i += 1
    events.append(event(i, "click", field("button", "Save")))
    meta = {"id": SESSION, "title": "Enter a supplier's bill", "kind": "demonstration", "status": "complete",
            "started_at": 1789380000, "screen": [1280, 720]}
    return meta, events


def to_values_question(skill):
    """Answer the lesson up to its question about which values change: the loop is done once per line."""

    replies = {"goal": "x", "start": "yes", "loop": "each"}
    while (question := learning.next_question(skill))["kind"] not in ("variables", "variable"):
        if question["kind"] not in replies:
            raise AssertionError(f"reached {question['kind']} before the values question: {question['text']}")
        learning.answer(skill, question, replies[question["kind"]])
    return question


def quantity(skill):
    return next(p for p in skill["params"] if (p.get("examples") or [p.get("example")])[0] == "1")


class AValueAlikeOnEveryLineIsAsked(unittest.TestCase):

    def test_the_values_question_waits_for_the_loop_and_lists_the_lines_quantity(self):
        skill = learning.build([one_bill()])
        kinds = []
        for question in learning.questions(skill):
            kinds.append(question["kind"])
        self.assertIn("loop", kinds)
        self.assertNotIn("variable", kinds, "no value is asked about before the loop is answered")
        self.assertNotIn("variables", kinds)

        asked = to_values_question(skill)
        self.assertEqual(asked["kind"], "variables")
        self.assertEqual(len(asked["values"]), 2, asked["text"])
        self.assertIn('1) "TechCorp" into combobox "Supplier"', asked["text"])
        self.assertIn('2) "1" into textbox "Accepted Qty" on every line - same for every line, or read from each line?',
                      asked["text"])
        # Unanswered, it is read from each line - not a constant from one demonstration.
        qty = quantity(skill)
        self.assertIsNone(qty["varies"])
        self.assertTrue(qty["list"])
        self.assertEqual(qty["examples"], ["1", "1"])

    def test_all_change_reads_it_from_each_line(self):
        skill = learning.build([one_bill()])
        learning.answer(skill, to_values_question(skill), "all change")
        qty = quantity(skill)
        self.assertEqual((qty["varies"], qty["list"], qty["default"]), (True, True, None))
        self.assertIn(qty["name"], {p["name"] for p in skills.list_params(skill)})

    def test_no_answer_keeps_it_read_from_each_line(self):
        skill = learning.build([one_bill()])
        asked = to_values_question(skill)
        with NO_MODEL:
            for _ in range(learning.MAX_UNCLEAR):
                result = learning.answer(skill, asked, "hmm, whatever you think")
        self.assertTrue(result["understood"])
        qty = quantity(skill)
        self.assertEqual((qty["varies"], qty["list"]), (True, True))

    def test_naming_only_other_values_does_not_fix_it(self):
        # "1 changes" leaves "the others as typed" - but a value on every line is fixed only when named.
        skill = learning.build([one_bill()])
        learning.answer(skill, to_values_question(skill), "1 changes")
        qty = quantity(skill)
        self.assertEqual((qty["varies"], qty["list"]), (True, True))

    def test_the_persons_same_makes_it_a_constant_typed_on_every_line(self):
        skill = learning.build([one_bill()])
        learning.answer(skill, to_values_question(skill), "all change except 2")
        qty = quantity(skill)
        self.assertEqual((qty["varies"], qty.get("list"), qty["default"]), (False, None, "1"))

        item = next(p["name"] for p in skill["params"] if p.get("list") and p["examples"][0] == "Cloud Compute")
        rate = next(p["name"] for p in skill["params"] if p.get("list") and p["examples"][0] == "40000")
        supplier = next(p["name"] for p in skill["params"] if p.get("example") == "TechCorp")
        program, flat = skills.expand(skill, {supplier: "Acme", item: "Bolts; Nuts; Pins", rate: "5; 6; 7"})
        typed = [s.get("param") for s in program["steps"] if s["kind"] == "type"]
        self.assertEqual(typed.count(qty["name"]), 3, "typed as taught on every line")
        self.assertNotIn(qty["name"], flat, "and not asked of a run")

    def test_alone_it_is_asked_on_its_own_and_same_fixes_it(self):
        skill = learning.build([one_bill(supplier="")])
        asked = to_values_question(skill)
        self.assertEqual(asked["kind"], "variable")
        self.assertIn('on every line you typed "1" into textbox "Accepted Qty"', asked["text"])
        self.assertIn("Is that the same for every line, or read from each line?", asked["text"])
        learning.answer(skill, asked, "same")
        qty = quantity(skill)
        self.assertEqual((qty["varies"], qty.get("list"), qty["default"]), (False, None, "1"))


# ---------------------------------------------------------------------------------------------------------------------
# 2. Steps the person says to leave out
# ---------------------------------------------------------------------------------------------------------------------

LIVE_QUESTION = ("Steps 37-42 re-enter quantity and rate again after steps 30-35 already filled them for the second item. "
                 "Is this double-entry required every time, or was it fixing a one-time glitch?")
LIVE_REPLY = ("It was fixing a one-time glitch: the screen lagged and the second item's quantity and rate were lost, so I "
              "typed them again. Enter each line's quantity and rate once. Step 20 (clicking 'Add multiple') and step "
              "29 (the drag) were glitches from the same lag - skip them; I picked each item from the dropdown that "
              "appears when you type it.")
LIVE_LINE = "I will leave out step 20 Add multiple and 29 drag 491,365 to 680,316"

# (kind, role, label, summary, typed) - steps 1-39 as the lesson's first question listed them.
QTY, RATE = ("textbox", "Accepted Qty"), ("textbox", "Rate (INR)")
LIVE_STEPS = [
    ("key", None, None, "press ctrl+l", None),
    ("type", None, None, "type {new_purchase_invoice_url} into the focused field",
     "host.docker.internal:8000/app/purchase-invoice/new"),
    ("key", None, None, "press Delete", None),
    ("key", None, None, "press Return", None),
    ("click", "combobox", "Supplier", 'click combobox "Supplier"', None),
    ("type", "combobox", "Supplier", 'type {supplier_name} into combobox "Supplier"', "TechCorp"),
    ("click", "option", "TechCorp Cloud Solutions India Pvt Ltd TechCorp Cloud Solutions India Pvt Ltd, …",
     'click option "TechCorp Cloud Solutions India Pvt Ltd TechCorp C…"', None),
    ("click", "textbox", "Supplier Invoice No", 'click textbox "Supplier Invoice No"', None),
    ("type", "textbox", "Supplier Invoice No", 'type {supplier_invoice_number} into textbox "Supplier Invoice No"',
     "TC/2026/0401"),
    ("click", "textbox", "Supplier Invoice Date", 'click textbox "Supplier Invoice Date"', None),
    ("type", "textbox", "Supplier Invoice Date", 'type {supplier_invoice_date} into textbox "Supplier Invoice Date"',
     "01-10-2026"),
    ("key", "textbox", "Supplier Invoice Date", 'press Escape on textbox "Supplier Invoice Date"', None),
    ("click", "textbox", "Due Date", 'click textbox "Due Date"', None),
    ("key", "textbox", "Due Date", 'press ctrl+a on textbox "Due Date"', None),
    ("type", "textbox", "Due Date", 'type {payment_due_date} into textbox "Due Date"', "15-10-2026"),
    ("key", "textbox", "Due Date", 'press Escape on textbox "Due Date"', None),
    ("scroll", None, None, "scroll down on 760,450", None),
    ("click", "combobox", "Item", 'click combobox "Item"', None),
    ("type", "combobox", "Item", 'type {first_item} into combobox "Item"', "Cloud Compute"),
    ("click", "button", "Add multiple", 'click button "Add multiple"', None),
    ("click", *QTY, 'click textbox "Accepted Qty"', None),
    ("key", *QTY, 'press ctrl+a on textbox "Accepted Qty"', None),
    ("type", *QTY, 'type {first_item_quantity} into textbox "Accepted Qty"', "1"),
    ("click", *RATE, 'click textbox "Rate (INR)"', None),
    ("key", *RATE, 'press ctrl+a on textbox "Rate (INR)"', None),
    ("type", *RATE, 'type {first_item_rate} into textbox "Rate (INR)"', "40000"),
    ("key", *RATE, 'press Tab on textbox "Rate (INR)"', None),
    ("type", "combobox", "Item", 'type {second_item} into combobox "Item"', "Managed Database"),
    ("drag", None, None, "drag 491,365 to 680,316", None),
    ("click", *QTY, 'click textbox "Accepted Qty"', None),
    ("key", *QTY, 'press ctrl+a on textbox "Accepted Qty"', None),
    ("type", *QTY, 'type {second_item_quantity} into textbox "Accepted Qty"', "1"),
    ("click", *RATE, 'click textbox "Rate (INR)"', None),
    ("key", *RATE, 'press ctrl+a on textbox "Rate (INR)"', None),
    ("type", *RATE, 'type {second_item_rate} into textbox "Rate (INR)"', "10000"),
    ("click", None, None, "click 1100,200", None),
    ("click", *QTY, 'click textbox "Accepted Qty"', None),
    ("key", *QTY, 'press ctrl+a on textbox "Accepted Qty"', None),
    ("type", *QTY, 'type {third_item_quantity} into textbox "Accepted Qty"', "1"),
    # 40-49: the rest of the recording, in its order.
    ("click", *RATE, 'click textbox "Rate (INR)"', None),
    ("key", *RATE, 'press ctrl+a on textbox "Rate (INR)"', None),
    ("type", *RATE, 'type {third_item_rate} into textbox "Rate (INR)"', "10000"),
    ("click", None, None, "click 1100,200", None),
] + [("scroll", None, None, "scroll down on 760,450", None)] * 5 + [("key", None, None, "press ctrl+s", None)]


def live_skill():
    steps, params = [], []
    for n, (kind, role, label, summary, typed) in enumerate(LIVE_STEPS, 1):
        step = {"id": f"s{n}", "n": n, "kind": kind, "summary": summary, "consequential": False, "expect": []}
        if role:
            step["target"] = {"role": role, "name": label}
        elif kind in ("click", "drag", "scroll"):
            step["point"] = [491, 365] if kind == "drag" else [1100, 200]
        if typed is not None:
            step["samples"] = [typed]
            step["param"] = summary.split("{")[1].split("}")[0]
            params.append({"name": step["param"], "label": label or "the focused field", "example": typed,
                           "default": typed, "varies": None, "step": n})
        steps.append(step)
    ids = [s["id"] for s in steps]
    # The loop as the lesson found it: steps 21-26 done three times, with 27-29 and 36 between.
    loop = {"id": "l1", "enabled": None, "length": 6, "reps": 3,
            "blocks": [ids[20:26], ids[29:35], ids[36:42]], "between": [ids[26:29], ids[35:36]]}
    return {"name": "enter-vendor-bill", "title": "enter-vendor-bill", "named": True, "steps": steps, "params": params,
            "answers": {"goal": "stop"}, "unclear": {}, "demos": 1, "loops": [loop], "notes": [],
            "understanding": {"summary": "Enter a supplier's bill as a draft purchase invoice.", "rules": [],
                              "dialogue": [], "questions": [{"id": "q1", "text": LIVE_QUESTION}]}}


class Model:
    """A stand-in model: the lesson's reading of the reply gets `reading`; a choice reading gets `choice`."""

    def __init__(self, reading, choice="unclear"):
        self.reading, self.choice, self.calls = reading, choice, []

    def chat(self, messages, purpose, **kw):
        self.calls.append((purpose, messages[-1]["content"]))
        if purpose == "py.lesson.gates":
            return {"content": json.dumps(self.reading)}, {}
        return {"content": json.dumps({"choice": self.choice, "instruction": ""})}, {}

    def __enter__(self):
        self.patches = [mock.patch.object(llm_client, "available", return_value=True),
                        mock.patch.object(llm_client, "chat", self.chat)]
        for p in self.patches:
            p.start()
        return self

    def __exit__(self, *exc):
        for p in self.patches:
            p.stop()


LIVE_READING = {"ask": [], "no_ask": [], "leave_out": ["20", "29"]}


def answered_live(reading=LIVE_READING):
    """The live study question answered with the live reply, the way a lesson takes it (learning.answer, then the
    study's own folding of it - here with no model, which keeps the words whole as a rule)."""

    skill = live_skill()
    question = learning.next_question(skill)
    assert question["kind"] == "study", question
    with Model(reading) as model:
        result = learning.answer(skill, question, LIVE_REPLY)
    with mock.patch.object(study, "available", return_value=False):
        study.absorb(skill, question["text"], LIVE_REPLY)
    return skill, result, model


def labels(skill):
    return [(s.get("target") or {}).get("name") or s["summary"] for s in skill["steps"]]


class StepsSaidToBeLeftOutAreTakenOut(unittest.TestCase):

    def test_the_live_reply_is_put_back_in_one_line(self):
        skill, result, model = answered_live()
        self.assertTrue(result["understood"])
        self.assertEqual([p for p, _ in model.calls], ["py.lesson.gates"])
        self.assertIn(LIVE_REPLY, model.calls[0][1], "the reading is given the person's words whole")
        asked = learning.next_question(skill)
        self.assertEqual(asked["kind"], "skips", "put back right after the answer that said it")
        self.assertTrue(asked["text"].startswith(LIVE_LINE + " - right?"), asked["text"])
        # Nothing is taken out until the person says yes.
        self.assertEqual(len(skill["steps"]), 49)
        self.assertIn("Add multiple", labels(skill))

    def test_a_yes_takes_them_out_of_the_steps_and_keeps_the_words_as_a_note(self):
        skill, _, _ = answered_live()
        with NO_MODEL:
            result = learning.answer(skill, learning.next_question(skill), "yes")
        self.assertTrue(result["understood"])
        self.assertIn("I left out step 20 Add multiple and 29 drag 491,365 to 680,316", result["note"])
        self.assertEqual(len(skill["steps"]), 47)
        self.assertNotIn("Add multiple", labels(skill))
        self.assertFalse(any(s["kind"] == "drag" for s in skill["steps"]))
        self.assertEqual([s["n"] for s in skill["steps"]], list(range(1, 48)))
        self.assertEqual(skill["steps"][19]["summary"], 'click textbox "Accepted Qty"', "21 is now 20")
        # The loop the steps were in keeps its repetitions; the drag is gone from what came between them.
        (loop,) = skill["loops"]
        self.assertEqual([len(b) for b in loop["blocks"]], [6, 6, 6])
        self.assertEqual(loop["between"], [["s27", "s28"], ["s36"]])
        self.assertEqual(learning.next_question(skill)["kind"], "loop")
        # The person's words stay with the task as a note: the study's rule, and what was left out and why.
        self.assertTrue(any(LIVE_REPLY in rule for rule in skill["understanding"]["rules"]))
        (left,) = skill["left_out"]
        self.assertEqual([s["summary"] for s in left["steps"]], ['click button "Add multiple"', "drag 491,365 to 680,316"])
        self.assertEqual(left["said"], [" ".join(LIVE_REPLY.split())])
        self.assertNotIn("skips_pending", skill)

    def test_a_no_keeps_every_step(self):
        skill, _, _ = answered_live()
        with NO_MODEL:
            result = learning.answer(skill, learning.next_question(skill), "no")
        self.assertTrue(result["understood"])
        self.assertEqual(len(skill["steps"]), 49)
        self.assertNotEqual(learning.next_question(skill)["kind"], "skips")

    def test_keep_one_by_number_and_the_line_is_put_back(self):
        skill, _, _ = answered_live()
        with NO_MODEL:
            learning.answer(skill, learning.next_question(skill), "keep 29")
            asked = learning.next_question(skill)
            self.assertTrue(asked["text"].startswith("I will leave out step 20 Add multiple - right?"), asked["text"])
            learning.answer(skill, asked, "yes")
        self.assertEqual(len(skill["steps"]), 48)
        self.assertTrue(any(s["kind"] == "drag" for s in skill["steps"]))

    def test_a_name_no_step_has_is_asked_about_alone(self):
        # Read by the names the person wrote: "Add multiple" is step 20's label; "the drag" is no step's label.
        skill, _, _ = answered_live({"ask": [], "no_ask": [], "leave_out": ["Add multiple", "the drag"]})
        asked = learning.next_question(skill)
        self.assertTrue(asked["text"].startswith("I couldn't find a step for \"the drag\" to leave out - which step is "
                                                 "it?"), asked["text"])
        self.assertIn("So far: I will leave out step 20 Add multiple.", asked["text"])
        with NO_MODEL:
            self.assertTrue(learning.answer(skill, asked, "step 29")["understood"])
            asked = learning.next_question(skill)
        self.assertTrue(asked["text"].startswith(LIVE_LINE + " - right?"), asked["text"])

    def test_plain_skip_and_numbers_need_no_model(self):
        skill = live_skill()
        with NO_MODEL:
            learning.answer(skill, learning.next_question(skill), "skip 20 and 29")
            self.assertTrue(learning.next_question(skill)["text"].startswith(LIVE_LINE + " - right?"))

    def test_never_made_sure_of_takes_nothing_out(self):
        skill, _, _ = answered_live()
        with NO_MODEL:
            for _ in range(learning.MAX_UNCLEAR):
                asked = learning.next_question(skill)
                self.assertEqual(asked["kind"], "skips")
                result = learning.answer(skill, asked, "hmm")
        self.assertTrue(result["understood"])
        self.assertIn("left every step in", result["note"])
        self.assertEqual(len(skill["steps"]), 49)
        self.assertNotIn("skips_pending", skill)


if __name__ == "__main__":
    unittest.main()
