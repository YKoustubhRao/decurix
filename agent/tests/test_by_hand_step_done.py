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

    tearDown, run_with = base.Replay.tearDown, base.Replay.run_with

    def setUp(self):
        base.Replay.setUp(self)
        replay._by_hand_asked.clear()
        self.addCleanup(replay._by_hand_asked.clear)

    def screen(self, password, *extra):
        return base.FakeScreen([base.field("textbox", "Username", value="meera"),
                                base.field("textbox", "Password", secret="password field", value=password),
                                base.field("button", "LOGIN"), *extra])

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

    # Independent review (2026-10-22): any empty secret field anywhere stopped the run, every resume, for ever.

    def goes_on(self, fake, program=None):
        result = self.run_with(fake, program or replay.compile_program(META, EVENTS), start=3)
        self.assertNotEqual(result["outcome"], replay.NEEDS_HUMAN, result.get("reason"))
        self.assertEqual([k for k, _ in fake.acted], ["click"])

    def test_a_field_whose_value_the_screen_cannot_read_is_not_empty(self):
        self.goes_on(self.screen(None))   # an app read by vision: None is "not known", never "empty"

    def test_hidden_fields_and_other_windows_do_not_stop_it(self):
        self.goes_on(self.screen("••••",
                                 base.field("textbox", "PIN", secret="password field", states=["offscreen"]),
                                 base.field("textbox", "Card code", secret="password field", box=[0, 0, 0, 0]),
                                 base.field("textbox", "Vault password", secret="password field",
                                            window_title="Password manager")))

    def test_a_field_a_later_step_fills_is_not_the_by_hand_steps(self):
        events = EVENTS[:2] + [base.event(2, "type", base.field("textbox", "OTP", secret="password field"),
                                          text=None, withheld={"text": "password field"}),
                               base.event(3, "click", base.field("button", "LOGIN"))]
        program = replay.compile_program(META, events)
        fake = self.screen("••••", base.field("textbox", "OTP", secret="password field"))
        result = self.run_with(fake, program, start=3)
        self.assertNotIn("has not been done yet", result.get("reason") or "")

    def test_it_stops_once_then_the_next_resume_goes_on(self):
        program = replay.compile_program(META, EVENTS)
        self.assertEqual(self.run_with(self.screen(""), program, start=3)["outcome"], replay.NEEDS_HUMAN)
        self.goes_on(self.screen(""), program)   # told again it is done: the steps after it show whether it was

    def test_a_screen_that_cannot_be_read_decides_nothing(self):
        fake = self.screen("")

        def broken(remember=True):
            raise RuntimeError("no display")
        fake.observe = broken
        replay.core = fake   # (put back by tearDown)
        self.assertEqual(replay._empty_secret_fields(), [])


if __name__ == "__main__":
    unittest.main()
