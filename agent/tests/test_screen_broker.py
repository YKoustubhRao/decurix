"""
The screen broker (broker mode, DECURIX_SCREEN_USER=1; docs/gate-boundary.md "Next: the input broker"): the gui
daemon as the screen user, the DevTools relay, and the worker's launch helper.

What runs anywhere (Windows too): which of a caller's variables the broker's child takes; the client writing the files
a command hands back, only at paths it named; the client never running a command itself in broker mode; the relay on a
stand-in browser over a real pipe, reached by cdp.py's own code (the socket swapped for a socket pair); the launch
helper's request handling and the program it starts; session.launch's routing on the screen side.

What needs fork and Unix sockets (skipped on Windows): the broker daemon end to end through the real client, the relay
as its own process with the stand-in browser on fds 3 and 4, and the launch helper on its socket.

    cd agent && PYTHONPATH=".:tests" python3 -m unittest tests.test_screen_broker
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

TESTS = os.path.dirname(os.path.abspath(__file__))
AGENT = os.path.dirname(TESTS)
CLIENT = os.path.join(AGENT, "decurix_agent", "computer", "gui_client.py")
STAND_IN = os.path.join(TESTS, "screen_broker_stand_in.py")

sys.path.insert(0, AGENT)
sys.path.insert(0, TESTS)

from decurix_agent.computer import broker, cdp, cdp_relay, daemon, gui_client, launch_helper, session  # noqa: E402
from screen_broker_stand_in import StandInBrowser  # noqa: E402

POSIX = hasattr(os, "fork") and hasattr(socket, "AF_UNIX") and hasattr(socket.socket, "sendmsg") \
    and getattr(socket, "SO_PEERCRED", None) is not None
SKIP = "needs fork, Unix sockets with descriptor passing and SO_PEERCRED (Linux)"


# ------------------------------------------------------------
# Part 2: what a broker's child takes of its caller
# ------------------------------------------------------------

class WhatTheBrokersChildTakes(unittest.TestCase):
    """Only the variables the agent's code reads, and none that change the interpreter, the loader, who or where the
    process is, where it reads, writes or connects, or what the gate is told."""

    def test_the_screen_side_keeps_its_own(self):
        for name in ("PYTHONPATH", "PYTHONSTARTUP", "PYTHONHOME", "LD_PRELOAD", "LD_LIBRARY_PATH", "HOME", "PATH",
                     "USER", "DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "TMPDIR", "LANG",
                     "LC_ALL", "DECURIX_DIR", "DECURIX_WORK_DIR", "DECURIX_TASK_FILE", "DECURIX_LOG_DIR",
                     "DECURIX_SKILLS_DIR", "DECURIX_RECORDINGS_DIR", "DECURIX_CDP_PORT", "DECURIX_CDP_RELAY_SOCKET",
                     "CONTROL_PLANE_URL", "OLLAMA_URL", "DECURIX_GATE_REQUIRED", "DECURIX_WORKER_STAGE",
                     "DECURIX_SCREEN_USER", "DECURIX_SCREEN_DIR", "DECURIX_GUI_DAEMON_DIR", "DECURIX_LAUNCH_DIR",
                     "DECURIX_AGENT_TOKEN", "OPENAI_API_KEY", "DECURIX_CALLER", "DECURIX_AGENT_BIND", "GTK2_MODULES"):
            self.assertFalse(daemon.caller_may_set(name), name)
        for name in ("DECURIX_GUI_TEXT_CHARS", "DECURIX_GUI_SCREENSHOT", "DECURIX_GUI_SCREENSHOT_SCALE",
                     "DECURIX_GUI_RULER", "DECURIX_TURN_ID", "DECURIX_REPLAY_FIND_SECONDS"):
            self.assertTrue(daemon.caller_may_set(name), name)

    def test_the_allowlist_is_what_the_package_reads(self):
        allowed = daemon.Broker.for_package(1001).allowed
        for name in (b"DECURIX_GUI_TEXT_CHARS", b"DECURIX_GUI_SCREENSHOT", b"DECURIX_GUI_SCREENSHOT_SCALE"):
            self.assertIn(name, allowed)
        for name in (b"PYTHONPATH", b"LD_PRELOAD", b"HOME", b"DECURIX_DIR", b"DECURIX_CDP_PORT", b"CONTROL_PLANE_URL",
                     b"NOTHING_READS_THIS_VARIABLE"):
            self.assertNotIn(name, allowed)

    def test_a_package_read_is_found_wherever_it_is(self):
        source = ("import os\nA = os.environ.get('AT_IMPORT')\n"
                  "def f():\n    return os.environ.get('PER_CALL'), os.getenv('ALSO'), os.environ['INDEXED']\n")
        self.assertEqual(daemon.env_reads(source), {"AT_IMPORT", "PER_CALL", "ALSO", "INDEXED"})

    def test_the_childs_environment(self):
        b = daemon.Broker(1001, {b"DECURIX_GUI_TEXT_CHARS", b"DECURIX_GUI_SCREENSHOT"})
        own = {b"HOME": b"/var/lib/screen", b"PATH": b"/usr/bin", b"PYTHONPATH": b"/opt",
               b"DECURIX_GUI_SCREENSHOT": b"/screen/own.png", b"DISPLAY": b":1"}
        caller = {b"HOME": b"/home/headless", b"PATH": b"/home/headless/.local/bin:/usr/bin",
                  b"PYTHONPATH": b"/home/headless/evil", b"LD_PRELOAD": b"/home/headless/evil.so",
                  b"DECURIX_GUI_TEXT_CHARS": b"900"}
        env = b.environment(caller, own)
        self.assertEqual(env, {b"HOME": b"/var/lib/screen", b"PATH": b"/usr/bin", b"PYTHONPATH": b"/opt",
                               b"DISPLAY": b":1", b"DECURIX_GUI_TEXT_CHARS": b"900"},
                         "an allowed variable is the caller's, or absent with it; nothing else of the caller's")

    def test_only_the_worker_is_served(self):
        import types

        allowed = daemon.Server.peer_allowed
        server = types.SimpleNamespace(broker=daemon.Broker(4242, set()))
        self.assertTrue(allowed(server, 4242))
        self.assertFalse(allowed(server, 4243))
        self.assertFalse(allowed(server, None), "a caller the system cannot name is refused")
        self.assertTrue(allowed(types.SimpleNamespace(broker=None), None), "the default daemon is unchanged")


# ------------------------------------------------------------
# Part 2: the files a command writes for its caller
# ------------------------------------------------------------

def _frame(flags, path, data):
    payload = gui_client.pack([b"%d" % flags, os.fsencode(path), data])
    return b"O" + len(payload).to_bytes(4, "big") + payload


class TheCallerWritesItsFiles(unittest.TestCase):
    """A screenshot the broker's child takes goes back over the connection and the client writes it, as the caller,
    and only at a path the caller named."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="screen-broker-")
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_the_paths_a_command_names(self):
        with mock.patch.dict(os.environ, {"DECURIX_GUI_SCREENSHOT": " /tmp/turn.png "}):
            paths = gui_client.caller_paths(["observe", "--screenshot", "a.png", "--out=b.png"])
        self.assertEqual(paths, {b"/tmp/turn.png", b"observe", b"--screenshot", b"a.png", b"--out=b.png", b"b.png"})

    def test_a_named_path_is_written_and_another_is_not(self):
        named = os.path.join(self.tmp, "shot.png")
        other = os.path.join(self.tmp, "elsewhere.png")
        allowed = {os.fsencode(named)}
        with mock.patch("os.write", wraps=os.write) as write:
            self.assertEqual(gui_client.save_output(gui_client.pack([b"1", os.fsencode(named), b"PNG1"]), allowed),
                             (True, True))
            self.assertEqual(gui_client.save_output(gui_client.pack([b"2", os.fsencode(other), b"PNG2"]), allowed),
                             (False, False))
        with open(named, "rb") as f:
            self.assertEqual(f.read(), b"PNG1")
        self.assertFalse(os.path.exists(other))
        self.assertFalse(os.path.exists(other + ".part"))
        said = b"".join(c.args[1] for c in write.call_args_list if c.args[0] == 2)
        self.assertIn(b"did not name", said)

    def test_an_atomic_file_is_renamed_into_place(self):
        named = os.path.join(self.tmp, "turn.png")
        written, _ = gui_client.save_output(gui_client.pack([b"2", os.fsencode(named), b"X" * 1000]),
                                            {os.fsencode(named)})
        self.assertTrue(written)
        self.assertFalse(os.path.exists(named + ".part"))
        self.assertEqual(os.path.getsize(named), 1000)

    def test_the_outcome_writes_frames_as_they_come(self):
        named = os.path.join(self.tmp, "shot.png")
        a, b = socket.socketpair()
        big = os.urandom(300000)

        def send():
            # From its own thread, as the broker's child sends: a frame larger than the socket's buffer (about 200 KB
            # on Linux) is only all sent while the client reads it.
            try:
                b.sendall(b"S\0" + _frame(gui_client.OUTPUT_REQUIRED, named, big) + b"C\0E\0")
            finally:
                b.close()

        sender = threading.Thread(target=send, daemon=True)
        sender.start()
        try:
            self.assertEqual(gui_client._outcome(a, {os.fsencode(named)}), ("exit", 0))
        finally:
            a.close()
            sender.join(30)
        self.assertFalse(sender.is_alive())
        with open(named, "rb") as f:
            self.assertEqual(f.read(), big)

    def test_a_required_file_not_written_fails_the_command(self):
        missing = os.path.join(self.tmp, "no-such-folder", "shot.png")
        for flags, want in ((gui_client.OUTPUT_REQUIRED, ("exit", 1)), (gui_client.OUTPUT_ATOMIC, ("exit", 0))):
            a, b = socket.socketpair()
            b.sendall(b"S\0" + _frame(flags, missing, b"PNG") + b"C\0E\0")
            b.close()
            with mock.patch("os.write", wraps=os.write):
                try:
                    self.assertEqual(gui_client._outcome(a, {os.fsencode(missing)}), want)
                finally:
                    a.close()

    def test_a_frame_too_large_to_follow_is_not_seen(self):
        a, b = socket.socketpair()
        b.sendall(b"S\0O" + (gui_client.MAX_OUTPUT + 1).to_bytes(4, "big"))
        b.close()
        try:
            self.assertEqual(gui_client._outcome(a, set()), ("unseen", None))
        finally:
            a.close()

    def test_deliver_writes_here_outside_a_broker(self):
        direct, atomic = os.path.join(self.tmp, "a.png"), os.path.join(self.tmp, "b.png")
        self.assertIsNone(broker.caller_connection())
        broker.deliver_to_caller(direct, b"one", required=True)
        broker.deliver_to_caller(atomic, b"two", atomic=True)
        with open(direct, "rb") as f:
            self.assertEqual(f.read(), b"one")
        with open(atomic, "rb") as f:
            self.assertEqual(f.read(), b"two")
        self.assertFalse(os.path.exists(atomic + ".part"))

    def test_deliver_hands_the_bytes_back_in_a_brokers_child(self):
        import types

        a, b = socket.socketpair()
        holder = types.ModuleType(broker.CALLER_MODULE)
        holder.conn = a
        named = os.path.join(self.tmp, "c.png")
        with mock.patch.dict(sys.modules, {broker.CALLER_MODULE: holder}):
            broker.deliver_to_caller(named, b"pixels", required=True)
        a.close()
        self.assertFalse(os.path.exists(named), "the screen side wrote the caller's file itself")
        data = b""
        while True:
            chunk = b.recv(65536)
            if not chunk:
                break
            data += chunk
        b.close()
        self.assertEqual(data[:1], b"O")
        flags, path, body = gui_client.unpack(data[5:5 + int.from_bytes(data[1:5], "big")])
        self.assertEqual((int(flags), path, body), (gui_client.OUTPUT_REQUIRED, os.fsencode(named), b"pixels"))


class TheClientNeverRunsItHere(unittest.TestCase):
    """In broker mode a command the broker does not take is an error, whatever DECURIX_GUI_DAEMON says."""

    def test_no_broker_is_an_error_not_an_in_process_run(self):
        tmp = tempfile.mkdtemp(prefix="screen-broker-")
        self.addCleanup(shutil.rmtree, tmp, True)
        env = {k: v for k, v in os.environ.items() if not k.startswith("DECURIX_")}
        for mode in ("", "off", "require"):
            env.update({"DECURIX_SCREEN_USER": "1", "DECURIX_SCREEN_DIR": os.path.join(tmp, "absent"),
                        "DECURIX_SCREEN_UID": "0", "DECURIX_GUI_DAEMON": mode,
                        # Were it run in-process, this would be its Python path.
                        "PYTHONPATH": AGENT})
            done = subprocess.run([sys.executable, "-I", "-S", CLIENT, "observe"], capture_output=True, env=env,
                                  cwd=tmp, timeout=60)
            self.assertEqual(done.returncode, gui_client.UNREACHABLE_EXIT, done.stderr)
            self.assertEqual(done.stdout, b"", "something ran")
            self.assertIn(b"cannot reach the screen", done.stderr)

    def test_why_it_was_not_taken(self):
        self.assertIn("serves another user", gui_client.unreachable("refused", 5))
        self.assertIn("not running", gui_client.unreachable("absent", None))
        self.assertIn("not the screen user's", gui_client.unreachable("untrusted", "the process at x is not the "
                                                                                    "screen user's"))


# ------------------------------------------------------------
# Part 3: the DevTools relay
# ------------------------------------------------------------

class _RelayOnAPipe:
    """The relay over a real pipe to a stand-in browser on a thread; each connection a socket pair served by
    Relay.handle on a thread of its own."""

    def __init__(self, peer_ok=lambda conn: True):
        to_browser_r, to_browser_w = os.pipe()
        from_browser_r, from_browser_w = os.pipe()
        self.fake = StandInBrowser(to_browser_r, from_browser_w)
        self.fake_thread = threading.Thread(target=self.fake.run, daemon=True)
        self.fake_thread.start()
        self.browser = cdp_relay.Browser(to_browser_w, from_browser_r).start()
        self.relay = cdp_relay.Relay(self.browser, peer_ok=peer_ok)
        self.fds = [to_browser_r, to_browser_w, from_browser_r, from_browser_w]

    def connect(self, path=None, timeout=None):
        mine, theirs = socket.socketpair()
        mine.settimeout(timeout)
        threading.Thread(target=self.relay.handle, args=(theirs,), daemon=True).start()
        return mine

    def stop_browser(self):
        if not self.browser.closed.is_set():
            try:
                self.browser.send({"id": 999999, "method": "Browser.close"})
            except OSError:
                pass
        self.browser.closed.wait(10)
        self.fake_thread.join(10)

    def close(self):
        # Both readers end first (the stand-in on Browser.close, the relay on its end of file): a descriptor closed
        # under a thread blocked reading it never returns on Windows.
        self.stop_browser()
        for fd in self.fds:
            try:
                os.close(fd)
            except OSError:
                pass


class TheRelayServesWhatCdpUses(unittest.TestCase):
    """cdp.py's own requests and WebSockets, through the relay, on a stand-in browser."""

    def setUp(self):
        self.relay = _RelayOnAPipe()
        self.addCleanup(self.relay.close)
        patches = [mock.patch.dict(os.environ, {"DECURIX_SCREEN_USER": "1",
                                                "DECURIX_CDP_RELAY_SOCKET": "/run/decurix-screen/cdp/cdp.sock"}),
                   mock.patch.object(cdp, "_relay_connect", self.relay.connect)]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)

    def test_cdp_finds_the_relay_in_broker_mode_only(self):
        self.assertEqual(broker.relay_socket(), "/run/decurix-screen/cdp/cdp.sock")
        with mock.patch.dict(os.environ, {"DECURIX_SCREEN_USER": ""}):
            self.assertIsNone(broker.relay_socket())

    def test_version_list_and_new(self):
        self.assertTrue(cdp.available())
        version = cdp._get("/json/version")
        self.assertEqual(version["Browser"], "StandIn/1.0")
        self.assertNotIn("webSocketDebuggerUrl", version, "no browser-wide WebSocket is offered")
        pages = cdp._page_targets()
        self.assertEqual([t["id"] for t in pages], ["p1", "p2"], "pages only, each with its WebSocket")
        self.assertTrue(all(t["webSocketDebuggerUrl"].endswith("/devtools/page/" + t["id"]) for t in pages))
        made = cdp._get("/json/new?" + "https%3A%2F%2Fthree.example%2F%3Fa%3D1", method="PUT")
        self.assertEqual((made["type"], made["url"]), ("page", "https://three.example/?a=1"))
        self.assertIn(made["id"], [t["id"] for t in cdp._page_targets()])

    def test_a_page_session_is_that_page_only(self):
        page = cdp.Page(cdp._page_targets()[1])
        try:
            self.assertEqual(page.evaluate("document.title"), "p2:document.title")
            # A message naming another session goes on this page's session all the same.
            page.ws.send(json.dumps({"id": 77, "method": "Runtime.evaluate", "sessionId": "S-somebody-else",
                                     "params": {"expression": "x"}}))
            answer = json.loads(page.ws.receive())
            self.assertEqual(answer, {"id": 77, "result": {"result": {"value": "p2:x"}}})
            sent = [m for m in self.relay.fake.received if m.get("id") == 77]
            self.assertEqual(len(sent), 1)
            self.assertTrue(sent[0]["sessionId"].endswith("-p2"), sent[0])
            # Events of the session come back, without its session id.
            page.call("Page.enable")
            self.assertEqual(page.wait_event("Page.loadEventFired", 5), {"timestamp": 1})
        finally:
            page.close()

    def test_two_pages_at_once_keep_their_own_answers(self):
        one, two = (cdp.Page(t) for t in cdp._page_targets())
        try:
            for i in range(20):
                self.assertEqual(one.evaluate(f"a{i}"), f"p1:a{i}")
                self.assertEqual(two.evaluate(f"b{i}"), f"p2:b{i}")
        finally:
            one.close()
            two.close()

    def test_a_closed_tab_closes_its_websocket(self):
        page = cdp.Page(cdp._page_targets()[0])
        try:
            # (The relay answers text there, as Chrome does, not JSON: asked raw.)
            conn = self.relay.connect()
            conn.sendall(b"GET /json/close/p1 HTTP/1.1\r\nHost: x\r\n\r\n")
            self.assertIn(b"200 OK", _read_all(conn))
            with self.assertRaises(cdp.ComputerError):
                page.call("Runtime.evaluate", {"expression": "1"}, timeout=5)
        finally:
            page.close()

    def test_what_the_relay_refuses(self):
        for request, status in ((b"GET /json/new?about:blank HTTP/1.1\r\n\r\n", b"405"),
                                (b"GET /json/protocol HTTP/1.1\r\n\r\n", b"404"),
                                (b"GET /devtools/browser/x HTTP/1.1\r\nUpgrade: websocket\r\n"
                                 b"Sec-WebSocket-Key: a2V5a2V5a2V5a2V5a2V5aw==\r\n\r\n", b"404"),
                                (b"GET /devtools/page/w1 HTTP/1.1\r\nUpgrade: websocket\r\n"
                                 b"Sec-WebSocket-Key: a2V5a2V5a2V5a2V5a2V5aw==\r\n\r\n", b"404"),
                                (b"GET /json/activate/nope HTTP/1.1\r\n\r\n", b"404")):
            conn = self.relay.connect()
            conn.sendall(request)
            self.assertIn(b" " + status + b" ", _read_all(conn).split(b"\r\n", 1)[0], request)

    def test_a_browser_gone_is_a_browser_not_running(self):
        self.relay.stop_browser()
        self.assertFalse(cdp.available())
        with self.assertRaises(ConnectionRefusedError):
            cdp._get("/json/list")

    def test_a_caller_of_another_user_gets_nothing(self):
        refusing = _RelayOnAPipe(peer_ok=lambda conn: False)
        self.addCleanup(refusing.close)
        conn = refusing.connect()
        try:
            conn.sendall(b"GET /json/version HTTP/1.1\r\n\r\n")
        except (BrokenPipeError, ConnectionResetError):
            pass        # refused before the request was sent (the relay closes at once): nothing, all the same
        self.assertEqual(_read_all(conn), b"")

    def test_frames_both_ways(self):
        for size in (0, 5, 125, 126, 65535, 65536, 70000):
            payload = os.urandom(size)
            mask = os.urandom(4)
            frame = cdp_relay.ws_frame(0x2, b"")[:1]
            n = len(payload)
            head = frame + (bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + n.to_bytes(2, "big")
                            if n < 65536 else bytes([0x80 | 127]) + n.to_bytes(8, "big"))
            a, b = socket.socketpair()
            a.sendall(head + mask + cdp_relay.unmask(payload, mask))
            a.close()
            self.assertEqual(cdp_relay.read_message(cdp_relay._Reader(b)), (0x2, payload))
            b.close()
        self.assertEqual(cdp_relay.accept_key("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=")


def _read_all(conn):
    data = b""
    conn.settimeout(10)
    while True:
        try:
            chunk = conn.recv(65536)
        except OSError:
            break
        if not chunk:
            break
        data += chunk
    conn.close()
    return data


# ------------------------------------------------------------
# Part 4: the worker's launch helper, and what the screen side starts itself
# ------------------------------------------------------------

class TheLaunchHelper(unittest.TestCase):
    """The helper takes a program and its arguments from the broker's uid only, and starts it with its own (the
    worker's) environment."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="launch-helper-")
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def ask(self, message, peer_uid, allowed_uid=500, start=None):
        started = []
        a, b = socket.socketpair()
        a.sendall((json.dumps(message) + "\n").encode())
        try:
            result = launch_helper.handle(b, allowed_uid, peer=lambda conn: peer_uid,
                                          start=start or (lambda argv: started.append(argv) or 4321))
            a.settimeout(10)
            answer = json.loads(launch_helper._read_line(a).decode())
        finally:
            a.close()
            b.close()
        return result, answer, started

    def test_another_caller_is_refused(self):
        for uid in (501, 0, None):
            result, answer, started = self.ask({"argv": ["xterm"]}, peer_uid=uid)
            self.assertFalse(result)
            self.assertEqual(started, [])
            self.assertIn("screen broker only", answer["error"])

    def test_the_broker_gets_its_program_started(self):
        result, answer, started = self.ask({"argv": ["mousepad", "/home/headless/notes.txt"],
                                            "env": {"LD_PRELOAD": "x"}, "cwd": "/"}, peer_uid=500)
        self.assertTrue(result)
        self.assertEqual(answer, {"pid": 4321})
        self.assertEqual(started, [["mousepad", "/home/headless/notes.txt"]], "only the program and its arguments")

    def test_a_request_that_is_not_a_program_is_refused(self):
        for message in ({"argv": []}, {"argv": "xterm"}, {"argv": ["x", 3]}, {"argv": ["a\0b"]}, {"program": "x"},
                        ["xterm"]):
            result, answer, started = self.ask(message, peer_uid=500)
            self.assertFalse(result, message)
            self.assertEqual(started, [])
            self.assertIn("not a request", answer["error"])

    def test_the_program_runs_with_the_workers_own_environment(self):
        out = os.path.join(self.tmp, "env.json")
        program = [sys.executable, "-c", "import json, os, sys; json.dump(dict(os.environ), open(sys.argv[1], 'w'))",
                   out]
        with mock.patch.dict(os.environ, {"WORKER_OWN_SETTING": "the worker's"}), \
                mock.patch.object(session, "_session_dbus_address", lambda: None):
            result, answer, _ = self.ask({"argv": program}, peer_uid=500, start=launch_helper.start_program)
        self.assertTrue(result, answer)
        deadline = time.time() + 30
        while not os.path.exists(out) and time.time() < deadline:
            time.sleep(0.05)
        time.sleep(0.2)
        with open(out) as f:
            env = json.load(f)
        self.assertEqual(env.get("WORKER_OWN_SETTING"), "the worker's")
        self.assertEqual(env.get("NO_AT_BRIDGE"), "0", "the accessibility settings every app gets")

    def test_a_program_that_cannot_start_is_said(self):
        with mock.patch.object(session, "_session_dbus_address", lambda: None):
            result, answer, _ = self.ask({"argv": [os.path.join(self.tmp, "no-such-program")]}, peer_uid=500,
                                         start=launch_helper.start_program)
        self.assertFalse(result)
        self.assertIn("could not start", answer["error"])


class WhatTheScreenSideStarts(unittest.TestCase):
    """On the screen side the browser is the only program started as the screen user; anything else goes to the
    worker's helper; a browser start that names a program, folder, extension or port is refused."""

    def setUp(self):
        self.popen = mock.patch.object(session.subprocess, "Popen", return_value=mock.Mock(pid=11)).start()
        self.helper = mock.patch.object(launch_helper, "request", return_value=22).start()
        self.side = mock.patch.object(broker, "screen_side", return_value=True).start()
        mock.patch.object(session, "_session_dbus_address", lambda: None).start()
        self.addCleanup(mock.patch.stopall)

    def test_another_program_goes_to_the_workers_helper(self):
        started = session.launch(["mousepad", "/home/headless/a.txt"])
        self.assertEqual(started.pid, 22)
        self.assertIsNone(started.poll())
        self.helper.assert_called_once_with(["mousepad", "/home/headless/a.txt"])
        self.popen.assert_not_called()

    def test_the_browser_starts_with_its_relay(self):
        session.launch(["chromium-browser", "https://example.com/", "--new-window"])
        argv = self.popen.call_args.args[0]
        self.assertEqual(argv, [sys.executable, "-m", "decurix_agent.computer.cdp_relay", "--", "chromium-browser",
                                "https://example.com/", "--new-window"])
        self.helper.assert_not_called()

    def test_a_browser_start_that_is_more_than_pages_is_refused(self):
        for argv in (["chromium-browser", "--renderer-cmd-prefix=xterm"], ["google-chrome", "--remote-debugging-port=1"],
                     ["chromium-browser", "--user-data-dir=/home/headless/p"], ["chromium-browser", "--app=javascript:1"],
                     ["/usr/bin/chromium-browser", "--load-extension=/home/headless/x"]):
            with self.assertRaises(session.RefusedLaunch):
                session.launch(argv)
        self.popen.assert_not_called()
        self.helper.assert_not_called()
        self.assertTrue(issubclass(session.RefusedLaunch, cdp.ComputerError))

    def test_the_default_mode_is_unchanged(self):
        self.side.return_value = False
        session.launch(["mousepad", "x"])
        self.assertEqual(self.popen.call_args.args[0], ["mousepad", "x"])
        self.helper.assert_not_called()


# ------------------------------------------------------------
# With fork and Unix sockets: each part as its own process
# ------------------------------------------------------------

@unittest.skipUnless(POSIX, SKIP)
class TheBrokerEndToEnd(unittest.TestCase):
    """The broker daemon on the stand-in screen, reached by the real client as `gui` runs it."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="screen-broker-")
        cls.dir = os.path.join(cls.tmp, "run")
        os.mkdir(cls.dir)
        os.chmod(cls.dir, 0o711)
        cls.home = os.path.join(cls.tmp, "home")
        cls.work = os.path.join(cls.tmp, "work")
        os.makedirs(cls.home)
        os.makedirs(cls.work)
        with open(os.path.join(cls.work, "broker_probe_module.py"), "w") as f:
            f.write("print('the caller planted this')\n")
        env = {k: v for k, v in os.environ.items() if not k.startswith(("DECURIX_", "STAND_IN_"))}
        env.update({"PYTHONPATH": AGENT, "DECURIX_DIR": cls.home, "DECURIX_LOG_DIR": cls.tmp, "TMPDIR": cls.tmp,
                    "DECURIX_TASK_FILE": os.path.join(cls.tmp, "task"), "CONTROL_PLANE_URL": "http://127.0.0.1:9",
                    "DECURIX_SCREEN_USER": "1", "DECURIX_SCREEN_DIR": cls.dir,
                    "DECURIX_SCREEN_UID": str(os.getuid()), "DECURIX_SCREEN_WORKER_UID": str(os.getuid())})
        cls.env = env
        cls.daemons = []

    @classmethod
    def tearDownClass(cls):
        for proc in cls.daemons:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def start(self, worker_uid):
        sock = os.path.join(self.dir, "gui.sock")
        if os.path.exists(sock):
            os.unlink(sock)
        log = open(os.path.join(self.tmp, "broker.log"), "ab")
        self.addCleanup(log.close)
        proc = subprocess.Popen([sys.executable, STAND_IN, "broker"], cwd="/", stdin=subprocess.DEVNULL, stdout=log,
                                stderr=log, env=dict(self.env, STAND_IN_BROKER_DIR=self.dir,
                                                     STAND_IN_WORKER_UID=str(worker_uid)))
        self.daemons.append(proc)
        self.addCleanup(lambda: (proc.kill(), proc.wait()) if proc.poll() is None else None)
        deadline = time.time() + 60
        while not os.path.exists(sock):
            if proc.poll() is not None or time.time() > deadline:
                with open(os.path.join(self.tmp, "broker.log"), "rb") as f:
                    raise AssertionError("the broker did not start: " + f.read().decode("utf-8", "replace"))
            time.sleep(0.05)
        mode = os.stat(sock).st_mode & 0o777
        self.assertEqual(mode, 0o666, "the broker's socket is open to every user; its callers are checked by uid")
        return proc

    def gui(self, *args, env=None):
        done = subprocess.run([sys.executable, "-I", "-S", CLIENT] + list(args), capture_output=True,
                              env=dict(self.env, **(env or {})), cwd=self.work, timeout=120)
        return done.returncode, done.stdout, done.stderr

    def test_a_command_runs_and_its_screenshot_is_the_callers(self):
        self.start(os.getuid())
        code, out, err = self.gui("observe", "--screenshot", "shot.png")
        self.assertEqual(code, 0, err)
        self.assertIn(b"Screenshot saved to shot.png", out)
        with open(os.path.join(self.work, "shot.png"), "rb") as f:
            self.assertEqual(f.read(8), b"\x89PNG\r\n\x1a\n")
        self.assertEqual(os.stat(os.path.join(self.work, "shot.png")).st_uid, os.getuid())

    def test_the_child_has_the_screen_sides_environment(self):
        self.start(os.getuid())
        code, out, err = self.gui("env-of-child", "PYTHONPATH", "LD_LIBRARY_PATH", "DECURIX_DIR",
                                  "DECURIX_GUI_TEXT_CHARS", "HOME",
                                  env={"PYTHONPATH": "/nonexistent/evil", "LD_LIBRARY_PATH": "/nonexistent/lib",
                                       "DECURIX_DIR": "/nonexistent/state", "DECURIX_GUI_TEXT_CHARS": "1234",
                                       "HOME": "/nonexistent/home"})
        self.assertEqual(code, 0, err)
        seen = json.loads(out.decode())
        self.assertEqual(seen, {"PYTHONPATH": AGENT, "LD_LIBRARY_PATH": self.env.get("LD_LIBRARY_PATH"),
                                "DECURIX_DIR": self.home, "DECURIX_GUI_TEXT_CHARS": "1234",
                                "HOME": self.env.get("HOME")})

    def test_the_callers_folder_is_never_on_the_path(self):
        self.start(os.getuid())
        code, out, err = self.gui("import-probe")
        self.assertEqual((code, out), (0, b"not imported\n"), err)

    def test_another_uid_is_refused_and_nothing_runs_here(self):
        self.start(os.getuid() + 1)
        code, out, err = self.gui("observe", env={"DECURIX_GUI_DAEMON": "off"})
        self.assertEqual(code, gui_client.UNREACHABLE_EXIT)
        self.assertEqual(out, b"")
        self.assertIn(b"serves another user", err)

    def test_a_broker_that_is_not_the_screen_users_is_not_trusted(self):
        self.start(os.getuid())
        code, out, err = self.gui("observe", env={"DECURIX_SCREEN_UID": str(os.getuid() + 1)})
        self.assertEqual((code, out), (gui_client.UNREACHABLE_EXIT, b""))
        self.assertIn(b"cannot reach the screen", err)

    def test_a_daemon_in_the_workers_user_does_not_serve_broker_mode(self):
        env = dict(self.env, DECURIX_SCREEN_WORKER_UID=str(os.getuid()))
        done = subprocess.run([sys.executable, "-m", "decurix_agent.computer.daemon", "--no-gi"], env=env,
                              capture_output=True, timeout=120, cwd="/")
        self.assertEqual(done.returncode, daemon.EXIT_DO_NOT_RESTART, done.stderr)
        self.assertIn(b"screen broker", done.stderr)


@unittest.skipUnless(POSIX, SKIP)
class TheRelayAsItsOwnProcess(unittest.TestCase):
    """The relay starts the stand-in browser with the pipe on its fds 3 and 4, serves cdp.py on a socket only this
    user may reach, hands a second start's page to the browser it serves, and ends with the browser."""

    def test_it_serves_and_ends_with_the_browser(self):
        tmp = tempfile.mkdtemp(prefix="cdp-relay-")
        self.addCleanup(shutil.rmtree, tmp, True)
        sock = os.path.join(tmp, "cdp", "cdp.sock")
        env = dict(os.environ, PYTHONPATH=AGENT)
        env.pop("DECURIX_BROWSER_DEBUG", None)
        start = [sys.executable, "-m", "decurix_agent.computer.cdp_relay", "--socket", sock, "--",
                 sys.executable, STAND_IN, "browser"]
        relay = subprocess.Popen(start + ["--no-startup-window"], env=env, stdin=subprocess.DEVNULL,
                                 start_new_session=True)
        self.addCleanup(lambda: relay.poll() is None and (os.killpg(relay.pid, 9), relay.wait()))
        deadline = time.time() + 30
        while not os.path.exists(sock) and time.time() < deadline:
            time.sleep(0.05)
        self.assertEqual(os.stat(os.path.dirname(sock)).st_mode & 0o777, 0o700)
        self.assertEqual(os.stat(sock).st_mode & 0o777, 0o600)
        with mock.patch.dict(os.environ, {"DECURIX_SCREEN_USER": "1", "DECURIX_CDP_RELAY_SOCKET": sock}):
            self.assertTrue(cdp.available())
            # The launcher is told to give the browser the pipe, and the browser had fds 3 and 4.
            self.assertEqual(cdp._get("/json/version")["Browser"], "StandIn/1.0 debug=pipe")
            page = cdp.Page(cdp._page_targets()[0])
            try:
                self.assertEqual(page.evaluate("1+1"), "p1:1+1")
            finally:
                page.close()
            # A second start while it serves hands its page over and leaves; no second browser.
            again = subprocess.run(start + ["https://four.example/"], env=env, timeout=60, capture_output=True)
            self.assertEqual(again.returncode, 0, again.stderr)
            pages = cdp._page_targets()
            self.assertIn("https://four.example/", [t["url"] for t in pages])
            # The last page closed, the browser ends (the stand-in as Chrome does); so does the relay, and its socket.
            for target in pages:
                conn = cdp._relay_connect(sock, 5)
                conn.sendall(("GET /json/close/%s HTTP/1.1\r\n\r\n" % target["id"]).encode())
                _read_all(conn)
        self.assertEqual(relay.wait(30), 0)
        self.assertFalse(os.path.exists(sock))


@unittest.skipUnless(POSIX, SKIP)
class TheLaunchHelperOnItsSocket(unittest.TestCase):
    """The helper on its own socket; the screen side's request() checks it is the worker's."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="launch-helper-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.dir = os.path.join(self.tmp, "launch")
        env = {"DECURIX_LAUNCH_DIR": self.dir, "DECURIX_SCREEN_WORKER_UID": str(os.getuid())}
        p = mock.patch.dict(os.environ, env)
        p.start()
        self.addCleanup(p.stop)

    def serve(self, allowed_uid):
        listener = launch_helper.bind(self.dir)
        self.assertIsNotNone(listener)
        self.assertEqual(os.stat(self.dir).st_mode & 0o777, 0o711)
        self.assertEqual(os.stat(launch_helper.socket_path(self.dir)).st_mode & 0o777, 0o666)
        threading.Thread(target=launch_helper.serve, args=(listener, allowed_uid), daemon=True).start()
        self.addCleanup(listener.close)

    def test_the_broker_has_a_program_started(self):
        self.serve(os.getuid())
        out = os.path.join(self.tmp, "ran")
        with mock.patch.object(session, "_session_dbus_address", lambda: None):
            pid = launch_helper.request([sys.executable, "-c", "import sys; open(sys.argv[1], 'w').write('x')", out])
        self.assertIsInstance(pid, int)
        deadline = time.time() + 30
        while not os.path.exists(out) and time.time() < deadline:
            time.sleep(0.05)
        self.assertTrue(os.path.exists(out))

    def test_a_caller_that_is_not_the_broker_is_refused(self):
        self.serve(os.getuid() + 1)
        with self.assertRaises(OSError) as raised:
            launch_helper.request(["true"])
        self.assertIn("screen broker only", str(raised.exception))

    def test_a_helper_that_is_not_the_workers_gets_nothing(self):
        self.serve(os.getuid())
        with mock.patch.dict(os.environ, {"DECURIX_SCREEN_WORKER_UID": str(os.getuid() + 1)}):
            with self.assertRaises(OSError) as raised:
                launch_helper.request(["true"])
        self.assertIn("not the worker's", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
