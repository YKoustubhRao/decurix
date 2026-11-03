"""
A question raised for a secret is marked so (review 2026-11-03, X-6): the person's reply may be the secret itself.

The mark is a structural fact of where the question came from (a step the recorder kept private, a withheld parameter,
a secret field), never a reading of the question's words. The control plane keeps none of such a reply (W1); here the
agent remembers it as a secret and never keeps it as a lesson. Also: the agent's own log banners withhold secrets.
"""

import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import azure_harness, messaging, replay_task, state, task_runner, thebot_harness, watchers


class TheQueryCarriesTheMark(unittest.TestCase):
    def sent(self, **kwargs):
        posted = []
        with mock.patch.object(messaging, "_post_querytext", side_effect=lambda p: posted.append(p) or ("sent", "")), \
                mock.patch.object(messaging, "progress"), mock.patch.object(messaging.usage, "snapshot", return_value={}):
            messaging.send_query("q.txt", "a question", kind="query", reply="a question", **kwargs)
        return posted[0]

    def test_a_secret_question_says_so(self):
        self.assertIs(self.sent(secret=True).get("secret"), True)

    def test_an_ordinary_question_carries_no_mark(self):
        self.assertNotIn("secret", self.sent())


class AReplayStoppedOnAPrivateStep(unittest.TestCase):
    PROGRAM = {"session": "s1", "steps": [
        {"n": 1, "kind": "click", "target": {"role": "button", "name": "Sign in"}},
        {"n": 2, "kind": "manual", "reason": "a private step"},
        {"n": 3, "kind": "type", "target": {"role": "textbox", "name": "Code"}, "param": "code"},
        {"n": 4, "kind": "type", "target": {"role": "textbox", "name": "Customer"}, "param": "customer"},
    ], "params": [{"name": "code", "step": 3, "secret": True}, {"name": "customer", "step": 4, "secret": False}]}

    def asked_secret(self, step):
        asked = []
        result = {"outcome": replay_task.replay.NEEDS_HUMAN, "step": step, "resume_from": step + 1, "text": "Do it."}
        self.addCleanup(setattr, state, "paused_replay", None)
        with mock.patch.object(replay_task.replay, "run", return_value=result), \
                mock.patch.object(replay_task, "ask_for_query", side_effect=lambda t, **k: asked.append(k)):
            replay_task._run(self.PROGRAM, {}, 1)
        return asked[0].get("secret")

    def test_a_private_step_or_a_withheld_value_is_a_secret_question(self):
        self.assertTrue(self.asked_secret(2))
        self.assertTrue(self.asked_secret(3))

    def test_an_ordinary_step_is_not(self):
        self.assertFalse(self.asked_secret(1))
        self.assertFalse(self.asked_secret(4))


class TheReplyToASecretQuestion(unittest.TestCase):
    def test_it_is_remembered_as_a_secret_and_never_kept_as_a_lesson(self):
        path = mock.Mock()
        path.name = "20261103_101500_ab12cd34.reply.t0123456789abcdef.txt"
        saved = (state.waiting_for_query, state.last_query_secret, state.last_query_text, state.current_task_id)
        self.addCleanup(lambda: (setattr(state, "waiting_for_query", saved[0]),
                                 setattr(state, "last_query_secret", saved[1]),
                                 setattr(state, "last_query_text", saved[2]),
                                 setattr(state, "current_task_id", saved[3])))
        state.waiting_for_query, state.last_query_secret = True, True
        state.last_query_text, state.current_task_id = "Type the portal's sign-in code.", "t0123456789abcdef"
        from decurix_agent.computer import privacy
        order = []
        with mock.patch.object(watchers, "is_reply_file", return_value=True), \
                mock.patch.object(watchers, "_mark_handled"), \
                mock.patch.object(watchers.approval, "note_reply", side_effect=lambda *a, **k: order.append("note_reply")), \
                mock.patch.object(watchers.approval, "resolve_reply", return_value=None), \
                mock.patch.object(watchers.lessons, "note") as note, \
                mock.patch.object(watchers.skill_task, "learn_answer"), \
                mock.patch.object(watchers.threading, "Thread"), \
                mock.patch.object(privacy, "remember_value", side_effect=lambda v: order.append("remembered")) as remember:
            watchers.answer_waiting(path, "Zq7-41xk")
        note.assert_not_called()
        remember.assert_called_once_with("Zq7-41xk")
        self.assertEqual(order[:2], ["remembered", "note_reply"], "remembered before anything else sees the reply")


class BannersAreWithheld(unittest.TestCase):
    def test_every_harness_banner_goes_through_withheld(self):
        for module in (thebot_harness, task_runner, azure_harness):
            with self.subTest(module=module.__name__):
                self.assertIs(module.withheld, messaging.withheld)
        import inspect
        for module, names in ((thebot_harness, ("perform_task", "continue_after_query")),
                              (task_runner, ("perform_task", "continue_after_query")),
                              (azure_harness, ("perform_task", "continue_after_query"))):
            for name in names:
                source = inspect.getsource(getattr(module, name))
                for line in [l for l in source.splitlines() if "log_banner(" in l]:
                    self.assertIn("withheld(", line, f"{module.__name__}.{name}: {line.strip()}")

    def test_a_received_request_is_logged_withheld(self):
        logged = []
        with mock.patch.object(messaging, "log", side_effect=logged.append), \
                mock.patch.object(messaging, "_withheld", side_effect=lambda t: "[W]"), \
                mock.patch("decurix_agent.computer.privacy.remember_secrets"), \
                mock.patch.object(messaging.config, "COMMAND_DIR", __import__("pathlib").Path(config_stub._OWN)):
            messaging.save_request("log in with the code Zq7-41xk", "")
        self.assertTrue(logged and all("Zq7-41xk" not in line for line in logged), logged)

    def test_withheld_hides_a_remembered_secret(self):
        from decurix_agent.computer import privacy
        with mock.patch.object(privacy, "withhold_known", side_effect=lambda t: t.replace("Zq7-41xk", "[withheld]")):
            self.assertEqual(messaging.withheld("log in with Zq7-41xk"), "log in with [withheld]")


if __name__ == "__main__":
    unittest.main()
