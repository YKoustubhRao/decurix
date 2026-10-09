"""
The in-desktop half of the GUI benchmark: puts the screen into a task's
starting state, and decides afterwards whether the task was really done.

Copied into a desktop (with tasks.json, odf.py and adminpanel.py) by
bench/gui/run.py and run there as the headless user:

    python3 bench.py reset <task-id>
    python3 bench.py check <task-id> [--answer-file PATH]

`check` prints one JSON object, {"pass": bool, "detail": "..."}. It judges
the outcome only - the admin panel's state file, the saved document, the
file on disk - never the agent's own account of what it did.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import odf  # noqa: E402

ROOT = Path("/home/headless/bench")
FILES = ROOT / "files"
OUT = ROOT / "out"
STATE = ROOT / "state" / "admin.json"
PANEL = "http://127.0.0.1:8765"

# The desktop's own session - `docker exec` starts with none of it.
SESSION = {
    "DISPLAY": ":1",
    "XAUTHORITY": "/home/headless/.Xauthority",
    "NO_AT_BRIDGE": "0",
    "HOME": "/home/headless",
}


def session_env():
    env = dict(os.environ)
    env.update(SESSION)

    if "DBUS_SESSION_BUS_ADDRESS" not in env:
        for proc in Path("/proc").iterdir():
            if not proc.name.isdigit():
                continue
            try:
                if (proc / "comm").read_text().strip() != "xfce4-session":
                    continue
                for item in (proc / "environ").read_bytes().split(b"\0"):
                    if item.startswith(b"DBUS_SESSION_BUS_ADDRESS="):
                        env["DBUS_SESSION_BUS_ADDRESS"] = item.split(b"=", 1)[1].decode()
            except OSError:
                continue

    return env


def load_tasks():
    with open(HERE / "tasks.json", encoding="utf-8") as f:
        return {task["id"]: task for task in json.load(f)["tasks"]}


def launch(argv):
    subprocess.Popen(
        argv,
        env=session_env(),
        cwd=str(ROOT),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )


def window_titles():
    try:
        out = subprocess.run(
            ["xdotool", "search", "--onlyvisible", "--name", "."],
            env=session_env(), capture_output=True, text=True, timeout=10,
        ).stdout.split()
    except (OSError, subprocess.TimeoutExpired):
        return []

    titles = []
    for wid in out:
        name = subprocess.run(
            ["xdotool", "getwindowname", wid],
            env=session_env(), capture_output=True, text=True, timeout=10,
        ).stdout.strip()
        if name:
            titles.append(name)
    return titles


def wait_for_window(pattern, timeout=60):
    deadline = time.time() + timeout
    regex = re.compile(pattern, re.IGNORECASE)

    while time.time() < deadline:
        if any(regex.search(t) for t in window_titles()):
            time.sleep(1.5)
            return True
        time.sleep(0.5)

    return False


APP_PROCESSES = {"chromium-browse", "chrome", "soffice.bin", "oosplash", "mousepad"}


def wait_for_page(path, timeout=30):
    """
    Wait until the browser's tab is on `path` with a title - a window on
    screen is not a loaded page, and a task that starts before its page has
    rendered is measuring the fixture, not the engine.
    """

    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen("http://127.0.0.1:9222/json/list", timeout=2) as response:
                tabs = json.loads(response.read().decode("utf-8"))
        except (OSError, ValueError):
            tabs = []
        for tab in tabs:
            url = urllib.parse.urlparse(tab.get("url", ""))
            if tab.get("type") == "page" and url.path == path and tab.get("title") and not tab["title"].startswith("127.0.0.1"):
                time.sleep(1.0)
                return True
        time.sleep(0.3)
    return False


def kill_apps():
    # By process name, never `pkill -f <word>`: a task id such as
    # "mousepad-save-as" is in this very process's command line.
    me = os.getpid()

    for proc in Path("/proc").iterdir():
        if not proc.name.isdigit() or int(proc.name) == me:
            continue
        try:
            comm = (proc / "comm").read_text().strip()
            cmdline = (proc / "cmdline").read_bytes().replace(b"\0", b" ").decode("utf-8", "replace")
        except OSError:
            continue
        if comm in APP_PROCESSES or "adminpanel.py --state" in cmdline:
            try:
                os.kill(int(proc.name), 15)
            except OSError:
                pass

    # Thunar's windows live in its daemon, so quitting it is how they close;
    # the next `thunar` starts a fresh one with the accessibility bridge on.
    subprocess.run(["thunar", "--quit"], env=session_env(), capture_output=True, timeout=10)

    time.sleep(1.5)

    # A killed LibreOffice leaves a recovery record that turns the next start
    # into a "recover documents?" dialog - which no task asked for.
    registry = Path("/home/headless/.config/libreoffice/4/user/registrymodifications.xcu")
    if registry.exists():
        text = registry.read_text(encoding="utf-8", errors="replace")
        text = re.sub(r'<item oor:path="/org.openoffice.Office.Recovery/RecoveryList">.*?</item>\n?', "", text)
        registry.write_text(text, encoding="utf-8")


def reset_files():
    for folder in (FILES, OUT):
        shutil.rmtree(folder, ignore_errors=True)
        folder.mkdir(parents=True, exist_ok=True)

    for lock in ROOT.glob("**/.~lock.*"):
        lock.unlink(missing_ok=True)

    odf.write_ods(FILES / "budget.ods", "Sheet1", [
        ["Category", "Q1", "Q2", "Q3"],
        ["Rent", 1200, 1200, None],
        ["Payroll", 5200, 5300, None],
        ["Software", 280, 295, None],
        ["Travel", 140, 60, None],
    ])

    odf.write_odt(
        FILES / "expense-claim.odt",
        "Expense claim",
        "Fill in the empty cell next to each label.",
        [
            ["Employee name", None],
            ["Date", None],
            ["Amount", None],
            ["Purpose", None],
        ],
    )

    (FILES / "notes.txt").write_text(
        "The colour of the logo is blue.\nPick a colour for the header.\nNo other colour changes.\n",
        encoding="utf-8",
    )

    (FILES / "draft.txt").write_text("Quarterly plan draft.\n", encoding="utf-8")


def start_panel():
    launch([sys.executable, str(HERE / "adminpanel.py"), "--state", str(STATE), "--reset"])

    deadline = time.time() + 20
    while time.time() < deadline:
        try:
            urllib.request.urlopen(PANEL + "/login", timeout=2).read()
            return
        except OSError:
            time.sleep(0.3)

    raise SystemExit("admin panel did not start")


def start_app(start):
    if not start:
        return

    if "browser" in start:
        path = start["browser"]
        url = PANEL + path if path == "/login" else (
            f"{PANEL}/autologin?token=bench-autologin&next={path}"
        )
        profile = "/tmp/bench-chromium"
        shutil.rmtree(profile, ignore_errors=True)
        launch([
            "chromium-browser", f"--user-data-dir={profile}",
            "--remote-debugging-port=9222", "--no-first-run",
            "--no-default-browser-check", "--disable-gpu", "--start-maximized",
            "--password-store=basic", url,
        ])
        wait_for_window(r"Chromium|Google Chrome")
        wait_for_page(path)
        return

    office = ["soffice", "--norestore", "--nologo"]

    if "calc" in start:
        launch(office + ["--calc", str(FILES / start["calc"])])
        wait_for_window(re.escape(start["calc"]) + r".*LibreOffice")
        return

    if "writer" in start:
        launch(office + ["--writer", str(FILES / start["writer"])])
        wait_for_window(re.escape(start["writer"]) + r".*LibreOffice")
        return

    if "writer_new" in start:
        launch(office + ["--writer"])
        wait_for_window(r"Untitled.*LibreOffice")
        return

    if "mousepad" in start:
        target = start["mousepad"]
        launch(["mousepad"] + ([str(FILES / target)] if target else []))
        wait_for_window(r"Mousepad")
        return

    if "thunar" in start:
        launch(["thunar", str(ROOT / start["thunar"])])
        wait_for_window(start["thunar"])
        return


def reset(task):
    kill_apps()
    reset_files()
    start_panel()
    start_app(task.get("start"))

    # Whatever the last task left in the agent's own screen state (element
    # refs, a pending approval) belongs to that task.
    shutil.rmtree("/home/headless/.decurix/computer", ignore_errors=True)


# ============================================================
# Checks
# ============================================================

def panel_state():
    with open(STATE, encoding="utf-8") as f:
        return json.load(f)


def find_user(state, name):
    for user in state["users"]:
        if user["name"].strip().lower() == name.lower():
            return user
    return None


def number_near(value, expected):
    try:
        return abs(float(str(value).replace(",", "")) - float(expected)) < 0.005
    except (TypeError, ValueError):
        return False


def check(task, answer):
    spec = task["check"]
    kind = spec["type"]

    if kind == "logins_at_least":
        logins = panel_state()["logins"]
        return logins >= spec["n"], f"successful sign-ins: {logins}"

    if kind == "user_exists":
        user = find_user(panel_state(), spec["name"])
        if user is None:
            return False, f"no user named {spec['name']}"
        ok = user["email"].lower() == spec["email"].lower() and user["role"] == spec["role"]
        return ok, f"found {user}"

    if kind == "user_email":
        user = find_user(panel_state(), spec["name"])
        return bool(user) and user["email"].lower() == spec["email"].lower(), f"found {user}"

    if kind == "user_role":
        user = find_user(panel_state(), spec["name"])
        return bool(user) and user["role"] == spec["role"], f"found {user}"

    if kind == "user_absent":
        state = panel_state()
        gone = find_user(state, spec["name"]) is None
        count = len(state["users"])
        return gone and count == spec["remaining"], f"{count} users, target present: {not gone}"

    if kind == "settings":
        settings = panel_state()["settings"]
        wrong = {k: settings.get(k) for k, v in spec["values"].items() if settings.get(k) != v}
        return not wrong, f"mismatched: {wrong}" if wrong else "all settings match"

    if kind == "ticket":
        tickets = panel_state()["tickets"]
        for t in tickets:
            if (t["subject"].lower() == spec["subject"].lower()
                    and t["priority"] == spec["priority"]
                    and t["description"].strip().rstrip(".").lower() == spec["description"].rstrip(".").lower()):
                return True, "ticket found"
        return False, f"tickets: {tickets}"

    if kind == "newsletter":
        n = panel_state()["newsletter"]
        return n["sent"] == spec["sent"], f"sent={n['sent']} count={n['send_count']}"

    if kind == "invoices":
        invoices = {i["id"]: i["paid"] for i in panel_state()["invoices"]}
        wrong = {k: invoices.get(k) for k, v in spec["paid"].items() if invoices.get(k) != v}
        return not wrong, f"paid: {invoices}"

    if kind == "answer_number":
        numbers = re.findall(r"(?<![\w.])\d+(?![\w.])", answer or "")
        ok = str(spec["value"]) in numbers and len(set(numbers)) == 1
        return ok, f"numbers in answer: {numbers}"

    if kind == "ods_cells":
        grid = odf.read_ods(FILES / spec["file"]).get(spec.get("sheet", "Sheet1"))
        if grid is None:
            return False, "sheet not found"
        wrong = {}
        for ref, expected in spec["cells"].items():
            c = odf.cell(grid, ref)
            got = c and (c["value"] if c["value"] is not None else c["text"])
            if not number_near(got, expected):
                wrong[ref] = got
        return not wrong, f"wrong cells: {wrong}" if wrong else "all cells match"

    if kind == "ods_formula":
        grid = odf.read_ods(FILES / spec["file"]).get(spec.get("sheet", "Sheet1"))
        c = grid and odf.cell(grid, spec["ref"])
        if not c:
            return False, f"{spec['ref']} is empty"
        ok = spec["contains"].lower() in c["formula"].lower() and number_near(c["value"], spec["value"])
        return ok, f"{spec['ref']}: formula={c['formula']!r} value={c['value']!r}"

    if kind == "ods_sheets":
        names = list(odf.read_ods(FILES / spec["file"]).keys())
        return names == spec["names"], f"sheets: {names}"

    if kind == "odt_form":
        path = FILES / spec["file"]
        _, tables = odf.read_odt(path)
        if not tables:
            return False, "no table in document"
        grid = tables[0]
        got = {row[0]["text"].strip(): row[1]["text"].strip() for row in grid if len(row) >= 2}
        wrong = {k: got.get(k) for k, v in spec["values"].items() if got.get(k, "").strip() != v}
        return not wrong, f"wrong fields: {wrong}" if wrong else "form filled"

    if kind == "odt_contains":
        path = Path(spec["path"])
        if not path.exists():
            return False, f"{path} does not exist"
        try:
            paragraphs, _ = odf.read_odt(path)
        except Exception as exc:  # a file saved in another format
            return False, f"not an ODF text document: {exc}"
        ok = any(spec["text"].lower() == p.strip().lower() for p in paragraphs)
        return ok, f"paragraphs: {paragraphs[:5]}"

    if kind == "file_text":
        path = Path(spec["path"])
        if not path.exists():
            return False, f"{path} does not exist"
        text = path.read_text(encoding="utf-8", errors="replace")
        return text.strip() == spec["text"].strip(), f"content: {text[:200]!r}"

    if kind == "file_renamed":
        old, new = FILES / spec["from"], FILES / spec["to"]
        return (not old.exists()) and new.exists(), f"{spec['from']} exists={old.exists()}, {spec['to']} exists={new.exists()}"

    return False, f"unknown check type {kind}"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["reset", "check", "list"])
    parser.add_argument("task", nargs="?")
    parser.add_argument("--answer-file")
    args = parser.parse_args()

    tasks = load_tasks()

    if args.command == "list":
        print("\n".join(tasks))
        return

    task = tasks[args.task]

    if args.command == "reset":
        reset(task)
        print(json.dumps({"reset": task["id"]}))
        return

    answer = ""
    if args.answer_file:
        answer = Path(args.answer_file).read_text(encoding="utf-8")

    try:
        passed, detail = check(task, answer)
    except Exception as exc:
        passed, detail = False, f"check crashed: {exc}"

    print(json.dumps({"pass": bool(passed), "detail": detail}))


if __name__ == "__main__":
    main()
