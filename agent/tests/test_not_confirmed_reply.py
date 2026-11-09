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


class TheExpertsNotConfirmed(unittest.TestCase):

    def test_is_read_as_leave_never_done_or_retry(self):
        for model in (False, True):
            with mock.patch.object(llm_client, "available", return_value=model):
                self.assertEqual(final_gate._done_reading(final_gate.NOT_CONFIRMED_REPLY)["choice"], "leave")
        self.assertFalse(final_gate._confirms(final_gate.NOT_CONFIRMED_REPLY))

    def test_the_run_ends_unchecked_with_no_verdict(self):
        finals, verdicts = [], []
        paused = {"mode": "confirm", "claim": "Entered 4 vouchers.", "verdict": {"verdict": "unsure", "why": "x"},
                  "engine": "skill", "run": {"skill": {"name": "s"}}, "result": None}
        with mock.patch.object(final_gate, "write_final_reply", lambda text, **kw: finals.append((text, kw))), \
                mock.patch.object(final_gate, "_record_verdict", lambda *a, **kw: verdicts.append(a)), \
                mock.patch.object(llm_client, "available", return_value=False):
            final_gate._confirm_reply(paused, final_gate.NOT_CONFIRMED_REPLY)
        self.assertEqual(verdicts, [])
        self.assertIs(finals[0][1]["checked"], False)
        self.assertEqual(finals[0][1]["kind"], "final")
        self.assertIn("Not confirmed", finals[0][0])


if __name__ == "__main__":
    unittest.main()
