"""
The desktop screen viewer (web/templates/screen.html) against the real control plane and a real desktop:
opens with no password prompt, loads in two requests, fits the desktop to the window, the session bar
hides and returns, the Record panel lives in it, clipboard both ways, typing, reconnect after a restart,
waking a paused desktop, starting a stopped one, and a phone (touch keyboard, pixel-exact scaling).

Needs Playwright (`uv venv /tmp/pw && uv pip install -p /tmp/pw/bin/python playwright &&
/tmp/pw/bin/playwright install chromium`) and a running desktop it may restart, pause and stop:

    /tmp/pw/bin/python bench/screen/e2e.py <desktop id> [--only open|reconnect|paused|stopped|touch]
"""
import json, os, subprocess, sys, tempfile, time, urllib.parse, urllib.request
HERE = os.path.dirname(os.path.abspath(__file__))
from playwright.sync_api import sync_playwright

B = "http://127.0.0.1:8080"
DESK = sys.argv[1]
ONLY = sys.argv[sys.argv.index("--only") + 1] if "--only" in sys.argv else None
OUT = os.environ.get("SCREEN_E2E_OUT", os.path.join(tempfile.gettempdir(), "decurix-screen-e2e"))
os.makedirs(OUT, exist_ok=True)
results = []


def api(path, **form):
    req = urllib.request.Request(B + path, data=urllib.parse.urlencode(dict(id=DESK, **form)).encode())
    return urllib.request.urlopen(req, timeout=120).read().decode()


def status():
    return json.load(urllib.request.urlopen(f"{B}/api/desktops/activity?ids={DESK}"))[DESK].get("status")


def sh(cmd):
    return subprocess.run(["docker", "exec", "-u", "headless", "-e", "DISPLAY=:1", f"desktop_{DESK}", "bash", "-lc", cmd],
                          capture_output=True, text=True, timeout=60).stdout.strip()


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS " if ok else "FAIL ") + name + (f" - {detail}" if detail else ""), flush=True)


def wait_live(pg, limit=120):
    t0 = time.time()
    pg.wait_for_function("() => document.getElementById('screen').classList.contains('live') && "
                         "document.getElementById('veil').classList.contains('gone')", timeout=limit * 1000)
    return time.time() - t0


def veil_text(pg):
    return pg.evaluate("() => document.getElementById('veil').classList.contains('gone') ? '' : document.getElementById('veil-status').textContent")


def run(name):
    return ONLY is None or ONLY == name


with sync_playwright() as p:
    browser = p.chromium.launch()
    ctx = browser.new_context(viewport={"width": 1440, "height": 810})
    ctx.grant_permissions(["clipboard-read", "clipboard-write"], origin=B)
    url = f"{B}/desktop/{DESK}/?autoconnect=true&resize=remote&ui=touch"

    if run("open"):
        pg = ctx.new_page()
        reqs = []
        pg.on("request", lambda r: reqs.append(r.url))
        errors = []
        pg.on("pageerror", lambda e: errors.append(str(e)))
        t0 = time.time()
        pg.goto(url)
        first = wait_live(pg)
        total = time.time() - t0
        pg.wait_for_timeout(600)
        pg.screenshot(path=f"{OUT}/open.png")
        check("opens straight onto the desktop (no password prompt)", True, f"{total:.2f}s to first picture")
        assets = [r for r in reqs if "/api/" not in r]
        check("the page loads in two requests", len(assets) == 2, f"{[r.rsplit('/', 1)[-1][:30] for r in assets]}")
        check("no script errors", not errors, "; ".join(errors)[:300])
        size = pg.evaluate("() => { const c = document.querySelector('#screen canvas'); return [c.width, c.height]; }")
        check("desktop resized to the window", size == [1440, 810], str(size))
        xr = sh("xrandr | head -1")
        check("Xvnc really at window size", "current 1440 x 810" in xr, xr)
        # The bar peeks on first connect then hides.
        pg.wait_for_timeout(3500)
        hidden = pg.evaluate("() => !document.getElementById('session').classList.contains('open')")
        check("session bar tucks itself away", hidden)
        pg.mouse.move(720, 400); pg.mouse.move(720, 2)
        pg.wait_for_timeout(500)
        check("session bar comes back at the top edge", pg.evaluate("() => document.getElementById('session').classList.contains('open')"))
        pg.screenshot(path=f"{OUT}/bar.png", clip={"x": 420, "y": 0, "width": 600, "height": 90})
        pg.mouse.move(720, 500); pg.wait_for_timeout(900)
        check("and leaves again", pg.evaluate("() => !document.getElementById('session').classList.contains('open')"))

        # Record lives in the bar: its panel opens under it and keeps the bar out.
        pg.mouse.move(720, 2); pg.wait_for_timeout(400)
        pg.click("#decurix-ticket-tab")
        pg.wait_for_timeout(400)
        panel = pg.evaluate("() => { const p = document.getElementById('decurix-ticket-panel').getBoundingClientRect(); return [p.width, p.height, p.top]; }")
        check("Record panel opens under the bar", panel[0] > 200 and panel[1] > 40 and panel[2] > 30, str(panel))
        pg.mouse.move(720, 600); pg.wait_for_timeout(900)
        check("bar stays while the Record panel is open", pg.evaluate("() => document.getElementById('session').classList.contains('open')"))
        pg.screenshot(path=f"{OUT}/record-panel.png")
        pg.click("#decurix-ticket-tab"); pg.wait_for_timeout(300)

        # Clipboard: desktop -> this computer.
        subprocess.run(f"docker exec -i -u headless desktop_{DESK} bash -c 'cat > /home/headless/.xclip-e2e.py' < {HERE}/xclip.py", shell=True)
        sh("nohup python3 ~/.xclip-e2e.py set 'from the desktop 42' >/dev/null 2>&1 &")
        pg.wait_for_timeout(1500)
        got = pg.evaluate("() => navigator.clipboard.readText()")
        check("copy in the desktop lands on this computer", got == "from the desktop 42", repr(got))

        # Clipboard: this computer -> desktop, by Ctrl+V.
        pg.evaluate("() => navigator.clipboard.writeText('pasted from the laptop 7')")
        pg.mouse.click(720, 400)
        pg.keyboard.press("Control+v")
        pg.wait_for_timeout(1200)
        remote = sh("python3 ~/.xclip-e2e.py get")
        check("Ctrl+V hands the desktop this computer's clipboard", remote == "pasted from the laptop 7", repr(remote))

        # Typing reaches the desktop: open a terminal with the keyboard and write a file.
        sh("rm -f /tmp/typed.txt; xfce4-terminal --disable-server >/dev/null 2>&1 &")
        pg.wait_for_timeout(3500)
        pg.mouse.click(720, 400)
        sh("xdotool search --class xfce4-terminal | tail -1 | xargs -r xdotool windowactivate")
        pg.wait_for_timeout(600)
        pg.keyboard.type("echo typed-through-the-viewer > /tmp/typed.txt\n", delay=15)
        pg.wait_for_timeout(1500)
        typed = sh("cat /tmp/typed.txt 2>/dev/null")
        check("keys typed in the viewer reach the desktop", typed == "typed-through-the-viewer", repr(typed))
        sh("pkill -f 'xfce4-terminal --disable-server' || true")
        pg.close()

    if run("reconnect"):
        pg = ctx.new_page()
        pg.goto(url); wait_live(pg)
        t0 = time.time()
        subprocess.run(["docker", "restart", "-t", "2", f"desktop_{DESK}"], capture_output=True)
        pg.wait_for_function("() => !document.getElementById('veil').classList.contains('gone')", timeout=30000)
        seen = veil_text(pg)
        pg.screenshot(path=f"{OUT}/reconnecting.png")
        took = wait_live(pg, 180)
        check("a restarted desktop reconnects by itself", True, f"said {seen!r}; back {time.time() - t0:.1f}s after the restart")
        pg.close()

    if run("paused"):
        api("/api/desktops/pause")
        time.sleep(1)
        check("desktop is paused before opening", status() == "paused", status())
        pg = ctx.new_page(); t0 = time.time()
        pg.goto(url)
        took = wait_live(pg, 120)
        check("opening a paused desktop wakes it", status() == "running", f"{time.time() - t0:.1f}s")
        pg.close()

    if run("stopped"):
        api("/api/desktops/stop")
        time.sleep(1)
        check("desktop is stopped before opening", status() == "stopped", status())
        pg = ctx.new_page(); t0 = time.time()
        pg.goto(url)
        pg.wait_for_timeout(1500)
        said = veil_text(pg)
        pg.screenshot(path=f"{OUT}/starting.png")
        wait_live(pg, 180)
        check("opening a stopped desktop starts it and connects", True, f"said {said!r}; live after {time.time() - t0:.1f}s")
        pg.close()

    if run("touch"):
        phone = browser.new_context(viewport={"width": 390, "height": 844}, has_touch=True, is_mobile=True, device_scale_factor=3)
        pg = phone.new_page()
        pg.goto(url)
        wait_live(pg)
        pg.wait_for_timeout(3500)
        size = pg.evaluate("() => { const c = document.querySelector('#screen canvas'); return [c.width, c.height]; }")
        check("a phone gets a desktop its own size", size == [390, 844], str(size))
        check("pixel-exact scaling on a 3x screen", pg.evaluate("() => document.getElementById('screen').classList.contains('crisp')"))
        check("keyboard button shown on touch", pg.is_visible("#kbd-btn"))
        pg.tap("#handle"); pg.wait_for_timeout(500)
        check("tapping the handle opens the bar", pg.evaluate("() => document.getElementById('session').classList.contains('open')"))
        pg.screenshot(path=f"{OUT}/phone.png")
        pg.tap("#screen", position={"x": 200, "y": 500}); pg.wait_for_timeout(500)
        check("tapping the desktop tucks the bar away", pg.evaluate("() => !document.getElementById('session').classList.contains('open')"))
        sh("rm -f /tmp/typed-phone.txt; xfce4-terminal --disable-server >/dev/null 2>&1 &")
        pg.wait_for_timeout(3500)
        sh("xdotool search --class xfce4-terminal | tail -1 | xargs -r xdotool windowactivate")
        pg.wait_for_timeout(500)
        pg.tap("#kbd-btn"); pg.wait_for_timeout(300)
        check("the keyboard button focuses the input", pg.evaluate("() => document.activeElement.id === 'kbd'"))
        pg.keyboard.type("echo from-a-phone > /tmp/typed-phone.txt", delay=20)
        pg.keyboard.press("Enter")
        pg.wait_for_timeout(1500)
        typed = sh("cat /tmp/typed-phone.txt 2>/dev/null")
        check("typing on the phone keyboard reaches the desktop", typed == "from-a-phone", repr(typed))
        sh("pkill -f 'xfce4-terminal --disable-server' || true")
        phone.close()

    browser.close()

failed = [r for r in results if not r[1]]
print(f"\n{len(results) - len(failed)}/{len(results)} passed")
sys.exit(1 if failed else 0)
