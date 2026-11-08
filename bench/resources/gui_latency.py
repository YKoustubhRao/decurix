#!/usr/bin/env python3
"""
gui latency with and without the gui daemon (docs/lightweight.md L1): the same commands, N times each, in one running
desktop, alternately in their own process (DECURIX_GUI_DAEMON=off) and through the daemon (=require, so a command the
daemon did not take shows as exit 125 instead of being timed in-process by mistake).

    py -3 bench/resources/gui_latency.py --container decurix-desktop-abc --host-shell "wsl -d Decurix -u root --"
    python3 bench/resources/gui_latency.py --container NAME -n 20 --command "gui observe" --command "gui --help"

Each command runs as the desktop's user through `bash -c`, as soul runs an engine's commands, with the interpreter
settings of the daemon's own start (LANG, LC_*, HOME, PYTHON* but PYTHONPATH, read from /proc/PID/environ; nothing
else of it, and nothing printed) - a `docker exec` has the container's environment, and a command whose settings
differ from the daemon's is not one it takes. Per arm: p50, p95 and mean wall time, and the CPU of the command's own
process tree (for the daemon arm that is the client only; the forked child's CPU is the daemon's, so the container's
CPU meanwhile is given too).

Standard library only; prints a markdown table and one JSON line.
"""

import argparse
import json
import shlex
import statistics
import subprocess
import sys

DEFAULT_COMMANDS = ["gui --help", "gui observe", "gui skills", "gui recordings", "gui verify"]

TIMER = r"""
import json, os, resource, subprocess, sys, time

def daemon_env():
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as f:
                cmd = f.read()
            if b"decurix_agent.computer.daemon" not in cmd or os.stat(f"/proc/{pid}").st_uid != os.getuid():
                continue
            with open(f"/proc/{pid}/environ", "rb") as f:
                items = [i.split(b"=", 1) for i in f.read().split(b"\0") if b"=" in i]
        except OSError:
            continue
        keep = {}
        for k, v in items:
            if k in (b"HOME", b"LANG") or k.startswith(b"LC_") or (k.startswith(b"PYTHON") and k != b"PYTHONPATH"):
                keep[k.decode()] = v.decode("utf-8", "surrogateescape")
        return int(pid), keep
    return None, {}

def cg():
    try:
        for line in open("/sys/fs/cgroup/cpu.stat"):
            if line.startswith("usage_usec"):
                return int(line.split()[1])
    except OSError:
        return None

commands, n = json.loads(sys.argv[1]), int(sys.argv[2])
pid, keep = daemon_env()
print(json.dumps({"daemon_pid": pid}), flush=True)
base = {k: v for k, v in os.environ.items() if k != "PYTHONPATH" and not k.startswith(("LC_", "PYTHON"))}
base.update(keep)
for i in range(n):
    for cmd in commands:
        for arm in (("off", "require") if i % 2 == 0 else ("require", "off")):
            env = dict(base, DECURIX_GUI_DAEMON=arm)
            r0 = resource.getrusage(resource.RUSAGE_CHILDREN); c0 = cg(); t0 = time.perf_counter()
            p = subprocess.run(["bash", "-c", cmd], capture_output=True, env=env, stdin=subprocess.DEVNULL)
            t1 = time.perf_counter(); c1 = cg(); r1 = resource.getrusage(resource.RUSAGE_CHILDREN)
            own = (r1.ru_utime - r0.ru_utime) + (r1.ru_stime - r0.ru_stime)
            print(json.dumps({"cmd": cmd, "arm": arm, "exit": p.returncode, "ms": round((t1 - t0) * 1000, 1),
                              "own_cpu_ms": round(own * 1000, 1),
                              "container_cpu_ms": None if c0 is None or c1 is None else round((c1 - c0) / 1000, 1),
                              "said": p.stderr.decode("utf-8", "replace")[-160:] if p.returncode == 125 else ""}),
                  flush=True)
"""


def pct(values, q):
    """The q-quantile of values, interpolated."""

    v = sorted(values)
    if not v:
        return None
    k = (len(v) - 1) * q
    lo, hi = int(k), min(int(k) + 1, len(v) - 1)
    return round(v[lo] + (v[hi] - v[lo]) * (k - lo), 1)


def main():
    """Time the commands in both arms and print the table."""

    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--container", required=True, help="the desktop container's name")
    ap.add_argument("--host-shell", default="", help="prefix that reaches the docker host, e.g. 'wsl -d Decurix -u root --'")
    ap.add_argument("-n", type=int, default=10, help="runs of each command in each arm")
    ap.add_argument("--command", action="append", help="a gui command line (repeatable); default: " + ", ".join(DEFAULT_COMMANDS))
    args = ap.parse_args()

    commands = args.command or DEFAULT_COMMANDS
    argv = shlex.split(args.host_shell) + ["docker", "exec", "-i", "-u", "headless", args.container,
                                           "python3", "-", json.dumps(commands), str(args.n)]
    p = subprocess.run(argv, input=TIMER.encode("utf-8"), capture_output=True, timeout=3600)
    rows = [json.loads(line) for line in p.stdout.decode("utf-8", "replace").splitlines() if line.startswith("{")]
    if not rows:
        sys.exit("the timer gave nothing: " + p.stderr.decode("utf-8", "replace")[-400:])
    head, runs = rows[0], rows[1:]
    if not head.get("daemon_pid"):
        print("No gui daemon is running in this desktop: the 'require' arm will be all exit 125.")

    out = {"schema": "decurix.gui-latency/v1", "container": args.container, "n": args.n, "commands": {}}
    print("| command | arm | p50 ms | p95 ms | mean ms | own CPU ms (mean) | container CPU ms (mean) | not served |")
    print("|---|---|---|---|---|---|---|---|")
    for cmd in commands:
        for arm in ("off", "require"):
            mine = [r for r in runs if r["cmd"] == cmd and r["arm"] == arm]
            served = [r for r in mine if r["exit"] != 125]
            ms = [r["ms"] for r in served]
            own = [r["own_cpu_ms"] for r in served]
            box = [r["container_cpu_ms"] for r in served if r["container_cpu_ms"] is not None]
            row = {"p50": pct(ms, 0.5), "p95": pct(ms, 0.95), "mean": round(statistics.mean(ms), 1) if ms else None,
                   "own_cpu": round(statistics.mean(own), 1) if own else None,
                   "container_cpu": round(statistics.mean(box), 1) if box else None,
                   "not_served": len(mine) - len(served),
                   "said": next((r["said"] for r in mine if r["said"]), "")}
            out["commands"].setdefault(cmd, {})["daemon" if arm == "require" else "in_process"] = row
            print(f"| `{cmd}` | {'daemon' if arm == 'require' else 'in-process'} | {row['p50']} | {row['p95']} | "
                  f"{row['mean']} | {row['own_cpu']} | {row['container_cpu']} | {row['not_served']} |")
    said = {c: v["daemon"]["said"] for c, v in out["commands"].items() if v["daemon"]["said"]}
    for cmd, why in said.items():
        print(f"- `{cmd}` was not taken by the daemon: {why.strip()}")
    print(json.dumps(out))


if __name__ == "__main__":
    main()
