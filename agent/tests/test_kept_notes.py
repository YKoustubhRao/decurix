"""
Every note a worker sends the control plane carries an id of its own (event_id), the same on every try, and a question
or an approval the control plane could not be told is kept and sent again as a task's outcome is - a desktop was left
waiting on a question nobody saw when the control plane was restarting for longer than the 180 s retry window. The
control plane applies each (desktop, event_id) once, and answers a repeat 200 {"status": "received", "duplicate": true}.

Also: notes are delivered in the order they happened (a new one never passes a kept one), and a task's outcome makes its
kept questions moot (never shown after it).

    PYTHONPATH="agent;agent/tests" python -m unittest test_kept_notes
"""

import json
import re
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401  (stubs decurix_agent.config on a machine with no desktop)

from decurix_agent import config, messaging, state, task_context

T1 = "t0123456789abcdef"
T2 = "tfedcba9876543210"


class KeptNotes(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.in_hand = T1
        for patch in (mock.patch.object(config, "DECURIX_DIR", Path(self.tmp.name)),
                      mock.patch.object(messaging, "SEND_RETRY_SECONDS", 0),
                      mock.patch.object(messaging.progress, "flush", lambda *a, **k: None),
                      mock.patch.object(messaging.usage, "snapshot", lambda: {}),
                      mock.patch.object(messaging.task_context, "skill", lambda *a, **k: None),
                      mock.patch.object(messaging.task_context, "current", lambda: self.in_hand)):
            patch.start()
            self.addCleanup(patch.stop)
        self.posted = []

    def kept(self):
        return [json.loads(p.read_text(encoding="utf-8"))
                for p in sorted((Path(self.tmp.name) / "unsent").glob("*.json"))]

    def post(self, *answers):
        answers = list(answers)

        def fake(payload):
            self.posted.append(json.loads(json.dumps(payload)))
            return answers.pop(0) if answers else ("sent", "")
        return mock.patch.object(messaging, "_post_querytext", fake)

    def ask(self, text, kind="query", task_id=T1, **kw):
        return messaging.send_query("q.txt", text, kind=kind, reply=text, task_id=task_id, **kw)

    def final(self, text, task_id=T1, kind="final", **kw):
        return messaging.send_query("f.txt", text, kind=kind, reply=text, task_id=task_id, **kw)

    # --- the id ---------------------------------------------------------------------------------------------------

    def test_every_note_has_an_id_of_its_own_the_same_on_every_try(self):
        with mock.patch.object(messaging, "SEND_RETRY_SECONDS", 60), mock.patch.object(messaging.time, "sleep"), \
                self.post(("unreachable", "x"), ("unreachable", "x"), ("sent", "")):
            self.assertTrue(self.ask("Which one?"))
        ids = [p["event_id"] for p in self.posted]
        self.assertEqual(len(ids), 3)
        self.assertEqual(len(set(ids)), 1)
        self.assertRegex(ids[0], r"^[0-9a-f]{32}$")
        with self.post():
            self.ask("And this one?")
        self.assertNotEqual(self.posted[-1]["event_id"], ids[0])

    def test_the_kept_copy_carries_the_same_id_and_every_resend_does_too(self):
        with self.post(("unreachable", "down")):
            self.assertFalse(self.final("Done."))
        first = self.posted[0]["event_id"]
        self.assertEqual([k["event_id"] for k in self.kept()], [first])
        with self.post(("unreachable", "still down")):
            self.assertEqual(messaging.resend_unsent(), 0)
        with self.post(("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual([p["event_id"] for p in self.posted], [first] * 3)

    def test_a_body_sent_in_place_of_a_refused_one_has_another_id(self):
        # The refused body was not applied: its short or least form is another note (nothing is sent twice either way,
        # and a control plane that noted the refused id would not swallow the replacement as a duplicate).
        with self.post(("refused", "HTTP 400"), ("refused", "HTTP 400"), ("sent", "")):
            self.assertTrue(self.final("Done. " * 200, fallback="Done."))
        ids = [p["event_id"] for p in self.posted]
        self.assertEqual(len(set(ids)), 3, ids)
        self.assertTrue(all(re.fullmatch(r"[0-9a-f]{32}", i) for i in ids))

    def test_a_note_kept_before_notes_had_ids_is_given_one_and_keeps_it(self):
        folder = Path(self.tmp.name) / "unsent"
        folder.mkdir()
        (folder / "00000000000000000001_abcdef12.json").write_text(
            json.dumps({"kind": "final", "task_id": T1, "content": "old", "reply": "old"}), encoding="utf-8")
        with self.post(("unreachable", "x")):
            messaging.resend_unsent()
        given = self.posted[0]["event_id"]
        self.assertRegex(given, r"^[0-9a-f]{32}$")
        self.assertEqual(self.kept()[0]["event_id"], given)
        with self.post(("sent", "")):
            messaging.resend_unsent()
        self.assertEqual(self.posted[-1]["event_id"], given)

    def test_a_duplicate_answer_is_delivered(self):
        response = mock.MagicMock(status=200)
        response.read.return_value = b'{"status":"received","duplicate":true}'
        cm = mock.MagicMock(__enter__=lambda s: response, __exit__=lambda *a: False)
        with mock.patch.object(messaging.urllib.request, "urlopen", return_value=cm), \
                mock.patch.object(messaging, "log") as logged:
            self.assertEqual(messaging._post_querytext({"kind": "query", "event_id": "a" * 32})[0], "sent")
        self.assertIn("already had", " ".join(str(c) for c in logged.call_args_list))

    def test_the_kept_copy_keeps_the_gates_signed_question_as_it_is(self):
        # A signature, or words, the secret check took a shape out of would make the person's reply grant nothing.
        gate = {"id": "r-1", "question": "Pay 1,200.00 to ACME Ltd, account 4111 1111 1111 1111, token sk-abc123def456?",
                "sig": "b60090e3052297aeb5a080889ce2fc4bca957e756faeb4df7d31800ca1e771ec"}
        with mock.patch("decurix_agent.computer.approval.signed_question", return_value=gate), \
                self.post(("unreachable", "down")):
            self.ask("May I pay?", kind="approval")
        self.assertEqual(self.posted[0]["gate"], gate)
        self.assertEqual(self.kept()[0]["gate"], gate)
        with self.post(("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual(self.posted[-1]["gate"], gate)
        self.assertEqual(self.posted[-1]["event_id"], self.posted[0]["event_id"])

    # --- questions are kept -----------------------------------------------------------------------------------------

    def test_a_question_that_cannot_be_delivered_is_kept_and_asked_later(self):
        with self.post(("unreachable", "connection refused")):
            self.assertFalse(self.ask("Which account?", secret=True))
        kept = self.kept()
        self.assertEqual([(k["kind"], k["reply"], k.get("secret")) for k in kept], [("query", "Which account?", True)])
        with self.post(("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual(self.kept(), [])
        self.assertEqual((self.posted[-1]["kind"], self.posted[-1]["task_id"]), ("query", T1))

    def test_a_question_the_control_plane_refuses_is_dropped_not_kept_or_shrunk(self):
        with self.post(("refused", "HTTP 409 the task is not open"), ("refused", "HTTP 409")):
            self.assertFalse(self.ask("Which one?"))
        self.assertEqual(self.kept(), [])
        # Kept while unreachable, then refused at a resend: let go, never a least form (it would drop the gate).
        with self.post(("unreachable", "x")):
            self.ask("Which one?", kind="approval")
        with self.post(("refused", "HTTP 409")):
            self.assertEqual(messaging.resend_unsent(), 0)
        self.assertEqual(self.kept(), [])
        with self.post():
            messaging.resend_unsent()
        self.assertEqual(len(self.posted), 4)

    def test_the_agent_still_waits_on_a_kept_question_and_its_reply_is_matched(self):
        for name in ("waiting_for_query", "current_task_id", "last_query_text", "last_query_kind", "last_query_secret",
                     "asked_query_id", "query_asked_at"):
            self.addCleanup(setattr, state, name, getattr(state, name, None))
        state.current_task_id = T1
        with mock.patch.object(messaging, "_query_gate", None), \
                mock.patch.object(messaging.task_context, "date_question", lambda *a: None), \
                mock.patch("decurix_agent.unit_answers.question", lambda *a: None), \
                mock.patch("decurix_agent.long_task.take_question", lambda *a: False), \
                mock.patch.object(config, "QUERYTEXT_DIR", Path(self.tmp.name)), \
                self.post(("unreachable", "down")):
            messaging.ask_for_query("Which bank account?")
        self.assertTrue(state.waiting_for_query)
        self.assertEqual(len(self.kept()), 1)
        # Delivered later; the person's reply on that task is the question's answer (marked as a reply).
        with self.post(("sent", "")):
            self.assertEqual(messaging.resend_unsent(), 1)
        with mock.patch.object(config, "COMMAND_DIR", Path(self.tmp.name)), \
                mock.patch.object(messaging, "log"):
            path = messaging.save_request("the second one", task_id=T1)
        self.assertIn(messaging.REPLY_MARK, path.name)

    # --- order --------------------------------------------------------------------------------------------------------

    def test_kept_notes_go_oldest_first_and_a_new_one_never_passes_them(self):
        with self.post(("unreachable", "x"), ("unreachable", "x"), ("unreachable", "x")):
            self.ask("first?")
            self.ask("second?")  # (the first is tried again, still down: the second is not posted ahead of it)
        self.assertEqual([p["reply"] for p in self.posted], ["first?", "first?"])
        self.assertEqual([k["reply"] for k in self.kept()], ["first?", "second?"])
        # The control plane is back: a new note sends the kept ones first, in order, then itself.
        self.posted.clear()
        with self.post():
            self.assertTrue(self.ask("third?"))
        self.assertEqual([p["reply"] for p in self.posted], ["first?", "second?", "third?"])
        self.assertEqual(self.kept(), [])

    def test_a_tasks_outcome_makes_its_kept_questions_moot(self):
        with self.post(("unreachable", "x")):
            self.ask("Which one?", task_id=T1)
        self.assertEqual(len(self.kept()), 1)
        # The task ends (a stop, a restart's report) while the question is still kept: the outcome goes, the question
        # never does - before or after it.
        with self.post():
            self.assertTrue(self.final("Stopped.", task_id=T1, kind="error"))
        self.assertEqual([(p["kind"], p["reply"]) for p in self.posted[1:]], [("error", "Stopped.")])
        self.assertEqual(self.kept(), [])

    def test_a_kept_outcome_drops_the_same_tasks_kept_question_at_resend(self):
        with self.post(("unreachable", "x"), ("unreachable", "x")):
            self.ask("Which one?", task_id=T1)
            self.final("Interrupted.", task_id=T1, kind="error")
        # (The outcome's send dropped the question already; one written by an earlier life is dropped at resend too.)
        folder = Path(self.tmp.name) / "unsent"
        (folder / "00000000000000000001_abcdef12.json").write_text(
            json.dumps({"kind": "query", "task_id": T1, "reply": "old?", "content": "old?", "event_id": "b" * 32}),
            encoding="utf-8")
        self.posted.clear()
        with self.post():
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual([(p["kind"], p["reply"]) for p in self.posted], [("error", "Interrupted.")])

    def test_a_question_of_a_task_no_longer_in_hand_is_not_asked(self):
        # The agent restarted (or the task ended) and does not hold the question: shown, it would wait for good.
        with self.post(("unreachable", "x")):
            self.ask("Which one?", task_id=T1)
        self.in_hand = T2
        with self.post():
            self.assertEqual(messaging.resend_unsent(), 0)
        self.assertEqual(self.kept(), [])
        self.assertEqual(len(self.posted), 1)

    def test_a_long_requests_parked_question_of_the_task_in_hand_is_asked(self):
        with self.post(("unreachable", "x")):
            self.assertFalse(messaging.post_question("Which of the two ledgers?"))
        with self.post():
            self.assertEqual(messaging.resend_unsent(), 1)
        self.assertEqual(self.posted[-1]["task_id"], T1)

    def test_a_reply_on_another_task_that_keeps_working_drops_no_question(self):
        # A long request answers a message on that message's own task and goes on: its parked question stands.
        with self.post(("unreachable", "x")):
            messaging.post_question("Which of the two ledgers?")
        with self.post():
            messaging.reply_on_task(T2, "Noted, carrying on.", still_working=True)
        self.assertEqual([p["reply"] for p in self.posted[1:]],
                         ["Which of the two ledgers?", "Noted, carrying on."])


if __name__ == "__main__":
    unittest.main()
