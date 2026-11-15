"""The `handoff` command (tools/handoff.py) against a mocked control plane: what it sends, how it waits, and that only
a result the receiving worker's check confirmed ever reads as done."""

import io
import json
import os
import tempfile
import unittest
import urllib.error
from pathlib import Path
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
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name)
        self.work = self.home / "work"
        (self.work / "in").mkdir(parents=True)
        (self.work / "in" / "a.csv").write_text("a,b\n1,2\n", encoding="utf-8")
        for p in (
            mock.patch.object(config, "WORK_DIR", self.work),
            mock.patch.object(config, "DECURIX_DIR", self.home / ".decurix"),
            mock.patch.dict(os.environ, {"DECURIX_SHARED_FOLDER": ""}),
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
                                 "--file", str(self.work / "in" / "a.csv"), "--expect", "a review note", "--wait", "60")
        self.assertEqual(code, handoff.EXIT_DONE)
        method, url, body = self.requests[0]
        self.assertEqual((method, url), ("POST", "http://cp:8080/api/handoffs"))
        self.assertEqual(body["desktop_id"], "w1")
        self.assertEqual(body["from_task"], "t0123456789abcdef")
        self.assertEqual(body["to_role"], "reviewer")
        self.assertNotIn("to_worker", body)
        self.assertEqual(body["inputs"], {"period": "last month", "pin": "[withheld]"})
        self.assertNotIn("hunter2", json.dumps(body))
        self.assertEqual(body["files"], [os.path.abspath(str(self.work / "in" / "a.csv"))])
        self.assertEqual(body["expected"], "a review note")
        self.assertEqual(self.requests[1][1], "http://cp:8080/api/handoffs/get?id=ho_1")
        self.assertIn("=== HANDOFF ho_1: DONE (verified) ===", out)
        self.assertIn("verified: yes", out)
        self.assertIn("  - Checked on the screen: the record is there.", out)
        self.assertIn("/returned/out.txt", out)
        self.assertIn("=== END HANDOFF ho_1 ===", out)

    def test_a_file_under_a_dot_folder_or_outside_the_work_is_never_carried(self):
        # Review 2026-11-02: a handoff could copy ~/.claude/.credentials.json to another worker.
        claude = self.home / ".claude"
        claude.mkdir()
        (claude / ".credentials.json").write_text("{}", encoding="utf-8")
        (self.work / ".ssh").mkdir()
        (self.work / ".ssh" / "id_rsa").write_text("k", encoding="utf-8")
        (self.home / ".decurix").mkdir()
        (self.home / ".decurix" / "withheld-secrets").write_text("x", encoding="utf-8")
        (self.home / "notes.txt").write_text("x", encoding="utf-8")
        for path in (claude / ".credentials.json", self.work / ".ssh" / "id_rsa",
                     self.home / ".decurix" / "withheld-secrets", self.home / "notes.txt",
                     self.work / "in" / ".." / ".." / ".claude" / ".credentials.json"):
            self.requests.clear()
            code, out = self.run_cli("delegate", "--to", "w2", "--request", "do it", "--file", str(path))
            self.assertEqual(code, handoff.EXIT_ERROR, path)
            self.assertIn("is not sent", out)
            self.assertEqual(self.requests, [], "nothing reached the control plane")

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


    def test_who_lists_the_other_workers_with_their_roles_and_skills(self):
        # Workers guessed role and skill names for --role/--skill and got "no other worker holds ...".
        self.answers = [{
            "skills": [
                {"name": "gstr1-prepare", "latest": 3, "roles": ["filing"],
                 "installs": [{"worker": "w2", "name": "gstr1-prepare", "version": 3},
                              {"worker": "w1", "name": "gstr1-prepare", "version": 3}]},
                {"name": "enter-bill", "latest": 1, "roles": [],
                 "installs": [{"worker": "w3", "name": "enter-bill", "version": 1}]},
            ],
            "workers": {"w1": ["clerk"], "w2": ["filing", "Reviewer"], "w4": ["finance"]},
        }]
        code, out = self.run_cli("who")
        self.assertEqual(code, handoff.EXIT_DONE)
        self.assertEqual(self.requests, [("GET", "http://cp:8080/api/skillhub", None)])
        self.assertIn("worker w2: roles: Reviewer, filing; skills: gstr1-prepare (v3)", out)
        self.assertIn("worker w3: roles: none; skills: enter-bill (v1)", out)
        self.assertIn("worker w4: roles: finance; skills: none", out)
        self.assertNotIn("worker w1", out)  # this desktop itself is not someone to hand work to
        self.assertTrue(out.strip().startswith("=== WORKERS ===") and out.strip().endswith("=== END WORKERS ==="))
        self.assertNotIn("=== HANDOFF", out)  # never read as a handed-back result

    def test_who_with_no_other_worker_says_to_ask_the_person(self):
        self.answers = [{"skills": [], "workers": {"w1": ["clerk"]}}]
        code, out = self.run_cli("who")
        self.assertEqual(code, handoff.EXIT_DONE)
        self.assertIn("No other worker", out)
        self.assertIn("ask the person", out)

    def test_who_when_the_registry_cannot_be_read(self):
        self.answers = [urllib.error.URLError("refused")]
        code, out = self.run_cli("who")
        self.assertEqual(code, handoff.EXIT_ERROR)
        self.assertIn("could not be reached", out)

    def test_who_is_bounded(self):
        many = {"skills": [], "workers": {f"w{n:03d}": ["r"] for n in range(handoff.MAX_WORKERS + 5)}}
        self.answers = [many]
        code, out = self.run_cli("who")
        self.assertEqual(out.count("\nworker "), handoff.MAX_WORKERS)
        self.assertIn("more workers not shown", out)

    def test_a_contract_is_sent_and_the_checked_object_printed(self):
        contract = '{"type": "object", "required": ["total"], "properties": {"total": {"type": "number"}}}'
        done = _record("done", verified=True, report="Totalled.")
        done["result"].update({"data": {"total": 12.5}, "schema_errors": [], "interrupted": False})
        self.answers = [done]
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "Total it", "--schema", contract, "--wait", "0")
        self.assertEqual(code, handoff.EXIT_DONE)
        self.assertEqual(self.requests[0][2]["output_schema"], json.loads(contract))
        self.assertIn('data: {"total": 12.5}', out)
        self.assertNotIn("contract errors", out)

    def test_a_contract_that_is_not_json_is_refused_before_anything_is_sent(self):
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "Total it", "--schema", "total: number")
        self.assertEqual(code, handoff.EXIT_ERROR)
        self.assertIn("--schema is not JSON", out)
        self.assertEqual(self.requests, [])

    def test_an_interrupted_receiver_reads_as_a_partial_result(self):
        failed = _record("failed", report="(stopped part way; its last steps:)\nread 14 invoices")
        failed["result"].update({"interrupted": True, "data": {"total": 4}, "schema_errors": []})
        self.answers = [failed]
        code, out = self.run_cli("delegate", "--to", "w2", "--request", "Total it", "--wait", "0")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        self.assertIn("=== HANDOFF ho_1: FAILED ===", out)
        self.assertIn("interrupted: yes", out)
        self.assertIn("NOT verified done", out)

    def test_contract_errors_are_listed(self):
        nd = _record("not_done", report="Totalled.")
        nd["result"]["schema_errors"] = ['key "total" is string, the contract says number']
        self.answers = [nd]
        code, out = self.run_cli("status", "ho_1")
        self.assertEqual(code, handoff.EXIT_NOT_DONE)
        self.assertIn('contract errors:\n  - key "total" is string', out)


if __name__ == "__main__":
    unittest.main()
