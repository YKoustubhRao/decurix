"""
decurix_tools, the read-only tool library for the worker's own scripts: reading verbs only (an action or a flag that
writes is refused before anything runs), a call budget per script, parsed results, and gui's exit 3 (approval needed)
printed unchanged and ending the script with 3 whatever the script catches. The commands are stand-ins here.
"""

import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest import mock

import decurix_tools

# A stand-in for gui, office and lookup: notes its argv, prints what STUB_OUT says, exits STUB_CODE.
STUB = textwrap.dedent('''
    import json, os, sys
    with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as f:
        f.write(json.dumps(sys.argv[1:]) + "\\n")
    sys.stdout.write(os.environ.get("STUB_OUT", ""))
    sys.exit(int(os.environ.get("STUB_CODE", "0")))
''')

OBSERVE = ('Screen 1280x720. Active window: "Ledger"\n== Ledger ==\ne3 push button "Save" [focused]\n'
           'e4 text "Amount" value="1,200.00" [editable]\nmenu: e1 e2\n')


class _Stubbed(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.stub = root / "stub.py"
        self.stub.write_text(STUB, encoding="utf-8")
        self.log = root / "calls.jsonl"
        commands = {name: [sys.executable, str(self.stub), name] for name in ("gui", "office", "lookup")}
        for p in (mock.patch.object(decurix_tools, "COMMANDS", commands),
                  mock.patch.dict(os.environ, {"STUB_LOG": str(self.log), "STUB_OUT": "", "STUB_CODE": "0"}),
                  mock.patch.dict(decurix_tools._calls, {"made": 0})):
            p.start()
            self.addCleanup(p.stop)
        os.environ.pop("DECURIX_TOOLS_BUDGET", None)

    def calls(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text(encoding="utf-8").splitlines()]


class OnlyReading(_Stubbed):
    def test_every_action_is_refused_before_anything_runs(self):
        for verb in ("click", "type", "key", "open", "select", "upload", "drag", "scroll", "replay", "do", "record",
                     "front", "verify", "guide", "ask", "double-click", "dblclick"):
            with self.assertRaises(decurix_tools.ToolRefused, msg=verb):
                decurix_tools.gui(verb, "e1")
        for verb in ("update", "create", "calculate", "email", "fill"):
            with self.assertRaises(decurix_tools.ToolRefused, msg=verb):
                decurix_tools.office(verb, "book.xlsx")
        self.assertEqual(self.calls(), [])
        self.assertEqual(decurix_tools.calls_left(), decurix_tools.BUDGET)  # (a refusal spends nothing)

    def test_a_flag_that_writes_is_refused(self):
        for args in (("observe", "--screenshot", "/tmp/x.png"), ("observe", "--screenshot=/tmp/x.png"),
                     ("skill", "pay", "--look", "2"), ("recall", "--learn", "x"), ("observe", "--screen", "x")):
            with self.assertRaises(decurix_tools.ToolRefused, msg=args):
                decurix_tools.gui(*args)
        self.assertEqual(self.calls(), [])

    def test_reading_verbs_run_the_command_as_written(self):
        os.environ["STUB_OUT"] = OBSERVE
        seen = decurix_tools.observe(window="Ledger")
        self.assertEqual(seen["text"], OBSERVE)
        self.assertEqual(seen["elements"], [
            {"ref": "e3", "role": "push button", "name": "Save", "value": None, "states": ["focused"]},
            {"ref": "e4", "role": "text", "name": "Amount", "value": "1,200.00", "states": ["editable"]}])
        decurix_tools.read("e4")
        decurix_tools.recall("supplier list")
        decurix_tools.recall_runs("supplier list", since="7d")
        decurix_tools.recall_runs(task="t-12", around=3)
        self.assertEqual(self.calls(), [["gui", "observe", "--window", "Ledger"], ["gui", "read", "e4"],
                                        ["gui", "recall", "supplier", "list"],
                                        ["gui", "recall", "--runs", "--since", "7d", "supplier", "list"],
                                        ["gui", "recall", "--runs", "--task", "t-12", "--around", "3"]])

    def test_parsed_listings(self):
        os.environ["STUB_OUT"] = ('r-20261101 "Monthly upload" - task, 14 actions\n'
                                  "`gui recording ID` shows one's steps; `gui replay ID` runs it again with no model.\n")
        self.assertEqual(decurix_tools.recordings(),
                         [{"id": "r-20261101", "title": "Monthly upload", "kind": "task", "actions": 14}])
        os.environ["STUB_OUT"] = "Report one\n[page image: /tmp/p1.png]\nline two\n"
        self.assertEqual(decurix_tools.office_read("~/work/a.pdf", pages="1-2"),
                         {"text": "Report one\nline two", "images": ["/tmp/p1.png"]})
        self.assertEqual(self.calls()[-1], ["office", "read", "~/work/a.pdf", "1-2"])

    def test_a_failed_command_raises_with_what_it_said(self):
        os.environ.update(STUB_OUT="gui: no element e9", STUB_CODE="1")
        with self.assertRaises(decurix_tools.ToolError) as caught:
            decurix_tools.read("e9")
        self.assertEqual(caught.exception.code, 1)
        self.assertIn("no element e9", str(caught.exception))


class TheBudget(_Stubbed):
    def test_past_the_budget_every_call_is_refused(self):
        with mock.patch.object(decurix_tools, "BUDGET", 3):
            for _ in range(3):
                decurix_tools.records()
            with self.assertRaises(decurix_tools.BudgetSpent):
                decurix_tools.records()
        self.assertEqual(len(self.calls()), 3)

    def test_the_environment_may_lower_it_never_raise_it(self):
        with mock.patch.dict(os.environ, {"DECURIX_TOOLS_BUDGET": "2"}):
            self.assertEqual(decurix_tools.calls_left(), 2)
        with mock.patch.dict(os.environ, {"DECURIX_TOOLS_BUDGET": "5000"}):
            self.assertEqual(decurix_tools.calls_left(), decurix_tools.BUDGET)
        self.assertEqual(decurix_tools.BUDGET, 50)


class ApprovalEndsTheScript(_Stubbed):
    def test_exit_3_prints_the_marker_unchanged_and_ends_with_3_whatever_the_script_catches(self):
        marker = "NEEDS_APPROVAL: press Pay on the payment page\nOnce it is approved, continue with: gui click e7\n"
        script = Path(self.tmp.name) / "worker_script.py"
        script.write_text(textwrap.dedent(f'''
            import sys
            sys.path.insert(0, {str(Path(decurix_tools.__file__).parent)!r})
            import decurix_tools
            decurix_tools.COMMANDS = {{"gui": [sys.executable, {str(self.stub)!r}, "gui"]}}
            try:
                decurix_tools.observe()
            except BaseException:
                print("CAUGHT")
            finally:
                print("FINALLY")
            print("AFTER")
        '''), encoding="utf-8")
        env = dict(os.environ, STUB_OUT=marker, STUB_CODE="3")
        done = subprocess.run([sys.executable, str(script)], capture_output=True, text=True, env=env, timeout=60)
        self.assertEqual(done.returncode, 3, done.stderr)
        self.assertEqual(done.stdout.replace("\r\n", "\n"), marker)


class ItIsShipped(unittest.TestCase):
    def test_it_imports_nothing_of_the_agent_and_says_its_results_are_claims(self):
        source = Path(decurix_tools.__file__).read_text(encoding="utf-8")
        self.assertNotIn("import decurix_agent", source)
        self.assertNotIn("from decurix_agent", source)
        self.assertIn("CLAIMS, not evidence", decurix_tools.__doc__)

    def test_the_desktop_image_installs_it(self):
        dockerfile = (Path(decurix_tools.__file__).resolve().parents[1] / "docker" / "desktop.basic.Dockerfile")
        text = dockerfile.read_text(encoding="utf-8")
        self.assertIn("COPY agent/decurix_tools.py /opt/decurix_tools.py", text)
        self.assertIn('python3 -c "import decurix_tools"', text)


if __name__ == "__main__":
    unittest.main()
