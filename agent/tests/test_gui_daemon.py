"""
The gui daemon (computer/daemon.py) and its thin client (computer/gui_client.py): a command run through the daemon
is the command run in its own process - the same stdout, stderr and exit code - and every way the daemon can be
missing, slow, refusing or gone runs it in-process instead, or says what it did was not seen.

Each test drives real processes: the client as agent/gui runs it (python3 -I -S), the daemon and the in-process
command on the stand-in screen (gui_daemon_stand_in.py). Needs fork and descriptor passing over Unix sockets, so it
skips on Windows.

    cd agent && PYTHONPATH=. python3 -m unittest tests.test_gui_daemon
"""

import array
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest

TESTS = os.path.dirname(os.path.abspath(__file__))
AGENT = os.path.dirname(TESTS)
CLIENT = os.path.join(AGENT, "decurix_agent", "computer", "gui_client.py")
STAND_IN = os.path.join(TESTS, "gui_daemon_stand_in.py")

POSIX = hasattr(os, "fork") and hasattr(socket, "AF_UNIX") and hasattr(socket.socket, "sendmsg") \
    and shutil.which("python3") is not None
SKIP = "needs fork, Unix sockets with descriptor passing and python3 on PATH (not Windows)"

sys.path.insert(0, AGENT)


class FakeGate:
    """The approval gate's socket: counts the checks it is asked and allows nothing."""

    def __init__(self, path):
        self.path, self.checks = path, []
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(path)
        self.sock.listen(8)
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            with conn:
                data = b""
                while not data.endswith(b"\n"):
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    data += chunk
                try:
                    asked = json.loads(data.decode("utf-8"))
                except ValueError:
                    continue
                if asked.get("op") == "check":
                    self.checks.append(asked)
                    reply = {"allowed": False, "request": {
                        "id": "r1", "fingerprint": "f1", "reason": "it sends", "about": "", "created": time.time(),
                        "action": "click button \"Send\"", "question": "q", "sig": "s"}}
                else:
                    reply = {}
                conn.sendall((json.dumps(reply) + "\n").encode("utf-8"))

    def close(self):
        self.sock.close()


@unittest.skipUnless(POSIX, SKIP)
class Base(unittest.TestCase):
    """A temporary desktop: a daemon folder, the agent's folder, a work folder and a fake gate."""

    daemon = None

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="gui-daemon-")
        cls.run_dir = os.path.join(cls.tmp, "run")
        cls.home = os.path.join(cls.tmp, "home")
        cls.work = os.path.join(cls.tmp, "work")
        os.makedirs(cls.home)
        os.makedirs(cls.work)
        cls.gate = FakeGate(os.path.join(cls.tmp, "gate.sock"))
        env = {k: v for k, v in os.environ.items() if not k.startswith(("DECURIX_", "STAND_IN_"))}
        env.update({
            "PYTHONPATH": AGENT, "DECURIX_GUI_DAEMON_DIR": cls.run_dir, "DECURIX_DIR": cls.home,
            "DECURIX_TASK_FILE": os.path.join(cls.tmp, "task"), "TMPDIR": cls.tmp,
            "CONTROL_PLANE_URL": "http://127.0.0.1:9", "DECURIX_GATE_REQUIRED": "1",
            "STAND_IN_GATE": cls.gate.path, "DECURIX_LOG_DIR": cls.tmp,
        })
        cls.env = env

    @classmethod
    def tearDownClass(cls):
        cls.stop_daemon()
        cls.gate.close()
        shutil.rmtree(cls.tmp, ignore_errors=True)

    @classmethod
    def start_daemon(cls):
        cls.daemon_log = open(os.path.join(cls.tmp, "daemon.log"), "ab")
        cls.daemon = subprocess.Popen([sys.executable, STAND_IN, "daemon"], env=cls.env, cwd="/",
                                      stdin=subprocess.DEVNULL, stdout=cls.daemon_log, stderr=cls.daemon_log)
        sock = os.path.join(cls.run_dir, "gui.sock")
        deadline = time.time() + 60
        while not os.path.exists(sock):
            if cls.daemon.poll() is not None or time.time() > deadline:
                raise AssertionError("the daemon did not start: " + cls.log())
            time.sleep(0.05)

    @classmethod
    def stop_daemon(cls):
        if cls.daemon is not None and cls.daemon.poll() is None:
            cls.daemon.kill()
            cls.daemon.wait()
        cls.daemon = None

    @classmethod
    def log(cls):
        with open(os.path.join(cls.tmp, "daemon.log"), "rb") as f:
            return f.read().decode("utf-8", "replace")

    def fresh(self):
        """Nothing a previous command kept on disk (what was shown, the task) reaches the next one."""

        for name in os.listdir(self.home):
            shutil.rmtree(os.path.join(self.home, name), ignore_errors=True)
        for name in ("task",):
            try:
                os.unlink(os.path.join(self.tmp, name))
            except FileNotFoundError:
                pass

    def call(self, args, via, stdin=b"", env=None, cwd=None):
        """Run `gui ARGS` in-process (via="here") or through the daemon only (via="daemon"): (code, out, err)."""

        self.fresh()
        run_env = dict(self.env, **(env or {}))
        if via == "daemon":
            run_env["DECURIX_GUI_DAEMON"] = "require"
            argv = [sys.executable, "-I", "-S", CLIENT] + list(args)
        elif via == "client":
            argv = [sys.executable, "-I", "-S", CLIENT] + list(args)
        else:
            argv = [sys.executable, STAND_IN, "gui"] + list(args)
        done = subprocess.run(argv, input=stdin, capture_output=True, env=run_env, cwd=cwd or self.work, timeout=120)
        return done.returncode, done.stdout, done.stderr

    def assertSame(self, args, **kwargs):
        here = self.call(args, "here", **kwargs)
        there = self.call(args, "daemon", **kwargs)
        self.assertEqual(there, here, f"gui {' '.join(args)}: the daemon's run differs\n{self.log()}")
        return here


class ThroughTheDaemon(Base):
    """What a command prints and exits with is the same through the daemon as in its own process."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.start_daemon()

    def test_commands_are_byte_identical(self):
        for args in (["--help"], [], ["frobnicate"], ["click"], ["observe"], ["observe", "--window", "Mail"],
                     ["look"], ["read", "e10"], ["key"], ["scroll", "sideways"]):
            with self.subTest(args=args):
                code, out, err = self.assertSame(args)
                if args[:1] in (["--help"], ["observe"], ["look"], ["read"]):
                    # Compared while working, not only while failing the same way.
                    self.assertEqual(code, 0, out + err)

    def test_stdin_reaches_the_command(self):
        code, out, _ = self.assertSame(["type", "-"], stdin=b"=IF(D2=\"x\",1,0)\n\tline two\n")
        self.assertEqual(code, 0)
        self.assertIn("line two", out.decode())

    def test_no_memory_of_an_earlier_command(self):
        # The stand-in counts its looks in the process: a fresh process says Look 1, and so must every command.
        for _ in range(3):
            code, out, _ = self.call(["observe"], "daemon")
            self.assertEqual(code, 0, out)
            self.assertIn('"Look 1"', out.decode())

    def test_each_commands_own_environment(self):
        # DECURIX_GUI_TEXT_CHARS is set by the engine on each command: a short budget cuts the list short.
        short = self.assertSame(["observe"], env={"DECURIX_GUI_TEXT_CHARS": "400"})[1]
        long = self.assertSame(["observe"], env={"DECURIX_GUI_TEXT_CHARS": "9000"})[1]
        self.assertLess(len(short), len(long))

    def test_each_commands_own_working_folder(self):
        for name in ("a", "b"):
            folder = os.path.join(self.work, name)
            os.makedirs(folder, exist_ok=True)
            code, out, _ = self.call(["observe", "--screenshot", "shot.png"], "daemon", cwd=folder)
            self.assertEqual(code, 0, out)
            with open(os.path.join(folder, "shot.png"), "rb") as f:
                self.assertTrue(f.read().startswith(b"\x89PNG"))

    def test_an_import_time_variable_imports_again(self):
        # DECURIX_SKILLS_DIR is read when skills.py is imported: the child imports the agent again under it.
        skills = os.path.join(self.tmp, "skills-elsewhere")
        os.makedirs(skills, exist_ok=True)
        self.assertSame(["skills"], env={"DECURIX_SKILLS_DIR": skills})

    def test_the_gate_is_asked_for_every_action(self):
        before = len(self.gate.checks)
        here = self.call(["click", "e9"], "here")
        self.assertEqual(here[0], 3, here)
        self.assertEqual(len(self.gate.checks), before + 1)
        for n in (2, 3):
            there = self.call(["click", "e9"], "daemon")
            self.assertEqual(there, here)
            # Asked again each time, from the command's own process: nothing the daemon kept said yes or no.
            self.assertEqual(len(self.gate.checks), before + n)
        self.assertEqual(self.gate.checks[-1]["material"]["element"]["name"], "Send")

    def test_a_killed_client_kills_its_command(self):
        mark = os.path.join(self.tmp, "mark-killed")
        env = dict(self.env, DECURIX_GUI_DAEMON="require", STAND_IN_SLOW="2", STAND_IN_MARK=mark)
        client = subprocess.Popen([sys.executable, "-I", "-S", CLIENT, "observe"], env=env, cwd=self.work,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(0.8)
        client.kill()
        client.wait()
        time.sleep(2.5)
        self.assertFalse(os.path.exists(mark), "the command went on after its caller killed it")

    def test_ctrl_c_interrupts_the_command_as_it_would_here(self):
        results = []
        for via in ("here", "daemon"):
            env = dict(self.env, STAND_IN_SLOW="3")
            if via == "daemon":
                env["DECURIX_GUI_DAEMON"] = "require"
                argv = [sys.executable, "-I", "-S", CLIENT, "observe"]
            else:
                argv = [sys.executable, STAND_IN, "gui", "observe"]
            proc = subprocess.Popen(argv, env=env, cwd=self.work, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            time.sleep(1.5)
            proc.send_signal(signal.SIGINT)
            out, err = proc.communicate(timeout=30)
            results.append((proc.returncode, out))
        self.assertEqual(results[1], results[0])
        self.assertEqual(results[0][0], 1)


class WithoutTheDaemon(Base):
    """No daemon, a dead one, a slow one, a refusing one: the command runs in-process, and runs once."""

    def test_no_daemon_runs_in_process(self):
        self.stop_daemon()
        shutil.rmtree(self.run_dir, ignore_errors=True)
        code, out, err = self.call(["--help"], "client")
        self.assertEqual((code, err), (0, b""))
        self.assertIn(b"gui - see and use this desktop's screen", out)
        self.assertEqual(self.call(["--help"], "daemon")[0], 125)   # "require" says it was not served

    def test_a_folder_that_is_not_the_users_own_is_not_trusted(self):
        os.makedirs(self.run_dir, exist_ok=True)
        os.chmod(self.run_dir, 0o755)
        try:
            self.assertEqual(self.call(["--help"], "daemon")[0], 125)
        finally:
            os.chmod(self.run_dir, 0o700)

    def test_a_daemon_killed_mid_command_still_ends_it_and_the_next_runs_in_process(self):
        self.start_daemon()
        env = dict(self.env, DECURIX_GUI_DAEMON="require", STAND_IN_SLOW="2")
        client = subprocess.Popen([sys.executable, "-I", "-S", CLIENT, "observe"], env=env, cwd=self.work,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(0.8)
        self.daemon.kill()          # the daemon, not the command's own process
        self.daemon.wait()
        out, err = client.communicate(timeout=30)
        self.assertEqual(client.returncode, 0, err)
        self.assertIn(b'"Look 1"', out)
        code, out, _ = self.call(["--help"], "client")       # its socket is left, nobody answers it
        self.assertEqual(code, 0)
        self.assertIn(b"gui - see", out)
        self.stop_daemon()

    def fake_daemon(self, behaviour):
        """A socket in the daemon's folder that answers as `behaviour` says: silent, refuse, accept-then-vanish,
        start-then-vanish."""

        os.makedirs(self.run_dir, mode=0o700, exist_ok=True)
        path = os.path.join(self.run_dir, "gui.sock")
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.bind(path)
        sock.listen(4)
        held = []

        def serve():
            try:
                conn, _ = sock.accept()
            except OSError:
                return
            held.append(conn)
            _, ancdata, _, _ = conn.recvmsg(65536, socket.CMSG_SPACE(64))
            # The client's own stdout and stderr came with it: held here, its caller would never see them end.
            for level, kind, data in ancdata:
                if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
                    for fd in array.array("i", data[:len(data) - len(data) % 4]):
                        os.close(fd)
            if behaviour == "silent":
                return
            if behaviour == "refuse":
                conn.sendall(b"R\x03")
                return
            conn.sendall(b"A\0")
            conn.recv(2)
            if behaviour == "start-then-vanish":
                conn.sendall(b"S\0")
            conn.close()

        threading.Thread(target=serve, daemon=True).start()
        self.addCleanup(lambda: [c.close() for c in held] and None)
        self.addCleanup(sock.close)
        self.addCleanup(lambda: os.path.exists(path) and os.unlink(path))

    def test_a_silent_daemon_runs_it_in_process(self):
        self.fake_daemon("silent")
        started = time.time()
        code, out, _ = self.call(["--help"], "client")
        self.assertEqual(code, 0)
        self.assertIn(b"gui - see", out)
        self.assertLess(time.time() - started, 30)

    def test_a_refusing_daemon_runs_it_in_process(self):
        self.fake_daemon("refuse")
        code, out, _ = self.call(["--help"], "client")
        self.assertEqual(code, 0)
        self.assertIn(b"gui - see", out)

    def test_a_daemon_gone_before_the_command_started_runs_it_in_process(self):
        self.fake_daemon("accept-then-vanish")
        code, out, _ = self.call(["--help"], "client")
        self.assertEqual(code, 0)
        self.assertIn(b"gui - see", out)

    def test_a_daemon_gone_after_the_command_started_is_not_run_again(self):
        # Started is started: what it did is unknown, and running it a second time could do it twice.
        self.fake_daemon("start-then-vanish")
        code, out, err = self.call(["--help"], "client")
        self.assertEqual(code, 1)
        self.assertEqual(out, b"")
        self.assertIn(b"was not seen", err)


@unittest.skipUnless(POSIX, SKIP)
class WhatTheDaemonCompares(unittest.TestCase):
    """The variables a module reads at import are found in its source; the interpreter's own are compared."""

    def test_import_time_reads_are_found_and_call_time_ones_are_not(self):
        from decurix_agent.computer import daemon

        source = (
            "import os\n"
            "A = os.environ.get('AT_IMPORT', '1')\n"
            "B = os.getenv('ALSO_AT_IMPORT')\n"
            "if 'GUARD' in os.environ:\n"
            "    C = os.environ['IN_THE_GUARD']\n"
            "class K:\n"
            "    D = os.environ.get('IN_A_CLASS')\n"
            "def f(x=os.environ.get('A_DEFAULT')):\n"
            "    return os.environ.get('PER_CALL')\n"
        )
        self.assertEqual(daemon.import_time_reads(source),
                         {"AT_IMPORT", "ALSO_AT_IMPORT", "GUARD", "IN_THE_GUARD", "IN_A_CLASS", "A_DEFAULT"})

    def test_the_gui_modules_import_time_reads_include_the_known_ones(self):
        from decurix_agent.computer import cli, daemon  # noqa: F401

        keys = daemon.import_keys(daemon.own_modules())
        for name in (b"DECURIX_DIR", b"DECURIX_CDP_PORT", b"DECURIX_SKILLS_DIR", b"DECURIX_TASK_FILE", b"TMPDIR"):
            self.assertIn(name, keys)
        # Set by TheBot on every command it runs: read per command, or every one would import again.
        for name in (b"DECURIX_GUI_TEXT_CHARS", b"DECURIX_GUI_SCREENSHOT", b"DECURIX_TASK_ID", b"DECURIX_TURN_ID",
                     b"DECURIX_OFFICE_IMAGES"):
            self.assertNotIn(name, keys)

    def test_interpreter_settings_that_differ_are_named(self):
        from decurix_agent.computer import daemon

        own = {b"HOME": b"/home/headless", b"PYTHONPATH": b"/opt", b"LANG": b"C.UTF-8", b"X": b"1"}
        self.assertEqual(daemon.startup_differs({**own, b"X": b"2"}, own), [])
        self.assertEqual(daemon.startup_differs({**own, b"PYTHONPATH": b"/opt:/opt"}, own), [b"PYTHONPATH"])
        self.assertEqual(daemon.startup_differs({**own, b"LC_ALL": b"C"}, own), [b"LC_ALL"])

    def test_secret_looking_variables_are_not_kept(self):
        from decurix_agent.computer import daemon

        for name in ("DECURIX_AGENT_TOKEN", "OPENAI_API_KEY", "AZURE_KEY", "VNC_PW", "DB_PASSWORD"):
            self.assertTrue(daemon.secret_looking(name), name)
        for name in ("DISPLAY", "HOME", "DECURIX_DIR", "PYTHONPATH"):
            self.assertFalse(daemon.secret_looking(name), name)


if __name__ == "__main__":
    unittest.main()
