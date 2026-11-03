#!/usr/bin/env python3
"""
Decurix GUI benchmark runner: success rate per engine on real tasks in
graphical apps (bench/gui/tasks.json).

Every engine is driven exactly the way a person drives it - a message sent
through the control plane's chat API, a wait for the agent's answer - and
judged on the outcome inside the desktop (bench/gui/fixtures/bench.py check),
never on what the agent says it did.

    # real engines, each on a fresh desktop created through the control plane
    python3 bench/gui/run.py --engine ollama --engine thebot --engine claude

    # the scripted oracle, against an existing desktop container: proves every
    # task is solvable through the gui command and every check is correct
    python3 bench/gui/run.py --engine oracle --container desktop_bench-oracle

Standard library only. Needs the `docker` CLI on this machine, and for real
engines a running control plane (default http://localhost:8080). The claude
engine also needs a shared Claude login already completed through the
dashboard (see README.md "Setting up the Claude harness") and a developer or
ai profile; the default profile here is developer, so all three engines run
on identical desktops.
"""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
FIXTURES = HERE / "fixtures"
# What the desktop's apps need (the admin panel, the reset half) lives in the
# agent's home. The answer key (tasks.json) and the checker live where the
# agent's user cannot read them, and checks run as root.
REMOTE = "/home/headless/bench/gui"
ORACLE = "/opt/decurix-bench-gui"
AGENT_FILES = ("adminpanel.py", "bench.py", "odf.py")

ENGINES = ("oracle", "ollama", "claude", "thebot")


# ============================================================
# docker
# ============================================================

def docker(*args, user="headless", stdin=None, timeout=600, check=False):
    command = ["docker", "exec"]
    if stdin is not None:
        command.append("-i")
    if user:
        command += ["-u", user]
    command += list(args)

    # Idle-pause freezes a desktop seconds after its task ends, and an exec into a frozen container
    # does not fail - it hangs until its timeout (measured: a 600 s "reset timed out").
    subprocess.run(["docker", "unpause", args[0]], capture_output=True, timeout=60)

    process = subprocess.run(
        command, input=stdin, capture_output=True, text=True, timeout=timeout,
    )

    # Idle-pause freezes a desktop seconds after its task ends, and exec refuses a paused container.
    if process.returncode != 0 and "is paused" in (process.stderr or ""):
        subprocess.run(["docker", "unpause", args[0]], capture_output=True, timeout=60)
        process = subprocess.run(command, input=stdin, capture_output=True, text=True, timeout=timeout)

    if check and process.returncode != 0:
        raise RuntimeError(f"{' '.join(command)} failed: {process.stderr.strip() or process.stdout.strip()}")

    return process


def keep_awake(plane, desktop_id, container):
    """
    Keep a desktop from being idle-paused for the length of a run, the way an
    open screen tab does: a thumbnail fetch is the control plane's "someone
    has this open" signal. Between two tasks nothing is running, which is
    exactly when idle-pause acts - in the middle of a reset.
    """

    import threading

    def loop():
        while True:
            try:
                urllib.request.urlopen(f"{plane.base}/api/desktops/screenshot?id={urllib.parse.quote(desktop_id)}", timeout=10).read()
            except (OSError, ValueError):
                pass
            subprocess.run(["docker", "unpause", container], capture_output=True)
            time.sleep(5)

    threading.Thread(target=loop, daemon=True).start()


def install_fixtures(container):
    # The agent's copy: the apps' files only. An older install's answer key goes.
    docker(container, "mkdir", "-p", REMOTE, check=True)
    docker(container, "rm", "-f", f"{REMOTE}/tasks.json", user="root", check=True)
    for name in AGENT_FILES:
        subprocess.run(["docker", "cp", str(FIXTURES / name), f"{container}:{REMOTE}/{name}"], check=True, capture_output=True)
    docker(container, "chown", "-R", "headless:headless", "/home/headless/bench", user="root", check=True)

    # The checker's copy, with the answer key: root's, mode 0700.
    docker(container, "sh", "-c", f"rm -rf {ORACLE} && mkdir -p {ORACLE}", user="root", check=True)
    subprocess.run(["docker", "cp", f"{FIXTURES}/.", f"{container}:{ORACLE}/"], check=True, capture_output=True)
    subprocess.run(["docker", "cp", str(HERE / "tasks.json"), f"{container}:{ORACLE}/tasks.json"], check=True, capture_output=True)
    docker(container, "sh", "-c", f"chown -R root:root {ORACLE} && chmod -R go-rwx {ORACLE}", user="root", check=True)


def reset_command(task):
    """The reset, run as the desktop's user: it is handed the task's starting
    state, so it never needs the answer key."""
    return ["python3", f"{REMOTE}/bench.py", "reset", task["id"], "--start", json.dumps(task.get("start") or {})]


def check_command(task):
    """The check, run as root from the root-only copy, with the answer key."""
    return ["python3", f"{ORACLE}/bench.py", "check", task["id"], "--answer-file", f"{ORACLE}/answer.txt"]


def reset_task(container, task):
    process = docker(container, *reset_command(task), timeout=600)
    if process.returncode != 0:
        raise RuntimeError(f"reset {task['id']} failed: {process.stdout[-500:]}{process.stderr[-500:]}")


def own_words(answer):
    """
    The agent's answer without the note the final check adds under it
    ("Checked on the screen: ... orders 1002, 1004 ..."): that note is the
    system's, and a task that asks for "just the number" is judged on the
    answer.
    """

    return (answer or "").split("\n\nChecked on the screen:")[0]


def check_task(container, task, answer):
    answer = own_words(answer)
    docker(container, "sh", "-c", f"cat > {ORACLE}/answer.txt", stdin=answer or "", user="root", check=True)
    process = docker(container, *check_command(task), user="root")
    try:
        return json.loads(process.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return {"pass": False, "detail": f"check produced no verdict: {process.stdout[-300:]} {process.stderr[-300:]}"}


def wait_for_agent(container, timeout=300):
    """Until the desktop's agent accepts connections on :8081 and X is up."""

    probe = (
        "import socket,os,sys\n"
        "socket.create_connection(('127.0.0.1', 8081), 2).close()\n"
        "sys.exit(0 if os.path.exists('/tmp/.X11-unix/X1') else 1)\n"
    )
    deadline = time.time() + timeout
    while time.time() < deadline:
        if docker(container, "python3", "-c", probe, timeout=20).returncode == 0:
            return
        time.sleep(3)
    raise RuntimeError(f"{container}: agent did not come up within {timeout}s")


# ============================================================
# control plane
# ============================================================

class ControlPlane:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def _request(self, path, form=None, timeout=60):
        data = urllib.parse.urlencode(form).encode() if form is not None else None
        request = urllib.request.Request(self.base + path, data=data, method="POST" if data is not None else "GET")
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8") or "{}")

    def create(self, name, profile, harness, brain=None):
        form = {"name": name, "profile": profile, "harness": harness}
        if brain:
            form["brain"] = brain
        return self._request("/api/desktops/create", form, timeout=300)

    def delete(self, desktop_id):
        return self._request("/api/desktops/delete", {"id": desktop_id}, timeout=120)

    def restart(self, desktop_id):
        return self._request("/api/desktops/restart", {"id": desktop_id}, timeout=120)

    def clear(self, desktop_id):
        return self._request("/api/desktops/chat/clear", {"id": desktop_id})

    def send(self, desktop_id, content):
        deadline = time.time() + 120
        while True:
            try:
                return self._request("/api/desktops/send", {"id": desktop_id, "content": content})
            except urllib.error.HTTPError as exc:
                # 503: the agent is still starting. Anything else is real.
                if exc.code != 503 or time.time() > deadline:
                    raise
                time.sleep(3)

    def chat(self, desktop_id):
        return self._request("/api/desktops/chat?id=" + urllib.parse.quote(desktop_id))


def wait_for_reply(plane, desktop_id, after, timeout):
    """
    The first agent message after index `after` in the transcript, plus the
    thread it came from, or (None, thread) on timeout.
    """

    deadline = time.time() + timeout
    thread = {}
    while time.time() < deadline:
        try:
            thread = plane.chat(desktop_id)
        except (OSError, ValueError):
            time.sleep(5)
            continue
        messages = thread.get("entries") or thread.get("messages") or []
        for message in messages[after:]:
            if message.get("role") == "agent":
                return message, thread
        time.sleep(3)
    return None, thread


def screen_actions(message):
    return sum(1 for step in (message or {}).get("steps") or [] if step.get("action"))


def gate_fired(message):
    """Whether the approval gate itself stopped an on-screen action in this turn."""

    return any(
        (step.get("action") or {}).get("outcome") == "needs_approval"
        for step in (message or {}).get("steps") or []
    )


# ============================================================
# engines
# ============================================================

def run_oracle(container, task):
    """Execute a task's scripted solution through the gui command."""

    answer = ""
    paused = False
    failures = []
    started = time.time()

    for step in task.get("oracle") or []:
        if isinstance(step, str):
            step = {"run": step}

        if "sleep" in step:
            time.sleep(step["sleep"])
            continue

        if "answer" in step:
            answer = step["answer"]
            continue

        if "approve" in step:
            # Exactly what the agent's command watcher does with a human reply.
            code = (
                "import sys; sys.path.insert(0, '/opt')\n"
                "from decurix_agent.computer import approval\n"
                "print(approval.resolve_reply(sys.argv[1]))\n"
            )
            process = docker(container, "python3", "-c", code, step["approve"])
            if process.stdout.strip() != "granted":
                failures.append(f"approval was not granted: {process.stdout.strip()} {process.stderr.strip()}")
            continue

        process = docker(container, "bash", "-c", "cd /tmp && " + step["run"], timeout=180)
        want = step.get("exit", 0)
        if process.returncode == 3:
            paused = True
        if process.returncode != want:
            failures.append(f"`{step['run']}` exited {process.returncode}, want {want}: {(process.stdout + process.stderr).strip()[-400:]}")

    return {
        "answer": answer,
        "paused_for_approval": paused,
        "gate_fired": paused,
        "reply_kind": "approval" if paused and task.get("expect") == "approval" and not task.get("approve") else "message",
        "screen_actions": None,
        "seconds": round(time.time() - started, 1),
        "errors": failures,
    }


def run_engine_task(plane, desktop_id, task, timeout):
    started = time.time()
    plane.clear(desktop_id)
    plane.send(desktop_id, task["prompt"])

    reply, thread = wait_for_reply(plane, desktop_id, 1, timeout)
    result = {
        "answer": (reply or {}).get("content", ""),
        "reply_kind": (reply or {}).get("kind"),
        "paused_for_approval": (reply or {}).get("kind") == "approval",
        "gate_fired": gate_fired(reply),
        "screen_actions": screen_actions(reply),
        # What the agent did, kept with the result: the chat is cleared for the next task.
        "transcript": [(step.get("text") or "")[:400] for step in (reply or {}).get("steps") or []],
        "errors": [],
    }

    if reply is None:
        result["errors"].append(f"no reply within {timeout}s")
    elif task.get("approve") and result["paused_for_approval"]:
        count = len(thread.get("entries") or thread.get("messages") or [])
        plane.send(desktop_id, task["approve"])
        second, _ = wait_for_reply(plane, desktop_id, count + 1, timeout)
        if second is None:
            result["errors"].append(f"no reply within {timeout}s after approving")
        else:
            result["answer"] = second.get("content", "")
            result["reply_kind"] = second.get("kind")
            result["screen_actions"] += screen_actions(second)
            result["transcript"] += [(step.get("text") or "")[:400] for step in second.get("steps") or []]

    result["seconds"] = round(time.time() - started, 1)
    return result


def agent_is_idle(run):
    """
    Whether the agent finished this task and is waiting for a new one.

    A task that ended on a help or approval request leaves the agent paused,
    and the agent takes the next message it receives as the *reply* to that
    request - measured on the first full run: after web-create-user paused,
    web-delete-user was delivered as "RECEIVED HUMAN HELP" and everything
    after it ran inside the wrong conversation. A task with no reply at all
    may still be running. Either way the next task needs a fresh agent.
    """

    return not run.get("errors") and run.get("reply_kind") in ("entry", "message", "error")


def restart_agent(plane, desktop_id, container):
    plane.restart(desktop_id)
    # Give the old session a moment to go away, so the readiness probe below
    # cannot see the agent that is about to be stopped.
    time.sleep(10)
    wait_for_agent(container)


def verdict(task, run, checked):
    """Pass means the outcome is right, and the agent paused where it must."""

    reasons = []

    if not checked.get("pass"):
        reasons.append("check failed: " + str(checked.get("detail")))

    # A pause only counts when the gate caused it. An agent that gives up and
    # asks for help for any other reason also leaves the invoice unpaid, and
    # would otherwise score on the very tasks meant to test the gate - which
    # is what the first full run did.
    if task.get("expect") == "approval" and not (run["paused_for_approval"] and run.get("gate_fired")):
        reasons.append("did not pause for approval at the gate" if run["paused_for_approval"] else "did not pause for approval")

    if task.get("expect") == "done" and run["reply_kind"] == "approval":
        reasons.append("asked for help instead of finishing")

    if run["errors"]:
        reasons.extend(run["errors"])

    return not reasons, "; ".join(reasons)


# ============================================================
# main
# ============================================================

def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--engine", action="append", choices=ENGINES, required=True)
    parser.add_argument("--tasks", help="comma-separated task ids (default: all)")
    parser.add_argument("--container", help="oracle only: the desktop container to run in")
    parser.add_argument("--control-plane", default=os.environ.get("DECURIX_URL", "http://localhost:8080"))
    parser.add_argument("--profile", default="developer", choices=["basic", "developer", "ai"])
    parser.add_argument("--brain", choices=["ollama", "claude", "azure"],
                        help="thebot only: the model it thinks with (a hosted one must be set up on the control plane)")
    parser.add_argument("--timeout", type=int, default=900, help="seconds to wait for one reply")
    parser.add_argument("--keep", action="store_true", help="keep the desktops created for real engines")
    parser.add_argument("--desktop", help="a real engine: run on this existing desktop id instead of creating one "
                                          "(it is kept) - for a desktop patched with code under test")
    parser.add_argument("--out", default=str(HERE / "results"))
    args = parser.parse_args()

    with open(HERE / "tasks.json", encoding="utf-8") as f:
        tasks = json.load(f)["tasks"]

    if args.tasks:
        wanted = set(args.tasks.split(","))
        tasks = [t for t in tasks if t["id"] in wanted]
        missing = wanted - {t["id"] for t in tasks}
        if missing:
            sys.exit(f"unknown task ids: {', '.join(sorted(missing))}")

    plane = ControlPlane(args.control_plane)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    report = {"started": stamp, "profile": args.profile, "brain": args.brain, "engines": {}}

    for engine in args.engine:
        created = None

        if engine == "oracle":
            if not args.container:
                sys.exit("--engine oracle needs --container")
            container = args.container
        else:
            name = f"bench-{engine}-{stamp}"
            brain = args.brain if engine == "thebot" else None
            if args.desktop:
                created, args.keep = {"id": args.desktop}, True
                print(f"[{engine}] using desktop {args.desktop}", flush=True)
            else:
                print(f"[{engine}] creating desktop {name} ({args.profile}{', brain ' + brain if brain else ''})", flush=True)
                created = plane.create(name, args.profile, engine, brain)
            container = f"desktop_{created['id']}"

        try:
            if created:
                keep_awake(plane, created["id"], container)
            wait_for_agent(container)
            install_fixtures(container)

            rows = []
            for task in tasks:
                print(f"[{engine}] {task['id']} ...", end=" ", flush=True)
                try:
                    reset_task(container, task)
                    if engine == "oracle":
                        run = run_oracle(container, task)
                    else:
                        run = run_engine_task(plane, created["id"], task, args.timeout)
                    checked = check_task(container, task, run["answer"])
                    passed, why = verdict(task, run, checked)
                except Exception as exc:  # one broken task must not end the run
                    run, checked, passed, why = {"seconds": None, "errors": [str(exc)]}, {}, False, f"runner error: {exc}"

                print(("PASS" if passed else "FAIL") + (f" ({why})" if why else "") + f" [{run.get('seconds')}s]", flush=True)
                rows.append({"task": task["id"], "app": task.get("app"), "pass": passed, "why": why,
                             "check": checked, **{k: v for k, v in run.items() if k != "errors"}})

                if engine != "oracle" and not agent_is_idle(run):
                    restart_agent(plane, created["id"], container)

            passed_count = sum(1 for r in rows if r["pass"])
            report["engines"][engine] = {
                "container": container,
                "passed": passed_count,
                "total": len(rows),
                "success_rate": round(passed_count / len(rows), 3) if rows else None,
                "tasks": rows,
            }
        finally:
            if created and not args.keep:
                try:
                    plane.delete(created["id"])
                except Exception as exc:
                    print(f"[{engine}] could not delete {created['id']}: {exc}")

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    out_path = out_dir / f"{stamp}.json"
    out_path.write_text(json.dumps(report, indent=2), encoding="utf-8")

    print()
    print("| engine | passed | success rate |")
    print("|---|---|---|")
    for engine, summary in report["engines"].items():
        print(f"| {engine} | {summary['passed']}/{summary['total']} | {summary['success_rate']:.0%} |")

    ids = [t["id"] for t in tasks]
    engines = list(report["engines"])
    print()
    print("| task | " + " | ".join(engines) + " |")
    print("|---|" + "---|" * len(engines))
    for task_id in ids:
        cells = []
        for engine in engines:
            row = next(r for r in report["engines"][engine]["tasks"] if r["task"] == task_id)
            cells.append("pass" if row["pass"] else "FAIL")
        print(f"| {task_id} | " + " | ".join(cells) + " |")

    print(f"\nFull results: {out_path}")
    return 0 if report["engines"] and all(summary["total"] and summary["passed"] == summary["total"]
                    for summary in report["engines"].values()) else 1


if __name__ == "__main__":
    sys.exit(main())
