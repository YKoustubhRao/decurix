"""The `handoff` command (tools/handoff.py) against a mocked control plane: what it sends, how it waits, and that only
a result the receiving worker's check confirmed ever reads as done."""

import io
import json
import unittest
import urllib.error
from unittest import mock

from decurix_agent import config, task_context
from decurix_agent.tools import handoff


class _Response:
    def __init__(self, body, status=200):
        self.status = status
        self._body = json.dumps(body).encode("utf-8")

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _record(status, verified=False, report="", **extra):
    rec = {"id": "ho_1", "kind": "delegate", "status": status, "to_worker": "w2", "task_id": "t1", "reason": "",
           "result": {"report": report, "verified": verified, "evidence": [], "files": [], "open_questions": []}}
    rec.update(extra)
    return rec


class HandoffToolTest(unittest.TestCase):
    def setUp(self):
        self.requests = []
        self.answers = []
        for p in (
            mock.patch.object(handoff.urllib.request, "urlopen", self._urlopen),
            mock.patch.object(config, "DESKTOP_ID", "w1"),
            mock.patch.object(config, "CONTROL_PLANE_URL", "http://cp:8080/"),
            mock.patch.object(task_context, "current", lambda: "t0123456789abcdef"),
            mock.patch.object(handoff.time, "sleep", lambda s: None),
            mock.patch.object(handoff, "_withhold", lambda text: (text or "").replace("hunter2", "[withheld]")),
        ):
            p.start()
            self.addCleanup(p.stop)

    def _urlopen(self, request, timeout=None):
        body = json.loads(request.data) if request.data else None
        self.requests.append((request.get_method(), request.full_url, body))
        answer = self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return _Response(answer)

    def run_cli(self, *argv):
        out = io.StringIO()
        code = handoff.main(list(argv), out=out)
        return code, out.getvalue()

    def test_delegate_posts_the_typed_request_and_waits_for_a_verified_result(self):
        done = _record("done", verified=True, report="Saved it.\nChecked on the screen: the record is there.",
                       result=None)
        done["result"] = {"report": "Saved it.\nChecked on the screen: the record is there.", "verified": True,
                          "evidence": ["Checked on the screen: the record is there."],
                          "files": ["/home/headless/handoffs/ho_1/returned/out.txt"], "open_questions": []}
        self.answers = [_record("running"), _record("running"), done]
        code, out = self.run_cli("delegate", "--role", "reviewer", "--request", "Review the sheet, password hunter2",
                                 "--input", "period=last month", "--input", "pin=hunter2",
                                 "--file", "/home/headless/in/a.csv", "--expect", "a review note", "--wait", "60")
        self.assertEqual(code, handoff.EXIT_DONE)
        method, url, body = self.requests[0]
        self.assertEqual((method, url), ("POST", "http://cp:8080/api/handoffs"))
        self.assertEqual(body["desktop_id"], "w1")
        self.assertEqual(body["from_task"], "t0123456789abcdef")
        self.assertEqual(body["to_role"], "reviewer")
        self.assertNotIn("to_worker", body)
        self.assertEqual(body["inputs"], {"period": "last month", "pin": "[withheld]"})
        self.assertNotIn("hunter2", json.dumps(body))
        self.assertEqual(body["files"], ["/home/headless/in/a.csv"])
        self.assertEqual(body["expected"], "a review note")
        self.assertEqual(self.requests[1][1], "http://cp:8080/api/handoffs/get?id=ho_1")
        self.assertIn("=== HANDOFF ho_1: DONE (verified) ===", out)
        self.assertIn("verified: yes", out)
        self.assertIn("  - Checked on the screen: the record is there.", out)
        self.assertIn("/returned/out.txt", out)
        self.assertIn("=== END HANDOFF ho_1 ===", out)

    def test_an_unverified_result_is_not_done(self):
        self.answers = [_record("not_done", verified=False, report="All done!",
                                reason="its final check did not confirm the result")]
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "do it", "--wait", "0")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        self.assertIn("NOT DONE (not verified)", out)
        self.assertIn("verified: no", out)
        self.assertIn("Do not report it as done", out)
        self.assertEqual(self.requests[0][2]["to_worker"], "w2")

    def test_a_done_status_without_verified_still_reads_not_verified(self):
        self.answers = [_record("done", verified=False)]
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "x", "--wait", "0")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        self.assertIn("verified: no", out)

    def test_wait_runs_out_while_running(self):
        clock = iter([0, 0, 5, 11, 20, 30]).__next__
        self.answers = [_record("running")] * 10
        rec = handoff.wait_for(_record("running"), 10, poll=5, sleep=lambda s: None, clock=clock)
        self.assertEqual(rec["status"], "running")
        self.assertEqual(handoff.exit_code(rec), handoff.EXIT_RUNNING)
        self.assertIn("STILL RUNNING", handoff.render(rec))
        self.assertIn("handoff status ho_1", handoff.render(rec))

    def test_open_questions_are_listed(self):
        self.answers = [_record("running", result=None)]
        self.answers[0]["result"] = {"report": "", "verified": False, "evidence": [], "files": [],
                                     "open_questions": ["Which month?"]}
        code, out = self.run_cli("status", "ho_1")
        self.assertEqual(code, handoff.EXIT_RUNNING)
        self.assertIn("open questions:\n  - Which month?", out)

    def test_ask_posts_the_question_and_marks_the_answer_advice(self):
        rec = _record("not_done", report="Column F.", kind="ask")
        self.answers = [{"handoff": rec, "answer": "Column F.", "verified": False, "answered_by": "w2"}]
        code, out = self.run_cli("ask", "--skill", "invoices", "Which", "column?")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        method, url, body = self.requests[0]
        self.assertEqual(url, "http://cp:8080/api/handoffs/ask")
        self.assertEqual((body["skill"], body["question"], body["desktop_id"]), ("invoices", "Which column?", "w1"))
        self.assertLessEqual(body["wait"], handoff.ASK_HOLD)
        self.assertIn("=== ANSWER ho_1:", out)
        self.assertIn("  Column F.", out)
        self.assertIn("advice", out)

    def test_refusal_prints_the_control_planes_reason(self):
        err = urllib.error.HTTPError("http://cp", 400, "Bad Request", {},
                                     io.BytesIO(json.dumps({"error": "request is required"}).encode()))
        self.answers = [err]
        code, out = self.run_cli("delegate", "--to", "w2", "--request", " ")
        self.assertEqual(code, handoff.EXIT_ERROR)
        self.assertIn("request is required", out)

    def test_unreachable_control_plane(self):
        self.answers = [urllib.error.URLError("refused")]
        code, out = self.run_cli("status", "ho_1")
        self.assertEqual(code, handoff.EXIT_ERROR)
        self.assertIn("could not be reached", out)

    def test_bad_input_pair(self):
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "x", "--input", "novalue")
        self.assertEqual(code, handoff.EXIT_ERROR)
        self.assertEqual(self.requests, [])

    def test_cancel_names_the_sender(self):
        self.answers = [_record("cancelled", reason="cancelled before a result came back")]
        code, out = self.run_cli("cancel", "ho_1")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        self.assertEqual(self.requests[0][2], {"id": "ho_1", "desktop_id": "w1"})
        self.assertIn("CANCELLED", out)


if __name__ == "__main__":
    unittest.main()
