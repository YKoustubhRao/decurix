"""
Review round 11's small items (REVIEW-PY-R11, 2026-11-05), each failing on the code before:

1. The agent starts isolated (`python3 -I`), as the gate does: no PYTHON* variable, no user site-packages, no .pth or
   usercustomize.py from the worker's home, and its own folder put on sys.path by itself.
2. A file's label backs a pick only with the value cell right after it, up to the next sentence or column break.
3. A kept field answer counts only for the field the person's question (the control plane's) named.
4. A taught skill's run held back by a ledger finding says so - not "the files it wrote do not hold up".
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

import test_skills as ts  # noqa: E402
from test_round11_persons_word import NEW, OFFER, SAVE, ThePersonsWord, pick  # noqa: E402

from decurix_agent import ledger_checks, person_record, skill_task  # noqa: E402
from decurix_agent.computer import demonstration, records  # noqa: E402

AGENT = Path(__file__).resolve().parents[1]


class TheAgentStartsIsolated(unittest.TestCase):
    def test_the_daemon_is_started_with_minus_i(self):
        text = (AGENT / "start_agent_terminal.sh").read_bytes().decode("utf-8")
        self.assertIn("python3 -I /opt/vm_server.py", text)
        self.assertNotIn("python3 /opt/vm_server.py", text)
        self.assertNotIn("\r\n", text)

    def test_its_imports_resolve_under_minus_i_and_a_planted_path_is_not_read(self):
        with tempfile.TemporaryDirectory() as tmp:
            planted = Path(tmp) / "planted" / "decurix_agent"
            planted.mkdir(parents=True)
            (planted / "__init__.py").write_text("raise SystemExit('the planted package was imported')\n", encoding="utf-8")
            (Path(tmp) / "planted" / "usercustomize.py").write_text("raise SystemExit('usercustomize ran')\n",
                                                                     encoding="utf-8")
            probe = ("import runpy, sys; runpy.run_path(sys.argv[1], run_name='probe'); import decurix_agent; "
                     "print(decurix_agent.__file__)")
            env = dict(os.environ, PYTHONPATH=str(Path(tmp) / "planted"), PYTHONUSERBASE=str(Path(tmp) / "planted"))
            r = subprocess.run([sys.executable, "-I", "-c", probe, str(AGENT / "vm_server.py")], cwd=tmp, env=env,
                               capture_output=True, text=True, timeout=120)
        self.assertEqual(r.returncode, 0, r.stderr[-2000:])
        self.assertEqual(Path(r.stdout.strip().splitlines()[-1]).resolve(),
                         (AGENT / "decurix_agent" / "__init__.py").resolve())

    def test_the_recorder_it_starts_is_isolated_too(self):
        started = []

        class Popen:
            pid = 1

            def __init__(self, argv, **kwargs):
                started.append(argv)
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(demonstration.subprocess, "Popen", Popen), \
                mock.patch.object(demonstration, "LOG_FILE", Path(tmp) / "log"), \
                mock.patch.object(demonstration, "_write_active"), mock.patch.object(demonstration, "_clear_active"), \
                mock.patch.object(demonstration.session, "app_env", lambda: {}), \
                mock.patch.object(demonstration, "status", lambda: None), \
                mock.patch.object(demonstration.recording, "new_demonstration_id",
                                  lambda: "demo-20261105T101500-aaaaaa"), \
                mock.patch.object(demonstration.recording, "ensure"):
            try:
                demonstration.start(title="a test")
            except Exception:  # noqa: BLE001 - only the command it starts matters here
                pass
        self.assertTrue(started, "the recorder was started")
        self.assertEqual(started[0][1:3], ["-I", "-c"])
        # The command it runs finds the package with nothing else on sys.path.
        r = subprocess.run([sys.executable, "-I", "-c", demonstration.ISOLATED_RUN, str(AGENT), "decurix_agent.values"],
                           cwd=tempfile.gettempdir(), capture_output=True, text=True, timeout=120)
        self.assertEqual(r.returncode, 0, r.stderr[-2000:])


class TheValueCellAfterTheLabel(unittest.TestCase):
    def test_a_merged_line_is_not_one_value(self):
        beside = records.option_beside_label
        self.assertFalse(beside("General", "Cost centre", "Cost centre: to be decided. General terms apply.", OFFER))
        self.assertFalse(beside("General", "Cost centre", "Cost centre: to be decided  General terms apply", OFFER))
        self.assertFalse(beside("General", "Cost centre", "Cost centre: tbd; General terms apply", OFFER))
        self.assertFalse(beside("General", "Cost centre", "General terms apply. Cost centre: Project Maple", OFFER))
        self.assertTrue(beside("General", "Cost centre", "Cost centre: General. Payment in 30 days.", OFFER))
        self.assertTrue(beside("General", "Cost centre", "Cost centre - General  Due: 30 days", OFFER))
        self.assertTrue(beside("General", "Cost centre", "Cost centre:\nGeneral", OFFER))
        self.assertFalse(beside("General", "Cost centre", "Cost centre: to be decided\nGeneral terms apply.", OFFER))


class AKeptAnswerIsForTheFieldItsQuestionNamed(ThePersonsWord):
    def learned(self, asked):
        source = {"actor": {"kind": "person"}, "text": "General, please."}
        if asked is not None:
            source["asked"] = asked
        self.control_plane(person_record.LEARNED_PATH, {"kind": person_record.FIELD_ANSWER}, {"learned": [{
            "kind": person_record.FIELD_ANSWER, "derived_from": "j_111111111111",
            "text": json.dumps({"label": "Cost centre", "host": "app", "option": "General"}), "source": source}]})

    def test_moved_onto_another_field_or_with_no_question_it_backs_nothing(self):
        self.events = [NEW, pick("General"), SAVE]
        self.learned("Which department does the bill belong to?")
        self.assertEqual(records.unbacked_picks(), ["General"])
        self.learned(None)
        self.assertEqual(records.unbacked_picks(), ["General"])
        self.learned("Which cost centre should the bill go to?")
        self.assertEqual(records.unbacked_picks(), [])


class ATaughtRunsLedgerFindingIsSaidAsOne(ts.TeachingBase):
    def test_not_the_files(self):
        sent = []
        run = {"skill": {"name": "enter-bill", "steps": []}, "values": {}}
        fault = records.finding(ledger_checks.UNBACKED_PICK, "Cost centre: 'General' was chosen from the list")
        with mock.patch.object(skill_task, "_read_back", lambda run, text: ([], [])), \
                mock.patch.object(skill_task, "_ledger_faults", lambda reply="", run=None: [fault]), \
                mock.patch.object(skill_task, "_own_run"), mock.patch.object(skill_task, "_record_verdict"), \
                mock.patch.object(skill_task, "write_final_reply",
                                  lambda text, kind="final", engine=None, **k: sent.append(text)):
            skill_task._succeed(run, {"text": "Saved INV-0001."}, "model")
        self.assertTrue(sent[0].startswith("Not done - a record it saved in the application does not hold up"), sent[0])
        self.assertNotIn("files it wrote", sent[0])
        self.assertIn("Ask the person (HELP)", sent[0])


if __name__ == "__main__":
    unittest.main()
