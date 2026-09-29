"""
Teaching a skill as a person does it, through the screen viewer: open a ticket from the session bar,
record a search while clicking the browser icon impatiently, End, `/learn` it answering quickly, run
the skill with a new value from a clean start, and again with the browser already covering the icon
(it must ask, not click blind; `skip` finishes it). Needs Playwright (see e2e.py) and a fresh desktop
whose XFCE desktop shows the Chromium icon. About four minutes on a 1-vCPU desktop.

    /tmp/pw/bin/python bench/screen/teach_e2e.py <desktop id>
"""
import json, os, re, subprocess, sys, tempfile, time, urllib.parse, urllib.request
from playwright.sync_api import sync_playwright

B = "http://127.0.0.1:8080"
DESK = sys.argv[1]
OUT = os.environ.get("SCREEN_E2E_OUT", os.path.join(tempfile.gettempdir(), "decurix-screen-e2e"))
os.makedirs(OUT, exist_ok=True)
results = []

# Whichever browser build the desktop runs (Chrome behind /usr/bin/chromium-browser now, Chromium before):
# the launcher execs the real binary, so its own name is not in the process list.
CLOSE_BROWSER = "pkill -f '[/]opt/google/chrome/chrome|[/]usr/lib/chromium-browser/chrom' || true"  # [/]: not this shell's own command line


def searched(title, words):
    """The window shows a search for `words`: its results page, or - when Google answers with its
    "unusual traffic" page, whose title is only the URL - that URL's query."""
    return words in title.lower() or ("q=" + words.replace(" ", "+")) in title.lower()


def check(name, ok, detail=""):
    results.append((name, ok))
    print(("PASS " if ok else "FAIL ") + name + (f" - {detail}" if detail else ""), flush=True)


def sh(cmd):
    return subprocess.run(["docker", "exec", "-u", "headless", "-e", "DISPLAY=:1", f"desktop_{DESK}", "bash", "-lc", cmd],
                          capture_output=True, text=True, timeout=60).stdout.strip()


def thread():
    d = json.load(urllib.request.urlopen(f"{B}/api/desktops/chat?id={DESK}"))
    return d.get("thread") or d


def send(text, timeout=90):
    before = len(thread().get("entries", []))
    t0 = time.time()
    req = urllib.request.Request(f"{B}/api/desktops/send", data=urllib.parse.urlencode({"id": DESK, "content": text}).encode())
    urllib.request.urlopen(req, timeout=120).read()
    while time.time() - t0 < timeout:
        new = [e for e in thread().get("entries", [])[before:] if e.get("role") == "agent"]
        if new:
            return new[-1], time.time() - t0
        time.sleep(0.4)
    return None, time.time() - t0


with sync_playwright() as p:
    browser = p.chromium.launch()
    pg = browser.new_page(viewport={"width": 1440, "height": 810})
    pg.goto(f"{B}/desktop/{DESK}/?autoconnect=true&resize=remote&ui=touch")
    pg.wait_for_function("() => document.getElementById('screen').classList.contains('live')", timeout=120000)
    pg.wait_for_timeout(1500)
    sh(CLOSE_BROWSER)
    pg.wait_for_timeout(1500)

    # Open a ticket from the bar.
    pg.mouse.move(720, 2); pg.wait_for_timeout(400)
    pg.click("#decurix-ticket-tab"); pg.wait_for_timeout(300)
    pg.fill("#decurix-ticket-panel input[type=text]", 'search "flamingo breeding season"')
    pg.click("#decurix-ticket-panel button.decurix-primary")
    pg.wait_for_selector("#decurix-ticket-panel button:has-text('Record')", timeout=60000)
    pg.click("#decurix-ticket-panel button:has-text('Record')")
    pg.wait_for_function("() => document.getElementById('decurix-ticket-dot').classList.contains('decurix-rec')", timeout=30000)
    check("ticket opened and recording from the screen's own bar", True)
    pg.wait_for_timeout(1500)

    # Where the browser icon is on this desktop right now.
    # Where XFCE put the browser icon (its own record of the grid), 1:1 with the page (remote resize).
    rc = sh("cat ~/.config/xfce4/desktop/icons.screen.latest.rc")
    row = int(re.search(r"chromium-browser.desktop\]\s*row=(\d+)", rc).group(1))
    pos = (69, 45 + 104 * row + 11)
    # An impatient person: click, press-and-wobble, double-click, then two more clicks while it starts.
    x, y = pos
    pg.mouse.click(x, y); pg.wait_for_timeout(1200)
    pg.mouse.move(x, y); pg.mouse.down(); pg.mouse.move(x - 1, y - 11, steps=4); pg.mouse.up(); pg.wait_for_timeout(1500)
    pg.mouse.dblclick(x, y - 11); pg.wait_for_timeout(900)
    pg.mouse.click(x, y - 11); pg.wait_for_timeout(700)
    pg.mouse.click(x, y - 11)
    for _ in range(40):
        if sh("xdotool search --onlyvisible --class 'chromium|google-chrome' | head -1"):
            break
        time.sleep(0.5)
    pg.wait_for_timeout(4000)
    pg.keyboard.press("Control+l"); pg.wait_for_timeout(600)
    pg.keyboard.type("fl", delay=40); pg.wait_for_timeout(1300)
    pg.keyboard.type("amingo breeding season", delay=35); pg.wait_for_timeout(600)
    pg.keyboard.press("Enter")
    pg.wait_for_timeout(7000)
    pg.screenshot(path=f"{OUT}/recorded.png")

    # End from the bar.
    pg.click("#decurix-ticket-panel button:has-text('End')")
    pg.wait_for_selector("#decurix-ticket-panel :text('Recording saved')", timeout=60000)
    href = pg.get_attribute("#decurix-ticket-panel a.decurix-download", "href") or ""
    session = urllib.parse.parse_qs(urllib.parse.urlparse(href).query).get("session", [""])[0] or \
        (re.search(r"demo-[0-9T]+-[0-9a-f]+", href) or [None])[0] if href else ""
    if not session:
        m = re.search(r"as (demo-[0-9T]+-[0-9a-f]+)", thread()["entries"][-1]["content"])
        session = m.group(1) if m else ""
    check("recording saved", bool(session), session)
    pg.screenshot(path=f"{OUT}/saved.png")
    browser.close()

# Teach it, answering the way the person did - fast.
entry, took = send(f'/learn {session} name="Flamingo search"', 60)
text = entry["content"] if entry else ""
print("\n--- first question ---\n" + text + "\n---")
check("lesson starts", entry is not None and entry["kind"] == "query", f"{took:.1f}s")
check("impatient clicks were tidied into one double click", "quick presses on the same spot" in text)
check("typing kept as one value", text.count("type {") == 1, "joined: " + str("typing split in two" in text))
check("Enter after the typing was recorded", "press Return" in text)

replies = {"goal": "skip", "coordinates": "keep", "variable": "changes", "start": "no", "confirm": "yes"}
asked = []
for _ in range(12):
    if entry is None or entry["kind"] != "query":
        break
    q = entry["content"]
    kind = ("coordinates" if "recorded as a click" in q or "recorded as clicks" in q else "variable" if "Is that the same every time" in q
            else "start" if "Should I check that before I start" in q else "confirm" if "What should I call it" in q or "I will save it as" in q
            else "goal")
    asked.append(kind)
    entry, took = send(replies[kind], 30)
    check(f"answer `{replies[kind]}` to the {kind} question comes back promptly", entry is not None and took < 15, f"{took:.1f}s")
print("asked:", asked)
check("positional clicks asked about once", asked.count("coordinates") <= 1, str(asked))
final = entry["content"] if entry else ""
print("\n--- lesson ends ---\n" + final + "\n---")
check("skill saved", entry is not None and entry["kind"] != "query" and "Flamingo search" in final)

# Run it with another value, from where the recording started (no browser open).
m = re.search(r"`(/skill [^`]+)`", final)
run = m.group(1) if m else ""
pm = re.search(r"(\w+)=", run)
param = pm.group(1) if pm else "typed_text"
sh(CLOSE_BROWSER); time.sleep(3)
entry, took = send(f'/skill flamingo-search {param}="penguin facts"', 300)
print("\n--- run 1 ---\n" + (entry["content"] if entry else "(nothing)") + "\n---")
check("skill run from a clean start finishes", entry is not None and entry["kind"] != "query", f"{took:.1f}s")
title = sh("xdotool getactivewindow getwindowname")
check("the browser shows the new search", searched(title, "penguin facts"), title)

# Again, with the browser already open over the icon it double-clicks: the icon is the browser's own
# launcher, so the run brings the open browser forward instead of clicking where the icon was drawn.
entry, took = send(f'/skill flamingo-search {param}="owl facts"', 300)
print("\n--- run 2 ---\n" + (entry["content"] if entry else "(nothing)") + "\n---")
check("with the icon covered it uses the open browser, not a blind click", entry is not None and entry["kind"] != "query", f"{took:.1f}s")
title = sh("xdotool getactivewindow getwindowname")
check("the browser shows the second search", searched(title, "owl facts"), title)

# With the browser closed and nothing to open it by (the icon is gone), the run must not click where the
# icon was drawn: it stops and asks, and `stop` ends it.
# The browser's icon, found by what it runs (its file name has changed between images).
ICON = sh("grep -l '^Exec=chromium-browser' ~/Desktop/*.desktop | head -1")
sh(CLOSE_BROWSER); sh(f"mv '{ICON}' /tmp/icon.desktop.off"); time.sleep(3)
check("the browser's desktop icon is out of the way", bool(ICON) and not sh(f"ls '{ICON}' 2>/dev/null"), ICON)
try:
    entry, took = send(f'/skill flamingo-search {param}="heron facts"', 300)
    print("\n--- run 3 ---\n" + (entry["content"] if entry else "(nothing)") + "\n---")
    check("with nothing to open it by it stops and asks rather than clicking blind", entry is not None and entry["kind"] == "query", f"{took:.1f}s")
    if entry is not None and entry["kind"] == "query":
        entry, took = send("stop", 120)
        check("`stop` ends the run", entry is not None and entry["kind"] != "query", f"{took:.1f}s")
finally:
    sh(f"mv /tmp/icon.desktop.off '{ICON}'")

tasks = json.load(urllib.request.urlopen(f"{B}/api/tasks?worker={DESK}"))
check("nothing left running", all(t["status"] not in ("running", "queued") for t in tasks),
      str([(t["request"][:30], t["status"]) for t in tasks[:6]]))
failed = [r for r in results if not r[1]]
print(f"\n{len(results) - len(failed)}/{len(results)} passed")
sys.exit(1 if failed else 0)
