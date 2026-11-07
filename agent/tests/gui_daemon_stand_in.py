"""
Stand-ins for the screen, for test_gui_daemon: the same `gui` commands run in-process and through the gui daemon on
one fixed screen, with no X display, accessibility bus or browser.

    python3 gui_daemon_stand_in.py gui ARGS...     `python3 -m decurix_agent.computer ARGS` on the stand-in screen
    python3 gui_daemon_stand_in.py daemon          the gui daemon, its children on the stand-in screen

The screen holds a page with a Send button (e9) and a Notes field (e10). Each look is counted in this process, and
the count is in the page's heading: a fresh `gui` process says "Look 1" on its first look, and so must every command
the daemon runs, or one command's memory reached the next. STAND_IN_SLOW=N makes a look take N seconds and, when
STAND_IN_MARK names a file, write it once the look is over (a command killed mid-look never writes it).

Not a test module (no test_ prefix): unittest discovery never imports it.
"""

import os
import sys
import time

_looks = [0]


def _observation():
    _looks[0] += 1
    slow = float(os.environ.get("STAND_IN_SLOW", "0") or 0)
    if slow:
        time.sleep(slow)
        mark = os.environ.get("STAND_IN_MARK")
        if mark:
            with open(mark, "w") as f:
                f.write("looked")

    def element(ref, role, name, **extra):
        base = {"ref": ref, "role": role, "name": name, "value": "", "states": [], "box": [10, 10, 80, 24],
                "path": f"site x > {role} \"{name}\"", "source": "cdp", "group": "cdp:1",
                "window_title": "Mail - Chromium"}
        base.update(extra)
        return base

    elements = [element("e8", "heading", f"Look {_looks[0]}")]
    elements.append(element("e9", "button", "Send"))
    elements.append(element("e10", "textbox", "Notes", value="draft " * 40))
    elements += [element(f"e{11 + i}", "link", f"Folder number {i} with a long name") for i in range(60)]
    return {"elements": elements, "windows": [{"title": "Mail - Chromium", "active": True}],
            "groups": [{"id": "cdp:1", "title": 'Chromium tab "Mail" https://mail.example.com/inbox',
                        "window_title": "Mail - Chromium"}], "frame": _Frame(), "page_url": None, "size": (1280, 800),
            "active_title": "Mail - Chromium"}


def _Frame():
    """A small all-black screenshot."""

    from decurix_agent.computer import screen

    return screen.Frame(64, 40, bytes(64 * 40 * 3))


def install():
    """Put the stand-in screen in place of the real one."""

    from decurix_agent.computer import approval, cli, core, grounder

    if os.environ.get("STAND_IN_GATE"):
        # The approval gate's socket: a fake gate the test serves (the real one is /run/decurix-gate, root-made).
        approval.GATE_SOCKET = os.environ["STAND_IN_GATE"]
    core.observe = lambda *a, **k: _observation()
    core.report_observation = lambda observation: None
    grounder.available = lambda: False
    cli.save_screenshot = lambda argv: None

    real_act = core.act

    def act(kind, *args, **kwargs):
        if kind == "type":
            # What was typed, said back: how a test sees stdin reach the command.
            return {"status": "done", "text": f"Typed {kwargs.get('text')!r}."}
        return real_act(kind, *args, **kwargs)

    core.act = act

    def perform(*args, **kwargs):
        raise core.ComputerError("the stand-in screen does not press anything")

    core._perform = perform


def main():
    """See the module's docstring."""

    sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    mode, rest = sys.argv[1], sys.argv[2:]
    if mode == "daemon":
        from decurix_agent.computer import daemon
        daemon.preload(screen=False)
        install()
        sys.argv = [sys.argv[0]] + rest
        sys.exit(daemon.main(["--no-gi"]))
    install()
    import runpy
    sys.argv = [sys.argv[0]] + rest
    runpy.run_module("decurix_agent.computer", run_name="__main__", alter_sys=True)


if __name__ == "__main__":
    main()
