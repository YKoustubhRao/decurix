"""
The final check's prompt and parse in soul are today's, byte for byte (plan rule 3, phase 2: `py.judge`).

With DECURIX_SOUL_JUDGE=1, verify.judge hands soul the judge's evidence as data (verify.judge_evidence: the task, the
values, the observations, the files' facts, the pictures as data: URLs) and soul's POST /api/llm/judge builds the prompt
and reads the answer (agent/thebot/internal/judge). The proof the move changes nothing is a recorded corpus,
agent/thebot/internal/judge/testdata/parity.json (fictional data only):

- for each case, the messages today's path sends (JUDGE_SYSTEM and the user's parts, in order) and the evidence the
  switch hands soul; for each recorded answer, the verdict today's _parse makes of it, and soul's decision;
- here: today's path (the switch off) still sends exactly those messages and returns those verdicts; with the switch
  on, verify.judge sends soul exactly that evidence (and, for a closer look, the terms and the lines), never asks the
  proxy or /api/llm/call, and maps soul's decision to the same verdict, error mapping included;
- in Go (internal/judge parity_test.go, cmd/soul judge_test.go): soul builds the same messages from that evidence,
  makes the same decision of each answer, and its provider sends the proxy the very bytes /api/llm/call sends for
  today's messages.

Regenerate after a deliberate change to the prompt or the parse (then soul's copy changes with it, in the same commit):
    cd agent && PYTHONPATH=".;tests" py -3 tests/test_judge_parity.py --write
"""

import base64
import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config, llm_client, shared, soul_call, state, task_context, verify
from decurix_agent.computer import approval

CORPUS = Path(__file__).resolve().parents[1] / "thebot" / "internal" / "judge" / "testdata" / "parity.json"

PNG = b"\x89PNG\r\n\x1a\n"
JPEG = b"\xff\xd8\xff\xe0"


def _b64(raw):
    return base64.b64encode(raw).decode("ascii")


def _verdict(verdict, confidence, seen="", why="", **more):
    return json.dumps({"verdict": verdict, "confidence": confidence, "seen": seen, "why": why, **more})


NOTE = ("On this desktop /home/headless/PC IS the person's own folder C:\\Users\\Pat\\Ledgers on their computer (the "
        "same folder, shared): a file saved under /home/headless/PC is saved in C:\\Users\\Pat\\Ledgers.")
LONG_TASK = ("  Reconcile the petty-cash book for Bluefin Bakery (₹ amounts) 🧾 with the receipts listed below.\n"
             + "".join(f"- receipt R-{n:04d}: ₹{n * 17 % 900 + 100}.00 paid to Vendor {n} ✓\n" for n in range(560))
             + "BINDING: the owner said the R-0007 receipt is a duplicate. ")
OBSERVED_LONG = [f"[{n:03d}] gui read: row {n} of the ledger shows ₹{n * 3}.50 for खाता {n}" for n in range(520)]
MANY_VALUES = {f"line_{n}__qty": f"{n} boxes of Fictional Item {n} (वस्तु)" for n in range(70)}
MANY_VALUES.update({f"item_{n}__name": f"Item described as 'Sample Widget {n}' in the order" for n in range(60)})

CASES = [
    {"name": "bare", "call": {"task": "Send the March statement to the client."},
     "answers": [_verdict("done", 0.95, "the sent folder shows the statement", "sent as asked")]},
    {"name": "files_only", "call": {
        "task": "Build totals.csv from the two bills in the shared folder.", "screen": False,
        "claim": "Built totals.csv: 2 rows, total 1,250.00.",
        "produced": "--- totals.csv\nBill,Amount\nB-101,750.00\nB-102,500.00",
        "found": "  totals.csv: present, 54 bytes, saved 2026-03-04 10:12  ",
        "deliverables": ["/home/headless/PC/out/totals.csv", "summary.txt"]},
     "env": {"mapping_note": NOTE},
     "answers": [_verdict("not_done", 0.9, "totals.csv has two rows", "the total is 1,250.00 but rows sum to 1,250.00?",
                          put_back=["bills.xlsx"])]},
    {"name": "values_and_choices", "live": True, "call": {
        "task": "Make a draft quotation for Northwind Traders: 40 notebooks at 175.75.",
        "claim": "Saved draft QTN-0042.",
        "values": {"customer": "Northwind Traders", "qty": 40, "rate": 175.75, "item__1": "Premium Ruled Notebook A5",
                   "note": "", "po": None, "urgent": True, "discount": 0},
        "chosen": ["item"], "criteria": "  A saved draft quotation with this run's customer.  ",
        "steps": "1. Open Quotation\n2. click 'Acme Stationers' in the list\n3. Save"},
     "answers": [_verdict("done", 0.6, "a draft quotation", "probably right")]},
    {"name": "observed_found_screen", "live": True, "reference": True, "call": {
        "task": "Enter the sales order from the customer's email and leave it as a draft.",
        "claim": "I stopped: the customer Contoso Ltd does not exist in the application and I may not create it.",
        "observed": ["gui observe: Sales Order list, 3 rows", "gui read: Customer field: 'Contoso Ltd' - no match"],
        "found": "no sales order was saved during this task",
        "screen_text": "Customer: \nTotal: 0.00\n\tStatus: Draft"},
     "answers": [_verdict("cannot", 0.85, "an empty sales order form", "the customer is missing; the worker said so")]},
    {"name": "documents", "live": True, "reference": True, "call": {
        "task": "Enter purchase order /home/headless/inbox/po-7781.pdf as a sales order for Fabrikam.",
        "claim": "Saved SO-0107 with 3 lines, total 4,200.00."},
     "env": {"doc_text": "--- /home/headless/inbox/po-7781.pdf\nPurchase Order 7781\nDate: 03/02/2026\n"
                         "1. Blue Pens x 100 @ 12.00\n2. Staplers x 20 @ 90.00\n3. Folders x 60 @ 20.00\nTotal: 4,200.00",
             "pictures": [["page1.png", PNG + b"fictional scan page 1"], ["page2.jpg", JPEG + b"fictional scan page 2"]]},
     "answers": ["Here is my verdict:\n" + _verdict("done", 0.92, "SO-0107 saved", "lines match the PO") + "\nThanks."]},
    {"name": "long_task_cut", "call": {"task": LONG_TASK, "claim": "Reconciled; R-0007 left out as a duplicate."},
     "answers": ['{"verdict": "done", "confidence": 0.9, "seen": "the book shows']},
    {"name": "blank_parts", "call": {
        "task": "\n\tCheck the inbox for the supplier's reply.\n", "claim": "   ", "criteria": "   ", "steps": "",
        "observed": [""], "found": "", "screen_text": "", "produced": ""},
     "answers": ["I think it is done, the reply is there."]},
    {"name": "observed_tail", "live": True, "call": {
        "task": "Copy the ledger's closing balances into balances.xlsx.", "observed": OBSERVED_LONG,
        "claim": "Copied 520 balances."},
     "answers": [_verdict("done", 0.88, "balances.xlsx open", "all rows copied", put_back=["ledger.xlsx", 7, "  "])]},
    {"name": "look_closer", "call": {
        "task": "Audit the expense claims in audit.xlsx and mark each approved or refused.", "screen": False,
        "produced": "--- audit.xlsx (first 40 of 300 rows)\nLine,Employee,Claimed,Allowed\nCL-4101,Ravi Iyer,100,100",
        "claim": "Marked all 300 claims; CL-4142 allowed 1,200."},
     "look_up": "--- audit.xlsx\n(line 43) CL-4142 | Neha Joshi | 1850 | 1200 | approved by requester",
     "answers": [_verdict("unsure", 0.3, "the CL-4142 row is not shown", "cannot see it",
                          look=["CL-4142", 1042, "  ", "Neha   Joshi", None]),
                 _verdict("done", 0.95, "CL-4142 allowed 1200", "matches the report")]},
    {"name": "look_closer_nothing_found", "call": {
        "task": "Audit the expense claims in audit.xlsx.", "screen": False, "produced": "--- audit.xlsx (cut)"},
     "look_up": "",
     "answers": [_verdict("unsure", 0.4, "a row is not shown", "need CL-9", look=["CL-9"])]},
    {"name": "values_cut", "live": True, "call": {
        "task": "Enter the order lines.", "values": MANY_VALUES, "chosen": ["item_3", "item_7", "line_2"] + [
            f"item_{n}" for n in range(10, 60)]},
     "answers": [_verdict("not_done", 1, "two lines are missing", "fewer items than asked")]},
    {"name": "unicode_mix", "call": {
        "task": "ग्राहक को चालान भेजें (send the invoice to the customer).",
        "criteria": "  " + "चालान सही ग्राहक को जाना चाहिए। " * 220 + " end",
        "steps": "\u2003" + "क्लिक करें 'भेजें' → " * 120,
        "found": "\u3000" + "फ़ाइल मौजूद है ✓ " * 300,
        "claim": "भेजा गया 📨 " * 420},
     "answers": [_verdict("DONE ", "0.97", "the invoice is sent", "sent")]},
    {"name": "screen_false_with_live", "live": True, "call": {
        "task": "Save the report as report.pdf.", "screen": False, "deliverables": ["/home/headless/PC/report.pdf"]},
     "env": {"mapping_note": NOTE},
     "answers": [_verdict("Not Done", 0.7, "report.pdf missing", "not in the folder")]},
]

# Answers as a model writes them, each read by today's _parse and by soul: every shape it accepts, cuts or refuses.
ANSWERS = [
    ("done", _verdict("done", 0.95, "saved", "matches")),
    ("done_unsure_confidence", _verdict("done", 0.79, "saved", "maybe")),
    ("done_exact_threshold", _verdict("done", 0.8, "saved", "matches")),
    ("not_done", _verdict("not_done", 0.9, "an error banner", "validation failed")),
    ("cannot", _verdict("cannot", 0.9, "no customer", "the worker stopped and said why")),
    ("unsure", _verdict("unsure", 0.5, "unclear", "cannot tell")),
    ("spaced_upper_verdict", _verdict(" NOT DONE\n", 0.9, "x", "y")),
    ("unknown_verdict", _verdict("maybe", 0.9, "x", "y")),
    ("verdict_missing", json.dumps({"confidence": 0.9, "seen": "x"})),
    ("verdict_number", '{"verdict": 1, "confidence": 0.9}'),
    ("verdict_python_space", '{"verdict": "\\u001cdone\\u001f", "confidence": 0.9, "seen": "s", "why": "w"}'),
    ("confidence_string", _verdict("done", "0.9", "x", "y")),
    ("confidence_string_underscore", _verdict("done", " 0.9_5 ", "x", "y")),
    ("confidence_string_bad", _verdict("done", "high", "x", "y")),
    ("confidence_nan_string", _verdict("done", "nan", "x", "y")),
    ("confidence_inf_string", _verdict("done", "-Infinity", "x", "y")),
    ("confidence_over", _verdict("done", 2, "x", "y")),
    ("confidence_under", _verdict("not_done", -1, "x", "y")),
    ("confidence_null", '{"verdict": "done", "confidence": null, "seen": "x", "why": "y"}'),
    ("confidence_true", '{"verdict": "done", "confidence": true, "seen": "x", "why": "y"}'),
    ("confidence_list", '{"verdict": "done", "confidence": [0.9], "seen": "x", "why": "y"}'),
    ("confidence_missing", '{"verdict": "done", "seen": "x", "why": "y"}'),
    ("confidence_exponent", _verdict("done", 9.5e-1, "x", "y")),
    # float() exactly as Python reads it (2026-11-08): a control character before the digits is no number, other
    # scripts' digits and white space are, and an integer past a float's range raises OverflowError (no number).
    ("confidence_control_char", json.dumps({"verdict": "done", "confidence": chr(0x1c) + "0.9", "seen": "x", "why": "y"})),
    ("confidence_arabic_digits", json.dumps({"verdict": "done", "confidence": chr(0x660) + "." + chr(0x669),
                                             "seen": "x", "why": "y"})),
    ("confidence_em_space", json.dumps({"verdict": "done", "confidence": "0.9" + chr(0x2003), "seen": "x", "why": "y"})),
    ("confidence_huge_int", '{"verdict": "done", "confidence": ' + "9" * 321 + ', "seen": "x", "why": "y"}'),
    ("prose_around", "Looking at the screen.\n```json\n" + _verdict("done", 0.9, "a", "b") + "\n```\nDone."),
    ("seen_why_spacing", _verdict("done", 0.9, "  the   form\n\tis saved  ", "line one\nline two\u00a0end")),
    ("seen_why_long", _verdict("done", 0.9, "देखा " * 120, "why " * 150)),
    ("seen_non_string", '{"verdict": "unsure", "confidence": 0.5, "seen": ["a", 1, 2.5, null, true, {"k": "v"}], '
                        '"why": 1e20}'),
    ("seen_falsy", '{"verdict": "unsure", "confidence": 0.5, "seen": 0, "why": false}'),
    ("seen_float_forms", '{"verdict": "unsure", "confidence": 0.5, "seen": 0.0001, "why": 1e-05}'),
    ("seen_big_int", '{"verdict": "unsure", "confidence": 0.5, "seen": 123456789012345678901234567890, "why": -0}'),
    ("seen_dict_repr", '{"verdict": "unsure", "confidence": 0.5, "seen": {"a": "it\'s", "b": "say \\"hi\\"", '
                       '"c": "both \' and \\"", "d": "tab\\there\\u200b\\u00e9\\ud83d\\ude00"}, "why": [1.0, 100.0, 1e16]}'),
    ("look_terms", _verdict("unsure", 0.4, "x", "y", look=["CL-1", 2, "  ", "a  b", None, False, "c" * 100, "d", "e"])),
    ("look_not_list", _verdict("unsure", 0.4, "x", "y", look="CL-1")),
    ("put_back_mixed", _verdict("done", 0.9, "x", "y", put_back=["a.xlsx", 3, "  ", " b  c.csv ", None] +
                                                                [f"f{n}.txt" for n in range(25)])),
    ("put_back_not_list", _verdict("done", 0.9, "x", "y", put_back="a.xlsx")),
    ("put_back_empty", _verdict("done", 0.9, "x", "y", put_back=[])),
    ("duplicate_keys", '{"verdict": "done", "confidence": 0.95, "verdict": "not_done", "seen": "x", "why": "y"}'),
    ("nested_braces", '{"verdict": "not_done", "confidence": 0.9, "seen": "the text {total} shows", "why": "{x}"}'),
    ("cut_off", '{"verdict": "done", "confidence": 0.9, "seen": "the screen shows'),
    ("cut_off_quotes", "{\"verdict\": \"done\", \"seen\": \"it's\n\ttab ₹ \u200b"),
    ("cut_off_single_quote_only", "{'verdict': 'done', it's"),
    ("cut_off_long", "{" + "x" * 250),
    ("not_json", "The task looks done to me."),
    ("not_json_quote", "It's done."),
    ("empty", ""),
    ("bad_json", '{"verdict": done}'),
    ("two_objects", '{"verdict": "done"} and {"verdict": "not_done"}'),
    ("trailing_comma", '{"verdict": "done", "confidence": 0.9,}'),
    ("array_inside", '{"verdict": "unsure", "confidence": 0.2, "look": ["{a}", "b}"]}'),
]


def _decision(text):
    """What soul answers for `text` (internal/judge Decide), worked out by today's code: _parse's verdict with the
    not-sure-enough rule applied, or the error _parse raised. The error mapping stays in Python (verify.judge)."""

    try:
        found = verify._parse(text)
    except verify.CutOff as exc:
        return {"error": str(exc), "cut_off": True}
    except ValueError as exc:
        return {"error": str(exc), "cut_off": False}
    if found["verdict"] == verify.DONE and found["confidence"] < verify.DONE_CONFIDENCE:
        found["why"] = f"not sure enough ({found['confidence']:.2f}): {found['why']}"
        found["verdict"] = verify.UNSURE
    return found


def _kwargs(case):
    """verify.judge's arguments for a case (its pictures as bytes)."""

    kwargs = dict(case["call"])
    if case.get("live"):
        kwargs["live_png"] = PNG + f"fictional live screen of {case['name']}".encode()
    if case.get("reference"):
        kwargs["reference_png"] = PNG + f"fictional demonstration screen of {case['name']}".encode()
    return kwargs


class _Judging:
    """verify.judge with its desktop's reading stood in for: the documents and the shared folder's note come from
    the case, and each model call's messages are kept."""

    def __init__(self, case, folder):
        self.case, self.sent, self.replies = case, [], [a for a in case["answers"]]
        env = case.get("env") or {}
        self.pictures = []
        for name, raw in env.get("pictures") or []:
            path = os.path.join(folder, name)
            with open(path, "wb") as f:
                f.write(raw)
            self.pictures.append(path)
        self.patches = [
            mock.patch.object(verify, "available", lambda: True),
            mock.patch.object(verify, "_documents", lambda task: (env.get("doc_text") or "", list(self.pictures))),
            mock.patch.object(shared, "mapping_note", lambda: env.get("mapping_note") or ""),
            mock.patch.object(verify, "log", lambda *a, **k: None),
        ]

    def chat(self, messages, max_tokens=None, purpose=None, **kw):
        self.sent.append(messages)
        return {"content": self.replies.pop(0)}, {}

    def run(self, chat=True):
        look_up = None
        if self.case.get("look_up") is not None:
            look_up = lambda terms: self.case["look_up"]  # noqa: E731
        patches = self.patches + ([mock.patch.object(verify.llm_client, "chat", self.chat)] if chat else [])
        for p in patches:
            p.start()
        try:
            return verify.judge(**_kwargs(self.case), look_up=look_up)
        finally:
            for p in reversed(patches):
                p.stop()


def build():
    """The corpus from today's code."""

    out = {"system": verify.JUDGE_SYSTEM, "judge_tokens": verify.JUDGE_TOKENS, "cases": [], "answers": []}
    with tempfile.TemporaryDirectory() as folder:
        for case in CASES:
            judging = _Judging(case, folder)
            judged = judging.run()
            record ={"name": case["name"], "call": case["call"], "live": bool(case.get("live")),
                      "reference": bool(case.get("reference")), "look_up": case.get("look_up"),
                      "env": {"mapping_note": (case.get("env") or {}).get("mapping_note") or "",
                              "doc_text": (case.get("env") or {}).get("doc_text") or "",
                              "pictures": [[n, _b64(raw)] for n, raw in (case.get("env") or {}).get("pictures") or []]},
                      "messages": judging.sent, "judged": judged,
                      "answers": [{"text": a, "decision": _decision(a)} for a in case["answers"][:len(judging.sent)]]}
            if hasattr(verify, "judge_evidence"):
                record["evidence"] = _evidence(case, folder)
            out["cases"].append(record)
    for name, text in ANSWERS:
        case = {"name": name, "call": {"task": "Check the work.", "screen": False}, "answers": [text]}
        with tempfile.TemporaryDirectory() as folder:
            judged = _Judging(case, folder).run()
        out["answers"].append({"name": name, "text": text, "decision": _decision(text), "judged": judged})
    return out


def _evidence(case, folder):
    """The evidence the switch hands soul for a case (verify.judge_evidence, its documents and folder note stood in)."""

    judging = _Judging(case, folder)
    for p in judging.patches:
        p.start()
    try:
        kwargs = _kwargs(case)
        kwargs.pop("look_up", None)
        return verify.judge_evidence(**kwargs)
    finally:
        for p in reversed(judging.patches):
            p.stop()


def _load():
    with open(CORPUS, encoding="utf-8") as f:
        return json.load(f)


def _restore(record):
    """A recorded case as CASES holds it: the pictures as bytes, the values in order."""

    case = {k: record[k] for k in ("name", "call", "live", "reference", "look_up")}
    case["call"] = dict(record["call"])
    case["answers"] = [a["text"] for a in record["answers"]]
    case["env"] = {"mapping_note": record["env"]["mapping_note"], "doc_text": record["env"]["doc_text"],
                   "pictures": [[n, base64.b64decode(b)] for n, b in record["env"]["pictures"]]}
    return case


class TodaysPathIsTheRecordedOne(unittest.TestCase):
    """The switch off: the same messages and the same verdicts as recorded (the corpus is today's path)."""

    def test_the_corpus_is_big_enough_and_fictional(self):
        corpus = _load()
        self.assertGreaterEqual(len(corpus["cases"]), 10)
        self.assertGreaterEqual(len(corpus["answers"]), 30)
        self.assertEqual(corpus["system"], verify.JUDGE_SYSTEM)
        self.assertEqual(corpus["judge_tokens"], verify.JUDGE_TOKENS)

    def test_messages_and_verdicts_are_todays(self):
        corpus = _load()
        with mock.patch.dict(os.environ, {"DECURIX_SOUL_JUDGE": ""}), tempfile.TemporaryDirectory() as folder:
            for record in corpus["cases"]:
                with self.subTest(case=record["name"]):
                    judging = _Judging(_restore(record), folder)
                    judged = judging.run()
                    self.assertEqual(judging.sent, record["messages"])
                    self.assertEqual(judged, record["judged"])
            for record in corpus["answers"]:
                with self.subTest(answer=record["name"]):
                    self.assertEqual(_decision(record["text"]), record["decision"])
                    case = {"name": record["name"], "call": {"task": "Check the work.", "screen": False},
                            "answers": [record["text"]]}
                    self.assertEqual(_Judging(case, folder).run(), record["judged"])

    def test_the_evidence_is_what_the_switch_hands_soul(self):
        corpus = _load()
        with tempfile.TemporaryDirectory() as folder:
            for record in corpus["cases"]:
                with self.subTest(case=record["name"]):
                    self.assertEqual(_evidence(_restore(record), folder), record["evidence"])


class _Soul(BaseHTTPRequestHandler):
    """A fake soul: /api/llm/judge answers each recorded answer with soul's recorded decision; any other route is
    recorded and refused, so a test sees a call that went another way."""

    seen, answers = [], []

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        type(self).seen.append({"path": self.path, "body": json.loads(raw or b"{}")})
        if self.path != soul_call.JUDGE_PATH or not type(self).answers:
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


class TheSwitchOnHandsSoulTheEvidence(unittest.TestCase):
    """DECURIX_SOUL_JUDGE=1 with soul up: verify.judge sends the recorded evidence to /api/llm/judge, never asks the
    proxy or /api/llm/call, and maps soul's decision to the recorded verdict (the error mapping is Python's)."""

    def setUp(self):
        _Soul.seen, _Soul.answers = [], []
        server = ThreadingHTTPServer(("127.0.0.1", 0), _Soul)
        threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.direct = []
        url = f"http://127.0.0.1:{server.server_address[1]}"
        for p in (mock.patch.dict(os.environ, {"DECURIX_AGENT_TOKEN": "agent-token-for-tests", "DECURIX_TURN_ID": "",
                                               "DECURIX_CALLER": "", "DECURIX_SOUL_JUDGE": "1"}),
                  mock.patch.object(config, "THEBOT_URL", url),
                  mock.patch.object(llm_client, "CHAT_URL", "http://127.0.0.1:9/never"),
                  mock.patch.object(llm_client, "PROVIDER", "azure"),
                  mock.patch.object(llm_client.usage, "add", lambda **kw: None),
                  mock.patch.object(llm_client.time, "sleep", lambda s: None),
                  mock.patch.object(state, "current_task_id", "t0123456789abcdef"),
                  mock.patch.object(task_context, "secret_values", lambda task_id=None: []),
                  mock.patch.object(approval, "IN_GATE", False),
                  mock.patch.dict(soul_call._state, {"down_until": 0.0, "absent": False, "noted": False,
                                                     "judge_absent": False})):
            p.start()
            self.addCleanup(p.stop)

    def _answer(self, text, decision):
        return 200, {"text": text, "truncated": False, "usage": {"prompt_tokens": 9, "completion_tokens": 3},
                     "decision": decision}

    def test_every_case(self):
        corpus = _load()
        with tempfile.TemporaryDirectory() as folder:
            for record in corpus["cases"]:
                with self.subTest(case=record["name"]):
                    _Soul.seen = []
                    _Soul.answers = [self._answer(a["text"], a["decision"]) for a in record["answers"]]
                    judged = _Judging(_restore(record), folder).run(chat=False)
                    self.assertEqual(judged, record["judged"])
                    self.assertEqual([s["path"] for s in _Soul.seen], [soul_call.JUDGE_PATH] * len(record["answers"]))
                    first = _Soul.seen[0]["body"]
                    self.assertEqual(first["evidence"], record["evidence"])
                    self.assertEqual((first["purpose"], first["brain"], first["max_tokens"], first["look"],
                                      first["closer"]), ("py.judge", "hosted", verify.JUDGE_TOKENS, [], ""))
                    self.assertNotIn("messages", first)
                    if len(_Soul.seen) > 1:
                        again = _Soul.seen[1]["body"]
                        self.assertEqual(again["evidence"], record["evidence"])
                        self.assertEqual(again["look"], record["answers"][0]["decision"]["look"])
                        self.assertEqual(again["closer"], record["look_up"])

    def test_every_answer(self):
        corpus = _load()
        with tempfile.TemporaryDirectory() as folder:
            for record in corpus["answers"]:
                with self.subTest(answer=record["name"]):
                    _Soul.answers = [self._answer(record["text"], record["decision"])]
                    case = {"name": record["name"], "call": {"task": "Check the work.", "screen": False},
                            "answers": [record["text"]]}
                    self.assertEqual(_Judging(case, folder).run(chat=False), record["judged"])

    def test_an_unreadable_decision_fails_closed(self):
        with tempfile.TemporaryDirectory() as folder:
            for decision in ({"verdict": "certainly"}, {"verdict": "done", "confidence": "high"}, [],
                             {"verdict": "done", "confidence": 0.9, "look": "x"}):
                with self.subTest(decision=decision):
                    _Soul.answers = [self._answer("{}", decision)] * 8
                    case = {"name": "x", "call": {"task": "Check.", "screen": False}, "answers": ["{}"]}
                    found = _Judging(case, folder).run(chat=False)
                    self.assertEqual(found["verdict"], verify.UNSURE)
                    self.assertEqual(found["confidence"], 0.0)

    def test_a_soul_without_the_route_goes_todays_way(self):
        # An older soul answers 404 on /api/llm/judge: the call is today's, through /api/llm/call (here refused too,
        # so straight to the proxy, which is not there: the check could not be made - unsure, never done).
        _Soul.answers = []
        with mock.patch.object(_Soul, "do_POST", _not_found), tempfile.TemporaryDirectory() as folder, \
                mock.patch.object(llm_client, "RETRY_BUDGET_SECONDS", 0):
            case = {"name": "x", "call": {"task": "Check.", "screen": False}, "answers": ["{}"]}
            found = _Judging(case, folder).run(chat=False)
        self.assertEqual(found["verdict"], verify.UNSURE)
        self.assertEqual([s["path"] for s in _Soul.seen], [soul_call.JUDGE_PATH, soul_call.CALL_PATH])
        self.assertTrue(soul_call._state["judge_absent"])
        self.assertTrue(soul_call._state["absent"])

    def test_the_switch_is_read_per_call(self):
        with mock.patch.dict(os.environ, {"DECURIX_SOUL_JUDGE": "0"}):
            self.assertFalse(verify.soul_judges())
        self.assertTrue(verify.soul_judges())


def _not_found(self):
    raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
    type(self).seen.append({"path": self.path, "body": json.loads(raw or b"{}")})
    data = b'{"error": "404 page not found"}'
    self.send_response(404)
    self.send_header("Content-Type", "application/json")
    self.send_header("Content-Length", str(len(data)))
    self.end_headers()
    self.wfile.write(data)


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
