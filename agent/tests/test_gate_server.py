"""The approval gate process (gate_server.py): what makes a yes, and what the worker, who can reach its socket, cannot."""

import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import gate_server as gate
from decurix_agent.computer import approval

KEY = "k3y"
ASKED = "Approval needed before I click Pay: it pays. Nothing was clicked or sent."


def signed(text="yes", asked=ASKED, kind="approval", ts=None, nonce="n1", key=KEY, task="t1"):
    ts = str(int(time.time()) if ts is None else ts)
    return {"task_id": task, "ts": ts, "nonce": nonce, "text": text, "asked": asked, "kind": kind,
            "sig": gate.sign(key, task, ts, nonce, text, asked, kind)}


class SignatureTests(unittest.TestCase):
    def setUp(self):
        gate._nonces.clear()

    def test_the_vector_the_control_plane_pins(self):
        # The same vector internal/desktop/gatereply_test.go holds: the two sides agree.
        self.assertEqual(gate.sign("k3y", "task-1", "1700000000", "abcd", "yes, pay it",
                                   "Approval needed before I click Pay", "approval"),
                         "87abad8e0ec160fa1d8ba7e729fcc6083e85f3da9e5e65c8a9f47418cc1731ff")

    def test_a_genuine_reply_passes_once(self):
        self.assertEqual(gate.verify(KEY, signed()), "")
        self.assertEqual(gate.verify(KEY, signed()), "replayed")

    def test_a_forged_or_altered_reply_is_refused(self):
        self.assertEqual(gate.verify(KEY, signed(key="another")), "bad signature")
        altered = signed()
        altered["text"] = "yes, and pay twice"
        self.assertEqual(gate.verify(KEY, altered), "bad signature")
        moved = signed()
        moved["task_id"] = "t2"
        self.assertEqual(gate.verify(KEY, moved), "bad signature")

    def test_stale_and_future_replies_are_refused(self):
        self.assertEqual(gate.verify(KEY, signed(ts=time.time() - 3600, nonce="old")), "stale")
        self.assertEqual(gate.verify(KEY, signed(ts=time.time() + 3600, nonce="new")), "stale")

    def test_a_gate_without_a_key_refuses_everything(self):
        self.assertEqual(gate.verify("", signed()), "this gate has no key")

    def test_incomplete_bodies_are_refused(self):
        self.assertEqual(gate.verify(KEY, {"text": "yes"}), "incomplete")


class GateStateTests(unittest.TestCase):
    """A yes exists only after a signed reply has been settled: nothing the worker can call makes one."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = Path(self.tmp.name)
        self.patches = [mock.patch.object(approval, "STATE_DIR", d), mock.patch.object(approval, "STATE_FILE", d / "approval.json")]
        for p in self.patches:
            p.start()
        gate._outcomes.clear()
        gate._nonces.clear()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(lambda: [p.stop() for p in self.patches])

    def ask(self):
        return gate.handle({"op": "check", "fingerprint": "fp1", "description": "click Pay in \"Bank\"",
                            "reason": "it pays", "control": "Pay", "about": ""})

    def test_an_unapproved_action_is_stopped_and_stays_pending(self):
        first = self.ask()
        self.assertFalse(first["allowed"])
        self.assertEqual(gate.handle({"op": "pending"})["pending"]["fingerprint"], "fp1")
        self.assertFalse(self.ask()["allowed"])

    def test_nothing_the_socket_offers_grants(self):
        self.ask()
        for call in ({"op": "reset"}, {"op": "has_grant"}, {"op": "pending"}, {"op": "last_reading", "reply": "yes"},
                     {"op": "outcome", "reply": "yes", "wait": 0}):
            gate.handle(call)
        self.assertFalse(gate.handle({"op": "has_grant"})["grant"])
        self.assertFalse(self.ask()["allowed"])
        self.assertIn("error", gate.handle({"op": "grant", "fingerprint": "fp1"}))
        self.assertIn("error", gate.handle({"op": "note_reply", "text": "yes"}))
        self.assertIn("error", gate.handle({"op": "resolve_reply", "reply": "yes"}))

    def test_a_signed_yes_to_the_pending_action_grants_that_one_action_once(self):
        request = self.ask()["request"]
        asked = f"Approval needed before I {request['action']}: it pays. Nothing was clicked or sent."
        with mock.patch.object(approval, "reading", return_value={"decision": "yes", "condition": ""}):
            gate.settle("yes", asked, "approval")
        self.assertEqual(gate.handle({"op": "outcome", "reply": "yes", "wait": 0}), {"seen": True, "outcome": "granted"})
        self.assertTrue(self.ask()["allowed"])
        self.assertFalse(self.ask()["allowed"], "a grant is one action's, once")

    def test_a_signed_no_grants_nothing(self):
        request = self.ask()["request"]
        asked = f"Approval needed before I {request['action']}: it pays."
        with mock.patch.object(approval, "reading", return_value={"decision": "no", "condition": ""}):
            gate.settle("no", asked, "approval")
        self.assertEqual(gate.handle({"op": "outcome", "reply": "no", "wait": 0})["outcome"], "declined")
        self.assertFalse(self.ask()["allowed"])

    def test_a_reply_that_never_arrived_signed_settles_nothing(self):
        self.ask()
        started = time.monotonic()
        said = gate.handle({"op": "outcome", "reply": "yes", "wait": 0.2})
        self.assertEqual(said, {"seen": False, "outcome": None})
        self.assertLess(time.monotonic() - started, 5)
        self.assertFalse(self.ask()["allowed"])

    def test_the_agent_waits_for_the_signed_copy_that_is_still_being_settled(self):
        request = self.ask()["request"]
        asked = f"Approval needed before I {request['action']}: it pays."
        result = {}

        def agent():
            result.update(gate.handle({"op": "outcome", "reply": "yes", "wait": 5}))

        t = threading.Thread(target=agent)
        t.start()
        time.sleep(0.2)
        with mock.patch.object(approval, "reading", return_value={"decision": "yes", "condition": ""}):
            gate.settle("yes", asked, "approval")
        t.join(5)
        self.assertEqual(result, {"seen": True, "outcome": "granted"})

    def test_soul_gets_its_reading_from_a_signed_reply_only(self):
        said = {"decision": "yes", "condition": "", "wants": ""}
        self.assertEqual(gate.handle({"op": "reading", "reply": "yes", "wait": 0}), {"seen": False, "reading": None})
        with mock.patch.object(approval, "reading", return_value=said):
            gate.settle("yes", "Approval needed before I run `rm -r build`: it deletes.", "approval")
        self.assertEqual(gate.handle({"op": "reading", "reply": "yes", "wait": 0}), {"seen": True, "reading": said})
        self.assertEqual(gate.handle({"op": "reading", "reply": "yes", "wait": 0})["seen"], False, "a reading is used once")
        # A reply to an ordinary question carries no approval reading.
        with mock.patch.object(approval, "reading", return_value=said):
            gate.settle("june", "Which month?", "query")
        self.assertFalse(gate.handle({"op": "reading", "reply": "june", "wait": 0})["seen"])

    def test_a_yes_to_another_question_does_not_approve_the_pending_action(self):
        self.ask()
        with mock.patch.object(approval, "reading", return_value={"decision": "yes", "condition": ""}):
            gate.settle("yes", "Which month's return should I use?", "query")
        self.assertFalse(self.ask()["allowed"])


class AgentSideTests(unittest.TestCase):
    def test_without_a_gate_the_local_store_is_used(self):
        with mock.patch.object(approval, "GATE_DIR", "/nonexistent/gate"):
            self.assertFalse(approval.remote())

    def test_with_a_gate_that_does_not_answer_nothing_is_allowed(self):
        with mock.patch.object(approval, "remote", return_value=True), \
                mock.patch.object(approval, "gate_call", side_effect=OSError("no gate")):
            decision = approval._decide("fp", "click Pay", "it pays", {"name": "Pay"}, "")
            self.assertFalse(decision.allowed)
            self.assertIsNone(approval.pending())
            self.assertFalse(approval.has_grant())
            self.assertIsNone(approval.resolve_reply("yes", "q"))

    def test_with_a_gate_the_agent_does_not_keep_the_replys_state(self):
        with mock.patch.object(approval, "remote", return_value=True), \
                mock.patch.object(approval, "_local_note_reply") as note, \
                mock.patch.object(approval, "_local_resolve_reply") as resolve:
            approval.note_reply("yes", asked="q")
            with mock.patch.object(approval, "gate_call", return_value={"seen": True, "outcome": "granted"}) as call:
                self.assertEqual(approval.resolve_reply("yes", "q"), "granted")
                self.assertEqual(call.call_args.args[0], "outcome")
            note.assert_not_called()
            resolve.assert_not_called()


if __name__ == "__main__":
    unittest.main()
