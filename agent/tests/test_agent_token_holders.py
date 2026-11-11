"""The agent token's two holders (docs/gate-boundary.md, "The agent token"): the entrypoint takes the token out of every
environment and leaves one file each for the agent (vm_server.py) and soul; each makes itself non-dumpable, reads its
file and deletes it. The scripts that start them look commands up on the system's PATH, never the worker's
~/.local/bin, and start the holders before anything that reads the worker's own settings.

The script checks read the scripts, so they run on any host; the token file and non-dumpable checks run the code.
"""

import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from decurix_agent import config  # noqa: E402

AGENT = Path(__file__).resolve().parents[1]
ENTRYPOINT = AGENT / "decurix-entrypoint.sh"
START = AGENT / "start_agent_terminal.sh"
WORKER = AGENT / "decurix-worker-session.sh"
BUS = AGENT / "decurix-session-bus.conf"
SYSTEM_PATH = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"


def code(text):
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))


class TheTokenFile(unittest.TestCase):
    def setUp(self):
        self.addCleanup(setattr, config, "_taken_token", config._taken_token)
        config._taken_token = ""

    def test_it_is_read_trimmed_and_deleted(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop(config.AGENT_TOKEN_ENV, None)
            path = Path(tmp) / "agent.token"
            path.write_text("abc123\n", encoding="ascii")
            self.assertTrue(config.take_agent_token(str(path)))
            self.assertFalse(path.exists())
            self.assertEqual(config.agent_token(), "abc123")
            self.assertEqual(config.agent_auth_headers(), {"Authorization": "Bearer abc123"})

    def test_the_environment_still_comes_first(self):
        config._taken_token = "from-file"
        with mock.patch.dict(os.environ, {config.AGENT_TOKEN_ENV: "from-env"}):
            self.assertEqual(config.agent_token(), "from-env")

    def test_no_file_no_token(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop(config.AGENT_TOKEN_ENV, None)
            os.environ.pop(config.AGENT_TOKEN_FILE_ENV, None)
            self.assertFalse(config.take_agent_token())
            self.assertFalse(config.take_agent_token(str(Path(tmp) / "missing")))
            self.assertEqual(config.agent_token(), "")

    @unittest.skipIf(os.name == "nt", "links need privileges on Windows")
    def test_a_link_gives_no_token_and_is_deleted(self):
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "elsewhere"
            target.write_text("not-yours", encoding="ascii")
            link = Path(tmp) / "agent.token"
            link.symlink_to(target)
            self.assertFalse(config.take_agent_token(str(link)))
            self.assertFalse(os.path.lexists(link))
            self.assertTrue(target.exists())


def _vm_server():
    import importlib.util

    spec = importlib.util.spec_from_file_location("vm_server_under_test", AGENT / "vm_server.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class TheDaemonTakesItsToken(unittest.TestCase):
    def test_todays_agent_takes_it_itself(self):
        calls = []

        class Config:
            @staticmethod
            def no_dump():
                calls.append("no_dump")
                return True

            @staticmethod
            def take_agent_token():
                calls.append("take")
                return True
        _vm_server()._take_token(Config)
        self.assertEqual(calls, ["no_dump", "take"])

    def test_an_older_agent_package_gets_it_in_its_environment_and_the_file_goes(self):
        # An eval arm pinned to an earlier revision copies its own decurix_agent onto today's image: its config has
        # no take_agent_token and reads only DECURIX_AGENT_TOKEN.
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "agent.token"
            path.write_text("old-arm-token\n", encoding="ascii")
            env = {"DECURIX_AGENT_TOKEN_FILE": str(path)}
            with mock.patch.dict(os.environ, env, clear=False):
                os.environ.pop("DECURIX_AGENT_TOKEN", None)
                _vm_server()._take_token(object())
                self.assertEqual(os.environ.get("DECURIX_AGENT_TOKEN"), "old-arm-token")
            self.assertFalse(path.exists())


@unittest.skipUnless(sys.platform.startswith("linux"), "prctl is Linux's")
class NonDumpable(unittest.TestCase):
    def test_the_process_is_non_dumpable_after_no_dump(self):
        probe = ("import ctypes, sys; sys.path.insert(0, sys.argv[1]); from decurix_agent import config; "
                 "ok = config.no_dump(); print(ok, ctypes.CDLL(None).prctl(3, 0, 0, 0, 0))")
        r = subprocess.run([sys.executable, "-I", "-c", probe, str(AGENT)], capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr[-2000:])
        self.assertEqual(r.stdout.split(), ["True", "0"])    # PR_GET_DUMPABLE answers 0


class TheEntrypoint(unittest.TestCase):
    def setUp(self):
        self.text = code(ENTRYPOINT.read_text(encoding="utf-8"))

    def test_the_token_leaves_the_environment_before_any_worker_program_starts(self):
        unset = self.text.index("unset DECURIX_AGENT_TOKEN\n")
        self.assertLess(unset, self.text.index('if [ "${DECURIX_SCREEN_USER:-0}" = "1" ]'))
        self.assertLess(unset, self.text.index("/opt/decurix-worker-session.sh"))
        self.assertLess(unset, self.text.rindex("/dockerstartup/startup.sh"))

    def test_one_file_per_holder_owned_by_the_worker_in_a_fresh_folder(self):
        block = self.text[self.text.index("AGENT_TOKEN_DIR=/run/decurix-agent"):self.text.index("unset DECURIX_AGENT_TOKEN")]
        self.assertIn('/usr/bin/rm -rf "$AGENT_TOKEN_DIR"', block)
        self.assertIn("for holder in agent soul; do", block)
        self.assertIn("umask 077", block)
        self.assertIn('/usr/bin/chown "$UID_WORKER:$GID_WORKER" "$AGENT_TOKEN_DIR/$holder.token"', block)
        # Made root's, filled, and only then handed over.
        self.assertLess(block.index("-o root -g root"), block.index('/usr/bin/chown "$UID_WORKER:$GID_WORKER" "$AGENT_TOKEN_DIR"\n'))


class TheStartScript(unittest.TestCase):
    def setUp(self):
        self.text = code(START.read_text(encoding="utf-8"))

    def test_commands_are_looked_up_on_the_systems_path_from_the_first_line(self):
        lines = [line for line in self.text.splitlines() if line.strip()]
        self.assertEqual(lines[0:3], ["WORKER_PATH=$PATH", SYSTEM_PATH, "export PATH"])

    def test_the_holders_start_by_absolute_path_with_their_token_file(self):
        self.assertRegex(self.text, r'/usr/bin/env PATH="\$WORKER_PATH" DECURIX_AGENT_TOKEN_FILE="\$AGENT_TOKEN_DIR/'
                                    r'agent\.token" \\\n\s+/usr/bin/setsid /usr/bin/nice -n 10 /usr/bin/python3 -I '
                                    r'/opt/vm_server\.py')
        self.assertRegex(self.text, r'/usr/bin/env PATH="\$WORKER_PATH" DECURIX_AGENT_TOKEN_FILE="\$AGENT_TOKEN_DIR/'
                                    r'soul\.token" \\\n\s+/usr/bin/setsid /usr/bin/nice -n 10 /opt/soul')

    def test_every_python_before_the_holders_is_isolated(self):
        before = self.text[:self.text.index("/opt/vm_server.py")]
        for call in re.findall(r"python3 [^\n]*", before):
            self.assertTrue(call.startswith("python3 -I "), call)

    def test_the_launch_helper_starts_after_both_tokens_are_taken(self):
        wait = self.text.index('while [ -e "$AGENT_TOKEN_DIR/agent.token" ] || [ -e "$AGENT_TOKEN_DIR/soul.token" ]')
        self.assertLess(self.text.index("/opt/soul"), wait)
        self.assertLess(wait, self.text.index("decurix_agent.computer.launch_helper"))
        self.assertIn('rm -f "$AGENT_TOKEN_DIR/agent.token" "$AGENT_TOKEN_DIR/soul.token"', self.text)


class TheWorkerSession(unittest.TestCase):
    def setUp(self):
        self.text = code(WORKER.read_text(encoding="utf-8"))

    def test_commands_are_looked_up_on_the_systems_path(self):
        self.assertIn("WORKER_PATH=$PATH\n" + SYSTEM_PATH + "\nexport PATH", self.text)

    def test_the_agent_starts_after_the_bus_and_before_the_parts_that_read_the_workers_settings(self):
        # With the container's PATH: the start script keeps it for what it starts (WORKER_PATH there).
        agent = self.text.index('/usr/bin/env PATH="$WORKER_PATH" /usr/bin/setsid /opt/start_agent_terminal.sh')
        self.assertLess(self.text.index("dbus-launch"), agent)
        wait = self.text.index('while [ -e "$AGENT_TOKEN_DIR/agent.token" ]')
        self.assertLess(agent, wait)
        for part in ("xdg-user-dirs-update", "xfce4-panel", "xfsettingsd", "Thunar --daemon"):
            self.assertLess(wait, self.text.index(part), part)

    def test_the_bus_starts_no_service_of_the_workers(self):
        self.assertIn("--config-file=/opt/decurix-session-bus.conf", self.text)
        bus = BUS.read_text(encoding="utf-8")
        self.assertNotIn("<standard_session_servicedirs", bus.split("-->", 1)[1])
        dirs = re.findall(r"<servicedir>([^<]+)</servicedir>", bus)
        self.assertEqual(dirs, ["/usr/local/share/dbus-1/services", "/usr/share/dbus-1/services"])
        self.assertNotIn("\r\n", bus)


if __name__ == "__main__":
    unittest.main()
