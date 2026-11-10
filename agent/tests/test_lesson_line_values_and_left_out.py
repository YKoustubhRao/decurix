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


# ---------------------------------------------------------------------------------------------------------------------
# 3. Steps left out of a repetition (freeze 2.2)
#
# Found live (2026-11-10, the same task taught again on desktop erpcurve2, task tf18e6dc0fe52f44b): the person said
# "skip steps 13-16" (a first try at the due date that did not take); the lesson put back only 13 and 16 (the reading's
# 14 and 15 were dropped as "not said", the reply saying them only as a range); the person answered "leave out 13, 14,
# 15 and 16 - all four were ..." and the lesson stopped on an internal error, 'samples': 13-16 and 17-20 were two
# repetitions of one block, and with 13 and 16 gone a key press stood where the others typed. Steps 1-44 as the lesson
# listed them; 45-49 (not shown) are stand-ins.
# ---------------------------------------------------------------------------------------------------------------------

DATE_GOAL_REPLY = (
    "Mostly right. Some fields lost their names in the recording: steps 8-9 type the bill number into 'Supplier "
    "Invoice No'; steps 10-12 type the bill date into 'Supplier Invoice Date'. Steps 13-16 were my first try at the due "
    "date, which did not take - skip steps 13-16; steps 17-20 enter the due date. Every value comes from the bill (the "
    "attached PDF): supplier, bill number, bill date, due date, and one item row per line of the bill with that line's "
    "quantity and rate - the number of lines and each line's quantity differ from bill to bill. Pick the supplier and "
    "each item from the dropdown ERPNext offers; never create a new supplier or item - if one is missing, leave the "
    "bill unsaved and tell me which. If an invoice with the same supplier bill number already exists, stop and ask me. "
    "If the taxes or grand total don't match the bill, stop and ask; never adjust them yourself. Save as a draft only, "
    "never submit.")
DATE_SKIP_REPLY = "leave out 13, 14, 15 and 16 - all four were the due-date try that did not take"
DATE_LINE = ('I will leave out step 13 click textbox "", 14 press ctrl+a on textbox "", 15 type {typed_text_4} into '
             'textbox "" and 16 press Escape on textbox ""')

EMPTY = ("textbox", "")
DUE = ("textbox", "Due Date")
DATE_STEPS = [
    ("key", None, None, "press ctrl+l", None),
    ("type", None, None, "type {typed_text} into the focused field", "host.docker.internal:8000/app/purchase-invoice/new"),
    ("key", None, None, "press Delete", None),
    ("key", None, None, "press Return", None),
    ("click", "combobox", "Supplier", 'click combobox "Supplier"', None),
    ("type", "combobox", "Supplier", 'type {supplier} into combobox "Supplier"', "TechCorp"),
    ("click", "option", "TechCorp Cloud Solutions India Pvt Ltd TechCorp Cloud Solutions India Pvt Ltd, …",
     'click option "TechCorp Cloud Solutions India Pvt Ltd TechCorp C…"', None),
    ("click", None, None, "click 520,556", None),
    ("type", None, None, "type {typed_text_2} into the focused field", "TC/2026/0401"),
    ("click", *EMPTY, 'click textbox ""', None),
    ("type", *EMPTY, 'type {typed_text_3} into textbox ""', "01-10-2026"),
    ("key", *EMPTY, 'press Escape on textbox ""', None),
    ("click", *EMPTY, 'click textbox ""', None),
    ("key", *EMPTY, 'press ctrl+a on textbox ""', None),
    ("type", *EMPTY, 'type {typed_text_4} into textbox ""', "15/10/2026"),
    ("key", *EMPTY, 'press Escape on textbox ""', None),
    ("click", *DUE, 'click textbox "Due Date"', None),
    ("key", *DUE, 'press ctrl+a on textbox "Due Date"', None),
    ("type", *DUE, 'type {due_date} into textbox "Due Date"', "15-10-2026"),
    ("key", *DUE, 'press Escape on textbox "Due Date"', None),
] + [("scroll", None, None, "scroll down on 760,450", None)] * 3 + [
    ("click", "combobox", "Item", 'click combobox "Item"', None),
    ("type", "combobox", "Item", 'type {item} into combobox "Item"', "Cloud Compute"),
    ("click", "option", "Cloud Compute Instances Cloud Compute Instances, Services",
     'click option "Cloud Compute Instances Cloud Compute Instances, …"', None),
    ("click", *QTY, 'click textbox "Accepted Qty"', None),
    ("key", *QTY, 'press ctrl+a on textbox "Accepted Qty"', None),
    ("type", *QTY, 'type {accepted_qty} into textbox "Accepted Qty"', "1"),
    ("click", *RATE, 'click textbox "Rate (INR)"', None),
    ("key", *RATE, 'press ctrl+a on textbox "Rate (INR)"', None),
    ("type", *RATE, 'type {rate_inr} into textbox "Rate (INR)"', "40000"),
    ("click", "button", "Add row", 'click button "Add row"', None),
    ("click", "combobox", "Item", 'click combobox "Item"', None),
    ("type", "combobox", "Item", 'type {item_2} into combobox "Item"', "Managed Database"),
    ("click", "option", "Managed Database Hosting Managed Database Hosting, Services",
     'click option "Managed Database Hosting Managed Database Hosting…"', None),
    ("click", *QTY, 'click textbox "Accepted Qty"', None),
    ("key", *QTY, 'press ctrl+a on textbox "Accepted Qty"', None),
    ("type", *QTY, 'type {accepted_qty_2} into textbox "Accepted Qty"', "1"),
    ("click", *RATE, 'click textbox "Rate (INR)"', None),
    ("key", *RATE, 'press ctrl+a on textbox "Rate (INR)"', None),
    ("type", *RATE, 'type {rate_inr_2} into textbox "Rate (INR)"', "10000"),
] + [("scroll", None, None, "scroll down on 760,450", None)] * 6 + [("key", None, None, "press ctrl+s", None)]


def date_skill(first_block=False):
    """The live draft at the first question: the date entries a loop (17-20 like 13-16, and with `first_block` the
    shorter 10-12 before them), the bill's lines another (24-32 and 34-42, "Add row" between)."""

    skill = live_skill()
    steps, params = [], []
    for n, (kind, role, label, summary, typed) in enumerate(DATE_STEPS, 1):
        step = {"id": f"s{n}", "n": n, "kind": kind, "summary": summary, "consequential": False, "expect": []}
        if role:
            step["target"] = {"role": role, "name": label}
        elif kind in ("click", "drag", "scroll"):
            step["point"] = [520, 556] if kind == "click" else [760, 450]
        if typed is not None:
            step["samples"] = [typed]
            step["param"] = summary.split("{")[1].split("}")[0]
            params.append({"name": step["param"], "label": label or "the focused field", "example": typed,
                           "default": typed, "varies": None, "step": n})
        steps.append(step)
    ids = [s["id"] for s in steps]
    dates = {"id": "l1", "enabled": None, "length": 4, "reps": 2 + first_block,
             "blocks": ([ids[9:12]] if first_block else []) + [ids[12:16], ids[16:20]],
             "between": ([[]] if first_block else []) + [[]]}
    lines = {"id": "l2", "enabled": None, "length": 9, "reps": 2, "blocks": [ids[23:32], ids[33:42]],
             "between": [ids[32:33]]}
    skill.update(steps=steps, params=params, loops=[dates, lines], answers={}, demos=1)
    skill["understanding"]["questions"] = []
    return skill


def aligned_kinds_agree(skill, loop):
    blocks, template = learning._template(skill, loop)
    return all(len({s["kind"] for s in learning._aligned(blocks, position, loop["length"])}) == 1
               for position in range(len(template)))


class LeftOutOfARepetition(unittest.TestCase):

    def told(self, reading, first_block=False):
        skill = date_skill(first_block)
        goal = learning.next_question(skill)
        self.assertEqual(goal["kind"], "goal")
        with Model(reading):
            learning.answer(skill, goal, DATE_GOAL_REPLY)
        return skill

    def test_a_range_is_put_back_whole_however_the_reading_lists_it(self):
        for leave_out in (["13", "14", "15", "16"], ["13-16"], ["steps 13-16"]):
            with self.subTest(leave_out=leave_out):
                skill = self.told({"ask": [], "no_ask": [], "leave_out": leave_out})
                asked = learning.next_question(skill)
                self.assertEqual(asked["kind"], "skips")
                self.assertTrue(asked["text"].startswith(DATE_LINE + " - right?"), asked["text"])

    def test_the_live_answers_end_with_the_four_out_and_a_lesson_that_goes_on(self):
        for first_block in (False, True):
            with self.subTest(first_block=first_block):
                # (the reading as it reached the lesson live: the range's two ends)
                skill = self.told({"ask": [], "no_ask": [], "leave_out": ["13", "16"]}, first_block)
                with NO_MODEL:
                    result = learning.answer(skill, learning.next_question(skill), DATE_SKIP_REPLY)
                    self.assertTrue(result["understood"])
                    asked = learning.next_question(skill)
                    self.assertTrue(asked["text"].startswith(DATE_LINE + " - right?"), asked["text"])
                    learning.answer(skill, asked, "yes")
                    nxt = learning.next_question(skill)   # (raised KeyError 'samples' live)
                self.assertIsNotNone(nxt)
                self.assertEqual(len(skill["steps"]), 45)
                self.assertEqual([s["summary"] for s in skill["steps"][12:16]],
                                 ['click textbox "Due Date"', 'press ctrl+a on textbox "Due Date"',
                                  'type {due_date} into textbox "Due Date"', 'press Escape on textbox "Due Date"'])
                for loop in skill["loops"]:
                    if loop["enabled"] is not False:
                        self.assertTrue(aligned_kinds_agree(skill, loop), loop)
                lines = next(l for l in skill["loops"] if l["id"] == "l2")
                self.assertIsNone(lines["enabled"], "the bill's lines are still a loop to ask about")

    def test_two_steps_out_of_one_repetition_never_mixes_kinds(self):
        # The live crash itself: only 13 and 16 taken out, a key press and a typing left of that repetition.
        skill = date_skill()
        skill["answers"]["goal"] = DATE_GOAL_REPLY[:300]
        skill["skips_pending"] = {"leave_out": ["s13", "s16"], "unmapped": [], "said": [DATE_GOAL_REPLY]}
        with NO_MODEL:
            learning.answer(skill, learning.next_question(skill), "yes")
            learning.questions(skill)
        dates = next(l for l in skill["loops"] if l["id"] == "l1")
        self.assertIs(dates["enabled"], False, "one repetition left that lines up: no loop")
        self.assertIn('press ctrl+a on textbox ""', [s["summary"] for s in skill["steps"]], "nothing else taken out")

    def test_a_repetition_that_no_longer_lines_up_leaves_the_loop_and_the_rest_stay_one(self):
        skill = date_skill(first_block=True)
        # Three full repetitions of the date entry: 13-16, 17-20 and a copy of them; one step out of the middle one.
        dates = next(l for l in skill["loops"] if l["id"] == "l1")
        dates["blocks"], dates["between"] = [skill_ids(skill, 13, 16), skill_ids(skill, 17, 20),
                                             skill_ids(skill, 24, 27)], [[], skill_ids(skill, 21, 23)]
        for s in skill["steps"][23:27]:
            s["kind"] = ["click", "key", "type", "key"][s["n"] - 24]
            s["samples"] = ["01/11/2026"] if s["kind"] == "type" else s.get("samples")
        skill["loops"] = [dates]
        learning._drop_steps(skill, {"s18"})
        self.assertEqual(dates["blocks"], [skill_ids_by_old(skill, 13, 16), skill_ids_by_old(skill, 24, 27)])
        self.assertIsNone(dates["enabled"])
        self.assertTrue(aligned_kinds_agree(skill, dates))
        self.assertEqual(dates["between"], [["s17", "s19", "s20", "s21", "s22", "s23"]])
        learning.questions(skill)

    def test_the_loop_question_reads_only_steps_of_the_same_kind(self):
        skill = date_skill()
        dates = next(l for l in skill["loops"] if l["id"] == "l1")
        dates["blocks"][0] = ["s14", "s15"]   # (a key press and a typing, as the live removal left them)
        text = learning._loop(skill, dates)["text"]
        self.assertIn("Steps 14-15", text)

    def test_ranges_are_read_whole_and_never_from_a_date(self):
        skill = date_skill()
        for name, expected in {"13-16": [13, 14, 15, 16], "steps 13 to 16": [13, 14, 15, 16], "13": [13],
                               "48-52": []}.items():
            with self.subTest(name=name):
                self.assertEqual([s["n"] for s in learning._steps_named(skill, name)], expected)
        self.assertTrue(learning._said_in("14", "skip steps 13-16"))
        self.assertTrue(learning._said_in("13-16", "skip steps 13-16"))
        self.assertFalse(learning._said_in("5", "the bill date 01-10-2026"))
        self.assertFalse(learning._said_in("17", "skip steps 13-16"))


def skill_ids(skill, low, high):
    return [s["id"] for s in skill["steps"] if low <= s["n"] <= high]


def skill_ids_by_old(skill, low, high):
    return [f"s{n}" for n in range(low, high + 1)]


if __name__ == "__main__":
    unittest.main()
