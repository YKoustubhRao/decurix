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


if __name__ == "__main__":
    unittest.main()
