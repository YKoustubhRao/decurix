"""
The question the gate shows a person keeps the task's secret values withheld; what it fingerprints and matches stays the
raw action (review S3, 2026-11-04). On a gated desktop the approval question is the gate's own words, made from the
action's material: a secret typed by a command reached the question, the chat and the judgement in the clear. The
gate now withholds, in what it shows, the values the control plane signed as the task's secrets (POST /secrets on the
gate's port, its own signature label) and any credential the words spell out. Input class: a one-word secret value.
"""

import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401

from decurix_agent import config
from decurix_agent import gate_server as gate
from decurix_agent.computer import approval

KEY = "test-gate-key"
TASK = "t00000000000000f2"
VALUE = "Copper71meadow"


def signed_secrets(values, key=KEY):
    ts, nonce = str(time.time()), uuid.uuid4().hex
    return {"task_id": TASK, "ts": ts, "nonce": nonce, "secrets": values,
            "sig": gate.sign_secrets(key, TASK, ts, nonce, values)}


class TheGatesQuestion(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        for p in (mock.patch.object(config, "DECURIX_DIR", Path(tmp.name)), mock.patch.object(approval, "IN_GATE", True)):
            p.start()
            self.addCleanup(p.stop)

    def command(self):
        return {"kind": "command", "command": f'gui type e3 "{VALUE}"', "request": "sign in to the staff portal",
                "workdir": "/home/headless/work"}

    def test_a_pending_gui_type_by_command_never_shows_the_value(self):
        body = signed_secrets([VALUE])
        self.assertEqual(gate.verify_secrets(KEY, body), "")
        gate.keep_secrets(TASK, body["secrets"])
        fingerprint, description, _, _ = approval.from_material(self.command())
        self.assertNotIn(VALUE, description)
        self.assertIn("[withheld]", description)
        pending = gate.handle({"op": "check", "material": self.command()})
        self.assertNotIn(VALUE, str(pending))

    def test_the_fingerprint_stays_the_raw_actions(self):
        before, _, _, _ = approval.from_material(self.command())
        gate.keep_secrets(TASK, [VALUE])
        after, _, _, _ = approval.from_material(self.command())
        self.assertEqual(before, after)
        other = dict(self.command(), command='gui type e3 "Another42value"')
        self.assertNotEqual(approval.from_material(other)[0], after)

    def test_a_typed_secret_into_a_password_field_is_withheld_by_structure(self):
        material = {"kind": "type", "text": VALUE,
                    "element": {"role": "password text", "name": "Password", "window_title": "Staff portal"},
                    "context": {"url": "https://portal.example.test/login"}}
        _, description, _, _ = approval.from_material(material)
        self.assertNotIn(VALUE, description)

    def test_secrets_not_signed_by_the_control_plane_are_not_taken(self):
        self.assertEqual(gate.verify_secrets(KEY, signed_secrets([VALUE], key="another key")), "bad signature")
        self.assertEqual(gate.verify_secrets(KEY, {"task_id": TASK, "secrets": [VALUE]}), "incomplete")
        replayed = signed_secrets([VALUE])
        self.assertEqual(gate.verify_secrets(KEY, replayed), "")
        self.assertEqual(gate.verify_secrets(KEY, replayed), "replayed")
        # A body signed as a request never stands for secrets.
        ts, nonce = str(time.time()), uuid.uuid4().hex
        as_request = {"task_id": TASK, "ts": ts, "nonce": nonce, "secrets": [VALUE],
                      "sig": gate.sign_request(KEY, TASK, ts, nonce, VALUE)}
        self.assertEqual(gate.verify_secrets(KEY, as_request), "bad signature")


if __name__ == "__main__":
    unittest.main()
