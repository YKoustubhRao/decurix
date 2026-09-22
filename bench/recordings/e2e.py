#!/usr/bin/env python3
"""
Live end-to-end check of recordings on a real desktop: what a click on
something the screen exposes no element for leaves in a recording, whether a
replay finds it again where it moved, and - what matters more - that it will
not click the wrong thing.

    python3 bench/recordings/e2e.py [--keep]

Needs Docker and a built decurix-desktop:basic (scripts/*/build-images.sh).
Takes about three minutes and leaves nothing behind unless --keep is given.

A real decurix-desktop container (real Xvnc, XFCE, Chromium) runs this working
tree's agent, bind-mounted. Its Chromium shows a page whose list is drawn on a
<canvas>: the accessibility tree has nothing for it, so a click there can only
be recorded as a position - the case a picture of the surroundings exists for.
Every outcome is judged by what the page then shows, not by what the replay
said about itself.
"""

import json
import subprocess
import sys
import time
from pathlib import Path

REPO = str(Path(__file__).resolve().parents[2])
NAME = "decurix-e2e-rec"
IMAGE = "decurix-desktop:basic"

PAGE = """<!doctype html><meta charset=utf-8><title>Invoices</title>
<style>body{margin:0;background:#f4f4f4;font:15px sans-serif}canvas{position:absolute;left:%(left)dpx;top:%(top)dpx}</style>
<canvas id=c width=420 height=200></canvas>
<p id=out role=status style="position:absolute;left:300px;top:440px">Nothing pressed</p>
<script>
var rows=%(rows)s, c=document.getElementById("c"), g=c.getContext("2d");
g.fillStyle="#f4f4f4"; g.fillRect(0,0,420,200);
rows.forEach(function(t,i){var y=20+i*56;g.fillStyle="#fff";g.fillRect(10,y,300,40);g.strokeStyle="#a0aec0";g.strokeRect(10.5,y+.5,300,40);
  g.fillStyle="#222";g.font="16px sans-serif";g.fillText(t,24,y+26)});
c.addEventListener("click",function(e){var r=c.getBoundingClientRect(),y=e.clientY-r.top,i=Math.floor((y-20)/56);
  if(i>=0&&i<rows.length&&(y-20)%%56<40){document.getElementById("out").textContent="Opened "+rows[i]}});
</script>
"""

FORM = """<!doctype html><meta charset=utf-8><title>Customers</title>
<h1>Customers</h1>
<label>Customer name <input type=text aria-label="Customer name"></label><br>
<label>Work email <input type=text aria-label="Work email"></label><br>
<button onclick="var o=document.getElementById('out');o.textContent='Saved '+document.querySelector('[aria-label=\\'Customer name\\']').value+' '+document.querySelector('[aria-label=\\'Work email\\']').value;o.setAttribute('role','status')">Save</button>
<p id=out></p>
"""

# What Chrome's DevTools Recorder exports for filling that form in (aria selectors first, as it writes them).
EXPORT = {
    "title": "Add a customer",
    "steps": [
        {"type": "setViewport", "width": 1280, "height": 720, "deviceScaleFactor": 1, "isMobile": False},
        {"type": "navigate", "url": "file:///home/headless/work/form.html",
         "assertedEvents": [{"type": "navigation", "url": "file:///home/headless/work/form.html", "title": "Customers"}]},
        {"type": "click", "selectors": [['aria/Customer name[role="textbox"]'], ["input:nth-of-type(1)"]], "offsetX": 5, "offsetY": 5},
        {"type": "change", "value": "Acme", "selectors": [['aria/Customer name[role="textbox"]'], ["input:nth-of-type(1)"]]},
        {"type": "click", "selectors": [['aria/Work email[role="textbox"]']]},
        {"type": "change", "value": "alice@acme.com", "selectors": [['aria/Work email[role="textbox"]']]},
        {"type": "click", "selectors": [['aria/Save[role="button"]'], ["button"]]},
        {"type": "hover", "selectors": [['aria/Save[role="button"]']]},
        {"type": "scroll", "x": 0, "y": 200},
    ],
}

ROWS = ["Invoice 1041 Paid", "Invoice 1042 Paid", "Invoice 1043 Due"]
# The second row, in the middle: canvas at (300,200) plus the browser's own 109 px of tabs and toolbar.
SECOND_ROW = (460, 405)

failures = []


def sh(*args, user="headless", timeout=180, check=True):
    r = subprocess.run(["docker", "exec", "-u", user, "-e", "PYTHONPATH=/opt", NAME, *args],
                       capture_output=True, text=True, timeout=timeout)
    if check and r.returncode:
        raise SystemExit(f"FAILED {args}: rc={r.returncode}\n{r.stdout}\n{r.stderr}")
    return r


def bash(script, **kw):
    return sh("bash", "-lc", script, **kw)


def page(rows=ROWS, left=300, top=200):
    body = PAGE % {"left": left, "top": top, "rows": json.dumps(rows)}
    bash("cat > ~/work/invoices.html <<'EOF'\n" + body + "EOF")


def shows(text):
    return bash(f"gui verify --text {json.dumps(text)}", check=False).returncode == 0


def check(name, ok, detail=""):
    print(f"  {'ok ' if ok else 'BAD'} {name}" + (f"  ({detail})" if detail and not ok else ""))
    if not ok:
        failures.append(name)


def replay(session, *extra):
    r = bash(f"gui replay {session} {' '.join(extra)}", check=False)
    return r.returncode, (r.stdout + r.stderr).strip()


def imported():
    print("\n== a recording made in Chrome's DevTools Recorder, imported and replayed here")
    bash("cat > ~/work/form.html <<'EOF'\n" + FORM + "EOF")
    body = json.dumps({"data": EXPORT})
    r = bash(f"python3 - <<'EOF'\nimport json,urllib.request as u\n"
             f"d={body!r}\n"
             f"print(u.urlopen(u.Request('http://localhost:8081/recordings/import',data=d.encode(),method='POST'),timeout=20).read().decode())\nEOF")
    result = json.loads(r.stdout)
    print("  imported:", {k: result[k] for k in ("session", "imported", "ignored")}, "skipped:", [(x["type"], x["why"][:40]) for x in result["skipped"]])
    session = result["session"]
    check("it became a recording", result["imported"] == 6, str(result))
    check("hover and scroll were left out, with a reason each", {x["type"] for x in result["skipped"]} == {"hover", "scroll"})

    show = json.loads(bash(f"python3 -c \"import json,urllib.request as u;print(u.urlopen('http://localhost:8081/recordings/{session}').read().decode())\"").stdout)
    program = show["program"]
    typed = [st for st in program["steps"] if st["kind"] == "type"]
    check("it is graded", show["lint"]["grade"] in ("ready", "rehearse"), show["lint"]["summary"])
    check("each typed field is checked to hold what was typed", len(typed) == 2
          and all(any(c["type"] == "value" for c in st["expect"]) for st in typed), json.dumps(typed)[:300])
    print("  lint:", show["lint"]["grade"], "-", show["lint"]["summary"][:120])

    rc, text = replay(session, "--param customer_name=Initech", "--param work_email=bob@initech.io")
    print(f"  replay: rc={rc}: {text.splitlines()[0][:160]}")
    check("it filled in and saved the form on the real page", shows("Saved Initech bob@initech.io"), text[:200])

    bash("gui open file:///home/headless/work/form.html", timeout=90)
    rc, text = replay(session, "--param work_email=not-an-email")
    print(f"  replay with a bad email: rc={rc}: {text.splitlines()[0][:160]}")
    check("a value the field cannot take is refused before anything is typed", "is not an email address" in text and rc != 0, text[:200])
    check("and the page is untouched", not shows("Saved"))


def main():
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME, "--add-host", "host.docker.internal:host-gateway", "--shm-size=1g",
        "--security-opt", f"seccomp={REPO}/internal/desktop/seccomp-desktop.json", "--ulimit", "core=0",
        "-e", "VNC_PW=admin", "-e", "VNC_RESOLUTION=1280x720", "-e", "DESKTOP_ID=e2e", "-e", "DESKTOP_NAME=e2e",
        "-e", "CONTROL_PLANE_URL=http://host.docker.internal:18081", "-e", "AGENT_HARNESS=ollama",
        "-e", "DECURIX_REPLAY_FIND_SECONDS=5", "-e", "DECURIX_REPLAY_EXPECT_SECONDS=3",
        "-v", f"{REPO}/agent/decurix_agent:/opt/decurix_agent:ro", IMAGE,
    ], check=True, capture_output=True)

    try:
        run()
    finally:
        if "--keep" not in sys.argv:
            subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)

    print("\nEND TO END:", "FAILED - " + ", ".join(failures) if failures else "OK")
    sys.exit(1 if failures else 0)


def run():
    print("waiting for the desktop's agent...")
    for _ in range(80):
        if sh("python3", "-c", "import urllib.request as u;u.urlopen('http://localhost:8081/recordings',timeout=3)", check=False).returncode == 0:
            break
        time.sleep(3)
    else:
        raise SystemExit("the agent never came up")

    bash("mkdir -p ~/work")

    # ---- record a click on a drawn row: no element, only a position ----
    print("\n== recording a click on the second row of a list drawn on a canvas")
    page()
    bash("printf %s t00000000000000f1 > /tmp/decurix-current-task; printf %s 'Open the second invoice' > /tmp/decurix-current-task.request")
    bash("gui open file:///home/headless/work/invoices.html", timeout=90)
    time.sleep(1)
    out = bash(f"gui click {SECOND_ROW[0]},{SECOND_ROW[1]}", timeout=90).stdout
    bash("printf %s '' > /tmp/decurix-current-task")
    session = "task-t00000000000000f1"

    check("the click made the page show the row", shows("Opened Invoice 1042 Paid"), out)

    info = json.loads(bash(f"""python3 - <<'EOF'
import json
from decurix_agent.computer import lint, recording, replay
meta, events = recording.load("{session}")
program = replay.compile_program(meta, events)
click = next(s for s in program["steps"] if s["kind"] == "click")
verdict = lint.lint(program)
print(json.dumps({{"target": click.get("target"), "point": click.get("point"), "has_look": bool(click.get("look")),
                   "detail": (click.get("look") or {{}}).get("detail"), "partial": (click.get("look") or {{}}).get("partial"),
                   "grade": verdict["grade"], "codes": [f["code"] for f in verdict["findings"]]}}))
EOF""").stdout)
    print("  recorded:", info)
    check("it was recorded as a position, not an element", info["target"] is None and info["point"] == list(SECOND_ROW))
    check("and kept a picture of what was around it", info["has_look"])
    check("lint says it is anchored", "anchored_clicks" in info["codes"] and "blind_click" not in info["codes"])

    def scenario(title, **page_args):
        print(f"\n== {title}")
        page(**page_args)
        t = time.time()
        rc, text = replay(session)
        print(f"  replay: rc={rc} in {time.time() - t:.1f}s: {text.splitlines()[0][:200] if text else ''}")
        return rc, text

    # ---- as recorded ----
    scenario("the page exactly as it was recorded")
    check("clicked the row", shows("Opened Invoice 1042 Paid"))

    # ---- it has moved ----
    scenario("the whole list has moved (a resized window, a scrolled page)", left=347, top=228)
    check("followed it and opened the right row", shows("Opened Invoice 1042 Paid"))

    # ---- the row the click was for is one place up ----
    scenario("the rows were reordered: the one that was meant is now first", rows=["Invoice 1042 Paid", "Invoice 1041 Paid", "Invoice 1043 Due"])
    check("clicked 1042, where it is now", shows("Opened Invoice 1042 Paid"))
    check("and did not click 1041, where the click was recorded", not shows("Opened Invoice 1041 Paid"))

    # ---- what must never happen ----
    scenario("a different row where it was, and the meant row gone", rows=["Invoice 1041 Paid", "Invoice 1042 Due", "Invoice 1043 Due"])
    check("clicked nothing", shows("Nothing pressed"))

    scenario("the row was removed: the next one moved up into its place", rows=["Invoice 1041 Paid", "Invoice 1043 Due"])
    check("clicked nothing", shows("Nothing pressed"))

    rc, text = scenario("the page is not there at all", rows=[])
    check("clicked nothing", shows("Nothing pressed"))
    check("and said why", "does not click there" in text or "not on the screen" in text, text[:200])

    imported()

    print("\n== how long a replay of an anchored click takes when nothing is wrong")
    page()
    t = time.time()
    replay(session)
    print(f"  {time.time() - t:.1f}s for open + click + check on this desktop")


main()
