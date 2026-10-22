import unittest

import test_recordings as base  # stubs config first; its fake screen and helpers

from decurix_agent.computer import replay


# ============================================================
# GST suite T9 (2026-10-22): told "use your judgement" at "type the client's
# portal password" (a step the recording kept private), the engine carried on
# from the next step with the password field empty; the login failed.
# ============================================================

META = {"id": "demo-20261022T000000-abc123", "title": "Sign in", "kind": "demonstration",
        "started_at": 1789380000, "actors": ["human"], "screen": [1280, 720]}
EVENTS = [
    base.event(0, "type", base.field("textbox", "Username"), text="meera"),
    base.event(1, "manual", None, reason="typing the client's portal password"),
    base.event(2, "click", base.field("button", "LOGIN")),
]


class TestAStepDoneByHandIsDoneBeforeTheReplayGoesOn(unittest.TestCase):

    setUp, tearDown, run_with = base.Replay.setUp, base.Replay.tearDown, base.Replay.run_with

    def screen(self, password):
        return base.FakeScreen([base.field("textbox", "Username", value="meera"),
                                base.field("textbox", "Password", secret="password field", value=password),
                                base.field("button", "LOGIN")])

    def test_an_empty_password_field_means_it_was_not_done(self):
        program = replay.compile_program(META, EVENTS)
        fake = self.screen("")
        result = self.run_with(fake, program, start=3)
        self.assertEqual(result["outcome"], replay.NEEDS_HUMAN)
        self.assertIn("has not been done yet", result["reason"])
        self.assertIn("Password", result["reason"])
        self.assertEqual(fake.acted, [])

    def test_a_filled_one_lets_it_go_on(self):
        program = replay.compile_program(META, EVENTS)
        fake = self.screen("••••")
        result = self.run_with(fake, program, start=3)
        self.assertNotEqual(result["outcome"], replay.NEEDS_HUMAN)
        self.assertEqual([k for k, _ in fake.acted], ["click"])


if __name__ == "__main__":
    unittest.main()
