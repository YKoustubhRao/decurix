"""
Every job sent to soul carries the person's replies on its task so far, each with the question it answered
(earlierSaid, from person_record.answers: what the control plane confirms). Found live (2026-11-08): a bill the final
check sent back started a new goal in soul, and its review, shown none of the person's replies, judged the cost centre
they gave as backed by nothing. Data beside the prompt: the prompt's bytes stay as they are.
"""

import unittest
from unittest.mock import patch

from decurix_agent import person_record, thebot_harness, workflow_task


class TheEarlierSaidField(unittest.TestCase):
    def setUp(self):
        self.bodies = []
        thebot_harness._plan.update(task=None, asked="", said="", pending=False)
        patches = [
            patch.object(thebot_harness, "_post_json", side_effect=lambda url, body: self.bodies.append(body) or {}),
            patch.object(thebot_harness, "_build_context", return_value=""),
            patch.object(workflow_task, "_active", None),
            patch.object(thebot_harness, "_request_id", return_value="t1"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def sent(self, request="enter the bill"):
        thebot_harness._run_thebot(request, reply=False)
        return self.bodies[-1]

    def test_the_confirmed_replies_go_with_their_questions(self):
        rows = [{"id": "j_1", "asked": "Which cost centre should I select?", "text": "CC-27, the supplier's state."}]
        with patch.object(person_record, "answers", return_value=rows):
            body = self.sent()
        self.assertEqual(body["earlierSaid"], [{"asked": "Which cost centre should I select?",
                                                "said": "CC-27, the supplier's state."}])
        self.assertEqual(body["prompt"], "enter the bill")

    def test_none_or_an_unreadable_record_sends_none(self):
        with patch.object(person_record, "answers", return_value=[]):
            self.assertEqual(self.sent()["earlierSaid"], [])
        with patch.object(person_record, "answers", side_effect=OSError("down")):
            self.assertEqual(self.sent()["earlierSaid"], [])

    def test_at_most_the_newest_twenty(self):
        rows = [{"id": f"j_{i}", "asked": "", "text": f"reply {i}"} for i in range(30)]
        with patch.object(person_record, "answers", return_value=rows):
            said = self.sent()["earlierSaid"]
        self.assertEqual(len(said), thebot_harness.MAX_EARLIER_SAID)
        self.assertEqual(said[-1]["said"], "reply 29")


if __name__ == "__main__":
    unittest.main()
