#!/usr/bin/env python3
"""
The sample tasks on a native Windows worker, each judged on its outcome.

bench/gui/run.py drives a Linux desktop container (docker exec, LibreOffice,
GTK apps). A native worker is a private desktop of this computer with no
container to exec into, so this is that benchmark's Windows half:

  - the twelve browser tasks of bench/gui/tasks.json, unchanged: the admin
    panel they run against (bench/gui/fixtures/adminpanel.py) is started on
    this computer, where the worker's browser reaches it, and each task is
    checked the same way (fixtures/bench.py's check, off the panel's state file)
  - Windows tasks in place of the LibreOffice and GTK ones: questions answered
    by a command, files and a workbook made in the worker's folder, and Notepad
    worked on the screen

Every task is sent the way a person sends it - a message through the control
plane's chat API - and judged by what it left behind, never by what the worker
says it did. A task marked `approval` passes only when the worker stopped to
ask before the consequential click and the click had not happened.

    python bench/native/run.py --create bench1 --brain azure
    python bench/native/run.py --desktop bench1 --only web-login,win-notepad-save

Standard library only. Needs a running control plane with native workers
(the Windows app, or a checkout started with DECURIX_NATIVE_*), and for a
hosted brain, that brain set up there.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
GUI = HERE.parent / "gui"
sys.path.insert(0, str(GUI / "fixtures"))

import bench as gui_bench  # noqa: E402  (bench/gui/fixtures/bench.py: the checks)

PANEL_PORT = 8765
PANEL = f"http://127.0.0.1:{PANEL_PORT}"

# How long one task may take. A screen task on a hosted model is a picture and
# a model call per step.
TASK_SECONDS = 900


# ============================================================
# The control plane
# ============================================================

class ControlPlane:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def call(self, path, data=None, timeout=120):
        body = urllib.parse.urlencode(data).encode() if data is not None else None
        try:
            with urllib.request.urlopen(urllib.request.Request(self.base + path, data=body), timeout=timeout) as r:
                raw = r.read().decode("utf-8", "replace")
                status = r.status
        except urllib.error.HTTPError as exc:
            raw, status = exc.read().decode("utf-8", "replace"), exc.code
        try:
            return status, json.loads(raw)
        except ValueError:
            return status, {"raw": raw}

    def thread(self, desktop):
        _, data = self.call("/api/desktops/chat?id=" + urllib.parse.quote(desktop))
        return data.get("thread") or data

    def desktops(self):
        _, data = self.call("/api/desktops")
        return data if isinstance(data, list) else []

    def home(self, desktop):
        for d in self.desktops():
            if d.get("id") == desktop:
                return Path(d["volume"])
        raise SystemExit(f"no worker named {desktop}")

    def create(self, name, brain):
        status, data = self.call("/api/desktops/create", {"name": name, "brain": brain})
        if status >= 300:
            raise SystemExit(f"could not create {name}: {data}")
        deadline = time.time() + 600
        while time.time() < deadline:
            _, st = self.call("/api/desktops/status?id=" + name)
            if st.get("ready") or (st.get("status") == "running" and not st.get("stage")):
                time.sleep(8)  # the agent binds its port and the engine starts
                return
            if st.get("status") == "failed":
                raise SystemExit(f"{name} was not created: {st}")
            time.sleep(1)
        raise SystemExit(f"{name} was not ready in time")

    def restart(self, desktop):
        self.call("/api/desktops/restart", {"id": desktop})
        time.sleep(12)

    def send(self, desktop, text, timeout=TASK_SECONDS):
        """Send a request and wait for the worker's answer: (entry, seconds), or (None, seconds)."""

        started = time.time()

        status, data = self.call("/api/desktops/send", {"id": desktop, "content": text})
        if status >= 300:
            return {"role": "agent", "kind": "refused", "content": json.dumps(data)}, 0

        while time.time() - started < timeout:
            time.sleep(3)
            t = self.thread(desktop)
            entries = t.get("entries") or []
            # What came after this request, found by the request itself: a long chat keeps only its latest entries,
            # so "how many there were before" stops meaning anything once it is full.
            sent = max((i for i, e in enumerate(entries) if e.get("role") == "user" and (e.get("content") or "").strip() == text.strip()),
                       default=None)
            new = [e for e in entries[sent + 1:] if e.get("role") != "user"] if sent is not None else []
            if new and not t.get("running"):
                return new[-1], time.time() - started

        return None, time.time() - started

    def open(self, desktop, target):
        return self.call("/api/desktops/open", {"id": desktop, "target": target}, timeout=180)

    def busy(self, desktop):
        _, data = self.call("/api/desktops/activity?ids=" + urllib.parse.quote(desktop))
        return bool((data.get(desktop) or {}).get("running"))


# ============================================================
# The admin panel, on this computer
# ============================================================

class Panel:
    def __init__(self, folder):
        self.state = Path(folder) / "state" / "admin.json"
        self.process = None
        gui_bench.STATE = self.state  # what the checks read

    def start(self):
        self.stop()
        flags = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0
        self.process = subprocess.Popen(
            [sys.executable, str(GUI / "fixtures" / "adminpanel.py"), "--state", str(self.state), "--port",
             str(PANEL_PORT), "--reset"],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=flags)
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                urllib.request.urlopen(PANEL + "/login", timeout=2).read()
                return
            except OSError:
                time.sleep(0.3)
        raise SystemExit("the admin panel did not start")

    def stop(self):
        if self.process is not None:
            self.process.kill()
            self.process.wait()
            self.process = None


# ============================================================
# A page with nothing to name: only a picture to look at
# ============================================================

CANVAS_PORT = 8766

CANVAS_PAGE = """<!doctype html><html><head><meta charset="utf-8"><title>Shapes</title>
<style>body{margin:0;background:#fff;font:15px sans-serif}p{margin:12px 16px}</style></head><body>
<p>Three shapes are drawn below.</p><canvas id="c" width="900" height="420"></canvas>
<script>
var shapes = SHAPES, c = document.getElementById('c'), g = c.getContext('2d');
shapes.forEach(function (s) {
  g.fillStyle = s.colour; g.beginPath();
  if (s.kind === 'circle') g.arc(s.x, s.y, s.r, 0, 7);
  else if (s.kind === 'square') g.rect(s.x - s.r, s.y - s.r, 2 * s.r, 2 * s.r);
  else { g.moveTo(s.x, s.y - s.r); g.lineTo(s.x + s.r, s.y + s.r); g.lineTo(s.x - s.r, s.y + s.r); }
  g.fill();
});
c.addEventListener('click', function (e) {
  var b = c.getBoundingClientRect(), x = e.clientX - b.left, y = e.clientY - b.top, hit = 'nothing';
  shapes.forEach(function (s) { if (Math.abs(x - s.x) <= s.r && Math.abs(y - s.y) <= s.r) hit = s.name; });
  fetch('/hit?shape=' + encodeURIComponent(hit));
  // What any real page does: it shows that the click was taken.
  g.fillStyle = '#fff'; g.fillRect(0, 380, 900, 40);
  g.fillStyle = '#111'; g.font = '18px sans-serif'; g.fillText('You clicked: ' + hit, 16, 405);
});
</script></body></html>"""

CANVAS_SHAPES = [
    {"name": "red square", "kind": "square", "colour": "#d62828", "x": 150, "y": 120, "r": 34},
    {"name": "green circle", "kind": "circle", "colour": "#2a9d4b", "x": 610, "y": 300, "r": 34},
    {"name": "blue triangle", "kind": "triangle", "colour": "#2563eb", "x": 380, "y": 170, "r": 34},
]


class Canvas:
    """Serves the shapes page and keeps what was clicked on it, in order."""

    def __init__(self):
        self.hits = []
        self.server = None

    def start(self):
        import http.server
        import threading

        canvas = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                url = urllib.parse.urlparse(self.path)
                if url.path == "/hit":
                    canvas.hits.append(urllib.parse.parse_qs(url.query).get("shape", [""])[0])
                    body, kind = b"ok", "text/plain"
                else:
                    body, kind = CANVAS_PAGE.replace("SHAPES", json.dumps(CANVAS_SHAPES)).encode(), "text/html; charset=utf-8"
                self.send_response(200)
                self.send_header("Content-Type", kind)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", CANVAS_PORT), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def stop(self):
        if self.server is not None:
            self.server.shutdown()


# ============================================================
# Windows tasks
# ============================================================

def xlsx_cells(path):
    """{cell: (value, formula)} of a workbook's first sheet, read as the zip it is (no library)."""

    cells = {}
    with zipfile.ZipFile(path) as z:
        shared = []
        if "xl/sharedStrings.xml" in z.namelist():
            raw = z.read("xl/sharedStrings.xml").decode("utf-8", "replace")
            shared = ["".join(re.findall(r"<t[^>]*>([^<]*)</t>", si)) for si in re.findall(r"<si>(.*?)</si>", raw, re.S)]
        sheet = sorted(n for n in z.namelist() if re.match(r"xl/worksheets/sheet\d+\.xml", n))[0]
        raw = z.read(sheet).decode("utf-8", "replace")
        for attrs, body in re.findall(r"<c\b([^>]*?)(?:/>|>(.*?)</c>)", raw, re.S):
            ref = re.search(r'\br="([A-Z]+\d+)"', attrs)
            if not ref:
                continue
            kind = re.search(r'\bt="(\w+)"', attrs)
            value = re.search(r"<v>([^<]*)</v>", body or "")
            inline = re.search(r"<t[^>]*>([^<]*)</t>", body or "")
            formula = re.search(r"<f[^>]*>([^<]*)</f>", body or "")
            text = value.group(1) if value else (inline.group(1) if inline else "")
            if kind and kind.group(1) == "s" and text.isdigit():
                text = shared[int(text)]
            cells[ref.group(1)] = (text, formula.group(1) if formula else "")
    return cells


def windows_tasks(work):
    """The tasks that stand in for the Linux desktop apps: (id, prepare, prompt, expect, check)."""

    out = work / "bench-out"
    files = work / "bench-files"

    def prepare_folders():
        for folder in (out, files):
            shutil.rmtree(folder, ignore_errors=True)
            folder.mkdir(parents=True, exist_ok=True)

    def prepare_count():
        prepare_folders()
        for i in range(7):
            (files / f"note-{i}.txt").write_text(f"note {i}\n", encoding="utf-8")
        for i in range(3):
            (files / f"data-{i}.csv").write_text("a,b\n1,2\n", encoding="utf-8")

    def prepare_notes():
        prepare_folders()
        (files / "notes.txt").write_text(
            "The colour of the logo is blue.\nPick a colour for the header.\nNo other colour changes.\n", encoding="utf-8")

    def answer_has(pattern):
        return lambda answer: (bool(re.search(pattern, answer or "", re.I)), f"answered: {(answer or '')[:160]!r}")

    def check_build(answer):
        build = str(sys.getwindowsversion().build)
        return build in (answer or ""), f"this computer's build is {build}; answered: {(answer or '')[:160]!r}"

    def check_hello(_answer):
        path = out / "hello.txt"
        if not path.exists():
            return False, f"{path} was not created"
        text = path.read_text(encoding="utf-8-sig", errors="replace").strip()
        return text == "hello from decurix", f"{path} holds {text[:80]!r}"

    def check_typed(_answer):
        path = out / "typed.txt"
        if not path.exists():
            return False, f"{path} was not saved"
        text = path.read_bytes().decode("utf-8-sig", "replace").strip()
        return text == "hello from the screen", f"{path} holds {text[:80]!r}"

    def check_replaced(_answer):
        text = (files / "notes.txt").read_text(encoding="utf-8-sig", errors="replace")
        return "colour" not in text and text.count("color") == 3, f"notes.txt now reads {text[:120]!r}"

    def check_workbook(_answer):
        path = out / "totals.xlsx"
        if not path.exists():
            return False, f"{path} was not created"
        try:
            cells = xlsx_cells(path)
        except (zipfile.BadZipFile, IndexError, KeyError) as exc:
            return False, f"{path} is not a workbook: {exc}"
        flat = " ".join(f"{v} {f}" for v, f in cells.values())
        has_rows = all(word in flat for word in ("Rent", "Payroll", "Software", "1200", "5400", "310"))
        has_total = bool(re.search(r"SUM\(", flat, re.I)) or "6910" in flat
        return has_rows and has_total, f"{len(cells)} cells; rows {has_rows}, total {has_total}"

    return [
        ("win-version", None,
         "What build number of Windows is this computer running? Reply with just the build number.", "done", check_build),
        ("win-count-files", prepare_count,
         f"How many .txt files are in the folder {files}? Reply with just the number.", "done", answer_has(r"\b7\b")),
        ("win-write-file", prepare_folders,
         f"Create a text file {out / 'hello.txt'} containing exactly this line: hello from decurix", "done", check_hello),
        ("win-workbook", prepare_folders,
         f"Create an Excel workbook {out / 'totals.xlsx'} with a header row Category, Amount and the rows Rent 1200, "
         "Payroll 5400, Software 310, then a Total row whose amount is a formula that sums them.", "done", check_workbook),
        ("win-notepad-save", prepare_folders,
         f"Open Notepad on your screen, type hello from the screen into it, and save it as {out / 'typed.txt'}.",
         "done", check_typed),
        ("win-notepad-replace", prepare_notes,
         f"Open {files / 'notes.txt'} in Notepad on your screen. Using the editor, replace every colour with color, "
         "then save the file.", "done", check_replaced),
        ("win-web-lookup", None,
         'search "who is the chief executive of Alphabet" and tell me the name.', "done", answer_has(r"pichai")),
    ]


# ============================================================
# Running
# ============================================================

def gate_fired(entry):
    """Whether the worker stopped at the approval gate: a step that ended needs_approval, or an approval question."""

    if entry is None:
        return False
    if entry.get("kind") == "approval":
        return True
    return any("needs_approval" in json.dumps(step).lower() or "needs your approval" in json.dumps(step).lower()
               for step in entry.get("steps") or [])


def settle(cp, desktop, entry):
    """Leave the worker idle for the next task: one left waiting on a question would take the next task as its answer."""

    if entry is None or entry.get("kind") in ("query", "approval"):
        cp.restart(desktop)


def run_task(cp, desktop, task_id, prompt, expect, check, approve=None, log=print):
    entry, took = cp.send(desktop, prompt)
    kind = (entry or {}).get("kind") or ("none" if entry is None else "final")
    answer = (entry or {}).get("content") or ""

    if expect == "approval":
        stopped = gate_fired(entry)
        if approve and stopped:
            entry, more = cp.send(desktop, approve)
            took += more
            answer = (entry or {}).get("content") or ""
        ok, detail = check(answer)
        ok = ok and stopped
        detail = ("stopped for approval; " if stopped else "did NOT stop for approval; ") + detail
    elif entry is None:
        ok, detail = False, f"no answer in {TASK_SECONDS} s"
    elif kind in ("query", "approval"):
        ok, detail = check(answer)
        ok, detail = False, f"asked instead of finishing ({answer[:160]!r}); outcome: {detail}"
    else:
        ok, detail = check(answer)

    steps = len((entry or {}).get("steps") or [])
    log(f"{'PASS' if ok else 'FAIL'}  {task_id:26} {took:5.0f}s  {steps:3} steps  {kind:9} {detail[:200]}")
    settle(cp, desktop, entry)
    return {"id": task_id, "pass": ok, "seconds": round(took), "kind": kind, "detail": detail, "answer": answer[:1500],
            "steps": [(s.get("text") or s.get("summary") or "")[:300] for s in (entry or {}).get("steps") or []]}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", default="http://127.0.0.1:8080", help="the control plane")
    parser.add_argument("--desktop", help="an existing native worker to run on")
    parser.add_argument("--create", help="create a worker with this name and run on it")
    parser.add_argument("--brain", default="azure", choices=("azure", "claude", "ollama"))
    parser.add_argument("--only", default="", help="comma-separated task ids")
    parser.add_argument("--skip", default="", help="comma-separated task ids to leave out")
    parser.add_argument("--out", default=str(HERE / "results"), help="where the results file goes")
    args = parser.parse_args()

    cp = ControlPlane(args.base)
    desktop = args.desktop or args.create
    if not desktop:
        parser.error("name a worker with --desktop, or --create one")
    if args.create:
        cp.create(args.create, args.brain)

    work = cp.home(desktop) / "work"
    only = {t for t in args.only.split(",") if t}
    skip = {t for t in args.skip.split(",") if t}
    wanted = lambda task_id: (not only or task_id in only) and task_id not in skip  # noqa: E731

    panel = Panel(tempfile.mkdtemp(prefix="decurix-bench-"))
    results = []

    try:
        with open(GUI / "tasks.json", encoding="utf-8") as f:
            web = [t for t in json.load(f)["tasks"] if t.get("app") == "chromium"]

        for task in web:
            if not wanted(task["id"]):
                continue
            panel.start()
            path = task["start"]["browser"]
            url = PANEL + path if path == "/login" else f"{PANEL}/autologin?token=bench-autologin&next={path}"
            status, opened = cp.open(desktop, url)
            if status >= 300:
                print(f"FAIL  {task['id']:26} could not open its page: {opened}")
                results.append({"id": task["id"], "pass": False, "detail": f"could not open the page: {opened}"})
                continue
            time.sleep(2)
            # The browser a native worker has is Edge; the task names the app the Linux desktops have.
            prompt = task["prompt"].replace("open in Chromium", "open in your browser").replace("Chromium is open", "Your browser is open")
            results.append(run_task(cp, desktop, task["id"], prompt, task["expect"],
                                    lambda answer, t=task: gui_bench.check(t, answer), approve=task.get("approve")))

        # Something only eyes can do: the shapes are pixels on a canvas, with no element to name.
        if wanted("see-canvas-click"):
            canvas = Canvas()
            canvas.start()
            try:
                cp.open(desktop, f"http://127.0.0.1:{CANVAS_PORT}/")
                time.sleep(2)
                results.append(run_task(
                    cp, desktop, "see-canvas-click",
                    "The page open in your browser has three shapes drawn on it. Click the green circle, once.", "done",
                    lambda _answer: (canvas.hits == ["green circle"], f"clicks landed on: {canvas.hits}")))
            finally:
                canvas.stop()

        for task_id, prepare, prompt, expect, check in windows_tasks(work):
            if not wanted(task_id):
                continue
            if prepare:
                prepare()
            results.append(run_task(cp, desktop, task_id, prompt, expect, check))
    finally:
        panel.stop()

    passed = sum(1 for r in results if r["pass"])
    print(f"\n{passed} of {len(results)} passed")

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    name = out / f"native-{args.brain}-{time.strftime('%Y%m%d-%H%M%S')}.json"
    name.write_text(json.dumps({"desktop": desktop, "brain": args.brain, "passed": passed, "total": len(results),
                                "results": results}, indent=2), encoding="utf-8")
    print(f"results: {name}")

    return 0 if results and passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
