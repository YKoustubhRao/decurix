"""
Record, teach and run a task on a page that keeps its app in an inner frame - end to end, as a person.

The task is the one this was found on (2026-10-03): "open LinkedIn and send <message> to <person>".
LinkedIn's Messaging page is a frame inside the tab's own page, and a worker that read only the tab's
own document saw two elements, typed into nothing and clicked at guessed coordinates. msgsite.py is a
site of the same shape with no account behind it; its own record of what was sent is what is checked.

    python bench/native/messaging_e2e.py --base http://127.0.0.1:8180 --desktop msg1

What it does, through the real viewer (screen.html + noVNC in headless Chrome - what a person uses):
  1. a person opens a ticket, presses Record, goes to Messaging, searches a name, opens the first
     conversation, writes a message, presses Send, presses End;
  2. the recording must name a control for every step (no typing "into the screen", no bare points);
  3. /learn, answered as a person answers it; only Send may be marked as needing approval;
  4. the task asked for in plain words with another person and message: one approval (Send), and the
     site must have exactly that message to exactly that person;
  5. the same with "no" at the approval: nothing is sent and the worker does not try again.

Needs: a control plane on this computer with a hosted brain set up, a running native worker (its
files are read from its home folder), `pip install playwright` (Chrome itself is used:
channel="chrome"). Takes about six minutes.
"""

import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent


class ControlPlane:
    def __init__(self, base, desk):
        self.base, self.desk = base.rstrip("/"), desk

    def api(self, path, data=None, timeout=180):
        body = urllib.parse.urlencode(data).encode() if data is not None else None
        try:
            with urllib.request.urlopen(urllib.request.Request(self.base + path, data=body), timeout=timeout) as resp:
                text, status = resp.read().decode("utf-8", "replace"), resp.status
        except urllib.error.HTTPError as exc:
            text, status = exc.read().decode("utf-8", "replace"), exc.code
        try:
            return status, json.loads(text)
        except ValueError:
            return status, text

    def thread(self):
        _, data = self.api(f"/api/desktops/chat?id={self.desk}")
        return data.get("thread") or data

    def idle(self, seconds=120):
        deadline = time.time() + seconds
        while time.time() < deadline and self.thread().get("running"):
            time.sleep(1)

    def send(self, text, timeout=600):
        """Send a request (or a reply) and wait for the agent's next entry."""

        before = len(self.thread().get("entries", []))
        status, answer = self.api("/api/desktops/send", {"id": self.desk, "content": text})
        if status >= 300:
            raise SystemExit(f"could not send {text!r}: {status} {answer}")
        deadline = time.time() + timeout
        while time.time() < deadline:
            new = [e for e in self.thread().get("entries", [])[before:] if e.get("role") == "agent"]
            if new:
                return new[-1]
            time.sleep(0.5)
        raise SystemExit(f"no answer to {text!r} in {timeout} s")

    def home(self):
        """The worker's home folder on this computer (a native worker's "volume")."""

        _, desktops = self.api("/api/desktops")
        return Path(next(d["volume"] for d in desktops if d["id"] == self.desk))

    def elements(self):
        """What the worker lists on its screen now (its own `gui observe`): name -> the middle of its box."""

        self.idle()
        self.send("run the command: `gui observe`", timeout=180)
        look = json.loads((self.home() / ".decurix" / "computer" / "observation.json").read_text(encoding="utf-8"))
        return {e["name"]: (e["box"][0] + e["box"][2] // 2, e["box"][1] + e["box"][3] // 2)
                for e in look["elements"] if e.get("box")}

    def recording(self, session):
        _, data = self.api(f"/api/desktops/recordings/get?id={self.desk}&session={session}")
        return data if isinstance(data, dict) else {}


class Person:
    """A person at the worker's screen, through the viewer."""

    def __init__(self, cp):
        from playwright.sync_api import sync_playwright

        self.cp = cp
        self._pw = sync_playwright().start()
        self.browser = self._pw.chromium.launch(channel="chrome", headless=True)
        self.pg = self.browser.new_page(viewport={"width": 1600, "height": 960})
        self.pg.goto(f"{cp.base}/desktop/{cp.desk}/")
        self.pg.wait_for_function("() => document.getElementById('screen').classList.contains('live')", timeout=120000)
        self.pg.wait_for_timeout(1500)

    def close(self):
        self.browser.close()
        self._pw.stop()

    def at(self, x, y):
        self.pg.wait_for_function("() => document.querySelector('#screen canvas') || document.querySelector('canvas')",
                                  timeout=60000)
        c = self.pg.evaluate("""() => {
            const c = document.querySelector('#screen canvas') || document.querySelector('canvas');
            const r = c.getBoundingClientRect();
            return {x: r.left, y: r.top, w: r.width, h: r.height, cw: c.width, ch: c.height};
        }""")
        return c["x"] + x * c["w"] / c["cw"], c["y"] + y * c["h"] / c["ch"]

    def click(self, x, y, wait=1.5):
        px, py = self.at(x, y)
        self.pg.mouse.move(px, py)
        self.pg.wait_for_timeout(120)
        self.pg.mouse.click(px, py)
        self.pg.wait_for_timeout(int(wait * 1000))

    def type(self, text, wait=0.8):
        self.pg.keyboard.type(text, delay=45)
        self.pg.wait_for_timeout(int(wait * 1000))

    def key(self, name, wait=0.8):
        self.pg.keyboard.press(name)
        self.pg.wait_for_timeout(int(wait * 1000))

    def record(self, task):
        status, answer = self.cp.api("/api/desktops/record", {"id": self.cp.desk, "task": task})
        assert status < 300, (status, answer)
        for _ in range(60):
            _, ticket = self.cp.api(f"/api/desktops/ticket?id={self.cp.desk}")
            if isinstance(ticket, dict) and (ticket.get("ticket") or {}).get("task_id"):
                break
            time.sleep(0.5)
        status, answer = self.cp.api("/api/desktops/ticket/action", {"id": self.cp.desk, "action": "begin"})
        assert status < 300, (status, answer)
        self.pg.wait_for_timeout(2500)

        # The ticket's panel opens over the top middle of the screen; a person closes it by its tab.
        self.pg.mouse.move(800, 2)
        self.pg.wait_for_timeout(500)
        if self.pg.evaluate("() => document.getElementById('decurix-ticket').classList.contains('decurix-open')"):
            self.pg.click("#decurix-ticket-tab")
        self.pg.mouse.move(800, 600)
        self.pg.wait_for_timeout(1500)

    def end(self):
        status, answer = self.cp.api("/api/desktops/ticket/action", {"id": self.cp.desk, "action": "end"})
        assert status < 300, (status, answer)
        session = ((answer.get("ticket") or {}) if isinstance(answer, dict) else {}).get("session_id")
        if not session:
            # End answers before the ticket's own thread has let go of it: the newest demonstration is this one.
            _, listed = self.cp.api(f"/api/desktops/recordings?id={self.cp.desk}")
            session = next(r["id"] for r in listed["recordings"] if r.get("kind") == "demonstration")
        for _ in range(60):
            if (self.cp.recording(session).get("meta") or {}).get("status") == "complete":
                break
            time.sleep(0.5)
        return session


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="http://127.0.0.1:8080")
    parser.add_argument("--desktop", required=True)
    parser.add_argument("--port", type=int, default=8791, help="where to serve the messaging site")
    args = parser.parse_args()

    cp = ControlPlane(args.base, args.desktop)
    site = f"http://127.0.0.1:{args.port}"
    server = subprocess.Popen([sys.executable, str(HERE / "msgsite.py"), str(args.port)])
    failures = []

    def state():
        return json.load(urllib.request.urlopen(site + "/api/state"))

    def reset():
        urllib.request.urlopen(urllib.request.Request(site + "/api/reset", data=b"{}", method="POST"))

    def check(ok, what):
        print(("  ok    " if ok else "  FAIL  ") + what)
        if not ok:
            failures.append(what)

    try:
        time.sleep(1)
        cp.idle()

        # Where things are on the worker's screen, as the worker reads them.
        cp.send(f"run the command: `gui open {site}/messaging/`", timeout=180)
        spots = cp.elements()
        search, row = spots["Search messages"], spots["This message has been deleted."]
        box, button = spots["Write a message…"], spots["Send"]
        cp.send(f"run the command: `gui open {site}/`", timeout=180)
        link = cp.elements()["Messaging, 0 new notifications"]
        cp.idle()
        reset()

        print("1. A person records the task")
        person = Person(cp)
        person.record('open messaging and send "hello test" to ohona sahoo')
        person.click(*link, wait=3)
        person.click(*search, wait=1.2)
        person.type("ohona sahoo")
        person.key("Enter", wait=2.5)
        person.click(*row, wait=2)
        person.click(*box, wait=1)
        person.type("hello test", wait=1.2)
        person.click(*button, wait=2.5)
        session = person.end()
        person.close()
        check(state()["sent"] == [{"to": "Ohona Sahoo", "text": "hello test"}], "the person's own message was sent")

        print("2. The recording names what was pressed and typed into")
        events = [e for e in cp.recording(session).get("events", []) if e.get("kind") != "note"]
        for event in events:
            named = bool((event.get("element") or {}).get("name"))
            check(named, f"step {event.get('i')}: {event.get('summary')}")
        typed = {(e.get("element") or {}).get("name"): e.get("text") for e in events if e.get("kind") == "type"}
        check(typed == {"Search messages": "ohona sahoo", "Write a message…": "hello test"}, f"what was typed where: {typed}")

        print("3. /learn")
        cp.idle()
        entry = cp.send(f"/learn {session}", timeout=300)
        approvals = ""
        for _ in range(14):
            if entry.get("kind") not in ("query", "approval"):
                break
            asked = entry["content"]
            low = asked.lower()
            if "i will always stop and ask you before" in low:
                approvals = asked.split("I will always stop and ask you before:", 1)[1].split("Each run", 1)[0]
            if "which of them change" in low:
                reply = "all change"
            elif "reply `keep`" in low:
                reply = "keep"
            elif "should i check that before i start" in low:
                reply = "no"
            elif "what should i call it" in low or "i will save it as" in low:
                reply = "message someone"
            elif "multiple" in low or "more than one" in low or "similar" in low:
                reply = "choose the first one"
            else:
                reply = "yes"
            entry = cp.send(reply, timeout=300)
        check(entry.get("kind") == "entry" and "Learned" in entry.get("content", ""), "the lesson was saved")
        check("Send" in approvals and "Return" not in approvals, f"only Send needs approval ({approvals.strip()!r})")

        def run(request, replies):
            reset()
            cp.idle()
            asked = []
            entry = cp.send(request)
            for _ in range(5):
                if entry.get("kind") not in ("query", "approval"):
                    break
                asked.append(entry["content"])
                entry = cp.send(replies.pop(0) if replies else "no")
            return entry, asked

        print("4. The task in plain words, for another person")
        entry, asked = run('open messaging and send "hello from decurix" to alex doe', ["yes"])
        check(len(asked) == 1 and 'button "Send"' in asked[0], f"asked once, for Send ({len(asked)} question(s))")
        check(state()["sent"] == [{"to": "Alex Doe", "text": "hello from decurix"}], f"the site got it: {state()['sent']}")
        check(entry.get("kind") == "entry", "the worker reported it done")

        print("5. No at the approval")
        entry, asked = run('open messaging and send "deck looks good" to rahul verma', ["no, do not send it"])
        check(state()["sent"] == [], "nothing was sent")
        check(len(asked) == 1, f"it did not try again ({len(asked)} question(s))")
    finally:
        server.terminate()

    print("\n" + ("PASSED" if not failures else f"FAILED: {len(failures)}"))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
