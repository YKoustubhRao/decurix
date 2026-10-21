"""Findings an earlier stage made before a change the person made since are not "use it; do not work it out again"."""
import unittest

from decurix_agent import long_task


def _state():
    steps = [{"id": "s1", "title": "Fee demand workbook"}, {"id": "s2", "title": "Fee letters"}]
    return {"spec": {"steps": steps},
            "steps": {"s1": {"status": "completed", "evidence": []}, "s2": {"status": "running", "evidence": []}}}


class StaleFindings(unittest.TestCase):
    def test_a_finding_from_before_a_later_change_is_marked_for_recheck(self):
        # Live (series 15 H3): "leave Ishaan out" came after the fee demand stage recorded the Rao family total.
        data = {"facts": [{"stage": "s1", "kind": "fact", "text": "Rao family: Ishaan and Tara, total Rs 89,300",
                           "seen": 0}],
                "decisions": [], "amendments": [{"n": 1, "text": "Leave Ishaan Rao out of everything."}]}
        text = long_task._ledger_text(_state(), data, {"id": "s2"})
        self.assertIn("BEFORE a change the person made since", text)
        self.assertIn("[found before change 1]", text)
        self.assertNotIn("do not work it out again", text)
        self.assertIn("Leave Ishaan Rao out", text)

    def test_a_finding_made_after_every_change_is_used_as_is(self):
        data = {"facts": [{"stage": "s1", "kind": "fact", "text": "Rao family: Tara, total Rs 41,200", "seen": 1}],
                "decisions": [], "amendments": [{"n": 1, "text": "Leave Ishaan Rao out of everything."}]}
        text = long_task._ledger_text(_state(), data, {"id": "s2"})
        self.assertIn("do not work it out again", text)
        self.assertNotIn("BEFORE a change", text)

    def test_findings_kept_before_this_field_existed_are_not_marked(self):
        data = {"facts": [{"stage": "s1", "kind": "fact", "text": "12 families billed"}],
                "decisions": [], "amendments": [{"n": 1, "text": "Leave Ishaan Rao out of everything."}]}
        text = long_task._ledger_text(_state(), data, {"id": "s2"})
        self.assertIn("do not work it out again", text)


if __name__ == "__main__":
    unittest.main()
