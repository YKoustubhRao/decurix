#!/usr/bin/env python3
"""
Live end-to-end check of teaching and running a skill on a real desktop.

    python3 bench/skills/e2e.py [--keep]

Needs Docker and a built decurix-desktop:basic (scripts/*/build-images.sh).
Takes about two minutes and leaves nothing behind unless --keep is given.

A real decurix-desktop container (real Xvnc, XFCE, Chromium) runs this
working tree's agent (bind-mounted over /opt/decurix_agent). A mock control
plane on the host catches what the agent posts (its questions and replies).
Recordings are made by the agent driving real Chromium through `gui`; /learn
and /skill are then sent to the agent's real HTTP API and the questions are
answered as a person in the chat would.
"""

import json
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = str(Path(__file__).resolve().parents[2])
NAME = "decurix-e2e"
PORT = 18080
IMAGE = "decurix-desktop:basic"

posts = []


class Mock(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
        try:
            data = json.loads(body)
        except ValueError:
            data = {"raw": body[:200].decode("utf-8", "replace")}
        posts.append((self.path, data))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *a):
        pass


def sh(*args, check=True, timeout=120, user="headless"):
    cmd = ["docker", "exec", "-u", user, NAME, *args]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if check and r.returncode:
        raise SystemExit(f"FAILED {args}: rc={r.returncode}\n{r.stdout}\n{r.stderr}")
    return r


def bash(script, **kw):
    return sh("bash", "-lc", script, **kw)


def replies(kind=None):
    return [d for p, d in posts if p == "/api/querytext" and (kind is None or d.get("kind") == kind)]


def wait_reply(count, timeout=150):
    end = time.time() + timeout
    while time.time() < end:
        got = replies()
        if len(got) >= count:
            return got[count - 1]
        time.sleep(1)
    raise SystemExit(f"timed out waiting for reply #{count}; got {len(replies())}:\n"
                     + "\n".join(str(r.get("kind")) + ": " + str(r.get("reply"))[:200] for r in replies()))


HTTP = ("import sys,json,urllib.request as u;"
        "d=sys.argv[2] if len(sys.argv)>2 else None;"
        "r=u.Request('http://localhost:8081'+sys.argv[1],data=d.encode() if d else None,method='POST' if d else 'GET');"
        "print(u.urlopen(r,timeout=20).read().decode())")


def agent(path, body=None):
    args = ["docker", "exec", NAME, "python3", "-c", HTTP, path] + ([json.dumps(body)] if body is not None else [])
    r = subprocess.run(args, capture_output=True, text=True, timeout=60)
    return r.stdout.strip() if not r.returncode else "ERR " + r.stderr.strip()[-300:]


def send(content, task_id):
    out = agent("/request", {"content": content, "task_id": task_id})
    assert '"received"' in out, out


def show(label, reply):
    print(f"\n--- {label} [{reply.get('kind')}, engine={reply.get('engine')}]")
    print(reply.get("reply") or reply.get("content"))


FORM = """<!doctype html><meta charset=utf-8><title>Customers</title>
<h1>Customers</h1>
<label>Customer name <input id=n type=text aria-label="Customer name"></label>
<button id=save onclick="var o=document.getElementById('out');o.textContent='Saved '+document.getElementById('n').value;o.setAttribute('role','status')">SAVEWORD</button>
<p id=out></p>
"""


def main():
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Mock)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME, "--add-host", "host.docker.internal:host-gateway", "--shm-size=1g",
        "--security-opt", f"seccomp={REPO}/internal/desktop/seccomp-desktop.json", "--ulimit", "core=0",
        "-e", "VNC_PW=admin", "-e", "VNC_RESOLUTION=1280x720", "-e", "DESKTOP_ID=e2e", "-e", "DESKTOP_NAME=e2e",
        "-e", f"CONTROL_PLANE_URL=http://host.docker.internal:{PORT}", "-e", "AGENT_HARNESS=ollama",
        "-e", "OLLAMA_URL=http://host.docker.internal:11434", "-e", "DECURIX_REPLAY_FIND_SECONDS=6",
        # This benchmark verifies deterministic healing and human correction.
        # A live model could choose the renamed button or wait for a cold load,
        # bypassing the question this scenario is specifically meant to test.
        "-e", "DECURIX_ESCALATION_MODEL_CALLS=0",
        "-v", f"{REPO}/agent/decurix_agent:/opt/decurix_agent:ro", IMAGE,
    ], check=True)

    try:
        run()
    except BaseException:
        print("\n== failure diagnostics", flush=True)
        print(bash("gui observe", check=False).stdout, flush=True)
        logs = subprocess.run(["docker", "logs", "--tail", "80", NAME], capture_output=True, text=True)
        print(logs.stdout + logs.stderr, flush=True)
        raise
    finally:
        if "--keep" not in sys.argv:
            subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)


def run():
    print("waiting for the desktop's agent (this build's /skills endpoint)...")
    end = time.time() + 240
    while time.time() < end:
        out = agent("/skills")
        if out.startswith('{"skills"'):
            break
        time.sleep(3)
    else:
        print(subprocess.run(["docker", "exec", NAME, "tail", "-30", "/tmp/decurix-startup.log"], capture_output=True, text=True).stdout)
        raise SystemExit("agent never came up")
    print("agent is up:", out)

    bash("mkdir -p ~/work && cat > ~/work/form.html <<'EOF'\n" + FORM.replace("SAVEWORD", "Save") + "EOF")

    # Two demonstrations of the same task, each its own recording.
    for n, (task_id, customer) in enumerate((("t00000000000000a1", "Acme"), ("t00000000000000a2", "Globex")), 1):
        bash(f"printf %s {task_id} > /tmp/decurix-current-task; printf %s 'Add a customer' > /tmp/decurix-current-task.request")
        print(f"\n== demonstration {n}: {customer}")
        for cmd in ('gui open file:///home/headless/work/form.html',
                    f'gui type "Customer name" "{customer}"',
                    'gui click "Save"'):
            r = bash(cmd, timeout=90)
            print(f"$ {cmd}\n{r.stdout.strip()[:300]}{r.stderr.strip()[:300]}")
            if r.returncode:
                raise SystemExit(f"gui failed rc={r.returncode}")
        bash("printf %s '' > /tmp/decurix-current-task")

    listing = json.loads(agent("/recordings"))
    print("\nrecordings:", [(m["id"], m["event_count"]) for m in listing["recordings"]])

    # ---- teach ----
    send("/learn task-t00000000000000a1 task-t00000000000000a2", "t00000000000000b1")
    seen = 1
    first = wait_reply(seen)
    show("question 1", first)

    answers = ["the page shows a line beginning Saved", ]
    reply_text = lambda r: (r.get("reply") or "")
    conversation = []
    while True:
        last = replies()[-1]
        text = reply_text(last)
        if last.get("kind") == "final" or last.get("kind") == "error":
            break
        if "Save it as" in text:
            answer = "yes"
        elif "same every time" in text:
            answer = "changes"
        elif "only sometimes happen" in text or "sometimes" in text and "always part" in text:
            answer = "always"
        elif "finished" in text:
            answer = answers[0]
        elif "check that before I start" in text:
            answer = "yes"
        else:
            raise SystemExit(f"a question this script does not know how to answer:\n{text}")
        conversation.append((text[:90].replace("\n", " "), answer))
        seen += 1
        send(answer, "t00000000000000b2")
        show(f"answer -> {answer!r}; next", wait_reply(seen))

    print("\nteaching ended:", last.get("kind"))
    assert last.get("kind") == "final", "teaching did not finish"
    skill = json.loads(subprocess.run(["docker", "exec", NAME, "cat", "/home/headless/.decurix/skills/add-a-customer.json"],
                                      capture_output=True, text=True).stdout)
    print("skill status:", skill["status"], "| steps:", [s["summary"] for s in skill["steps"]],
          "| params:", [(p["name"], p["varies"]) for p in skill["params"]])
    assert skill["status"] == "ready"

    # ---- do it ----
    print("\n== running the skill")
    before = len(replies())
    send("/skill add a customer", "t00000000000000c1")
    q = wait_reply(before + 1)
    show("it asks", q)
    assert "customer_name" in q["reply"] and q["kind"] == "query"

    send("Initech", "t00000000000000c2")
    done = wait_reply(before + 2, timeout=180)
    show("result", done)
    assert done["kind"] == "final", "the skill run did not complete"

    v = bash("gui verify --text 'Saved Initech'", check=False)
    print("$ gui verify --text 'Saved Initech' ->", v.returncode, v.stdout.strip()[:200])
    assert v.returncode == 0, "the real page does not show what the skill typed"

    # ---- the form changed ----
    # A rename the healing ladder is sure of ("Save" -> "Save changes": same words, same
    # place, same neighbours) is repaired without a question, and says so.
    print("\n== the Save button is renamed to something plainly the same; the skill heals it and says so")
    bash("sed -i 's/>Save</>Save changes</' ~/work/form.html")
    before = len(replies())
    send('/skill add-a-customer customer_name="Umbrella"', "t00000000000000d1")
    healed = wait_reply(before + 1, timeout=180)
    show("result", healed)
    assert healed["kind"] == "final" and "repaired step 3" in healed["reply"], "the rename was not healed"

    v = bash("gui verify --text 'Saved Umbrella'", check=False)
    print("$ gui verify --text 'Saved Umbrella' ->", v.returncode)
    assert v.returncode == 0

    # One it cannot be sure of ("Save changes" -> "Go": nothing in common) is a question,
    # not a guess - and what the person answers is remembered only because it then worked.
    print("\n== the button is renamed to something it cannot recognise; the skill must ask instead of guessing")
    bash("sed -i 's/>Save changes</>Go</' ~/work/form.html")
    before = len(replies())
    send('/skill add-a-customer customer_name="Wayne"', "t00000000000000e1")
    stuck = wait_reply(before + 1, timeout=180)
    show("it asks", stuck)
    assert stuck["kind"] == "query" and "is not on the screen" in stuck["reply"]

    send('use "Go"', "t00000000000000e2")
    fixed = wait_reply(before + 2, timeout=180)
    show("result", fixed)
    assert fixed["kind"] == "final" and "remembered" in fixed["reply"]

    v = bash("gui verify --text 'Saved Wayne'", check=False)
    print("$ gui verify --text 'Saved Wayne' ->", v.returncode)
    assert v.returncode == 0

    skill = json.loads(subprocess.run(["docker", "exec", NAME, "cat", "/home/headless/.decurix/skills/add-a-customer.json"],
                                      capture_output=True, text=True).stdout)
    save = [s for s in skill["steps"] if s.get("target", {}).get("role") == "button"][0]
    print("skill now: version", skill["version"], "| runs", skill["runs"], "| button:", save["target"]["name"], "| learned:", skill["learned"])
    assert save["target"]["name"] == "Go" and skill["version"] >= 2

    # ---- what an engine sees ----
    print("\n== gui skills / gui skill")
    print(bash("gui skills").stdout.strip())
    print(bash("gui skill add-a-customer").stdout.strip())
    print("\nEND TO END: OK")


main()
