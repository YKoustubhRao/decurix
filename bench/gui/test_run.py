"""The benchmark process must fail when independent task checks reject a run."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import run


class BenchmarkExitTests(unittest.TestCase):
    def run_checks(self, verdicts):
        with tempfile.TemporaryDirectory() as folder, contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(run.sys, "argv", ["run.py", "--engine", "oracle",
                "--container", "isolated-test", "--tasks", "web-login,web-create-user", "--out", folder]))
            for name in ("wait_for_agent", "install_fixtures", "reset_task"):
                stack.enter_context(patch.object(run, name))
            stack.enter_context(patch.object(run, "run_oracle", return_value={"answer": "success", "seconds": 1}))
            stack.enter_context(patch.object(run, "check_task", return_value={"independent": True}))
            stack.enter_context(patch.object(run, "verdict", side_effect=verdicts))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = run.main()
            report = json.loads(next(Path(folder).glob("*.json")).read_text())
            return code, report["engines"]["oracle"]

    def test_rejected_business_result_fails_process_despite_successful_actions(self):
        code, report = self.run_checks([(True, ""), (False, "wrong saved result")])
        self.assertEqual(code, 1)
        self.assertEqual((report["passed"], report["total"]), (1, 2))
        self.assertEqual(report["tasks"][1]["why"], "wrong saved result")

    def test_all_checked_results_pass_process(self):
        code, report = self.run_checks([(True, ""), (True, "")])
        self.assertEqual(code, 0)
        self.assertEqual((report["passed"], report["total"]), (2, 2))


class AnswerKeyTests(unittest.TestCase):
    """The agent's home gets the apps' files, never the answer key; the check
    runs as root from a root-only copy."""

    def test_the_agent_copy_holds_no_answer_key(self):
        self.assertNotIn("tasks.json", run.AGENT_FILES)
        self.assertFalse(run.ORACLE.startswith("/home/"))
        commands = []
        with patch.object(run, "docker", side_effect=lambda *a, **k: commands.append((a, k))), \
                patch.object(run.subprocess, "run", side_effect=lambda cmd, **k: commands.append((tuple(cmd), k))):
            run.install_fixtures("c")
        copies = [c[0] for c in commands if c[0][:2] == ("docker", "cp")]
        for cmd in copies:
            if cmd[-1].startswith("c:" + run.REMOTE):
                self.assertNotIn("tasks.json", cmd[2], cmd)
        self.assertTrue(any(cmd[-1] == f"c:{run.ORACLE}/tasks.json" for cmd in copies))
        self.assertTrue(any("go-rwx" in " ".join(c[0]) and c[1].get("user") == "root" for c in commands))

    def test_reset_is_handed_its_start_and_check_runs_from_the_root_copy(self):
        task = {"id": "calc-sum", "start": {"calc": "budget.ods"}, "check": {"type": "answer_number", "value": 5}}
        reset = run.reset_command(task)
        self.assertTrue(reset[1].startswith(run.REMOTE))
        self.assertEqual(json.loads(reset[reset.index("--start") + 1]), {"calc": "budget.ods"})
        self.assertNotIn("answer_number", " ".join(reset))
        self.assertTrue(run.check_command(task)[1].startswith(run.ORACLE))
        calls = []
        with patch.object(run, "docker", side_effect=lambda *a, **k: calls.append((a, k)) or
                          type("P", (), {"stdout": '{"pass": true, "detail": ""}', "stderr": "", "returncode": 0})()):
            run.check_task("c", task, "5")
        self.assertTrue(all(k.get("user") == "root" for _, k in calls))


class PanelStateTests(unittest.TestCase):
    """The admin panel's state, which the checks read, is out of the agent's
    reach: the panel runs as its own user, its state lives outside the agent's
    home in a 0700 directory, and nothing writes it as the agent's user."""

    def test_the_panel_runs_as_its_own_user_with_private_state(self):
        start = run.panel_start_command("c")
        self.assertEqual(start[start.index("-u") + 1], run.PANEL_USER)
        self.assertNotEqual(run.PANEL_USER, "headless")
        self.assertIn(run.PANEL_STATE, start)
        self.assertFalse(run.PANEL_STATE.startswith("/home/"))
        script = run.panel_setup_script()
        self.assertIn(f"chown {run.PANEL_USER}:{run.PANEL_USER} {run.PANEL_STATE_DIR}", script)
        self.assertIn(f"chmod 0700 {run.PANEL_STATE_DIR}", script)
        self.assertNotIn("adminpanel.py", run.AGENT_FILES)

    def test_reset_and_check_never_write_the_state_as_the_agent(self):
        calls = []
        def fake_docker(*a, **k):
            calls.append((a, k))
            return type("P", (), {"stdout": '{"pass": true}', "stderr": "", "returncode": 0})()
        with patch.object(run, "docker", side_effect=fake_docker), \
                patch.object(run, "check_panel_owner"), \
                patch.object(run.subprocess, "run", side_effect=lambda cmd, **k: calls.append((tuple(cmd), k))):
            run.reset_task("c", {"id": "web-login", "start": {"browser": "/login"}})
            run.check_task("c", {"id": "web-login"}, "")
        for args, kwargs in calls:
            line = " ".join(args)
            if run.PANEL_STATE in line or "adminpanel" in line:
                user = kwargs.get("user") or (args[args.index("-u") + 1] if "-u" in args else "headless")
                self.assertNotEqual(user, "headless", line)
        headless = [" ".join(a) for a, k in calls if k.get("user", "headless") == "headless" and a and a[0] != "docker"]
        self.assertTrue(all(run.PANEL_STATE not in line for line in headless))
        self.assertTrue(any(k.get("user") == "root" and "check" in a for a, k in calls))


class PanelStartTests(unittest.TestCase):
    """The panel starts with a clean environment in isolated mode, and a reset
    fails unless its port is held by the panel's user."""

    TCP = ("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
           "   0: 0100007F:223D 00000000:0000 0A 00000000:00000000 00:00000000 00000000   {uid}        0 1 1\n"
           "   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2 1\n")

    def test_the_panel_starts_clean_and_isolated(self):
        start = run.panel_start_command("c")
        tail = start[start.index("c") + 1:]
        self.assertEqual(tail[:3], ["env", "-i", "PATH=/usr/bin:/bin"])
        self.assertEqual(tail[3:5], ["python3", "-I"])

    def test_listener_uids_reads_only_the_listening_port(self):
        self.assertEqual(run.listener_uids(8765, self.TCP.format(uid=999)), [999])
        self.assertEqual(run.listener_uids(8765, self.TCP.replace(" 0A ", " 01 ").format(uid=999)), [])

    def _owner_check(self, table):
        def fake_docker(container, *args, **kw):
            out = "999\n" if args[:2] == ("id", "-u") else table
            return type("P", (), {"stdout": out, "stderr": "", "returncode": 0})()
        with patch.object(run, "docker", side_effect=fake_docker), patch.object(run.time, "sleep"):
            run.check_panel_owner("c", attempts=2)

    def test_a_port_held_by_another_user_fails_the_reset(self):
        self._owner_check(self.TCP.format(uid=999))  # the panel's own: passes
        with self.assertRaises(RuntimeError):
            self._owner_check(self.TCP.format(uid=1000))  # the agent's user took the port
        with self.assertRaises(RuntimeError):
            self._owner_check(self.TCP.replace("223D", "0050").format(uid=999))  # nothing listening

    def test_reset_checks_the_owner_after_starting_the_panel(self):
        order = []
        with patch.object(run, "docker", side_effect=lambda *a, **k: order.append("docker") or
                          type("P", (), {"stdout": "", "stderr": "", "returncode": 0})()), \
                patch.object(run.subprocess, "run", side_effect=lambda cmd, **k: order.append("start")), \
                patch.object(run, "check_panel_owner", side_effect=lambda c: order.append("owner")):
            run.reset_task("c", {"id": "web-login", "start": {"browser": "/login"}})
        self.assertLess(order.index("start"), order.index("owner"))


if __name__ == "__main__":
    unittest.main()
