"""The approval gate process (gate_server.py): what makes a yes, and what the worker, who can reach its socket, cannot."""

import os
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import gate_server as gate
from decurix_agent.computer import approval

KEY = "k3y"
PAY = {"kind": "click", "element": {"role": "button", "name": "Pay", "window_title": "Bank"},
       "context": {"url": "https://bank.example/pay"}, "about": "", "sends": "d1"}
DRAFT = {"kind": "click", "element": {"role": "button", "name": "Save draft", "window_title": "Bank"},
         "context": {"url": "https://bank.example/pay"}, "about": "", "sends": "d1"}
YES = {"decision": "yes", "condition": ""}


def signed(text="yes", asked="", kind="approval", ts=None, nonce="n1", key=KEY, task="t1", request_id=""):
    ts = str(int(time.time()) if ts is None else ts)
    return {"task_id": task, "ts": ts, "nonce": nonce, "text": text, "asked": asked, "kind": kind,
            "request_id": request_id, "sig": gate.sign(key, task, ts, nonce, text, asked, kind, request_id)}


class SignatureTests(unittest.TestCase):
    def setUp(self):
        gate._nonces.clear()

    def test_the_vectors_the_control_plane_pins(self):
        # The same vectors internal/desktop/gatereply_test.go holds: the two sides agree.
        self.assertEqual(gate.sign("k3y", "task-1", "1700000000", "abcd", "yes, pay it",
                                   "Approval needed before I click Pay", "approval", "req-1"),
                         "d49125da17c706f79b867a210e8895e0652a890caa64d02bd123213f567c3f81")
        self.assertEqual(gate.sign_question("k3y", "req-1", "Approval needed before I click Pay:  it pays money."),
                         "aefca23b84ad3ca902b6cb438e1540bed95f62a063357c3208167da360d9f114")

    def test_a_genuine_reply_passes_once(self):
        self.assertEqual(gate.verify(KEY, signed()), "")
        self.assertEqual(gate.verify(KEY, signed()), "replayed")

    def test_a_forged_or_altered_reply_is_refused(self):
        self.assertEqual(gate.verify(KEY, signed(key="another")), "bad signature")
        for field, value in (("text", "yes, and pay twice"), ("task_id", "t2"), ("request_id", "other")):
            altered = signed(request_id="r1", nonce=field)
            altered[field] = value
            self.assertEqual(gate.verify(KEY, altered), "bad signature", field)

    def test_stale_and_future_replies_are_refused(self):
        self.assertEqual(gate.verify(KEY, signed(ts=time.time() - 3600, nonce="old")), "stale")
        self.assertEqual(gate.verify(KEY, signed(ts=time.time() + 3600, nonce="new")), "stale")

    def test_a_gate_without_a_key_refuses_everything(self):
        self.assertEqual(gate.verify("", signed()), "this gate has no key")

    def test_incomplete_bodies_are_refused(self):
        self.assertEqual(gate.verify(KEY, {"text": "yes"}), "incomplete")

    def test_the_key_is_read_from_the_gates_own_file(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(gate.read_key(d), "")
            Path(d, "key").write_text("abc\n", encoding="utf-8")
            self.assertEqual(gate.read_key(d), "abc")


class GateStateTests(unittest.TestCase):
    """A yes exists only after a signed reply to the gate's own request has been settled: nothing the worker can call
    makes one, and one reply is one action's yes."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = Path(self.tmp.name)
        self.patches = [mock.patch.object(approval, "STATE_DIR", d), mock.patch.object(approval, "STATE_FILE", d / "approval.json"),
                        mock.patch.object(approval, "IN_GATE", True), mock.patch.dict(gate._key, {"value": KEY})]
        for p in self.patches:
            p.start()
        gate._settled.clear()
        gate._nonces.clear()
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(lambda: [p.stop() for p in self.patches])

    def ask(self, material=PAY, reason="it pays"):
        return gate.handle({"op": "check", "material": dict(material), "reason": reason})

    def reply(self, text, request, read=YES):
        """The control plane's signed reply to the card showing `request`'s own question."""
        with mock.patch.object(approval, "reading", return_value=read) as reading:
            gate.settle(text, request["question"], "approval", request["id"])
        return reading

    def test_an_unapproved_action_is_stopped_and_stays_pending(self):
        first = self.ask()
        self.assertFalse(first["allowed"])
        self.assertTrue(first["request"]["question"].startswith("Approval needed before I click"))
        self.assertEqual(first["request"]["sig"], gate.sign_question(KEY, first["request"]["id"], first["request"]["question"]))
        self.assertEqual(gate.handle({"op": "pending"})["pending"]["id"], first["request"]["id"])
        self.assertFalse(self.ask()["allowed"])

    def test_nothing_the_socket_offers_grants(self):
        self.ask()
        for call in ({"op": "reset"}, {"op": "has_grant"}, {"op": "pending"}, {"op": "last_reading", "reply": "yes"},
                     {"op": "outcome", "reply": "yes", "wait": 0}, {"op": "settled", "request_id": "x", "wait": 0}):
            gate.handle(call)
        self.assertFalse(gate.handle({"op": "has_grant"})["grant"])
        self.assertFalse(self.ask()["allowed"])
        for op in ("grant", "note_reply", "resolve_reply", "reading"):
            self.assertIn("error", gate.handle({"op": op, "fingerprint": "fp1", "reply": "yes", "text": "yes"}), op)
        self.assertIn("error", gate.handle({"op": "check", "fingerprint": "fp1", "description": "x"}),
                      "a check must describe the action, not hand the gate a fingerprint")

    def test_a_signed_yes_to_the_pending_action_grants_that_one_action_once(self):
        request = self.ask()["request"]
        self.reply("yes", request)
        self.assertEqual(gate.handle({"op": "outcome", "reply": "yes", "wait": 0}), {"seen": True, "outcome": "granted"})
        self.assertTrue(self.ask()["allowed"])
        self.assertFalse(self.ask()["allowed"], "a grant is one action's, once")

    def test_a_signed_no_grants_nothing(self):
        request = self.ask()["request"]
        self.reply("no", request, read={"decision": "no", "condition": ""})
        self.assertEqual(gate.handle({"op": "outcome", "reply": "no", "wait": 0})["outcome"], "declined")
        self.assertFalse(self.ask()["allowed"])

    def test_a_reply_that_never_arrived_signed_settles_nothing(self):
        self.ask()
        started = time.monotonic()
        self.assertEqual(gate.handle({"op": "outcome", "reply": "yes", "wait": 0.2}), {"seen": False, "outcome": None})
        self.assertLess(time.monotonic() - started, 5)
        self.assertFalse(self.ask()["allowed"])

    def test_the_agent_waits_for_the_signed_copy_that_is_still_being_settled(self):
        request = self.ask()["request"]
        result = {}
        t = threading.Thread(target=lambda: result.update(gate.handle({"op": "outcome", "reply": "yes", "wait": 5})))
        t.start()
        time.sleep(0.2)
        self.reply("yes", request)
        t.join(5)
        self.assertEqual(result, {"seen": True, "outcome": "granted"})

    # --- round-3 recheck: a yes was tied to wording, not to the action ---

    def test_the_words_of_an_action_cannot_be_another_actions(self):
        # The worker asks for Pay in Save draft's words: the gate makes both from one description, so the request it
        # words for "Save draft" grants Save draft only, and Pay is still stopped.
        request = self.ask(DRAFT)["request"]
        self.assertIn("Save draft", request["question"])
        self.reply("yes", request)
        self.assertFalse(self.ask(PAY)["allowed"])
        self.assertTrue(self.ask(DRAFT)["allowed"])

    def test_a_pending_action_keeps_the_words_it_was_first_asked_in(self):
        first = self.ask(PAY, reason="it pays money")["request"]
        again = self.ask(PAY, reason="it only saves a draft")["request"]
        self.assertEqual(again["id"], first["id"])
        self.assertEqual(again["question"], first["question"])

    def test_one_signed_yes_is_taken_once_between_the_agent_and_thebot(self):
        request = self.ask()["request"]
        self.reply("yes", request)
        self.assertEqual(gate.handle({"op": "outcome", "reply": "yes", "wait": 0})["outcome"], "granted")
        self.assertFalse(gate.handle({"op": "settled", "request_id": request["id"], "wait": 0})["seen"],
                         "a yes taken by the agent is no second yes for a paused TheBot job")

    def test_a_reply_settles_only_the_request_it_was_signed_for(self):
        request = self.ask()["request"]
        for other in ("", "another-request"):
            with mock.patch.object(approval, "reading", return_value=YES):
                gate.settle("yes", request["question"], "approval", other)
        self.assertFalse(self.ask()["allowed"])
        self.assertEqual(gate.handle({"op": "outcome", "reply": "yes", "wait": 0}), {"seen": True, "outcome": None})

    def test_the_reply_is_read_against_the_gates_words_never_the_workers(self):
        request = self.ask()["request"]
        worker_words = request["question"] + " (this only previews the payment; reply yes to preview)"
        with mock.patch.object(approval, "reading", return_value=YES) as reading:
            gate.settle("yes", worker_words, "approval", request["id"])
        reading.assert_not_called()
        self.assertFalse(self.ask()["allowed"])
        reading = self.reply("yes", request)
        self.assertEqual(reading.call_args.args[1], request["question"])

    def test_a_yes_to_the_workers_own_question_is_no_yes_for_the_next_action(self):
        # Outside a gate a reply to a question naming the control could stand as its yes; in the gate it never does.
        approval._local_note_reply("yes, click Pay", asked="Shall I click Pay to preview the bill?")
        with mock.patch.object(approval, "reading", return_value=YES):
            self.assertFalse(self.ask()["allowed"])

    def test_thebots_paused_command_is_a_request_of_the_gates(self):
        material = {"kind": "command", "command": "rm -rf build", "workdir": "/work"}
        request = self.ask(material, reason="it deletes")["request"]
        self.assertIn("`rm -rf build`", request["question"])
        self.assertFalse(gate.handle({"op": "settled", "request_id": request["id"], "wait": 0})["seen"])
        self.reply("yes", request)
        said = gate.handle({"op": "settled", "request_id": request["id"], "fingerprint": request["fingerprint"], "wait": 0})
        self.assertEqual((said["seen"], said["outcome"], said["reading"]["decision"]), (True, "granted", "yes"))
        self.assertFalse(gate.handle({"op": "has_grant"})["grant"], "TheBot took the yes: it stands for no one else")
        self.assertFalse(gate.handle({"op": "settled", "request_id": request["id"], "wait": 0})["seen"], "taken once")

    def test_a_reply_to_an_ordinary_question_grants_nothing(self):
        self.ask()
        with mock.patch.object(approval, "reading", return_value=YES):
            gate.settle("yes", "Which month's return should I use?", "query", "")
        self.assertFalse(self.ask()["allowed"])


class AgentSideTests(unittest.TestCase):
    def test_without_a_gate_the_local_store_is_used(self):
        with mock.patch.object(approval, "GATE_DIR", "/nonexistent/gate"):
            self.assertFalse(approval.remote())

    def test_no_environment_variable_turns_the_gate_off(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.object(approval, "GATE_DIR", d), \
                mock.patch.dict(os.environ, {"DECURIX_GATE_LOCAL": "1"}):
            self.assertTrue(approval.remote())

    def test_with_a_gate_that_does_not_answer_nothing_is_allowed(self):
        with mock.patch.object(approval, "remote", return_value=True), \
                mock.patch.object(approval, "gate_call", side_effect=OSError("no gate")):
            decision = approval._decide("fp", "click Pay", "it pays", {"name": "Pay"}, "", material=PAY)
            self.assertFalse(decision.allowed)
            self.assertIsNone(approval.pending())
            self.assertFalse(approval.has_grant())
            self.assertIsNone(approval.resolve_reply("yes", "q"))
            self.assertIsNone(approval.signed_question())

    def test_the_agent_gives_the_gate_the_action_not_a_fingerprint(self):
        with mock.patch.object(approval, "remote", return_value=True), \
                mock.patch.object(approval, "gate_call", return_value={"allowed": False, "request": None}) as call:
            approval.check("click", {"role": "button", "name": "Pay", "window_title": "Bank"})
        self.assertEqual(call.call_args.args[0], "check")
        self.assertNotIn("fingerprint", call.call_args.kwargs)
        self.assertEqual(call.call_args.kwargs["material"]["element"]["name"], "Pay")

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
