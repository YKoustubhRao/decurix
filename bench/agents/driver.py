"""
Runs INSIDE a Decurix desktop, driven by bench/agents/e2e.py.

For each episode BrowserGym solved (gen_trajectories.py): open that MiniWoB++ task
page in the desktop's own Chromium, put it in the SAME instance BrowserGym solved
(the seed BrowserGym handed the page's random generator), import the trajectory
in the given format, replay it with `gui replay`, and read the page's own reward.
The reward is what the benchmark says, not what the replay says about itself.

One JSON object per line on stdout.
"""

import json
import os
import re
import subprocess
import sys
import time

from decurix_agent.computer import cdp, ingest, replay

WORK = "/home/headless/work"
PAGES = f"{WORK}/miniwob-html/miniwob"


def gui(*args, timeout=180):
    r = subprocess.run(["gui", *args], capture_output=True, text=True, timeout=timeout)
    return r.returncode, (r.stdout + r.stderr).strip()


def evaluate(expression):
    page = cdp.active_page()
    try:
        return page.evaluate(expression)
    finally:
        page.close()


def prepare(task, seed):
    """The task's page, in the instance `seed` makes - what BrowserGym's own setup does."""

    gui("open", f"file://{PAGES}/{task}.html", timeout=90)
    time.sleep(0.6)
    evaluate(f"Math.seedrandom({seed}); core.EPISODE_MAX_TIME = 1000000; core.startEpisodeReal();")

    for _ in range(50):
        if evaluate("typeof WOB_TASK_READY !== 'undefined' && WOB_TASK_READY === true"):
            break
        time.sleep(0.1)

    # BrowserGym takes the transparent overlays out before an agent acts; a person's pointer
    # would have to go through them too.
    evaluate("['click-canvas','sync-task-cover'].forEach(function(i){var e=document.getElementById(i); if(e){e.remove();}})")


def reward():
    raw = evaluate("JSON.stringify({done: WOB_DONE_GLOBAL, raw: WOB_RAW_REWARD_GLOBAL, reward: WOB_REWARD_GLOBAL, "
                   "reason: (typeof WOB_REWARD_REASON !== 'undefined' ? WOB_REWARD_REASON : null)})")
    return json.loads(raw) if raw else {}


def secrets(task, goal):
    """What the recording will not have kept - given by the person, here read from the task's own words."""

    quoted = re.findall(r'"([^"]*)"', goal)

    # A negative control: the same replays with the wrong secret. The benchmark must say so.
    if os.environ.get("E2E_WRONG_SECRETS"):
        quoted = ["not-the-password"] * 3

    if task == "enter-password" and quoted:
        return {"password": quoted[0], "verify_password": quoted[0]}
    if task == "login-user" and len(quoted) > 1:
        return {"password": quoted[1]}
    return {}


def episode(entry, fmt, directory):
    task, name = entry["task"], entry["name"]
    data = json.load(open(f"{directory}/{name}.{fmt}.json"))

    prepare(task, entry["miniwob_seed"])

    imported = ingest.import_recording(data)
    session = imported["session"]
    meta, events = replay.recording.load(session)
    program = replay.compile_program(meta, events)
    params = secrets(task, entry["goal"])
    given = {p["name"]: params[p["name"]] for p in program["params"] if p["secret"] and p["name"] in params}

    # An OpenHands stream begins by browsing to the page, which the page has just been given.
    args = ["replay", session] + (["--from", "2"] if fmt.startswith("openhands") and program["steps"] and
                                  program["steps"][0]["kind"] == "open" else [])
    for key, value in given.items():
        args += ["--param", f"{key}={value}"]

    started = time.time()
    code, text = gui(*args)
    result = reward()

    return {
        "name": name, "task": task, "format": fmt, "goal": entry["goal"], "steps": len(program["steps"]),
        "params": [(p["name"], p["secret"]) for p in program["params"]],
        "outcome": (text.split(":", 1)[0] if text else ""), "code": code, "seconds": round(time.time() - started, 1),
        "browsergym_reward": entry["reward"], "done": result.get("done"), "raw": result.get("raw"),
        "reward": result.get("reward"), "reason": result.get("reason"), "note": text[:300],
        "warnings": imported.get("warnings"), "skipped": len(imported.get("skipped") or []),
    }


def main():
    directory, formats = sys.argv[1], sys.argv[2].split(",")
    manifest = json.load(open(f"{directory}/manifest.json"))

    only = [t for t in os.environ.get("E2E_TASKS", "").split(",") if t]

    for entry in manifest:
        if only and entry["task"] not in only:
            continue
        for fmt in formats:
            try:
                print(json.dumps(episode(entry, fmt, directory)), flush=True)
            except Exception as exc:  # one episode's failure is a result, not the end of the run
                print(json.dumps({"name": entry["name"], "task": entry["task"], "format": fmt,
                                  "error": f"{type(exc).__name__}: {exc}"}), flush=True)


main()
