"""evals/run.py: arms, the suite tar, the agent tree's git command, and what it writes from a control plane.
Run: py -3 -m unittest discover -s evals/tests"""

import contextlib
import gzip
import io
import json
import sys
import tarfile
import tempfile
import threading
import unittest
import unittest.mock
from email.parser import BytesParser
from email.policy import default as email_policy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

EVALS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(EVALS))

import run  # noqa: E402


class ArmTest(unittest.TestCase):
    def test_arms(self):
        self.assertEqual(run.parse_arm("default"), {"name": "default", "kind": "model", "provider": "azure", "model": "default", "expert": False})
        self.assertEqual(run.parse_arm("fast+expert"), {"name": "fast+expert", "kind": "model", "provider": "azure", "model": "fast", "expert": True})
        a = run.parse_arm("default@eb15981")
        self.assertEqual((a["model"], a["agent_tree_rev"], a["expert"]), ("default", "eb15981", False))
        a = run.parse_arm("claude:fast+expert@HEAD~2")
        self.assertEqual((a["provider"], a["model"], a["expert"], a["agent_tree_rev"]), ("claude", "fast", True, "HEAD~2"))
        self.assertEqual(run.parse_arm("oracle"), {"name": "oracle", "kind": "oracle", "expert": False})
        self.assertEqual(run.parse_arm("nothing")["kind"], "nothing")
        self.assertEqual(run.parse_arm("ask")["kind"], "ask")
        a = run.parse_arm("default+expert+soul")
        self.assertEqual((a["name"], a["expert"], a.get("soul")), ("default+expert+soul", True, True))
        self.assertNotIn("soul", run.parse_arm("default+expert"))
        a = run.parse_arm("default+soul@eb15981")
        self.assertEqual((a["expert"], a["soul"], a["agent_tree_rev"]), (False, True, "eb15981"))
        for bad in ("gpt-5", "default+expert+expert", "openai:default", "default@", "fast@a b", "", "default+soul+expert",
                    "default+soul+soul", "oracle+soul"):
            with self.assertRaises(run.Usage, msg=bad):
                run.parse_arm(bad)


class SuiteTarTest(unittest.TestCase):
    def test_the_suite_tar_holds_the_suite_and_its_mocks(self):
        body = run.suite_tar(EVALS / "suites" / "core-v1")
        with tarfile.open(fileobj=io.BytesIO(body)) as tar:
            names = tar.getnames()
            self.assertIn("suite.json", names)
            self.assertIn("mocks/records-app/mock_app.py", names)
            self.assertIn("cases/bill-entry-day-first/files/bill.pdf", names)
            self.assertIn("cases/web-create-user/fixture.json", names)
            self.assertFalse([n for n in names if n.startswith("/") or ".." in n or "__pycache__" in n])
            self.assertEqual(names, sorted(names))
            self.assertEqual(tar.extractfile("mocks/records-app/mock_app.py").read(),
                             (EVALS / "mocks" / "records-app" / "mock_app.py").read_bytes())
            self.assertTrue(all(m.mtime == 0 and m.mode == 0o644 for m in tar.getmembers()))
        self.assertEqual(body, run.suite_tar(EVALS / "suites" / "core-v1"), "the same files give the same tar")

    def test_a_missing_mock_is_said(self):
        with tempfile.TemporaryDirectory() as d:
            suite = Path(d) / "suites" / "s"
            suite.mkdir(parents=True)
            (suite / "suite.json").write_text(json.dumps({"mocks": {"nope": {"hosts": ["x.test"]}}}), encoding="utf-8")
            with self.assertRaises(run.Usage):
                run.suite_tar(suite)


class AgentTreeTest(unittest.TestCase):
    def test_the_git_archive_command_line(self):
        calls = []

        def fake(cmd, **kw):
            calls.append((cmd, kw))
            return type("P", (), {"returncode": 0, "stdout": b"TAR", "stderr": b""})()

        self.assertEqual(run.agent_tree("eb15981", run=fake), b"TAR")
        cmd = calls[0][0]
        self.assertEqual(cmd[0], "git")
        self.assertEqual(cmd[-4:], ["archive", "--format=tar", "--prefix=decurix_agent/", "eb15981:agent/decurix_agent"])
        self.assertTrue(calls[0][1]["capture_output"])

        def failing(cmd, **kw):
            return type("P", (), {"returncode": 128, "stdout": b"", "stderr": b"fatal: not a valid object name"})()

        with self.assertRaises(run.Usage):
            run.agent_tree("nope", run=failing)


class RevTest(unittest.TestCase):
    def test_a_rev_is_resolved_to_its_commit(self):
        if not (run.ROOT / ".git").exists():
            self.skipTest("not a git checkout (an exported tree)")
        sha = run.resolve_rev("HEAD")
        self.assertRegex(sha, r"^[0-9a-f]{40}$")
        with self.assertRaises(run.Usage):
            run.resolve_rev("no-such-rev-here")


class FakeControlPlane(BaseHTTPRequestHandler):
    started = []
    polls = 0

    def _send(self, status, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        msg = BytesParser(policy=email_policy).parsebytes(
            b"Content-Type: " + self.headers["Content-Type"].encode() + b"\r\n\r\n" + raw)
        parts = {p.get_param("name", header="content-disposition"): p.get_payload(decode=True) for p in msg.iter_parts()}
        FakeControlPlane.started.append(parts)
        if self.path == "/api/evals/runs":
            return self._send(202, {"run": {"id": "e1", "suite_name": "core-v1", "suite_sha": "ab" * 32, "status": "running"}})
        self._send(404, {"error": "not found"})

    def do_GET(self):
        if self.path == "/api/evals/runs/e1":
            FakeControlPlane.polls += 1
            done = FakeControlPlane.polls > 1
            return self._send(200, {"run": {"id": "e1", "status": "done" if done else "running"}, "active": not done,
                                    "progress": {"done": 3, "total": 4, "arm": "default", "repeat": 1, "case": "c1"}})
        if self.path == "/api/evals/runs/e1/table?format=md":
            return self._send(200, b"# Eval e1: core-v1\n", "text/markdown")
        if self.path == "/api/evals/runs/e1/rows":
            return self._send(200, {"rows": [{"case_id": "c1", "passed": True}, {"case_id": "c2", "passed": False}]})
        if self.path == "/api/evals/runs/e9":
            return self._send(404, {"error": "no such run"})
        self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


class CommandTest(unittest.TestCase):
    def setUp(self):
        FakeControlPlane.started, FakeControlPlane.polls = [], 0
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeControlPlane)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.cp = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.out = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.enterContext(unittest.mock.patch.object(run, "RESULTS", self.out))
        self.enterContext(unittest.mock.patch.object(run, "POLL_SECONDS", 0))

    def main(self, *argv):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            code = run.main(["--cp", self.cp, *argv])
        return code, out.getvalue()

    def test_run_waits_and_writes_the_table_and_rows(self):
        code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "default", "--arm", "nothing",
                               "--repeats", "2", "--cases", "calc-sum,web-create-user")
        self.assertEqual(code, 0, said)
        sent = FakeControlPlane.started[0]
        arms = json.loads(sent["arms"])
        self.assertEqual([a["name"] for a in arms], ["default", "nothing"])
        self.assertEqual(sent["repeats"], b"2")
        self.assertEqual(sent["cases"], b"calc-sum,web-create-user")
        # Sent gzip-compressed (F-ENV-1), and the same suite packs to the same bytes.
        self.assertEqual(sent["suite"][:2], b"\x1f\x8b")
        self.assertEqual(gzip.decompress(sent["suite"]), run.suite_tar(EVALS / "suites" / "core-v1"))
        with tarfile.open(fileobj=io.BytesIO(sent["suite"])) as tar:
            self.assertIn("suite.json", tar.getnames())
        self.assertEqual((self.out / "e1" / "table.md").read_text(encoding="utf-8"), "# Eval e1: core-v1\n")
        rows = (self.out / "e1" / "rows.jsonl").read_text(encoding="utf-8").splitlines()
        self.assertEqual([json.loads(r)["case_id"] for r in rows], ["c1", "c2"])
        self.assertIn("3/4 rows", said)
        self.assertNotIn("carry_state", sent)  # never the default

    def test_carry_state_is_asked_for_only_with_its_flag(self):
        code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "default",
                               "--repeats", "5", "--carry-state", "--detach")
        self.assertEqual(code, 0, said)
        self.assertEqual(FakeControlPlane.started[0]["carry_state"], b"1")

    def test_an_arm_at_a_rev_sends_its_tree(self):
        full = "eb15981" + "0" * 33
        with unittest.mock.patch.object(run, "agent_tree", return_value=b"TREE") as tree,                 unittest.mock.patch.object(run, "resolve_rev", return_value=full) as rev:
            code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "default@eb15981", "--repeats", "1", "--detach")
        self.assertEqual(code, 0, said)
        rev.assert_called_once_with("eb15981")
        tree.assert_called_once_with(full)
        sent = FakeControlPlane.started[0]
        self.assertEqual(gzip.decompress(sent["tree_default@eb15981"]), b"TREE")
        self.assertEqual(json.loads(sent["arms"])[0]["agent_tree_rev"], full)
        self.assertIn("n=1: not a result", said)
        self.assertTrue(said.strip().endswith("e1"))

    def test_the_l3_shape_old_and_new_code_both_with_the_expert(self):
        full = "eb15981" + "0" * 33
        with unittest.mock.patch.object(run, "agent_tree", return_value=b"OLD") as tree, \
                unittest.mock.patch.object(run, "resolve_rev", return_value=full):
            code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "default+expert@eb15981",
                                   "--arm", "default+expert", "--repeats", "5", "--detach")
        self.assertEqual(code, 0, said)
        tree.assert_called_once_with(full)
        sent = FakeControlPlane.started[0]
        arms = json.loads(sent["arms"])
        self.assertEqual([(a["name"], a["expert"], a.get("agent_tree_rev")) for a in arms],
                         [("default+expert@eb15981", True, full), ("default+expert", True, None)])
        self.assertEqual(gzip.decompress(sent["tree_default+expert@eb15981"]), b"OLD")
        self.assertNotIn("tree_default+expert", sent)

    def test_an_arm_at_a_named_rev_is_named_by_its_commit(self):
        full = "abcdef1234567890" + "0" * 24
        with unittest.mock.patch.object(run, "agent_tree", return_value=b"T"), \
                unittest.mock.patch.object(run, "resolve_rev", return_value=full):
            code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "default+expert@HEAD",
                                   "--repeats", "1", "--detach")
        self.assertEqual(code, 0, said)
        sent = FakeControlPlane.started[0]
        self.assertEqual(json.loads(sent["arms"])[0]["name"], "default+expert@abcdef123456")
        self.assertIn("tree_default+expert@abcdef123456", sent)

    def test_an_oracle_at_a_rev_sends_its_tree(self):
        full = "eb15981" + "0" * 33
        with unittest.mock.patch.object(run, "agent_tree", return_value=b"OLD") as tree, \
                unittest.mock.patch.object(run, "resolve_rev", return_value=full):
            code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "oracle@eb15981",
                                   "--arm", "oracle", "--repeats", "1", "--detach")
        self.assertEqual(code, 0, said)
        tree.assert_called_once_with(full)
        sent = FakeControlPlane.started[0]
        arms = json.loads(sent["arms"])
        self.assertEqual([(a["name"], a["kind"], a.get("agent_tree_rev")) for a in arms],
                         [("oracle@eb15981", "oracle", full), ("oracle", "oracle", None)])
        self.assertEqual(gzip.decompress(sent["tree_oracle@eb15981"]), b"OLD")

    def test_table_rows_and_status(self):
        self.assertEqual(self.main("table", "e1")[0], 0)
        self.assertTrue((self.out / "e1" / "table.md").exists())
        code, said = self.main("rows", "e1")
        self.assertEqual(code, 0)
        self.assertIn("2 rows", said)
        code, said = self.main("status", "e1")
        self.assertIn("e1: running", said)

    def test_an_unknown_run_is_said(self):
        with self.assertRaises(SystemExit) as err:
            self.main("status", "e9")
        self.assertIn("404", str(err.exception.code))

    def test_a_bad_arm_is_a_usage_error(self):
        code, said = self.main("run", "--suite", str(EVALS / "suites" / "core-v1"), "--arm", "gpt-5")
        self.assertEqual(code, 2)
        self.assertIn("an arm is", said)

    def test_the_curve_from_the_control_plane_says_a_reset_run_is_no_curve(self):
        code, said = self.main("curve", "e1")
        self.assertEqual(code, 0, said)
        self.assertIn("not a learning curve", said)
        self.assertTrue((self.out / "e1" / "curve.md").exists())


def _row(case, arm="default+expert", passed=True, tokens_in=100_000, tokens_out=5_000, cached=50_000, calls=20,
         q_other=0, false_done=False, **kw):
    return dict(case_id=case, arm=arm, passed=passed, tokens_in=tokens_in, tokens_out=tokens_out, tokens_cached=cached,
                calls=calls, q_required=0, q_other=q_other, false_done=false_done, **kw)


class CurveTest(unittest.TestCase):
    ORDER = ["b1", "r1", "b2", "r2", "b3"]
    KINDS = {"b1": "doc_to_record", "b2": "doc_to_record", "b3": "doc_to_record", "r1": "reconcile", "r2": "reconcile"}

    def test_a_point_is_the_median_of_its_valid_rows(self):
        cell = run.curve_cell([_row("b1", tokens_in=100_000, calls=10), _row("b1", tokens_in=300_000, calls=30),
                               _row("b1", tokens_in=200_000, calls=20, passed=False),
                               _row("b1", tokens_in=9_000_000, calls=900, infra=True)])
        self.assertEqual((cell["n"], cell["left_out"], cell["pass"]), (3, 1, 2))
        self.assertEqual((cell["tokens_in"], cell["calls"]), (200_000, 20))
        self.assertAlmostEqual(cell["cached_share"], 150_000 / 600_000)

    def test_later_cases_cheaper_at_the_same_correctness_meet_the_bar(self):
        rows = [_row("b1", tokens_in=400_000, q_other=0), _row("r1", tokens_in=300_000),
                _row("b2", tokens_in=200_000), _row("r2", tokens_in=310_000), _row("b3", tokens_in=150_000)]
        points, verdicts = run.curve(rows, self.ORDER, self.KINDS)
        self.assertEqual([c for c, _ in points["default+expert"]], self.ORDER)
        self.assertTrue(verdicts["default+expert"]["doc_to_record"]["met"])
        rec = verdicts["default+expert"]["reconcile"]
        self.assertFalse(rec["met"])
        self.assertIn("not below", rec["missed"][0])

    def test_a_kind_that_never_passes_does_not_meet_the_bar(self):
        rows = [_row("b1", tokens_in=1_400_000, passed=False), _row("b2", tokens_in=200_000, passed=False),
                _row("b3", tokens_in=100_000, passed=False)]
        v = run.curve(rows, self.ORDER, self.KINDS)[1]["default+expert"]["doc_to_record"]
        self.assertFalse(v["met"])
        self.assertIn("no later case passed", v["missed"][0])
        # A first case that failed and later ones that pass still meet it.
        rows = [_row("b1", tokens_in=400_000, passed=False), _row("b2", tokens_in=200_000), _row("b3", tokens_in=100_000)]
        self.assertTrue(run.curve(rows, self.ORDER, self.KINDS)[1]["default+expert"]["doc_to_record"]["met"])

    def test_cheaper_by_skipping_a_question_or_a_false_done_does_not(self):
        rows = [_row("b1", tokens_in=400_000), _row("b2", tokens_in=100_000, passed=False),
                _row("b3", tokens_in=100_000, false_done=True, passed=False)]
        v = run.curve(rows, self.ORDER, self.KINDS)[1]["default+expert"]["doc_to_record"]
        self.assertFalse(v["met"])
        self.assertEqual(v["later_false_done"], 1)
        self.assertTrue(any("pass" in m for m in v["missed"]))
        rows = [_row("b1", tokens_in=400_000), _row("b2", tokens_in=100_000, q_other=2), _row("b3", tokens_in=100_000)]
        v = run.curve(rows, self.ORDER, self.KINDS)[1]["default+expert"]["doc_to_record"]
        self.assertFalse(v["met"])
        self.assertTrue(any("not required" in m for m in v["missed"]))

    def test_the_curve_command_reads_rows_and_the_suite(self):
        suite = EVALS / "suites" / "learn-v1"
        order, kinds = run.suite_kinds(suite)
        self.assertEqual(len(order), 10)
        self.assertEqual(sorted(set(kinds.values())), ["doc_to_record", "reconcile"])
        with tempfile.TemporaryDirectory() as d:
            rows_file = Path(d) / "rows.jsonl"
            rows = [_row(c, tokens_in=500_000 - 40_000 * i, calls=40 - 3 * i) for i, c in enumerate(order)]
            rows_file.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
            out = io.StringIO()
            with contextlib.redirect_stdout(out), unittest.mock.patch.object(run, "RESULTS", Path(d)):
                code = run.main(["curve", "e7", "--suite", str(suite), "--rows", str(rows_file)])
            said = out.getvalue()
            self.assertEqual(code, 0, said)
            self.assertIn("| 1 | " + order[0] + " | doc_to_record | 1/1 |", said)
            self.assertIn("| 40 |", said)  # calls
            self.assertIn("Cheaper at the same correctness: **met**", said)
            self.assertTrue((Path(d) / "e7" / "curve.md").exists())



if __name__ == "__main__":
    unittest.main()
