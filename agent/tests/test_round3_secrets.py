"""
The secrets and gate review's round 3 (REVIEW-PY-A-2, 2026-11-04): each finding as a test that failed on the code
before, and a test for each fix that no test pinned. Values are made up, of the input classes named.

- R2-B1: a marked request's values were filed under the task in hand - on an idle desktop the one that had ended -
  and outlived their own task; and every word of 4+ characters was kept as a substring.
- R2-B2: the gate kept the values of the last two tasks only, so an open task's values went when two newer requests
  came; and the control plane's "task ended" notice (/task-ended, H-CP-11).
- R2-B3: a question one of whose parts could not be checked was asked blind; a refusal crashed the worker's side.
- Pinned now: the log's own withholding, values let go at the task's end, note_secret_values' literal keep, the
  shared-folder manifest and the planned run's ledger and checkpoint, the typed text across the quote's cut.
- Nits: an address URL with no default port is asked about; an unreadable store is kept, never wiped.
"""

import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from test_secret_first import SecretFirst, TASK, VALUE  # noqa: E402

from decurix_agent import config, gate_server as gate, logging_utils, messaging, state, task_context  # noqa: E402
from decurix_agent.computer import approval, privacy  # noqa: E402

LONG = f"Sign in to the staff portal as mira.k with {VALUE} and download the roster"
ENDED = "t00000000000000e0"
OTHER = "t00000000000000b1"


def literal_file_values():
    path = privacy._literal_file()
    kept = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
    return [v for vs in (kept.get("tasks") or {}).values() for v in vs]


class ValuesLiveAsLongAsTheirTask(SecretFirst):
    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)

    def end(self, task):
        with mock.patch.object(messaging, "send_query", lambda *a, **k: True), \
                mock.patch.object(messaging, "_final_gate", None), \
                mock.patch.object(messaging.progress, "flush", lambda: None), \
                mock.patch.object(messaging, "_keep_facts", lambda *a, **k: None):
            messaging.write_final_reply("Done.", task_id=task)

    def test_a_marked_request_on_an_idle_desktop_is_filed_under_its_own_task(self):
        # The probe's class: the desktop's last task has ended (state still named it), a marked request arrives.
        with mock.patch.object(state, "current_task_id", ENDED), mock.patch.object(state, "waiting_for_query", False):
            self.send(LONG)
        privacy._literal_cache.clear()
        self.assertIn(VALUE, " ".join(privacy.literals(TASK)))
        self.assertEqual(privacy.literals(ENDED), [])
        self.end(TASK)
        privacy._literal_cache.clear()
        self.assertNotIn(VALUE, " ".join(literal_file_values()))
        self.assertEqual(privacy.withhold_known("downloading the staff roster"), "downloading the staff roster")

    def test_values_of_one_task_do_not_outlive_it_into_the_next(self):
        self.send(LONG)
        self.end(TASK)
        self.assertEqual(state.current_task_id, "")  # the ended task is not the one in hand any more
        privacy._literal_cache.clear()
        self.assertEqual(privacy.literals(TASK), [])
        task_context.start(OTHER, request="Download the staff roster")
        self.assertEqual(task_context.withhold("downloading the staff roster", OTHER), "downloading the staff roster")

    def test_a_marked_message_is_one_whole_value_never_its_every_word(self):
        values = privacy.marked_values(LONG)
        self.assertEqual(values, [LONG])
        self.assertEqual(privacy.marked_values("my password is Zebra-42-quiet, thanks"),
                         ["Zebra-42-quiet", "my password is Zebra-42-quiet, thanks"])
        # The whole message, with other spacing, inside a longer text.
        task_context.note_secret_values(TASK, values)
        held = task_context.withhold("Use this:\n" + LONG.replace(" with ", "\nwith ") + "\nok", task_id=TASK)
        self.assertNotIn(VALUE, held)
        self.assertNotIn(VALUE, privacy.withhold_known("Use this:\n" + LONG.replace(" with ", "\nwith ")))

    def test_the_value_is_let_go_at_the_tasks_end(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        privacy._literal_cache.clear()
        self.assertEqual(privacy.literals(TASK), ["Copper71meadow"])
        self.end(TASK)
        privacy._literal_cache.clear()
        self.assertEqual(privacy.literals(TASK), [])
        self.assertNotIn(TASK, task_context._secret_values)

    def test_note_secret_values_keeps_them_for_every_process(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        task_context._secret_values.clear()  # another process: nothing in its memory
        privacy._literal_cache.clear()
        self.assertNotIn("Copper71meadow", privacy.withhold_known("typed Copper71meadow"))

    def test_the_log_withholds_every_line(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        path = self.root / "agent.log"
        with mock.patch.object(logging_utils, "_log_path", str(path)):
            logging_utils.log("typed Copper71meadow into the field")
        written = path.read_text(encoding="utf-8")
        self.assertIn("typed [withheld] into the field", written)
        self.assertNotIn("Copper71meadow", written)

    def test_an_unreadable_literal_file_is_kept_and_its_values_still_withheld(self):
        task_context.note_secret_values(TASK, ["Copper71meadow"])
        path = privacy._literal_file()
        path.write_text("{not json", encoding="utf-8")
        privacy._literal_cache.clear()
        privacy.remember_literals(["Velvet88harbor"], OTHER)
        self.assertEqual(path.read_text(encoding="utf-8"), "{not json")  # kept as it was, never written over
        self.assertNotIn("Velvet88harbor", privacy.withhold_known("x Velvet88harbor"))  # this process still has it
        privacy._unsaved_literals.discard("Velvet88harbor")


class TheB2WritersByBehaviour(SecretFirst):
    """The shared folder's manifest and a planned run's ledger and checkpoint, given a marked request."""

    def setUp(self):
        super().setUp()
        privacy._literal_cache.clear()
        self.addCleanup(privacy._literal_cache.clear)
        task_context.note_secret_values(TASK, privacy.marked_values(LONG))

    def test_the_shared_folders_manifest(self):
        from decurix_agent import shared, source_guard
        with mock.patch.object(shared, "active", lambda: True), mock.patch.object(shared, "walk", lambda: []), \
                mock.patch.object(source_guard, "_folder", lambda task_id: self.root / "guard" / task_id):
            source_guard.take(LONG, TASK)
        written = (self.root / "guard" / TASK / "manifest.json").read_text(encoding="utf-8")
        self.assertNotIn(VALUE, written)

    def test_a_planned_runs_ledger_and_checkpoint(self):
        from decurix_agent import long_task, workflow_runner
        runner = workflow_runner.WorkflowRunner(self.root / "runs-wf")
        with mock.patch.object(long_task, "_runner", lambda: runner):
            long_task._save_ledger("r1", {"request": LONG, "task_id": TASK})
        runner._save({"run_id": "r1", "inputs": {"request": LONG}, "status": "running"})
        for path in (self.root / "runs-wf").rglob("*.json"):
            self.assertNotIn(VALUE, path.read_text(encoding="utf-8"), path.name)

    def test_write_scrubbed_fails_closed(self):
        with mock.patch.object(privacy, "_withhold_known", side_effect=RuntimeError("cannot check")):
            with self.assertRaises(OSError):
                privacy.write_scrubbed(self.root / "x.txt", "anything")
        self.assertFalse((self.root / "x.txt").exists())


class InTheGate(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root), mock.patch.object(approval, "IN_GATE", True)):
            p.start()
            self.addCleanup(p.stop)

    def shown(self, task, typed="Copper71meadowRiver"):
        material = {"kind": "command", "command": f'gui type e3 "{typed}"', "request": "r", "workdir": "",
                    "task_id": task}
        return approval.from_material(material)[1]


class AnOpenTaskKeepsItsValues(InTheGate):
    a, b, c = "t000000000000000a", "t000000000000000b", "t000000000000000c"

    def test_after_two_newer_requests(self):
        gate.keep_request(self.a, "task a")
        gate.keep_secrets(self.a, ["Copper71meadowRiver"])
        gate.keep_request(self.b, "task b")
        gate.keep_request(self.c, "task c")
        self.assertNotIn("Copper71meadowRiver", self.shown(self.a))

    def test_until_the_control_plane_says_it_ended(self):
        gate.keep_request(self.a, "task a")
        gate.keep_secrets(self.a, ["Copper71meadowRiver"])
        gate.keep_secrets(self.b, ["Velvet88harbor"])
        gate.end_task(self.a)
        self.assertIn("Copper71meadowRiver", self.shown(self.a))
        self.assertNotIn("Velvet88harbor", self.shown(self.b, "Velvet88harbor"))
        self.assertEqual(approval.gate_request_text(self.a), "")  # its request copy went too
        gate.end_task("t00000000000000ff")  # a task it holds nothing for is no error

    def test_by_age_as_the_backstop(self):
        gate.keep_secrets(self.a, ["Copper71meadowRiver"], now=time.time() - gate.SECRET_KEEP_SECONDS - 60)
        gate.keep_secrets(self.b, ["Velvet88harbor"])
        self.assertIn("Copper71meadowRiver", self.shown(self.a))
        self.assertNotIn("Velvet88harbor", self.shown(self.b, "Velvet88harbor"))

    def test_by_size_oldest_first(self):
        with mock.patch.object(gate, "SECRET_STORE_BYTES", 400):
            gate.keep_secrets(self.a, ["A" * 150], now=time.time() - 10)
            gate.keep_secrets(self.b, ["B" * 150], now=time.time() - 5)
            gate.keep_secrets(self.c, ["C" * 150])
        kept = approval.gate_secret_values()
        self.assertNotIn("A" * 150, kept)
        self.assertIn("C" * 150, kept)

    def test_the_round_two_store_is_read(self):
        approval.gate_secrets_path().write_text(json.dumps({self.a: ["Copper71meadowRiver"]}), encoding="utf-8")
        self.assertNotIn("Copper71meadowRiver", self.shown(self.a))
        gate.keep_secrets(self.b, ["Velvet88harbor"])
        self.assertNotIn("Copper71meadowRiver", self.shown(self.a))

    def test_an_unreadable_store_is_kept_and_holds_the_words_back(self):
        path = approval.gate_secrets_path()
        path.write_text("{not json", encoding="utf-8")
        with self.assertRaises(OSError):
            gate.keep_secrets(self.a, ["Copper71meadowRiver"])
        with self.assertRaises(OSError):
            gate.end_task(self.a)
        self.assertEqual(path.read_text(encoding="utf-8"), "{not json")
        self.assertEqual(self.shown(self.a), privacy.HELD_BACK)

    def test_the_notice_over_http(self):
        import threading
        import urllib.error
        import urllib.request
        from http.server import ThreadingHTTPServer
        key = "test-gate-key"
        gate.keep_secrets(self.a, ["Copper71meadowRiver"])
        with mock.patch.object(gate.ReplyHandler, "key", key):
            server = ThreadingHTTPServer(("127.0.0.1", 0), gate.ReplyHandler)
            threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
            self.addCleanup(server.server_close)
            self.addCleanup(server.shutdown)

            def post(body):
                request = urllib.request.Request(f"http://127.0.0.1:{server.server_address[1]}/task-ended",
                                                 data=json.dumps(body).encode(), method="POST",
                                                 headers={"Content-Type": "application/json"})
                try:
                    with urllib.request.urlopen(request, timeout=5) as response:
                        return response.status
                except urllib.error.HTTPError as exc:
                    return exc.code
            ts, nonce = str(int(time.time())), "0123456789abcdef0123456789abcdef"
            self.assertEqual(post({"task_id": self.a, "ts": ts, "nonce": nonce, "sig": "0" * 64}), 403)
            self.assertNotIn("Copper71meadowRiver", self.shown(self.a))
            sig = gate.sign_ended(key, self.a, ts, nonce)
            self.assertEqual(post({"task_id": self.a, "ts": ts, "nonce": nonce, "sig": sig}), 204)
            self.assertIn("Copper71meadowRiver", self.shown(self.a))
            self.assertEqual(post({"task_id": self.a, "ts": ts, "nonce": nonce, "sig": sig}), 403)  # once only


real_spoken = privacy.withhold_spoken_secrets
TYPED = "Copper71meadowRiver"


def flaky(text):
    """Withholding that fails on one part only - the class of probe_heldback."""
    if text == TYPED or str(text).startswith("gui type"):
        raise RuntimeError("cannot check")
    return real_spoken(text)


class APartThatCannotBeCheckedRefuses(InTheGate):
    def setUp(self):
        super().setUp()
        p = mock.patch.object(privacy, "withhold_spoken_secrets", flaky)
        p.start()
        self.addCleanup(p.stop)

    def check(self, material):
        return gate.handle({"op": "check", "material": dict(material, task_id="t000000000000000a")})

    def assert_refused(self, out):
        self.assertFalse(out["allowed"])
        self.assertIsNone(out["request"], f"asked blind: {out.get('request')}")
        self.assertIs(out.get("refused"), True)  # H-SOUL-8: soul takes it as a no to this one action, and resumes
        self.assertEqual(out["reason"], gate.REFUSED_REASON)
        self.assertNotIn(TYPED, json.dumps(out))

    def test_typed_text(self):
        self.assert_refused(self.check({"kind": "type", "text": TYPED, "context": {"url": "https://portal.test/"},
                                        "element": {"role": "textbox", "name": "Notes", "window_title": "Portal"}}))

    def test_a_command(self):
        self.assert_refused(self.check({"kind": "command", "command": "gui type e3 x", "request": "r",
                                        "workdir": ""}))

    def test_what_it_acts_on(self):
        with mock.patch.object(approval, "shown_by_gate",
                               lambda text: privacy.HELD_BACK if text == "the Pay button" else text):
            self.assert_refused(self.check({"kind": "click", "about": "the Pay button",
                                            "element": {"role": "button", "name": "Pay", "window_title": "Shop"}}))

    def test_a_question_that_can_be_checked_is_still_asked(self):
        out = gate.handle({"op": "check", "material": {"kind": "command", "command": "rm -rf /tmp/x", "request": "r",
                                                       "workdir": "", "task_id": "t000000000000000a"}})
        self.assertIsNotNone(out["request"])
        self.assertNotIn("refused", out)


class TheWorkersSideOfARefusal(unittest.TestCase):
    def test_request_text_of_none(self):
        self.assertEqual(approval.request_text(None), "")

    def test_act_raises_the_reason_and_waits_on_nobody(self):
        import test_approval_hardening as hardening
        from decurix_agent.computer import cdp, core
        from decurix_agent.computer.errors import ComputerError
        case = hardening.EnterIsAPressOnTheSubmitControl("test_enter_in_a_search_form_is_not")
        case.setUp()
        self.addCleanup(case.doCleanups)
        form = {"form": "Checkout", "fields": [["Name on card", "A Rao"]], "submit": "Pay now"}
        refusal = approval.Decision(False, reason=gate.REFUSED_REASON)
        with mock.patch.object(cdp, "enter_form_fields", return_value=form), \
                mock.patch.object(approval, "check", lambda *a, **k: refusal):
            with self.assertRaises(ComputerError) as caught:
                core.act("key", ref="e1", keys=["Return"], check_form=True)
        self.assertIn("could not check this action's words", str(caught.exception))
        self.assertIsNone(approval.pending())


class TheTypedTextAcrossTheCut(InTheGate):
    def test_a_value_across_the_quotes_cut_leaves_nothing_of_itself(self):
        value = "Copper71meadowRiver"
        gate.keep_secrets("t000000000000000a", [value])
        text = "n" * 52 + value  # the value starts before the 60-character quote's cut and ends after it
        material = {"kind": "type", "text": text, "task_id": "t000000000000000a",
                    "element": {"role": "textbox", "name": "Notes", "window_title": "Portal"}}
        description = approval.from_material(material)[1]
        self.assertNotIn(value[:6], description)


class AnAddressWithNoPort(unittest.TestCase):
    def test_is_a_site_nothing_names(self):
        host = approval._host("gopher://10.0.0.7/login")
        self.assertTrue(host)
        self.assertFalse(approval._names_host("open gopher://10.0.0.7/login", host, "gopher://10.0.0.7/login"))
        self.assertTrue(approval._host("http://[::1"))  # one that cannot be read is asked about too
        self.assertEqual(approval._host(""), "")       # no URL at all: no site (a desktop app)


if __name__ == "__main__":
    unittest.main()
