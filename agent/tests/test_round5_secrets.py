"""
The secrets review's round 5 (REVIEW-PY-A-4) and the values review's last nits (REVIEW-PY-B-4), 2026-11-04. Values are
made up, of the input classes named.

- R4-B1: every value-shaped word of a marked message was a secret of the task, withheld as a substring - its own
  dates, amounts and codes too - so the record ledger kept "[withheld]" for them and the task's saves could never
  pass. Now a word that is data by structure is no secret, and the rest are withheld as whole tokens only.
- R4-B2: a taught skill's (or a recording's) secret parameter given with the run was logged and kept raw in the guided
  run: nothing noted it before the request was saved.
- Nits: a planned run keeps its resume copy; bytes that read as text are checked; a transient mkstemp error is tried
  once more; the code basis needs every per-source figure backed, compared exactly.
"""

import errno
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from test_secret_first import SecretFirst, TASK, VALUE  # noqa: E402

from decurix_agent import attachments, config, guidance, http_api, state, task_context, thebot_harness  # noqa: E402
from decurix_agent.computer import privacy, records, recording  # noqa: E402

MESSAGE = (f"File the return for mira.k: total 1,250.00, due 05/11/2024 at 10:30, and don't wait. Sign in with "
           f"{VALUE}. PIN 4821, password: x9!k")


class AMarkedMessagesDataIsNoSecret(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)

    def test_the_secret_is_withheld_and_the_data_is_not(self):
        self.send(MESSAGE)
        line = privacy.withhold_known(f"Saved the return of mira.k: total 1,250.00, due 05/11/2024 at 10:30, don't "
                                      f"wait - signed in with {VALUE}; INV-2024-0042; PIN 4821; x9!k")
        for data in ("mira.k", "1,250.00", "05/11/2024", "10:30", "don't", "INV-2024-0042"):
            self.assertIn(data, line, data)
        for secret in (VALUE, "4821", "x9!k"):  # the unlabelled word, and the labelled ones by structure
            self.assertNotIn(secret, line, secret)
        self.assertEqual(thebot_harness.readable_step(f'skill:operator -> $ gui type e3 "{VALUE}"'),
                         "Running: gui type e3 [text]")
        self.assertIn(VALUE, thebot_harness._job_secrets())

    def test_the_request_copy_its_checks_read_keeps_its_data(self):
        # probe_round4's input class: a long marked request carrying data is not withheld whole - its checks read it.
        self.send(MESSAGE)
        task_context.start(TASK, request=MESSAGE)
        copy = task_context.current_request()
        self.assertIn("1,250.00", copy)
        self.assertIn("05/11/2024", copy)
        self.assertNotIn(VALUE, copy)
        self.assertNotIn(MESSAGE, privacy.marked_values(MESSAGE))

    def test_a_whole_token_only(self):
        self.send(MESSAGE)
        self.assertEqual(privacy.withhold_known(f"x{VALUE}y and {VALUE}."), f"x{VALUE}y and [withheld].")

    def test_the_record_ledger_and_its_save(self):
        self.send(MESSAGE)
        bill = self.root / "bill.txt"
        bill.write_text("Bill B-11\nTotal: 1,250.00\nDue: 05/11/2024\n", encoding="utf-8")
        with mock.patch.object(records, "LEDGER_DIR", self.root / "ledger"), \
                mock.patch("decurix_agent.computer.evidence.read", lambda task_id=None: ""), \
                mock.patch("decurix_agent.computer.evidence.looks", lambda task_id=None: []), \
                mock.patch("decurix_agent.task_context.source_date_order", lambda task_id=None: "day"), \
                mock.patch("decurix_agent.task_context.app_date_order", lambda task_id=None: "day"):
            rec, _ = records.declare("B-11", str(bill), {"Total": "1,250.00", "Due": "05/11/2024"})
            kept = json.loads(next((self.root / "ledger").glob("*.json")).read_text(encoding="utf-8"))
            self.assertEqual([r["fields"]["Total"]["intent"] for r in kept], ["1,250.00"])
            self.assertEqual([r["fields"]["Due"]["intent"] for r in kept], ["05/11/2024"])
            blocking, _, _ = records.presave(records.open_record(records.load()),
                                             [("Total", "1250.00"), ("Due", "05-11-2024")])
        self.assertEqual(blocking, [])

    def test_a_word_the_input_files_hold_is_their_data(self):
        source = self.root / "ledger-codes.txt"
        source.write_text("Codes in use: Ref-77A1 and Ref-88B2\n", encoding="utf-8")
        values = privacy.marked_values(f"Post Ref-77A1 with Zebra-42-quiet{attachments.block([str(source)])}")
        self.assertNotIn(privacy.WHOLE_TOKEN + "Ref-77A1", values)
        self.assertIn(privacy.WHOLE_TOKEN + "Zebra-42-quiet", values)


class ASkillsSecretParameter(SecretFirst):
    """R4-B2 (probe_round4.ASkillsSecretParameter): `/skill portal portal_key=...` was logged with the value."""

    KEY = "Copper71meadowKey"

    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)
        handler = mock.Mock()
        handler._json = lambda status, body: body
        self.handler = handler

    def test_run_skill(self):
        skill = {"status": http_api.skills.READY, "params": [{"name": "portal_key", "secret": True},
                                                             {"name": "user", "secret": False}]}
        with mock.patch.object(http_api.skills, "load", lambda name: skill), \
                mock.patch.object(http_api, "save_request", http_api.save_request):
            http_api.Handler._run_skill(self.handler, "portal", {"task_id": TASK, "params": {
                "portal_key": self.KEY, "user": "asha"}})
        self.assertNotIn(self.KEY, "\n".join(self.logged))
        self.assertIn("Received request: [withheld: marked secret]", self.logged)
        run_file = self.root / "guided-run.json"
        with mock.patch.object(guidance, "RUN_FILE", run_file), mock.patch.object(state, "current_task_id", TASK):
            guidance.begin("portal", "task", "text", values={"portal_key": self.KEY, "user": "asha"})
        kept = run_file.read_text(encoding="utf-8")
        self.assertNotIn(self.KEY, kept)
        self.assertIn("asha", kept)

    def test_replay(self):
        program = {"params": [{"name": "pin", "secret": True, "step": 2}]}
        with mock.patch.object(http_api.recording, "load", lambda sid: ({}, [])), \
                mock.patch.object(http_api.replay, "compile_program", lambda meta, events: program), \
                mock.patch.object(http_api.replay_task, "hold_params", lambda *a: None):
            http_api.Handler._replay(self.handler, "s1", {"task_id": TASK, "params": {"pin": "Velvet88harbor"}})
        self.assertIn("Velvet88harbor", " ".join(privacy.plain_values(task_context.secret_values(TASK))))
        self.assertNotIn("Velvet88harbor", privacy.withhold_known("typed Velvet88harbor"))


class APlannedRunKeepsItsResumeCopy(unittest.TestCase):
    """The mutation removing keep_resume_copy from long_task.perform survived round 4."""

    def test_perform_keeps_it(self):
        import test_long_requests as base
        from decurix_agent import long_task
        kept = []
        case = base.LongRun("test_stages_run_one_by_one_and_the_report_is_plain")
        case.setUp()
        self.addCleanup(case.doCleanups)
        with mock.patch.object(privacy, "keep_resume_copy", lambda key, task, text: kept.append((key, text))):
            long_task.perform(base.LONG, None)
        self.assertTrue(kept)
        self.assertEqual(kept[0][1], base.LONG)


class BytesThatReadAsText(unittest.TestCase):
    def test_are_checked(self):
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(privacy, "_read_literals", lambda: {TASK: ["Copper71meadow"]}):
            path = Path(tmp) / "meta.json"
            recording.write_whole(path, '{"title": "typed Copper71meadow"}'.encode("utf-8"))
            self.assertNotIn("Copper71meadow", path.read_text(encoding="utf-8"))
            frame = Path(tmp) / "f.png"
            recording.write_whole(frame, b"\x89PNG\r\n\x1a\n\x00\xff")
            self.assertEqual(frame.read_bytes(), b"\x89PNG\r\n\x1a\n\x00\xff")


class ATransientTempFileError(unittest.TestCase):
    def test_is_tried_once_more_then_raised(self):
        import tempfile as real
        calls = []
        original = real.mkstemp

        def flaky(*a, **k):
            calls.append(1)
            if len(calls) == 1:
                raise OSError(errno.EINVAL, "Invalid argument")
            return original(*a, **k)
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(real, "mkstemp", flaky):
            privacy.write_scrubbed(Path(tmp) / "c.json", {"a": 1}, atomic=True)
            self.assertEqual(json.loads((Path(tmp) / "c.json").read_text(encoding="utf-8")), {"a": 1})
        self.assertEqual(len(calls), 2)
        with tempfile.TemporaryDirectory() as tmp, \
                mock.patch.object(real, "mkstemp", mock.Mock(side_effect=OSError(errno.EINVAL, "Invalid argument"))):
            with self.assertRaises(OSError):
                privacy.write_scrubbed(Path(tmp) / "c.json", {"a": 1}, atomic=True)


if __name__ == "__main__":
    unittest.main()
