"""
A person's Delete of a recording, and what an export carries (recording.delete, recording.export, http_api).

Delete removed a held run's recording (a labelled run the control plane had not pulled yet, retention.held) with no
word, before its export reached the run record. It stays the person's act, but the agent now answers 409 with
held: true unless force is sent, so the page can say what is lost and ask. And the README of an export listed obs/
(what each action was resolved against) while export() never wrote it: it does now, only blobs of the shape
audit.keep_observation writes, already withheld and scrubbed at write.

    cd agent && PYTHONPATH=. python3 -m unittest tests.test_recording_delete_export
"""

import io
import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
import zipfile
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

import config_stub  # noqa: F401 - first: stubs decurix_agent.config on a machine with no desktop

from decurix_agent import config, http_api, retention
from decurix_agent.computer import audit, recording, screen

TASK = "t0000000000000d01"
SESSION = "task-" + TASK
DEMO = "demo-20261107T101500-abc123"


def frame(fill):
    return screen.Frame(4, 4, bytearray(bytes([fill]) * 48))


class Base(unittest.TestCase):

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        for p in (mock.patch.object(config, "DECURIX_DIR", self.root),
                  mock.patch.object(recording, "ROOT", self.root / "recordings"),
                  mock.patch.object(retention, "log", lambda line: None),
                  mock.patch("decurix_agent.progress.emit", lambda *a, **k: None)):
            p.start()
            self.addCleanup(p.stop)
        recording.append(SESSION, {"kind": "click", "actor": "agent"}, before=frame(1), defaults={"title": "pay"})
        recording.append(DEMO, {"kind": "click", "actor": "human"}, before=frame(2))

    def export_run(self):
        """EXPORTED as the control plane writes it after its pull (test_retention's shape)."""
        folder = self.root / "runs" / TASK
        read = (folder / "LABELLED").read_text(encoding="utf-8")
        (folder / "EXPORTED").write_text("e_1\nabc\n" + read.rstrip("\r\n") + "\n", encoding="utf-8")


class HeldDelete(Base):

    def test_a_held_runs_recording_is_refused_without_force(self):
        retention.mark_labelled(TASK, "final")
        self.assertTrue(recording.held(SESSION))
        with self.assertRaises(recording.Held):
            recording.delete(SESSION)
        self.assertTrue((recording.ROOT / SESSION).is_dir(), "a refused delete removes nothing")

    def test_force_is_the_persons_say_so(self):
        retention.mark_labelled(TASK, "final")
        recording.delete(SESSION, force=True)
        self.assertFalse((recording.ROOT / SESSION).exists())

    def test_once_exported_it_goes_without_asking(self):
        retention.mark_labelled(TASK, "final")
        self.export_run()
        self.assertFalse(recording.held(SESSION))
        recording.delete(SESSION)
        self.assertFalse((recording.ROOT / SESSION).exists())

    def test_an_unlabelled_run_and_a_demonstration_are_never_held(self):
        self.assertFalse(recording.held(SESSION))
        self.assertFalse(recording.held(DEMO))
        recording.delete(DEMO)
        self.assertFalse((recording.ROOT / DEMO).exists())


class ExportCarriesObs(Base):

    def test_obs_is_exported_already_withheld(self):
        observation = {"active_title": "Sign in", "elements": [
            {"role": "password text", "name": "Password", "value": "hunter2-not-kept"},
            {"role": "push button", "name": "Sign in"},
        ]}
        name = audit.keep_observation(SESSION, observation)
        self.assertTrue(name)
        obs = recording.ROOT / SESSION / "obs"
        # Nothing audit writes: another name, and one bigger than audit lets a blob be.
        (obs / "notes.json").write_text("{}", encoding="utf-8")
        (obs / ("e" * 16 + ".json")).write_bytes(b" " * (recording.MAX_OBS_EXPORT_BYTES + 1))

        buffer = io.BytesIO()
        recording.export(SESSION, buffer)
        archive = zipfile.ZipFile(io.BytesIO(buffer.getvalue()))
        kept = [n for n in archive.namelist() if "/obs/" in n]

        self.assertEqual(kept, [f"{SESSION}/obs/{name}.json"])
        blob = archive.read(kept[0]).decode("utf-8")
        self.assertNotIn("hunter2-not-kept", blob)
        self.assertIn("Sign in", blob)
        self.assertIn("obs/", archive.read(f"{SESSION}/README.txt").decode("utf-8"))

    def test_the_export_cap_is_audits(self):
        self.assertEqual(recording.MAX_OBS_EXPORT_BYTES, audit.MAX_OBS_BYTES)

    @unittest.skipIf(os.name == "nt", "symlinks need privileges on Windows")
    def test_a_link_out_of_the_recording_is_not_followed(self):
        obs = recording.ROOT / SESSION / "obs"
        obs.mkdir(parents=True, exist_ok=True)
        outside = self.root / "elsewhere.json"
        outside.write_text('{"secret": "outside"}', encoding="utf-8")
        os.symlink(outside, obs / ("a" * 16 + ".json"))

        buffer = io.BytesIO()
        recording.export(SESSION, buffer)
        self.assertFalse([n for n in zipfile.ZipFile(io.BytesIO(buffer.getvalue())).namelist() if "/obs/" in n])


class DeleteRoute(Base):
    """The agent's POST /recordings/<id>/delete, as the control plane calls it."""

    def setUp(self):
        super().setUp()
        token = mock.patch.dict(os.environ, {"DECURIX_AGENT_TOKEN": "test-agent-token"})
        token.start()
        self.addCleanup(token.stop)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), http_api.Handler)
        threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def post(self, path, body):
        request = urllib.request.Request(self.base + path, data=json.dumps(body).encode(), method="POST",
                                         headers={"Content-Type": "application/json",
                                                  "Authorization": "Bearer test-agent-token"})
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as err:
            with err:
                return err.code, json.loads(err.read() or b"{}")

    def test_held_is_a_409_until_force(self):
        retention.mark_labelled(TASK, "final")
        status, body = self.post(f"/recordings/{SESSION}/delete", {})
        self.assertEqual(status, 409, body)
        self.assertIs(body.get("held"), True)
        self.assertIn("run record", body.get("error", ""))
        self.assertTrue((recording.ROOT / SESSION).is_dir())

        # Only a JSON true is force: the string the form carried is not.
        status, _ = self.post(f"/recordings/{SESSION}/delete", {"force": "1"})
        self.assertEqual(status, 409)

        status, body = self.post(f"/recordings/{SESSION}/delete", {"force": True})
        self.assertEqual(status, 200, body)
        self.assertFalse((recording.ROOT / SESSION).exists())

    def test_a_recording_not_held_is_deleted_at_once(self):
        status, body = self.post(f"/recordings/{DEMO}/delete", {})
        self.assertEqual(status, 200, body)
        self.assertFalse((recording.ROOT / DEMO).exists())


if __name__ == "__main__":
    unittest.main()
