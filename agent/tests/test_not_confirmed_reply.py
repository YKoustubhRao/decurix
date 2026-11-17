"""
A person who cannot check a result now - the scripted expert's fixed reply among them - leaves it as reported, not
confirmed: never read as done, never as a correction that sends the work round again (owner's decision 2026-11-08: a
correct result was retried for 50 minutes on "I don't know").

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests -p test_not_confirmed_reply.py
"""

import unittest
from unittest import mock

import test_recordings  # noqa: F401 - stubs decurix_agent.config first

from decurix_agent import final_gate, llm_client
from decurix_agent import skill_task  # noqa: F401 - registers the gate's run hooks (final_gate._RUN)

# A reply of a person who cannot check (an evaluation's scripted expert sends this one). The harness holds no copy of
# it (review of freeze 2, B): it is read like any reply.
CANNOT_CHECK = "I can't check that from here - report it as you see it, not confirmed."


def model_says(choice):
    return mock.patch.object(llm_client, "chat",
                             return_value=({"content": '{"choice": "%s", "instruction": ""}' % choice}, None))


class TheExpertsNotConfirmed(unittest.TestCase):

    def test_is_read_like_any_reply_never_done_or_retry(self):
        self.assertFalse(hasattr(final_gate, "NOT_CONFIRMED_REPLY"))
        with mock.patch.object(llm_client, "available", return_value=True), model_says("leave") as chat:
            self.assertEqual(final_gate._done_reading(CANNOT_CHECK)["choice"], "leave")
        chat.assert_called_once()  # read by the model, not matched in code
        final_gate.approval._CHOSEN.clear()
        with mock.patch.object(llm_client, "available", return_value=False):
            self.assertEqual(final_gate._done_reading(CANNOT_CHECK)["choice"], "unclear")  # asked again, never guessed
            for plain in ("I can't check", "not confirmed", "leave it as it is"):
                self.assertEqual(final_gate._done_reading(plain)["choice"], "leave", plain)
            self.assertFalse(final_gate._confirms(CANNOT_CHECK))

    def test_the_run_ends_unchecked_with_no_verdict(self):
        finals, verdicts = [], []
        paused = {"mode": "confirm", "claim": "Entered 4 vouchers.", "verdict": {"verdict": "unsure", "why": "x"},
                  "engine": "skill", "run": {"skill": {"name": "s"}}, "result": None}
        with mock.patch.object(final_gate, "write_final_reply", lambda text, **kw: finals.append((text, kw))), \
                mock.patch.object(final_gate, "_record_verdict", lambda *a, **kw: verdicts.append(a)), \
                mock.patch.object(llm_client, "available", return_value=False):
            final_gate._confirm_reply(paused, "I can't check")
        self.assertEqual(verdicts, [])
        self.assertIs(finals[0][1]["checked"], False)
        self.assertEqual(finals[0][1]["kind"], "final")
        self.assertIn("Not confirmed", finals[0][0])


class ABillGivenForAHoleInTheSeries(unittest.TestCase):
    """Found in the learning curves (2026-11-16): asked about a bill missing from the mail, the person gave it in words;
    the whole task went back to the model and the bill was never entered."""

    def test_is_entered_by_hand_in_the_rest_phase_never_the_whole_task_again(self):
        given = []
        run = {"skill": {"name": "s"}, "gathered": "done", "documents": ["/d/1058.docx", "/d/1059.docx"]}
        paused = {"mode": "confirm", "series": {"helped": False}, "run": run, "claim": "Entered 1058 and 1059.",
                  "verdict": {"verdict": "unsure", "seen": "1060 missing"}, "engine": "skill",
                  "result": {"text": "Replayed", "outcome": "completed_unverified"}}
        answer = "Bill 1060 went to Bhuvana Hospital Trust on 20-01-2027, 10,000.00 plus 18%; enter it with the others."
        with mock.patch.object(final_gate, "_by_model", lambda r, **kw: given.append(kw)), \
                mock.patch.object(final_gate, "_engine_can_drive", return_value=True), \
                mock.patch.object(final_gate, "_done_reading", return_value={"choice": "correction", "instruction": answer}):
            final_gate._confirm_reply(paused, answer)
        self.assertEqual(given, [{"phase": "rest"}])
        self.assertTrue(run["by_hand"][0]["said"])
        self.assertTrue(run["asked_person"])
        from decurix_agent import skill_task
        told = "\n".join(skill_task._rest_how(run))
        self.assertIn("Bhuvana Hospital Trust", told)
        self.assertIn("1058.docx", told)  # (entered by the taught steps: not again)


class AStopToARunThatCouldNotGoOn(unittest.TestCase):
    """Found in the learning curves (2026-11-16): the worker rightly filed nothing - a receiver's GSTIN was cancelled -
    and said which invoice; the person said stop; the run ended "Not done - marked as a failure" without that report."""

    def test_ends_with_the_workers_own_report_unchecked(self):
        finals, verdicts = [], []
        paused = {"mode": "confirm", "blocked": True, "claim": "Nothing filed: invoice KCI-071 is to a cancelled GSTIN.",
                  "verdict": {"verdict": "cannot", "seen": "nothing filed"}, "engine": "skill", "run": {"skill": {"name": "s"}}}
        with mock.patch.object(final_gate, "write_final_reply", lambda text, **kw: finals.append((text, kw))), \
                mock.patch.object(final_gate, "_record_verdict", lambda *a, **kw: verdicts.append((a, kw))), \
                mock.patch.object(final_gate, "_done_reading", return_value={"choice": "stop", "instruction": ""}):
            final_gate._confirm_reply(paused, "Do not file it. Stop, and tell me which invoice.")
        self.assertIn("KCI-071", finals[0][0])
        self.assertEqual((finals[0][1]["kind"], finals[0][1]["checked"]), ("final", False))
        self.assertIs(verdicts[0][0][1], False)  # never a success for the skill

    def test_a_stop_to_a_result_that_claimed_done_is_still_a_failure(self):
        finals = []
        paused = {"mode": "confirm", "claim": "Entered 4 vouchers.", "verdict": {"verdict": "unsure", "seen": "x"},
                  "engine": "skill", "run": None}
        with mock.patch.object(final_gate, "write_final_reply", lambda text, **kw: finals.append((text, kw))), \
                mock.patch.object(final_gate, "_done_reading", return_value={"choice": "stop", "instruction": ""}):
            final_gate._confirm_reply(paused, "stop")
        self.assertEqual(finals[0][1]["kind"], "error")


if __name__ == "__main__":
    unittest.main()
