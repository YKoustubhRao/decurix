"""
The review pass after a task checked done (review_pass.py; docs/learning-loop.md "The review pass"): when it runs, that
it never delays or fails the report, the code checks every proposal passes before it is staged, and the call soul is
asked to make.
"""

import os
import threading
import time
import unittest
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import (config, kept_facts, messaging, person_record, playbooks, review_pass, soul_call, state,
                           task_context, verdict as verdicts, workflow_task)

TASK = "t1111111111111111"
DONE = verdicts.make(verdicts.DONE, "verify", checks=[verdicts.check("looked", verdicts.PASS, "screen")])


def proposal(text, quotes, in_run=None, kind="lesson", why="said so", replaces=""):
    return {"kind": kind, "text": text, "why": why, "evidence": list(quotes),
            "in_run": list(in_run if in_run is not None else [True] * len(quotes)), "replaces": replaces}


class WhenItRuns(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.object(config, "HOSTED_BRAIN", True)
        patcher.start()
        self.addCleanup(patcher.stop)
        env = mock.patch.dict(os.environ, {}, clear=False)
        env.start()
        self.addCleanup(env.stop)
        os.environ.pop(review_pass.SWITCH, None)
        os.environ.pop(review_pass.MIN_COMMANDS_SETTING, None)

    def test_only_a_checked_done_of_the_models_own_run_on_a_hosted_brain(self):
        self.assertEqual(review_pass.wanted("final", True, DONE, None), (True, ""))
        self.assertTrue(review_pass.wanted("final", True, DONE, "")[0])
        unchecked = verdicts.make(verdicts.UNCHECKED, "none")
        for args, kwargs in [
            (("error", True, DONE, None), {}),
            (("final", False, DONE, None), {}),
            (("final", True, unchecked, None), {}),
            (("final", True, None, None), {}),
            (("final", True, DONE, "replay"), {}),
            (("final", True, DONE, "tool"), {}),
            (("final", True, DONE, "skill"), {}),
            (("final", True, DONE, "human"), {}),
            (("final", True, DONE, None), {"quiet": True}),
            (("final", True, DONE, None), {"interrupted": True}),
            (("final", True, DONE, None), {"stopped": True}),
        ]:
            self.assertFalse(review_pass.wanted(*args, **kwargs)[0], (args, kwargs))
        with mock.patch.object(config, "HOSTED_BRAIN", False):
            self.assertFalse(review_pass.wanted("final", True, DONE, None)[0], "a local brain")
        os.environ[review_pass.SWITCH] = "0"
        self.assertEqual(review_pass.wanted("final", True, DONE, None), (False, "the review pass is off"))

    def test_something_to_learn_is_an_answer_or_enough_commands(self):
        self.assertTrue(review_pass.something_to_learn([{"text": "yes"}], 0))
        self.assertFalse(review_pass.something_to_learn([], review_pass.DEFAULT_MIN_COMMANDS - 1))
        self.assertTrue(review_pass.something_to_learn([], review_pass.DEFAULT_MIN_COMMANDS))
        os.environ[review_pass.MIN_COMMANDS_SETTING] = "3"
        self.assertTrue(review_pass.something_to_learn([], 3))
        self.assertFalse(review_pass.something_to_learn([], 2))
        os.environ[review_pass.MIN_COMMANDS_SETTING] = "lots"
        self.assertEqual(review_pass.min_commands(), review_pass.DEFAULT_MIN_COMMANDS)


class NeverCostsTheReport(unittest.TestCase):
    """A review that raises, or takes long, neither fails nor delays the report."""

    def _task(self, held=()):
        for target, name, value in [
            (config, "HOSTED_BRAIN", True),
            (task_context, "current", mock.Mock(return_value=TASK)),
            (task_context, "held_values", mock.Mock(return_value=list(held))),
            (task_context, "secret_values", mock.Mock(return_value=[])),
            (task_context, "current_request", mock.Mock(return_value="send the weekly summary")),
            (playbooks, "ran_commands", mock.Mock(return_value=["a"] * 9)),
        ]:
            patcher = mock.patch.object(target, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_a_slow_or_raising_review_returns_at_once(self):
        self._task()
        release = threading.Event()
        with mock.patch.object(review_pass, "review", side_effect=lambda *a: release.wait(5)):
            began = time.monotonic()
            thread = review_pass.after_report(TASK, "final", True, DONE, None)
            self.assertLess(time.monotonic() - began, 0.5)
            self.assertIsNotNone(thread)
            release.set()
            thread.join(5)
        with mock.patch.object(review_pass, "review", side_effect=RuntimeError("boom")), \
                mock.patch.object(review_pass, "log") as logged:
            thread = review_pass.after_report(TASK, "final", True, DONE, None)
            thread.join(5)
        self.assertTrue(any("boom" in str(c) for c in logged.call_args_list))

    def test_a_task_that_held_a_secret_is_not_reviewed(self):
        self._task(held=["s3cret-value"])
        with mock.patch.object(review_pass, "review") as review:
            self.assertIsNone(review_pass.after_report(TASK, "final", True, DONE, None))
        review.assert_not_called()

    def test_the_report_goes_out_whatever_the_review_does(self):
        self._task()
        saved = (state.current_task, workflow_task._active)
        self.addCleanup(lambda: (setattr(state, "current_task", saved[0]), setattr(workflow_task, "_active", saved[1])))
        state.current_task, workflow_task._active = "send the weekly summary", None
        release = threading.Event()
        self.addCleanup(release.set)
        for effect in (RuntimeError("boom"), lambda *a: release.wait(5)):
            sent = []
            with mock.patch.object(messaging, "send_query", side_effect=lambda *a, **k: sent.append(1) or True), \
                    mock.patch.object(messaging, "progress"), mock.patch.object(messaging, "_final_gate", None), \
                    mock.patch.object(messaging.config, "QUERYTEXT_DIR", mock.MagicMock()), \
                    mock.patch.object(kept_facts, "after_task", return_value=None), \
                    mock.patch.object(review_pass, "review", side_effect=effect) as review:
                began = time.monotonic()
                messaging.write_final_reply("Sent.", kind="final", checked=True, verdict=DONE, task_id=TASK)
                self.assertLess(time.monotonic() - began, 2.0)
            self.assertEqual(sent, [1])
            for _ in range(50):
                if review.called:
                    break
                time.sleep(0.02)
            self.assertTrue(review.called, "the review was not started for a checked done")
            state.current_task = "send the weekly summary"


class TheCodeChecks(unittest.TestCase):
    def setUp(self):
        self.answers = [{"id": "j_abc123", "asked": "Which column first?", "text": "Always put the region column first."}]

    def check(self, raw, **kwargs):
        args = {"request": "send the weekly summary", "answers": self.answers, "secrets": (), "knowledge": [],
                "memory": []}
        args.update(kwargs)
        return review_pass.check(raw, **args)

    def test_every_quote_must_stand_in_the_run_or_the_persons_words(self):
        got = self.check([
            proposal("The region column comes first.", ["put the region column first"], in_run=[False]),
            proposal("The summary sheet has one tab.", ["1 sheet(s): Summary"], in_run=[True]),
            proposal("Made up.", ["nothing anyone said"], in_run=[False]),
            proposal("Half made up.", ["put the region column first", "nothing anyone said"], in_run=[False, False]),
            proposal("Spacing aside.", ["put  the region\ncolumn first"], in_run=[False]),
            proposal("Case kept.", ["PUT THE REGION COLUMN FIRST"], in_run=[False]),
        ])
        self.assertEqual([p["text"] for p in got],
                         ["The region column comes first.", "The summary sheet has one tab.", "Spacing aside."])
        self.assertEqual(got[0]["evidence"], [{"quote": "put the region column first", "where": "person"}])
        self.assertEqual(got[1]["evidence"][0]["where"], "run")

    def test_nothing_holding_a_secret_is_proposed(self):
        got = self.check([
            proposal("The upload key is Zq81-secret-value.", ["put the region column first"]),
            proposal("Sign in with password: hunter2-Xy9.", ["put the region column first"]),
            proposal("The region column comes first.", ["put the region column first"]),
        ], secrets=("Zq81-secret-value",))
        self.assertEqual([p["text"] for p in got], ["The region column comes first."])

    def test_duplicates_of_what_is_known_or_proposed_are_dropped(self):
        got = self.check([
            proposal("Totals are shown with two decimals.", ["put the region column first"]),
            proposal("The client prefers email.", ["put the region column first"]),
            proposal("The region column comes first.", ["put the region column first"]),
            proposal("the region column  comes first", ["put the region column first"]),
        ], knowledge=["Totals are shown with two decimals."],
            memory=["Title: About a client\nCategory: lesson\n\nThe client prefers email. Said in March."])
        self.assertEqual([p["text"] for p in got], ["The region column comes first."])

    def test_a_replaces_must_quote_a_given_line_exactly(self):
        known = ["Totals are shown with two decimals."]
        right = proposal("Totals are shown as whole units.", ["put the region column first"],
                         replaces="Totals are shown with two decimals.")
        wrong = proposal("Totals are rounded.", ["put the region column first"], replaces="Totals use two decimals.")
        got = self.check([right, wrong], knowledge=known)
        self.assertEqual([p["text"] for p in got], ["Totals are shown as whole units."])
        self.assertEqual(got[0]["replaces"], "Totals are shown with two decimals.")

    def test_shape_kind_and_cap(self):
        raw = [proposal(f"Rule number {w}.", ["put the region column first"]) for w in ("one", "two", "three", "four")]
        raw.insert(0, proposal("A rule.", ["put the region column first"], kind="rule"))
        raw.insert(0, "not a proposal")
        raw.insert(0, proposal("Too many quotes.", ["put the region column first"] * 4))
        got = self.check(raw)
        self.assertEqual([p["text"] for p in got], ["Rule number one.", "Rule number two.", "Rule number three."])
        self.assertEqual(self.check(None), [])


class TheReview(unittest.TestCase):
    def test_soul_is_asked_and_only_what_passes_is_staged(self):
        answer = {"text": "{}", "model": "m", "decision": {"source": "transcript", "proposals": [
            proposal("The region column comes first.", ["put the region column first"], in_run=[True]),
            proposal("Made up.", ["no one said this"], in_run=[False]),
        ]}}
        answers = [{"id": "j_abc123", "asked": "Which first?", "text": "Always put the region column first."}]
        with mock.patch.object(person_record, "answers", return_value=answers), \
                mock.patch.object(soul_call, "review", return_value=answer) as asked, \
                mock.patch.object(review_pass, "_known", return_value=([], [])), \
                mock.patch.object(review_pass, "post") as post:
            got = review_pass.review(TASK, "send the weekly summary", 2, ("v1",))
        self.assertEqual([p["text"] for p in got], ["The region column comes first."])
        args, kwargs = asked.call_args
        self.assertEqual(args, (TASK, ("v1",)))
        self.assertIn("Said: Always put the region column first.", kwargs["notes"])
        post.assert_called_once()
        self.assertEqual(post.call_args.args[0], TASK)

    def test_an_answer_cut_off_at_the_limit_is_asked_again_with_room(self):
        # Found live (2026-11-15): a reasoning brain spent the 1200 tokens thinking; the review read no JSON.
        cut = {"text": "", "truncated": True, "model": "m", "decision": {"error": "the answer holds no JSON object"}}
        whole = {"text": "{}", "model": "m", "decision": {"source": "transcript", "proposals": [
            proposal("The region column comes first.", ["put the region column first"], in_run=[True])]}}
        answers = [{"id": "j_abc123", "asked": "Which first?", "text": "Always put the region column first."}]
        with mock.patch.object(person_record, "answers", return_value=answers), \
                mock.patch.object(soul_call, "review", side_effect=[cut, whole]) as asked, \
                mock.patch.object(review_pass, "_known", return_value=([], [])), \
                mock.patch.object(review_pass, "post") as post:
            got = review_pass.review(TASK, "send the weekly summary", 2, ("v1",))
        self.assertEqual([p["text"] for p in got], ["The region column comes first."])
        self.assertEqual(asked.call_count, 2)
        self.assertGreater(asked.call_args_list[1].kwargs["max_tokens"], review_pass.MAX_TOKENS)
        post.assert_called_once()

    def test_nothing_to_learn_asks_nothing_and_an_unusable_soul_stages_nothing(self):
        with mock.patch.object(person_record, "answers", return_value=[]), \
                mock.patch.object(soul_call, "review") as asked:
            self.assertEqual(review_pass.review(TASK, "r", 1, ()), [])
        asked.assert_not_called()
        with mock.patch.object(person_record, "answers", return_value=[]), \
                mock.patch.object(soul_call, "review", side_effect=soul_call.Unusable("no route")), \
                mock.patch.object(review_pass, "post") as post:
            self.assertEqual(review_pass.review(TASK, "r", 50, ()), [])
        post.assert_not_called()


class TheCallToSoul(unittest.TestCase):
    def test_the_review_names_its_purpose_task_and_secrets_and_sends_no_messages(self):
        sent = {}

        def fake_post(path, payload, timeout, absent):
            sent.update(path=path, payload=payload, absent=absent)
            return {"text": "{}", "decision": {"proposals": []}}

        saved = dict(soul_call._state)
        self.addCleanup(soul_call._state.update, saved)
        soul_call._state["review_absent"] = False
        with mock.patch.object(soul_call, "credential", return_value="token"), \
                mock.patch.object(soul_call, "_post", side_effect=fake_post):
            soul_call.review(TASK, ("v1",), notes="Said: yes", max_tokens=900, timeout=30)
        self.assertEqual(sent["path"], soul_call.REVIEW_PATH)
        p = sent["payload"]
        self.assertEqual((p["purpose"], p["task"], p["secrets"], p["notes"], p["brain"]),
                         ("py.learn.review", TASK, ["v1"], "Said: yes", "hosted"))
        self.assertNotIn("messages", p)
        soul_call._state["review_absent"] = True
        with self.assertRaises(soul_call.Unusable):
            soul_call.review(TASK, (), max_tokens=900, timeout=30)


if __name__ == "__main__":
    unittest.main()
