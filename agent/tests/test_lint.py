"""
Lint: what a compiled program says about how far it can be trusted, before
anything is run.

    PYTHONPATH=agent python3 -m unittest discover -s agent/tests
"""

import sys
import types
import unittest

try:
    from decurix_agent import config  # noqa: F401
except OSError:
    import decurix_agent

    stub = types.ModuleType("decurix_agent.config")
    stub.DESKTOP_ID = "test-box"
    stub.DESKTOP_NAME = "test-box"
    stub.CONTROL_PLANE_URL = "http://127.0.0.1:9"
    stub.AGENT_HARNESS = "ollama"
    sys.modules["decurix_agent.config"] = stub
    decurix_agent.config = stub

from decurix_agent.computer import lint


def step(n, kind="click", **fields):
    base = {"n": n, "kind": kind, "summary": f"step {n}", "consequential": False,
            "expect": [{"type": "url", "value": "http://x.test/*"}]}
    base.update(fields)
    return base


def program(*steps, params=None):
    return {"session": "demo-x", "params": params or [], "steps": list(steps)}


def codes(result):
    return [f["code"] for f in result["findings"]]


class Grades(unittest.TestCase):

    def test_a_program_whose_every_step_is_found_and_checked_is_ready(self):
        result = lint.lint(program(step(1, target={"role": "button", "name": "Save", "path": "w > Save"}),
                                   step(2, "type", target={"role": "textbox", "name": "Name", "path": "w > Name"},
                                        param="name")))

        self.assertEqual(result["grade"], lint.READY)
        self.assertEqual(result["findings"], [])
        self.assertIn("Every step can be found again", result["summary"])

    def test_a_click_on_a_bare_position_is_risky(self):
        result = lint.lint(program(step(1, point=[10, 20])))

        self.assertEqual(result["grade"], lint.RISKY)
        self.assertIn("blind_click", codes(result))
        self.assertEqual(result["coverage"]["blind"], 1)

    def test_the_same_click_with_a_picture_of_its_surroundings_is_only_information(self):
        result = lint.lint(program(step(1, point=[10, 20], look={"png": "x"})))

        self.assertEqual(result["grade"], lint.READY)
        self.assertEqual(codes(result), ["anchored_clicks"])
        self.assertEqual(result["coverage"]["anchored"], 1)

    def test_an_anchor_cut_off_by_its_window_says_what_it_cannot_tell_apart(self):
        result = lint.lint(program(step(1, point=[10, 20], look={"png": "x", "partial": True}),
                                   step(2, point=[30, 40], look={"png": "x", "partial": False})))

        partial = next(f for f in result["findings"] if f["code"] == "partial_anchor")
        self.assertEqual(partial["steps"], [1])
        self.assertEqual(result["grade"], lint.READY)

    def test_a_click_with_nothing_to_find_it_by_is_risky(self):
        result = lint.lint(program(step(1)))
        self.assertEqual((result["grade"], codes(result)), (lint.RISKY, ["no_target"]))

    def test_an_action_a_replay_cannot_do_is_risky(self):
        result = lint.lint(program(step(1, "middle_click", point=[1, 1])))
        self.assertEqual((result["grade"], codes(result)), (lint.RISKY, ["unreplayable"]))

    def test_a_step_that_needs_approval_and_leaves_nothing_to_check_is_risky(self):
        result = lint.lint(program(step(1, target={"role": "button", "name": "Pay now", "path": "w > Pay"},
                                        consequential=True, expect=[])))
        self.assertIn("unchecked_consequential", codes(result))
        self.assertEqual(result["grade"], lint.RISKY)

    def test_a_target_named_for_a_record_is_a_warning_to_rehearse(self):
        result = lint.lint(program(step(1, target={"role": "link", "name": "Invoice INV-1043", "path": "w > Invoice"})))
        self.assertEqual((result["grade"], codes(result)), (lint.REHEARSE, ["volatile_target"]))

    def test_a_target_a_repair_was_learned_for_is_not_called_volatile(self):
        result = lint.lint(program(step(1, target={"role": "link", "name": "Invoice INV-1043", "path": "w > I"},
                                        alternates=[{"role": "link", "name": "Invoice"}])))
        self.assertNotIn("volatile_target", codes(result))

    def test_a_program_that_mostly_checks_nothing_is_a_warning_to_rehearse(self):
        target = {"role": "button", "name": "Next", "path": "w > Next"}
        result = lint.lint(program(step(1, target=target, expect=[]), step(2, target=target, expect=[]),
                                   step(3, target=target)))

        self.assertEqual((result["grade"], result["coverage"]["checked"]), (lint.REHEARSE, 1))
        self.assertIn("mostly_unchecked", codes(result))

    def test_secrets_and_manual_steps_are_information_not_a_grade(self):
        result = lint.lint(program(
            step(1, "type", target={"role": "textbox", "name": "Password", "path": "w > P"}, param="password"),
            step(2, "manual", reason="sign in"),
            params=[{"name": "password", "label": "Password", "secret": True, "step": 1}]))

        self.assertEqual(result["grade"], lint.READY)
        self.assertEqual(set(codes(result)), {"secret_params", "manual_steps"})

    def test_an_empty_program_is_risky(self):
        result = lint.lint(program())
        self.assertEqual((result["grade"], codes(result)), (lint.RISKY, ["empty"]))

    def test_the_worst_finding_sets_the_grade_and_leads_the_summary(self):
        result = lint.lint(program(step(1, point=[1, 2]),
                                   step(2, target={"role": "link", "name": "Order 55", "path": "w > O"})))

        self.assertEqual(result["grade"], lint.RISKY)
        self.assertIn("Step 1", result["summary"])


class Wording(unittest.TestCase):

    def test_steps_are_listed_and_long_lists_are_cut(self):
        self.assertEqual(lint._steps_text([3]), "Step 3")
        self.assertEqual(lint._steps_text([3, 4]), "Steps 3, 4")
        self.assertEqual(lint._steps_text(list(range(1, 10))), "Steps 1, 2, 3, 4, 5, 6 and 3 more")

    def test_findings_name_their_steps_and_a_fix(self):
        finding = lint.lint(program(step(1, point=[1, 2])))["findings"][0]
        self.assertEqual(finding["steps"], [1])
        self.assertTrue(finding["fix"])


if __name__ == "__main__":
    unittest.main()
