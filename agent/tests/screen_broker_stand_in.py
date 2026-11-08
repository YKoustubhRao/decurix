"""
Stand-ins for test_screen_broker: a browser that speaks DevTools over --remote-debugging-pipe, and the screen broker
on the stand-in screen of gui_daemon_stand_in.py. Never a real browser, X display or accessibility bus.

    python3 screen_broker_stand_in.py browser    a browser on its fds 3 (commands in) and 4 (answers out), as the
                                                 DevTools relay starts one
    python3 screen_broker_stand_in.py broker     the gui daemon in broker mode, serving STAND_IN_BROKER_DIR for the
                                                 uid STAND_IN_WORKER_UID, its children on the stand-in screen

The stand-in browser (StandInBrowser, also run on a thread by the tests) holds pages p1 and p2 and a service worker.
It answers the browser-wide calls the relay makes, and on a page session: Runtime.evaluate -> "<target>:<expression>",
Page.enable -> the event Page.loadEventFired on that session. Every message it got is kept, so a test can see which
session a message came on. Browser.close ends it; closing a target detaches its sessions, and (as its own
process) closing the last page ends it, as Chrome ends with its last window.

The broker's children also know two commands of their own (only here): `env-of-child NAME...` prints those variables
as the child has them, and `import-probe` says whether a module named broker_probe_module could be imported.

Not a test module (no test_ prefix): unittest discovery never imports it.
"""

import json
import os
import sys
import threading


class StandInBrowser:
    """A browser's DevTools on a pipe: NUL-separated JSON in on `read_fd`, out on `write_fd`."""

    def __init__(self, read_fd, write_fd, product="StandIn/1.0", exit_with_last_page=False):
        self.read_fd, self.write_fd, self.product = read_fd, write_fd, product
        self.exit_with_last_page = exit_with_last_page
        self.targets = {"p1": {"targetId": "p1", "type": "page", "title": "One", "url": "https://one.example/"},
                        "p2": {"targetId": "p2", "type": "page", "title": "Two", "url": "https://two.example/"},
                        "w1": {"targetId": "w1", "type": "service_worker", "title": "sw", "url": "https://one.example/sw"}}
        self.sessions = {}      # sessionId -> targetId
        self.received = []
        self.lock = threading.Lock()
        self.count = 0

    def write(self, message):
        data = json.dumps(message).encode("utf-8") + b"\0"
        with self.lock:
            view = memoryview(data)
            while view:
                view = view[os.write(self.write_fd, view):]

    def run(self):
        buf = b""
        while True:
            chunk = os.read(self.read_fd, 65536)
            if not chunk:
                break
            buf += chunk
            while b"\0" in buf:
                raw, buf = buf.split(b"\0", 1)
                message = json.loads(raw.decode("utf-8"))
                self.received.append(message)
                if self.answer(message) == "close":
                    os.close(self.write_fd)
                    return

    def answer(self, message):
        method, params, mid = message.get("method"), message.get("params") or {}, message.get("id")
        sid = message.get("sessionId")
        if sid is not None:
            target = self.sessions.get(sid)
            if target is None:
                return self.write({"id": mid, "sessionId": sid, "error": {"message": "No session with given id"}})
            if method == "Runtime.evaluate":
                return self.write({"id": mid, "sessionId": sid,
                                   "result": {"result": {"value": f"{target}:{params.get('expression')}"}}})
            if method == "Page.enable":
                self.write({"id": mid, "sessionId": sid, "result": {}})
                return self.write({"method": "Page.loadEventFired", "sessionId": sid, "params": {"timestamp": 1}})
            return self.write({"id": mid, "sessionId": sid, "result": {}})
        if method == "Browser.close":
            self.write({"id": mid, "result": {}})
            return "close"
        if method == "Browser.getVersion":
            # What the launcher was told (DECURIX_BROWSER_DEBUG), so a test sees the relay asked for the pipe.
            debug = os.environ.get("DECURIX_BROWSER_DEBUG", "")
            return self.write({"id": mid, "result": {
                "product": self.product + (f" debug={debug}" if debug else ""), "protocolVersion": "1.3",
                "userAgent": "StandIn", "jsVersion": "1", "revision": "@1"}})
        if method == "Target.getTargets":
            return self.write({"id": mid, "result": {"targetInfos": list(self.targets.values())}})
        if method == "Target.getTargetInfo":
            info = self.targets.get(params.get("targetId"))
            if info is None:
                return self.write({"id": mid, "error": {"message": "No target with given id found"}})
            return self.write({"id": mid, "result": {"targetInfo": info}})
        if method == "Target.createTarget":
            self.count += 1
            tid = f"n{self.count}"
            self.targets[tid] = {"targetId": tid, "type": "page", "title": params.get("url"), "url": params.get("url")}
            return self.write({"id": mid, "result": {"targetId": tid}})
        if method == "Target.attachToTarget":
            tid = params.get("targetId")
            if tid not in self.targets or params.get("flatten") is not True:
                return self.write({"id": mid, "error": {"message": "cannot attach"}})
            self.count += 1
            sid = f"S{self.count}-{tid}"
            self.sessions[sid] = tid
            return self.write({"id": mid, "result": {"sessionId": sid}})
        if method == "Target.detachFromTarget":
            self.sessions.pop(params.get("sessionId"), None)
            return self.write({"id": mid, "result": {}})
        if method in ("Target.activateTarget", "Target.closeTarget"):
            tid = params.get("targetId")
            if tid not in self.targets:
                return self.write({"id": mid, "error": {"message": "No target with given id found"}})
            self.write({"id": mid, "result": {"success": True} if method == "Target.closeTarget" else {}})
            if method == "Target.closeTarget":
                del self.targets[tid]
                for sid in [s for s, t in self.sessions.items() if t == tid]:
                    del self.sessions[sid]
                    self.write({"method": "Target.detachedFromTarget", "params": {"sessionId": sid, "targetId": tid}})
                if self.exit_with_last_page and not any(t["type"] == "page" for t in self.targets.values()):
                    # As Chrome does when its last window closes.
                    return "close"
            return None
        return self.write({"id": mid, "error": {"message": f"'{method}' wasn't found"}})


def install_probes():
    """The broker's children's two extra commands (see the module's docstring)."""

    from decurix_agent.computer import cli

    real_run = cli.run

    def run(argv):
        if argv[:1] == ["env-of-child"]:
            print(json.dumps({name: os.environ.get(name) for name in argv[1:]}, sort_keys=True))
            return 0
        if argv[:1] == ["import-probe"]:
            try:
                import broker_probe_module  # noqa: F401
                print("imported")
            except ImportError:
                print("not imported")
            return 0
        return real_run(argv)

    cli.run = run


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    sys.path.insert(0, os.path.join(here, ".."))
    mode = sys.argv[1]
    if mode == "browser":
        # The relay's pipe: fd 3 is what the browser reads, fd 4 what it writes.
        os.fstat(3)
        os.fstat(4)
        StandInBrowser(3, 4, exit_with_last_page=True).run()
        return 0
    if mode == "broker":
        import gui_daemon_stand_in
        from decurix_agent.computer import daemon

        daemon.preload(screen=False)
        gui_daemon_stand_in.install()
        install_probes()
        server = daemon.Server(os.environ["STAND_IN_BROKER_DIR"],
                               daemon.Broker.for_package(int(os.environ["STAND_IN_WORKER_UID"])))
        if not server.bind():
            return 3
        server.serve()
        return 0
    raise SystemExit(f"unknown mode {mode}")


if __name__ == "__main__":
    sys.exit(main())
