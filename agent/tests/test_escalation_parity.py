"""
The escalation ladder's questions, worded and read in soul, are today's byte for byte (plan rule 3, phase 2: the
second decision moved, `py.escalation.*`).

With DECURIX_SOUL_ESCALATION=1, a skill run's ladder (escalation.Ladder) hands soul each question's evidence as data
(the candidates, the screen as the tools render it, the lines of text read off the frames) and soul's POST
/api/llm/escalation builds the prompt and reads the answer (agent/thebot/internal/escalation). The proof the move changes
nothing is a recorded corpus, agent/thebot/internal/escalation/testdata/parity.json (fictional data only):

- for each case (ground, judge, detour, point), the evidence, the messages today's path sends (the system text and the
  prompt), and for each answer the decision decide_* makes of it and what the ladder then did (its result, its trail,
  what it kept and reported); for every answer of a long list of awkward ones, each question's decision of it;
- here: today's path (no ask_decided) still sends exactly those messages and does exactly that; a ladder handing its
  questions to soul sends exactly that evidence, to /api/llm/escalation only, and does the same with soul's decision -
  on the hosted brain and on a local one - and fails closed on a decision that is not one; a soul without the route
  sends the question today's way;
- in Go (internal/escalation parity_test.go, cmd/soul escalation_test.go): soul builds the same messages from that
  evidence, makes the same decision of each answer, and its provider sends the proxy the very bytes /api/llm/call sends
  for today's messages.

Regenerate after a deliberate change to a prompt or a reading (then soul's copy changes with it, in the same commit):
    cd agent && PYTHONPATH=".;tests" py -3 tests/test_escalation_parity.py --write
"""

import io
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, llm_client, ollama_client, skill_task, soul_call, state, task_context
from decurix_agent.computer import approval, detour as detours, escalation, ocr, screen_call

CORPUS = Path(__file__).resolve().parents[1] / "thebot" / "internal" / "escalation" / "testdata" / "parity.json"
KINDS = (escalation.GROUND, escalation.JUDGE, escalation.DETOUR, escalation.POINT)


def U(hex4):
    """A JSON \\u escape, written in pieces (no source file here holds one whole)."""

    return "\\" + "u" + hex4


def _answer(**fields):
    return json.dumps(fields)


def _el(role, name, x=500, y=400, **extra):
    return {"role": role, "name": name, "box": [x, y, 80, 30], "path": f'site x > form "Order" > {role} "{name}"',
            **extra}


def _step(n=1, **extra):
    step = {"n": n, "id": f"s{n}", "kind": "click", "summary": 'click button "Save"',
            "target": {"role": "button", "name": "Save"}}
    step.update(extra)
    return step


def _lines(texts, top=100):
    return [{"text": t, "box": [40, top + 24 * i, 300, 18]} for i, t in enumerate(texts)]


LONG_INTENT = ("  The person said:\tthe Save button   on the order form itself, not the one in the toolbar.\n" * 8)
NAME_QUOTES = 'Save "draft" copy of the purchase order for Fabrikam Fictional Furniture and Fittings Ltd. (Mumbai branch)'
SCREEN_LINES = [f"Order SO-{1000 + n} Northwind Fictional Traders total {n * 37}.50 due 0{n % 9 + 1}/03/2026"
                for n in range(60)]

CASES = [
    {"name": "ground_basic", "kind": "ground", "call": {
        "goal": "Save the order", "total": 4, "step": _step(),
        "observation": {"active_title": "Shop - Chromium", "elements": []},
        "ranked": [{"element": _el("button", "Store"), "score": 0.61},
                   {"element": _el("button", "Cancel", 400, states=["disabled"]), "score": 0.4}]},
     "answers": [_answer(choice=1, confidence=0.9, reason="same place"), _answer(choice=2, confidence=0.5),
                 "I am not sure."]},
    {"name": "ground_everything", "kind": "ground", "call": {
        "goal": "Make a draft quotation for Northwind Fictional Traders", "total": 0,
        "step": _step(3, summary="click the button that saves", intent=LONG_INTENT,
                      target={"role": "button", "name": "  Save\n  order "}),
        "observation": {"active_title": "Quotation QTN-0042 - New \"draft\" - " + "x" * 120, "elements": [
            _el("dialog", "Session expired"), _el("alert", 'Fill in "Customer" first'), _el("dialog", "Third one"),
            _el("dialog", "")]},
        "ranked": [{"element": _el("button", NAME_QUOTES), "score": 0.7},
                   {"element": _el(None, None), "score": 0.5},
                   {"element": _el("link", "सहेजें (save) ✓ ₹"), "score": 0.45},
                   {"element": _el("menuitem", "Save\tand\nclose", states="disabled-ish"), "score": 0.3}]},
     "answers": [_answer(choice=3, confidence=0.8), _answer(choice="4", confidence="0.95"),
                 _answer(choice=1, confidence=0.75)]},
    {"name": "ground_no_role_many", "kind": "ground", "call": {
        "goal": "", "total": 12,
        "step": {"n": 7, "id": "s7", "kind": "click", "target": {"name": None}, "intent": 0},
        "observation": {"active_title": None, "elements": [_el("alert", None), _el("alert", "")]},
        "ranked": [{"element": _el("button", f"Option {n}"), "score": 0.9 - n / 20} for n in range(1, 9)]},
     "answers": [_answer(choice=8, confidence=1), _answer(choice=9, confidence=1), _answer(choice=True, confidence=True)]},
    {"name": "judge_basic", "kind": "judge", "call": {
        "goal": "Save the order", "step": _step(),
        "observation": {"elements": [_el("heading", "Orders")], "windows": [{"title": "Shop"}], "size": [1280, 720],
                        "active_title": "Shop", "page_url": "http://shop.test/"},
        "missing": ['an alert "Saved"']},
     "answers": [_answer(achieved=True, confidence=0.95), _answer(achieved=True, confidence=0.84),
                 _answer(achieved=None, confidence=0.9)]},
    {"name": "judge_long_screen", "kind": "judge", "call": {
        "goal": "", "step": _step(5, summary="press Submit on the Fictional Expense claim"),
        "observation": {"elements": [_el("row", line, 20, 100 + 12 * i) for i, line in enumerate(SCREEN_LINES)],
                        "windows": [{"title": "Ledger - Fictional Books"}, {"title": "Notes"}], "size": [1920, 1080],
                        "active_title": "Ledger - Fictional Books"},
        "missing": ['an alert "Submitted"', 'the field "Status" reads "Submitted"', "a row CL-4142"]},
     "answers": [_answer(achieved=True, confidence=0.86, reason="the status reads Submitted"),
                 _answer(achieved="true", confidence=1)]},
    {"name": "judge_unicode", "kind": "judge", "call": {
        "goal": "ग्राहक को चालान भेजें 📨", "step": _step(2, summary="चालान सहेजें"),
        "observation": {"elements": [_el("alert", "चालान सहेजा गया ✓")], "windows": [], "size": [1280, 720],
                        "active_title": "चालान"},
        "missing": []},
     "answers": [_answer(achieved=True, confidence=0.9)]},
    {"name": "detour_basic", "kind": "detour", "call": {
        "goal": "Search the fictional catalogue", "typed": [["blue pens", "red staplers"], ["10", "20"]],
        "steps": [_step(4, summary="click Accept cookies"), _step(5, summary="click Close"),
                  _step(6, summary='click link "Results"')],
        "find": {"to": 2, "margin": 0.3, "here": ["We use cookies", "Accept all", "Manage"],
                 "there": ["Results for blue pens", "Blue Pen x 10"]},
        "now": ["Results for red staplers", "Stapler, red", "Stapler, heavy duty"]},
     "answers": [_answer(skip=True, confidence=0.9), _answer(skip=False, confidence=0.8, reason="the banner is there"),
                 _answer(skip=None, confidence=0.9)]},
    {"name": "detour_long_text", "kind": "detour", "call": {
        "goal": "", "typed": [[f"typed {n}", f"now {n} " + "y" * 90] for n in range(5)],
        "steps": [{"n": 9, "id": "s9", "kind": "click"}, _step(10), _step(11), _step(12, summary=None)],
        "find": {"to": 3, "margin": 0.1, "here": SCREEN_LINES[:40], "there": ["z" * 900],
                 "there_note": "one line longer than the cut"},
        "now": ["सत्र समाप्त", "Sign in again ✓"] + SCREEN_LINES[40:]},
     "answers": [_answer(skip=True, confidence=0.69), _answer(skip=False, confidence=0.7, reason="r" * 200),
                 _answer(skip=True, confidence=1.0)]},
    {"name": "detour_margin_only", "kind": "detour", "call": {
        "goal": "Pay the fictional bill", "typed": [],
        "steps": [_step(1, summary="click Not now"), _step(2, summary="click Pay")],
        "find": {"to": 1, "margin": 0.25, "here": ["Save your card?", "Not now"], "there": ["Pay ₹1,200.00"]},
        "now": ["Pay ₹1,350.00"]},
     "answers": ["no JSON here", _answer(skip=False, confidence=0.5, reason=["a", 1])]},
    {"name": "point_basic", "kind": "point", "call": {
        "goal": "Open the first search result", "typed": [["pens", "staplers"]], "explanation": "",
        "step": _step(2, kind="click", point=[60, 130], summary="click at 60,130", target=None),
        "before": _lines(["Results for pens", "Blue Pen pack of 10", "Black Pen pack of 5"]), "clicked": 1,
        "now": _lines(["Results for staplers", "Stapler, red", "ok", "Stapler, heavy duty"])},
     "answers": [_answer(choice=2, confidence=0.9), _answer(choice=3, confidence=0.9), _answer(choice=2, confidence=0.7)]},
    {"name": "point_everything", "kind": "point", "call": {
        "goal": "Open the fictional customer's latest invoice",
        "typed": [["Northwind", "Contoso"], ["2025", "2026"], ["a", "b"], ["c", "d"]],
        "explanation": "  The person explained: " + "open the newest invoice of the customer searched for. " * 20,
        "step": _step(4, kind="double_click", point=[80, 172], summary="double-click the newest invoice",
                      intent="the newest invoice in the list", target=None),
        "before": _lines([f"INV-{n:04d} Northwind Fictional " + "w" * 130 for n in range(12)]), "clicked": 2,
        "now": _lines([f"INV-{n:04d} Contoso Fictional ₹{n * 100}.00 देय" for n in range(50)] + ["12", "--"])},
     "answers": [_answer(choice=40, confidence=0.99), _answer(choice=41, confidence=0.99),
                 _answer(choice="1", confidence="0.76")]},
    {"name": "point_plain", "kind": "point", "call": {
        "goal": "", "typed": [], "explanation": "",
        "step": _step(1, kind="click", point=[45, 104], summary=None, intent="", target=None),
        "before": _lines(["Inbox", "Fictional newsletter"]), "clicked": 0,
        "now": _lines(["Inbox (3)", "Fictional newsletter", "Sent"])},
     "answers": [_answer(choice=1, confidence=0.8)]},
]

# Answers as a model writes them, each read by every question's decide_* and by soul's: every shape json.loads, int()
# and float() take, cut or refuse.
_NESTED = '{"choice": 1, "confidence": 0.9, "x": ' + "[" * 50 + "]" * 50 + "}"
ANSWERS = [
    ("chosen", _answer(choice=1, confidence=0.9, achieved=True, skip=True, reason="same place")),
    ("threshold_exact", _answer(choice=1, confidence=0.75, achieved=True, skip=True)),
    ("threshold_detour", _answer(choice=1, confidence=0.7, achieved=True, skip=False)),
    ("threshold_judge", _answer(choice=1, confidence=0.85, achieved=True, skip=True)),
    ("just_under", _answer(choice=1, confidence=0.749, achieved=True, skip=True)),
    ("zero", _answer(choice=0, confidence=0.99, achieved=False, skip=False)),
    ("prose_and_fence", "Looking at it.\n```json\n" + _answer(choice=2, confidence=0.95, skip=True) + "\n```\nDone."),
    ("no_json", "I would click the second one."),
    ("empty", ""),
    ("cut_off", '{"choice": 1, "confidence": 0.9, "reason": "the bu'),
    ("cut_then_whole", '{"choice": 1, "conf then {"choice": 2, "confidence": 0.9}'),
    ("two_objects", '{"choice": 0} {"choice": 1, "confidence": 1, "skip": true}'),
    ("bad_then_good", '{choice: 1} and {"choice": 2, "confidence": 0.95, "achieved": true}'),
    ("nested_object", '{"choice": 1, "confidence": 0.9, "detail": {"x": 1}}'),
    ("brace_in_string", '{"choice": 1, "confidence": 0.9, "reason": "the {total} field"}'),
    ("single_quotes", "{'choice': 1, 'confidence': 0.9}"),
    ("trailing_comma", '{"choice": 1, "confidence": 0.9,}'),
    ("raw_newline_in_string", '{"choice": 1, "confidence": 0.9, "reason": "a\nb"}'),
    ("spacing", '{ "choice" :1 ,"confidence":0.95 ,\n\t"skip":true\r}'),
    ("duplicate_keys", '{"choice": 1, "confidence": 0.9, "choice": 2, "skip": false, "skip": true}'),
    ("choice_string", _answer(choice="2", confidence=0.9)),
    ("choice_string_spaced", _answer(choice=" 2 ", confidence=0.9)),
    ("choice_string_float", _answer(choice="2.0", confidence=0.9)),
    ("choice_string_word", _answer(choice="two", confidence=0.9)),
    ("choice_string_signed", _answer(choice="+1", confidence=0.9)),
    ("choice_string_underscore", _answer(choice="0_1", confidence=0.9)),
    ("choice_string_arabic_indic", _answer(choice="١", confidence=0.9)),
    ("choice_string_nbsp", _answer(choice=" 2", confidence=0.9)),
    ("choice_string_unit_separator", _answer(choice="\x1f2", confidence=0.9)),
    ("choice_float", _answer(choice=1.9, confidence=0.9)),
    ("choice_float_small", _answer(choice=0.5, confidence=0.9)),
    ("choice_negative", _answer(choice=-1, confidence=0.9)),
    ("choice_exponent", '{"choice": 2E0, "confidence": 8E-1}'),
    ("choice_negative_zero", '{"choice": -0, "confidence": 0.9}'),
    ("choice_overflow", '{"choice": 1e400, "confidence": 0.9}'),
    ("choice_nan", '{"choice": NaN, "confidence": 0.9}'),
    ("choice_infinity", '{"choice": Infinity, "confidence": 0.9, "skip": true}'),
    ("choice_huge_int", '{"choice": 123456789012345678901234567890, "confidence": 0.9}'),
    ("choice_true", '{"choice": true, "confidence": 0.9}'),
    ("choice_false", '{"choice": false, "confidence": 0.9}'),
    ("choice_null", '{"choice": null, "confidence": 0.9}'),
    ("choice_list", '{"choice": [1], "confidence": 0.9}'),
    ("choice_empty_string", '{"choice": "", "confidence": 0.9}'),
    ("confidence_string", _answer(choice=1, confidence="0.9", achieved=True, skip=True)),
    ("confidence_underscore", _answer(choice=1, confidence=" 0.9_5 ", achieved=True, skip=True)),
    ("confidence_word", _answer(choice=1, confidence="high", achieved=True, skip=True)),
    ("confidence_nan_string", _answer(choice=1, confidence="nan", achieved=True, skip=True)),
    ("confidence_inf_string", _answer(choice=1, confidence="inf", achieved=True, skip=True)),
    ("confidence_minus_infinity_string", _answer(choice=1, confidence="-Infinity", achieved=True, skip=True)),
    ("confidence_arabic_indic", _answer(choice=1, confidence="٠.٩", achieved=True, skip=True)),
    ("confidence_em_space", _answer(choice=1, confidence=" 0.9　", achieved=True, skip=True)),
    ("confidence_unit_separator", _answer(choice=1, confidence="\x1c0.9", achieved=True, skip=True)),
    ("confidence_over", _answer(choice=1, confidence=2, achieved=True, skip=True)),
    ("confidence_under", _answer(choice=1, confidence=-1, achieved=True, skip=False)),
    ("confidence_null", '{"choice": 1, "confidence": null, "achieved": true}'),
    ("confidence_true", '{"choice": 1, "confidence": true, "achieved": true, "skip": true}'),
    ("confidence_list", '{"choice": 1, "confidence": [0.9], "achieved": true}'),
    ("confidence_missing", '{"choice": 1, "achieved": true, "skip": true}'),
    ("confidence_nan_literal", '{"choice": 1, "confidence": NaN, "achieved": true, "skip": true}'),
    ("confidence_infinity_literal", '{"choice": 1, "confidence": Infinity, "achieved": true, "skip": true}'),
    ("confidence_overflow", '{"choice": 1, "confidence": 1e400, "achieved": true}'),
    ("confidence_huge_int", '{"choice": 1, "confidence": 1' + "0" * 320 + ', "achieved": true, "skip": true}'),
    ("confidence_exponent", '{"choice": 1, "confidence": 9.5e-1, "achieved": true, "skip": true}'),
    ("achieved_string", _answer(achieved="true", confidence=0.99, skip="yes")),
    ("achieved_one", _answer(achieved=1, confidence=0.99, skip=1)),
    ("achieved_null", _answer(achieved=None, confidence=0.99, skip=None)),
    ("reason_long", _answer(skip=False, confidence=0.9, reason="the banner is still on the screen " * 10)),
    ("reason_unicode", _answer(skip=False, confidence=0.9, reason="देखा 📨 बैनर ✓ " * 12)),
    ("reason_number", '{"skip": false, "confidence": 0.9, "reason": 1e20}'),
    ("reason_float_forms", '{"skip": false, "confidence": 0.9, "reason": 0.0001}'),
    ("reason_list", '{"skip": false, "confidence": 0.9, "reason": ["a", 1, 2.5, null, true, NaN, -Infinity]}'),
    ("reason_falsy", '{"skip": false, "confidence": 0.9, "reason": 0}'),
    ("reason_false", '{"skip": false, "confidence": 0.9, "reason": false}'),
    ("reason_spaces", '{"skip": false, "confidence": 0.9, "reason": "   "}'),
    ("reason_escapes", '{"skip": false, "confidence": 0.9, "reason": "tab\\there ' + U("00e9") + " "
     + U("d83d") + U("de00") + ' quote\\" back\\\\slash"}'),
    ("lone_surrogate_unread", '{"choice": 1, "confidence": 0.9, "note": "' + U("d800") + '"}'),
    ("bad_escape", '{"choice": 1, "confidence": 0.9, "note": "\\x"}'),
    ("unicode_key", '{"चुनाव": 2, "choice": 1, "confidence": 0.9}'),
    ("deep_lists", _NESTED),
    ("int_too_long", '{"choice": 1, "confidence": 0.9, "pad": ' + "9" * 4301 + "}"),
    ("int_at_limit", '{"choice": 1, "confidence": 0.9, "pad": ' + "9" * 4300 + "}"),
    ("long_prose", "Let me think. " * 400 + _answer(choice=1, confidence=0.8, achieved=True, skip=True)),
    ("leading_zero", '{"choice": 01, "confidence": 0.9}'),
    ("bare_exponent", '{"choice": 1e, "confidence": 0.9}'),
]


# -- driving the ladder ---------------------------------------------------

class _Lines:
    def __init__(self, texts):
        self.lines = [{"text": t} for t in texts]


def _ask(replies, sent):
    queue = list(replies)

    def ask(system, prompt, purpose=None):
        sent.append({"system": system, "prompt": prompt, "purpose": purpose})
        return queue.pop(0)
    return ask


def _drive(case, ask_model, ask_decided=None):
    """Run the case's question on a fresh ladder: (what it returned, what it kept, its trail and reports), as JSON."""

    call = case["call"]
    reports = []
    ladder = escalation.Ladder(ask_model=ask_model, goal=call.get("goal", ""), total=call.get("total", 0),
                               explanation=call.get("explanation", ""), ask_decided=ask_decided)
    ladder.attach(reports.append)
    ladder.typed = [tuple(p) for p in call.get("typed") or []]
    kind = case["kind"]
    patches = [mock.patch.object(ocr, "available", lambda: True)]
    if kind == escalation.GROUND:
        ranked = call["ranked"]
        picked = ladder._ground(call["step"], call["observation"], ranked)
        result = None if picked is None else {"picked": ranked.index(picked[0]), "confidence": picked[1]}
        kept = {"healed": ladder.healed}
    elif kind == escalation.JUDGE:
        result = ladder.judge(call["step"], call["observation"], call["missing"])
        kept = {"judged": ladder.judged}
    elif kind == escalation.DETOUR:
        find = call["find"]
        found = {"to": find["to"], "margin": find["margin"], "here": _Lines(find["here"]),
                 "there": _Lines(find["there"])}
        patches.append(mock.patch.object(detours, "find", lambda *a, **k: found))
        for p in patches:
            p.start()
        try:
            result = ladder.detour(call["steps"], 0, _Lines(call["now"]), None)
        finally:
            for p in reversed(patches):
                p.stop()
        kept = {"detours": ladder.detours}
    else:
        before, now = call["before"], call["now"]
        patches += [mock.patch.object(ocr, "read", lambda w, h, rgb: before),
                    mock.patch.object(ocr, "line_at", lambda lines, point: before[call["clicked"]]),
                    mock.patch.object(ocr, "read_frame", lambda frame: now)]
        for p in patches:
            p.start()
        try:
            result = ladder._point_by_text(call["step"], object(), (1280, 720, b""))
        finally:
            for p in reversed(patches):
                p.stop()
        kept = {"grounded": ladder.grounded}
    return json.loads(json.dumps({"result": result, "kept": kept, "trail": ladder.trail, "reports": reports,
                                  "model_calls": ladder.model_calls}))


def _evidence(case):
    """The evidence a ladder handing its questions to soul hands over for the case (its first question)."""

    seen = []

    def decided(system, prompt, evidence, *, purpose):
        seen.append(evidence)
        return case["answers"][0], None
    _drive(case, lambda *a, **k: "", decided)
    return json.loads(json.dumps(seen[0]))


def build():
    """The corpus from today's code."""

    out = {"systems": dict(escalation.SYSTEMS), "cases": [], "answers": []}
    for case in CASES:
        sent, answers = [], []
        evidence = _evidence(case)
        for text in case["answers"]:
            outcome = _drive(case, _ask([text], sent))
            answers.append({"text": text, "decision": escalation.DECIDE[case["kind"]](text, evidence),
                            "outcome": outcome})
        messages = [{"role": "system", "content": sent[0]["system"]}, {"role": "user", "content": sent[0]["prompt"]}]
        out["cases"].append({"name": case["name"], "kind": case["kind"], "call": case["call"], "evidence": evidence,
                             "messages": messages, "answers": answers})
    base = {kind: next(c for c in out["cases"] if c["kind"] == kind) for kind in KINDS}
    for name, text in ANSWERS:
        record = {"name": name, "text": text, "decisions": {}, "outcomes": {}}
        for kind in KINDS:
            record["decisions"][kind] = escalation.DECIDE[kind](text, base[kind]["evidence"])
            record["outcomes"][kind] = _drive(_restore(base[kind]), _ask([text], []))
        out["answers"].append(record)
    return json.loads(json.dumps(out))


def _load():
    with open(CORPUS, encoding="utf-8") as f:
        return json.load(f)


def _restore(record):
    return {"name": record["name"], "kind": record["kind"], "call": record["call"],
            "answers": [a["text"] for a in record["answers"]]}


def _base(corpus, kind):
    return next(c for c in corpus["cases"] if c["kind"] == kind)


class TodaysPathIsTheRecordedOne(unittest.TestCase):
    """A ladder with no ask_decided (the switch off): the same messages, decisions and doings as recorded."""

    def test_the_corpus_is_big_enough_and_fictional(self):
        corpus = _load()
        self.assertGreaterEqual(len(corpus["cases"]), 12)
        self.assertEqual({c["kind"] for c in corpus["cases"]}, set(KINDS))
        self.assertGreaterEqual(len(corpus["answers"]), 60)
        self.assertEqual(corpus["systems"], escalation.SYSTEMS)

    def test_messages_decisions_and_doings_are_todays(self):
        corpus = _load()
        for record in corpus["cases"]:
            with self.subTest(case=record["name"]):
                for answer in record["answers"]:
                    sent = []
                    self.assertEqual(_drive(_restore(record), _ask([answer["text"]], sent)), answer["outcome"])
                    self.assertEqual(len(sent), 1)
                    self.assertEqual([{"role": "system", "content": sent[0]["system"]},
                                      {"role": "user", "content": sent[0]["prompt"]}], record["messages"])
                    self.assertEqual(sent[0]["purpose"], "py.escalation." + record["kind"])
                    self.assertEqual(escalation.DECIDE[record["kind"]](answer["text"], record["evidence"]),
                                     answer["decision"])
        for record in corpus["answers"]:
            with self.subTest(answer=record["name"]):
                for kind in KINDS:
                    base = _base(corpus, kind)
                    self.assertEqual(escalation.DECIDE[kind](record["text"], base["evidence"]),
                                     record["decisions"][kind], kind)
                    self.assertEqual(_drive(_restore(base), _ask([record["text"]], [])), record["outcomes"][kind],
                                     kind)

    def test_the_evidence_words_todays_prompt(self):
        corpus = _load()
        for record in corpus["cases"]:
            with self.subTest(case=record["name"]):
                self.assertEqual(_evidence(_restore(record)), record["evidence"])
                self.assertEqual(escalation.PROMPTS[record["kind"]](record["evidence"]),
                                 record["messages"][1]["content"])

    def test_soul_not_deciding_is_todays_reading(self):
        # A ladder handing its questions to soul, whose soul could not decide (decision None): today's reading.
        corpus = _load()
        for record in corpus["cases"]:
            for answer in record["answers"]:
                with self.subTest(case=record["name"], answer=answer["text"][:40]):
                    asked = []

                    def decided(system, prompt, evidence, *, purpose, text=answer["text"]):
                        asked.append((system, prompt, evidence, purpose))
                        return text, None
                    self.assertEqual(_drive(_restore(record), lambda *a, **k: "", decided), answer["outcome"])
                    self.assertEqual(asked[0][:2], (record["messages"][0]["content"], record["messages"][1]["content"]))


class _Soul(BaseHTTPRequestHandler):
    """A fake soul: /api/llm/escalation answers with the queued answers; any other route is recorded and fails as the
    provider, so a test sees a call that went another way."""

    seen, answers = [], []

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        type(self).seen.append({"path": self.path, "body": json.loads(raw or b"{}")})
        if self.path != soul_call.ESCALATION_PATH or not type(self).answers:
            status, body = 418, {"error": "not this route", "where": "provider"}
        else:
            status, body = type(self).answers.pop(0)
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


def _not_found(self):
    raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
    type(self).seen.append({"path": self.path, "body": json.loads(raw or b"{}")})
    data = b'{"error": "404 page not found"}'
    self.send_response(404)
    self.send_header("Content-Type", "application/json")
    self.send_header("Content-Length", str(len(data)))
    self.end_headers()
    self.wfile.write(data)


class TheSwitchOnHandsSoulTheEvidence(unittest.TestCase):
    """A skill run's ladder with DECURIX_SOUL_ESCALATION=1 and soul up: each question's recorded evidence goes to
    /api/llm/escalation only, and soul's decision does what today's reading did."""

    def setUp(self):
        _Soul.seen, _Soul.answers = [], []
        server = ThreadingHTTPServer(("127.0.0.1", 0), _Soul)
        threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = f"http://127.0.0.1:{server.server_address[1]}"
        for p in (mock.patch.dict(os.environ, {"DECURIX_AGENT_TOKEN": "agent-token-for-tests", "DECURIX_TURN_ID": "",
                                               "DECURIX_CALLER": "", "DECURIX_SOUL_ESCALATION": "1"}),
                  mock.patch.object(config, "THEBOT_URL", url),
                  mock.patch.object(config, "HOSTED_BRAIN", True),
                  mock.patch.object(llm_client, "CHAT_URL", "http://127.0.0.1:9/never"),
                  mock.patch.object(llm_client, "PROVIDER", "azure"),
                  mock.patch.object(llm_client.usage, "add", lambda **kw: None),
                  mock.patch.object(llm_client.time, "sleep", lambda s: None),
                  mock.patch.object(ollama_client.usage, "add", lambda **kw: None),
                  mock.patch.object(ollama_client.usage, "set_model", lambda *a, **kw: None),
                  mock.patch.object(ollama_client, "ensure_model_resident", lambda: None),
                  mock.patch.object(state, "current_task_id", "t0123456789abcdef"),
                  mock.patch.object(state, "backend_ready", True),
                  mock.patch.object(task_context, "secret_values", lambda task_id=None: []),
                  mock.patch.object(approval, "IN_GATE", False),
                  mock.patch.dict(soul_call._state, {"down_until": 0.0, "absent": False, "noted": False,
                                                     "judge_absent": False, "escalation_absent": False})):
            p.start()
            self.addCleanup(p.stop)

    def _run(self, record, decision, text="{}"):
        _Soul.seen = []
        _Soul.answers = [(200, {"text": text, "truncated": False, "usage": {"prompt_tokens": 9, "completion_tokens": 3},
                                "decision": decision})]
        ladder = skill_task._ladder({"title": record["call"].get("goal", "")})
        self.assertIsNotNone(ladder.ask_decided)
        return _drive(_restore(record), ladder.ask_model, ladder.ask_decided)

    def test_every_case_on_the_hosted_brain(self):
        corpus = _load()
        for record in corpus["cases"]:
            for answer in record["answers"]:
                with self.subTest(case=record["name"], answer=answer["text"][:40]):
                    self.assertEqual(self._run(record, answer["decision"], answer["text"]), answer["outcome"])
                    self.assertEqual([s["path"] for s in _Soul.seen], [soul_call.ESCALATION_PATH])
                    body = _Soul.seen[0]["body"]
                    self.assertEqual(body["evidence"], record["evidence"])
                    self.assertEqual((body["purpose"], body["brain"], body["max_tokens"], body["temperature"],
                                      body["stop"]), ("py.escalation." + record["kind"], "hosted", 1500, None, []))
                    self.assertNotIn("messages", body)

    def test_every_answer(self):
        corpus = _load()
        for record in corpus["answers"]:
            for kind in KINDS:
                with self.subTest(answer=record["name"], kind=kind):
                    self.assertEqual(self._run(_base(corpus, kind), record["decisions"][kind], record["text"]),
                                     record["outcomes"][kind])

    def test_a_local_brain_asks_soul_with_its_own_options(self):
        corpus = _load()
        record = _base(corpus, escalation.GROUND)
        with mock.patch.object(llm_client, "PROVIDER", ""), mock.patch.object(config, "HOSTED_BRAIN", False):
            self.assertEqual(self._run(record, record["answers"][0]["decision"]), record["answers"][0]["outcome"])
        body = _Soul.seen[0]["body"]
        self.assertEqual((body["brain"], body["max_tokens"], body["temperature"], body["stop"]),
                         ("local", ollama_client.NUM_PREDICT, 0.2, list(ollama_client.STOP_SEQUENCES)))
        self.assertEqual(body["evidence"], record["evidence"])

    def test_a_decision_that_is_not_one_fails_closed(self):
        corpus = _load()
        bad = {
            escalation.GROUND: [{"choice": 1, "confidence": 0.5, "verdict": "chosen"}, {"choice": 9, "confidence": 1,
                                "verdict": "chosen"}, {"choice": True, "confidence": 1, "verdict": "chosen"},
                                {"choice": 1, "confidence": "0.9", "verdict": "chosen"}, []],
            escalation.JUDGE: [{"achieved": True, "confidence": 0.5}, {"achieved": 1, "confidence": 0.99},
                               {"achieved": True, "confidence": 1.5}],
            escalation.DETOUR: [{"verdict": "skip", "confidence": 0.5, "reason": ""}, {"verdict": "maybe",
                                "confidence": 0.9, "reason": ""}, {"verdict": "keep", "confidence": 0.9, "reason": 1}],
            escalation.POINT: [{"choice": 3, "confidence": 0.9, "verdict": "doubt"}, {"choice": 0, "confidence": 0.9,
                               "verdict": "chosen"}],
        }
        for kind, decisions in bad.items():
            record = _base(corpus, kind)
            for decision in decisions:
                with self.subTest(kind=kind, decision=decision):
                    done = self._run(record, decision)
                    self.assertTrue(any("could not be asked" in line for line in done["trail"]), done["trail"])
                    if kind == escalation.DETOUR:
                        # Not asked, a detour is the screens' to decide alone (their margin), as today.
                        self.assertFalse(any("the engine agreed" in line for line in done["trail"]))
                        self.assertEqual(done["result"], None if record["call"]["find"]["margin"] < detours.SURE_MARGIN
                                         else record["call"]["find"]["to"])
                    else:
                        self.assertIn(done["result"], (None, False))

    def test_a_soul_without_the_route_goes_todays_way(self):
        # An older soul answers 404 on /api/llm/escalation: the question is today's, through /api/llm/call (refused
        # too, so straight to the proxy, which is not there: the engine could not be asked - never a pick).
        record = _base(_load(), escalation.GROUND)
        with mock.patch.object(_Soul, "do_POST", _not_found), mock.patch.object(llm_client, "RETRY_BUDGET_SECONDS", 0):
            done = self._run(record, None)
        self.assertIsNone(done["result"])
        self.assertTrue(any("could not be asked" in line for line in done["trail"]))
        self.assertEqual([s["path"] for s in _Soul.seen], [soul_call.ESCALATION_PATH, soul_call.CALL_PATH])
        self.assertTrue(soul_call._state["escalation_absent"])

    def test_the_switch_is_read_for_each_run(self):
        with mock.patch.dict(os.environ, {"DECURIX_SOUL_ESCALATION": "0"}):
            self.assertFalse(escalation.soul_escalates())
            self.assertIsNone(skill_task._ladder({"title": "x"}).ask_decided)
        self.assertTrue(escalation.soul_escalates())
        self.assertIsNotNone(skill_task._ladder({"title": "x"}).ask_decided)


class TheBrokerCarriesTheDecision(unittest.TestCase):
    """In broker mode the replay's ladder runs on the screen side and its questions come back to the agent: an ask with
    its evidence is answered by the agent's ask_decided, soul's decision beside the text."""

    def test_an_ask_with_evidence_is_decided_and_one_without_is_asked(self):
        class Pipe(io.BytesIO):
            def flush(self):
                pass

        class Process:
            stdin = Pipe()

        decided, asked = [], []

        def on_decide(system, prompt, evidence, *, purpose):
            decided.append((system, prompt, evidence, purpose))
            return "text", {"choice": 1}

        def on_ask(system, prompt, purpose):
            asked.append((system, prompt, purpose))
            return "plain"
        process = Process()
        screen_call._answer(process, {"system": "s", "prompt": "p", "purpose": "py.escalation.ground",
                                      "evidence": {"goal": ""}}, on_ask, on_decide)
        screen_call._answer(process, {"system": "s", "prompt": "p", "purpose": "py.escalation.ground"}, on_ask,
                            on_decide)
        screen_call._answer(process, {"system": "s", "prompt": "p", "purpose": "py.escalation.ground",
                                      "evidence": {"goal": ""}}, on_ask)
        replies = [json.loads(line) for line in process.stdin.getvalue().splitlines()]
        self.assertEqual(replies, [{"answer": "text", "decision": {"choice": 1}}, {"answer": "plain"},
                                   {"answer": "plain"}])
        self.assertEqual(decided, [("s", "p", {"goal": ""}, "py.escalation.ground")])
        self.assertEqual(len(asked), 2)

    def test_the_screen_sides_ladder_hands_soul_its_questions_only_when_the_callers_does(self):
        def ask(system, prompt, purpose=None):
            return ""

        def decide(system, prompt, evidence, *, purpose):
            return "", None
        for caller, want in ((escalation.Ladder(ask_model=ask, ask_decided=decide), True),
                             (escalation.Ladder(ask_model=ask), False), (escalation.Ladder(ask_decided=decide), False)):
            state_ = screen_call.loads(screen_call.dumps(screen_call.ladder_state(caller)))
            self.assertNotIn("ask_decided", state_)
            side = screen_call.new_ladder(state_, ask, decide)
            self.assertEqual(side.ask_decided is decide, want)
            screen_call.apply_state(caller, screen_call.ladder_state(side))
            self.assertEqual(caller.ask_decided is decide, caller.ask_decided is not None)


if __name__ == "__main__":
    if "--write" in sys.argv:
        target = Path(sys.argv[sys.argv.index("--write") + 1]) if len(sys.argv) > sys.argv.index("--write") + 1 \
            else CORPUS
        target.parent.mkdir(parents=True, exist_ok=True)
        with open(target, "w", encoding="utf-8", newline="\n") as f:
            json.dump(build(), f, ensure_ascii=False, indent=1)
            f.write("\n")
        print(f"wrote {target}")
    else:
        unittest.main()
