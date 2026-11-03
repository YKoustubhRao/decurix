#!/usr/bin/env python3
"""
Live end-to-end check of importing other agents' work: BrowserGym solves MiniWoB++
tasks; Decurix imports what it did (as a BrowserGym / AgentLab trajectory and as
the event stream OpenHands would have saved) and replays it, with no model, in a
real desktop's Chromium - and the task page's own reward says whether it worked.

    python3 bench/agents/e2e.py --traj <dir> --miniwob <dir> [--keep] [--formats a,b]

    --negative  replay the password tasks with a wrong password; the page must NOT score them as solved
    --traj      what gen_trajectories.py wrote (needs BrowserGym; see that file)
    --miniwob   the `miniwob` wheel's `miniwob/html` directory (the task pages)

Needs Docker and a built decurix-desktop:basic. A few minutes; leaves nothing
behind unless --keep is given.

What it reports, per format: how many replays the benchmark scored as solved -
against BrowserGym's own score for the same instance - and, for the
OpenHands trajectories that had their accessibility trees stripped when saved,
that nothing was done at all: a step it cannot find a control for stops the run
before anything is clicked, and the task's reward stays where it was.
"""

import json
import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from agent_token import PY_AGENT, docker_env as token_env  # noqa: E402

REPO = str(Path(__file__).resolve().parents[2])
NAME = "decurix-e2e-agents"
IMAGE = "decurix-desktop:basic"

FORMATS = ["browsergym", "openhands", "openhands-saved"]


def sh(*args, user="headless", timeout=900, check=True):
    r = subprocess.run(["docker", "exec", "-u", user, "-e", "PYTHONPATH=/opt", NAME, *args],
                       capture_output=True, text=True, timeout=timeout)
    if check and r.returncode:
        raise SystemExit(f"FAILED {args}: rc={r.returncode}\n{r.stdout}\n{r.stderr}")
    return r


def option(flag, default=None):
    args = sys.argv[1:]
    return args[args.index(flag) + 1] if flag in args else default


def main():
    traj, pages = option("--traj"), option("--miniwob")
    formats = (option("--formats") or ",".join(FORMATS)).split(",")

    if not traj or not pages:
        raise SystemExit(__doc__)

    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME, "--add-host", "host.docker.internal:host-gateway", "--shm-size=1g",
        "--security-opt", f"seccomp={REPO}/internal/desktop/seccomp-desktop.json", "--ulimit", "core=0",
        "-e", "VNC_PW=admin", "-e", "VNC_RESOLUTION=1280x720", "-e", "DESKTOP_ID=e2e", "-e", "DESKTOP_NAME=e2e",
        "-e", "CONTROL_PLANE_URL=http://host.docker.internal:18082", "-e", "AGENT_HARNESS=ollama", *token_env(),
        "-e", "DECURIX_REPLAY_FIND_SECONDS=5", "-e", "DECURIX_REPLAY_EXPECT_SECONDS=3",
        "-v", f"{REPO}/agent/decurix_agent:/opt/decurix_agent:ro", IMAGE,
    ], check=True, capture_output=True)

    results = []
    try:
        results = run(traj, pages, formats)
    finally:
        if "--keep" not in sys.argv:
            subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)

    sys.exit(report(results, formats))


def run(traj, pages, formats):
    print("waiting for the desktop's agent...")
    for _ in range(80):
        if sh("python3", "-c", PY_AGENT + "agent('/recordings', timeout=3)",
              check=False).returncode == 0:
            break
        time.sleep(3)
    else:
        raise SystemExit("the agent never came up")

    # The trajectories name the pages by where BrowserGym found them; here they are in the desktop's own folder.
    staged = Path(traj) / "_staged"
    staged.mkdir(exist_ok=True)
    for f in Path(traj).glob("*.json"):
        text = re.sub(r"file:///[^\"]*?/miniwob/", "file:///home/headless/work/miniwob-html/miniwob/", f.read_text())
        (staged / f.name).write_text(text)

    subprocess.run(["docker", "cp", pages, f"{NAME}:/home/headless/work/miniwob-html"], check=True, capture_output=True)
    subprocess.run(["docker", "cp", str(staged), f"{NAME}:/home/headless/work/traj"], check=True, capture_output=True)
    subprocess.run(["docker", "cp", str(Path(__file__).with_name("driver.py")), f"{NAME}:/home/headless/driver.py"],
                   check=True, capture_output=True)
    subprocess.run(["docker", "exec", "-u", "root", NAME, "chown", "-R", "headless:headless", "/home/headless/work",
                    "/home/headless/driver.py"], check=True, capture_output=True)

    print(f"replaying every episode, in {', '.join(formats)}...\n")
    results = []
    negative = ["-e", "E2E_WRONG_SECRETS=1", "-e", "E2E_TASKS=login-user,enter-password"] if "--negative" in sys.argv else []
    proc = subprocess.Popen(["docker", "exec", "-u", "headless", "-e", "PYTHONPATH=/opt", *negative, NAME, "python3",
                             "/home/headless/driver.py", "/home/headless/work/traj", ",".join(formats)],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    for line in proc.stdout:
        try:
            r = json.loads(line)
        except ValueError:
            continue
        results.append(r)
        print(describe(r))

    proc.wait()
    if proc.returncode:
        print(proc.stderr.read()[-1500:])
    return results


def solved(r):
    return isinstance(r.get("raw"), (int, float)) and r["raw"] > 0


def describe(r):
    if r.get("error"):
        return f"  ERR {r['name']:22s} {r['format']:16s} {r['error'][:150]}"

    mark = "ok " if solved(r) else "-- "
    return (f"  {mark}{r['name']:22s} {r['format']:16s} {r['outcome']:22s} reward={r['raw']!s:5s} "
            f"{r['seconds']:5.1f}s")


def report(results, formats):
    print("\n== result")
    problems = []

    if "--negative" in sys.argv:
        # A control for the benchmark itself: with the wrong secret nothing may score as solved.
        wrong = [r for r in results if solved(r)]
        print(f"\n  with the wrong password: {len(wrong)} of {len(results)} scored as solved "
              f"(rewards: {sorted({r.get('raw') for r in results if not r.get('error')}, key=str)})")
        print("\nNEGATIVE CONTROL:", "FAILED - the benchmark called a wrong password a success" if wrong else "OK - it noticed")
        return 1 if wrong else 0

    for fmt in formats:
        rows = [r for r in results if r["format"] == fmt and not r.get("error")]
        errors = [r for r in results if r["format"] == fmt and r.get("error")]
        won = [r for r in rows if solved(r)]

        print(f"\n  {fmt}: {len(won)} of {len(rows) + len(errors)} solved on the task page's own reward"
              f" (BrowserGym: {sum(1 for r in rows if r['browsergym_reward'] > 0)} of {len(rows)})"
              + (f", {len(errors)} errored" if errors else ""))

        by_task = {}
        for r in rows:
            by_task.setdefault(r["task"], []).append(solved(r))
        for task, marks in sorted(by_task.items()):
            print(f"     {task:18s} {sum(marks)}/{len(marks)}")

        if fmt == "openhands-saved":
            # Trees stripped: it must do nothing, not something.
            # "halted_before_effect" is the replay saying no step ran. Judged by that, not by the
            # reward alone: typing into whatever has focus leaves the reward where it was.
            acted = [r for r in rows if r["outcome"] != "halted_before_effect" or r.get("done")
                     or (r.get("raw") not in (0, None))]
            print(f"     stopped before touching anything: {len(rows) - len(acted)} of {len(rows)}")
            if acted:
                problems.append(f"{len(acted)} stripped trajectories did something on the page")
            continue

        for r in rows:
            if not solved(r):
                problems.append(f"{r['name']} [{fmt}]: {r['outcome']} reward={r['raw']} :: {r['note'][:200]}")
        problems += [f"{r['name']} [{fmt}]: {r['error']}" for r in errors]

    if problems:
        print("\n  not solved:")
        for p in problems[:25]:
            print("   -", p)

    print("\nEND TO END:", "FAILED" if problems else "OK")
    return 1 if problems else 0


main()
