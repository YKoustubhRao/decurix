"""
The secrets and gate review's round 4 (REVIEW-PY-A-3, 2026-11-04). Values are made up, of the input classes named.

- R3-B1 (the live F-S3 again): a long marked message whose secret is an unlabelled word. The job to soul named only
  the whole message, and the step line of the command that typed the word quoted it. Now each word of a marked
  message with a value's shape is a value of its task, and a step line never quotes what `gui type` types.
- Nits: a withheld mark is never typed; a planned run's request (and a workflow's inputs) come back as they were from
  the task's resume copy; the stores that had shapes off have them on, with the compared fields exempt by name; the
  local approval store goes through write_scrubbed; a refused check hands the engine the gate's own reason; the
  values of a request that named no task are let go at a final report.
"""

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from test_secret_first import SecretFirst, TASK, VALUE  # noqa: E402

from decurix_agent import config, gate_server as gate, messaging, progress, task_context, thebot_harness  # noqa: E402
from decurix_agent.computer import approval, privacy  # noqa: E402
from decurix_agent.computer.errors import ComputerError  # noqa: E402

LONG = f"Sign in to the staff portal as mira.k with {VALUE} and download the roster"


def posted_line(text):
    posted = []
    with mock.patch.object(progress.urllib.request, "urlopen",
                           side_effect=lambda r, timeout=None: posted.append(json.loads(r.data)) or mock.MagicMock()):
        progress._post(text)
    return posted[0]["text"]


class AnUnlabelledWordOfAMarkedMessage(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)

    def test_the_step_line_the_final_reply_and_the_job(self):
        self.send(LONG)
        line = thebot_harness.readable_step(f'skill:operator -> $ gui type e3 "{VALUE}"')
        self.assertEqual(line, "Running: gui type e3 [text]")
        self.assertNotIn(VALUE, posted_line(f"Typing {VALUE} into the field"))
        self.assertIn(VALUE, thebot_harness._job_secrets())  # soul is told the word (the job's "secrets")
        posted = []
        (self.root / "querytext").mkdir()
        with mock.patch.object(messaging, "_post_querytext", lambda p: posted.append(p) or ("sent", "")), \
                mock.patch.object(config, "QUERYTEXT_DIR", self.root / "querytext"), \
                mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(messaging.progress, "flush", lambda: None), \
                mock.patch.object(messaging, "_keep_facts", lambda *a, **k: None):
            messaging.write_final_reply(f"Signed in with {VALUE}.", task_id=TASK)
        written = "".join(p.read_text(encoding="utf-8") for p in (self.root / "querytext").glob("*.txt"))
        self.assertIn("Signed in with", written)
        self.assertTrue(posted)
        self.assertNotIn(VALUE, written + json.dumps(posted) + "\n".join(self.logged))

    def test_only_words_shaped_like_values(self):
        # (Round 5, R4-B1: a whole-token value; "mira.k" is a host name's shape, data by structure.)
        self.assertEqual(privacy.marked_values(LONG), [LONG, privacy.WHOLE_TOKEN + VALUE])
        self.assertEqual(privacy.marked_values("please download the roster, today"),
                         ["please download the roster, today"])
        self.send(LONG)
        self.assertEqual(privacy.withhold_known("downloading the staff roster"), "downloading the staff roster")


class AStepLineNeverQuotesTypedText(unittest.TestCase):
    def test_each_form_of_the_command(self):
        for command, shown in (('gui type e3 "Zebra-42-quiet"', "gui type e3 [text]"),
                               ("gui type e3 plain words here && gui click e4", "gui type e3 [text]"),
                               ('gui type e3 "ab;cd9xyz"', "gui type e3 [text]"),  # (round 5: the whole rest)
                               ("GUI TYPE e3 Zebra-42-quiet", "GUI TYPE e3 [text]"),
                               ('gui type --enter e7 "x"', "gui type --enter e7 [text]"),
                               ("gui click e3", "gui click e3")):
            self.assertEqual(privacy.step_command(command), shown)


class AWithheldMarkIsNeverTyped(unittest.TestCase):
    def test_act_refuses_it(self):
        from decurix_agent.computer import core
        with mock.patch.object(core, "observe", side_effect=AssertionError("looked")):
            for text in ("[withheld]", "user [withheld] pass", "[held back: this text could not be checked]"):
                with self.assertRaises(ComputerError) as caught:
                    core.act("type", ref="e3", text=text)
                self.assertIn("Ask the person", str(caught.exception))


class TheResumeCopy(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)
        task_context.note_secret_values(TASK, privacy.marked_values(LONG))

    def test_a_planned_runs_request_comes_back_as_it_was(self):
        from decurix_agent import long_task, workflow_runner
        runner = workflow_runner.WorkflowRunner(self.root / "runs-wf")
        with mock.patch.object(long_task, "_runner", lambda: runner):
            privacy.keep_resume_copy("r1", TASK, LONG)
            long_task._save_ledger("r1", {"request": LONG, "task_id": TASK})
            on_disk = (self.root / "runs-wf" / "r1" / "ledger.json").read_text(encoding="utf-8")
            self.assertNotIn(VALUE, on_disk)
            self.assertEqual(long_task.ledger("r1")["request"], LONG)  # an agent restart resumes with the value
            privacy.clear_literals(TASK)  # the task's final report
            self.assertNotIn(VALUE, long_task.ledger("r1")["request"])

    def test_a_workflows_inputs_too(self):
        from decurix_agent import workflow_runner
        runner = workflow_runner.WorkflowRunner(self.root / "runs-wf")
        spec = {"version": 1, "steps": [{"id": "s1", "action": "noop"}]}
        with mock.patch.object(workflow_runner, "validate", lambda spec: None), \
                mock.patch.object(workflow_runner, "missing_inputs", lambda spec, inputs: []):
            try:
                runner.create(spec, {"request": LONG}, run_id="w1")
            except Exception:  # noqa: BLE001 - only the inputs matter here
                pass
            kept = privacy.resume_copy("workflow:w1")
        self.assertEqual(json.loads(kept), {"request": LONG})


class ShapesOnWithTheComparedFieldsExempt(unittest.TestCase):
    def test_exempt_by_name(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "s.json"
            privacy.write_scrubbed(path, {"name": "the code is password: Zebra-42-quiet",
                                          "value": "4111 1111 1111 1111", "rows": [{"value": "4111111111111111"}]},
                                   exempt=("value",))
            data = json.loads(path.read_text(encoding="utf-8"))
        self.assertNotIn("Zebra-42-quiet", data["name"])
        self.assertEqual(data["value"], "4111 1111 1111 1111")
        self.assertEqual(data["rows"][0]["value"], "4111111111111111")

    def test_the_observation_state(self):
        from decurix_agent.computer import core
        frame = mock.Mock()
        frame.sample.return_value = b"\x00\x01"
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(core, "STATE_DIR", Path(tmp)), \
                mock.patch.object(core, "STATE_FILE", Path(tmp) / "observation.json"):
            core._save_state({"frame": frame, "elements": [{"ref": "e1", "name": "token: ghp_Ab12Cd34Ef56Gh78Ij90",
                                                            "value": "4111111111111111"}]}, 5)
            data = json.loads((Path(tmp) / "observation.json").read_text(encoding="utf-8"))
        self.assertNotIn("ghp_Ab12Cd34Ef56Gh78Ij90", json.dumps(data))
        self.assertEqual(data["elements"][0]["value"], "4111111111111111")
        self.assertEqual(data["next_ref"], 5)


class ARefusedCheckSaysTheGatesReason(unittest.TestCase):
    def test_the_engine_is_told_nobody_was_asked(self):
        refusal = {"allowed": False, "approved": False, "request": None, "by_reply": "", "refused": True,
                   "reason": gate.REFUSED_REASON}
        with mock.patch.object(approval, "remote", lambda: True), \
                mock.patch.object(approval, "gate_call", lambda op, **a: refusal):
            decision = approval._decide("f", "type x", "make sure it is the right site", None, "",
                                        material={"kind": "type"})
        self.assertFalse(decision.allowed)
        self.assertIsNone(decision.request)
        self.assertEqual(decision.reason, gate.REFUSED_REASON)
        self.assertIn("do not try it again", decision.reason)


class AnUntaskedRequestsValues(SecretFirst):
    def test_are_let_go_at_a_final_report(self):
        privacy.remember_literals(["Velvet88harbor"], "")
        privacy._literal_cache.clear()
        self.assertIn("Velvet88harbor", privacy.literals(""))
        privacy.clear_literals(TASK)
        privacy._literal_cache.clear()
        self.assertEqual(privacy.literals(""), [])


class TheLocalApprovalStore(unittest.TestCase):
    def test_a_desktop_with_no_gate_withholds_it(self):
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(approval, "STATE_FILE", Path(tmp) / "approval.json"), \
                mock.patch.object(approval, "IN_GATE", False), \
                mock.patch.object(privacy, "_read_literals", lambda: {TASK: ["Copper71meadow"]}):
            with approval._locked() as state:
                state["pending"] = {"id": "abc123def456", "fingerprint": "f" * 24,
                                    "action": "type Copper71meadow into Notes"}
            written = (Path(tmp) / "approval.json").read_text(encoding="utf-8")
        self.assertNotIn("Copper71meadow", written)
        self.assertIn("f" * 24, written)


if __name__ == "__main__":
    unittest.main()
