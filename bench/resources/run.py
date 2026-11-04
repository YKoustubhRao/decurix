#!/usr/bin/env python3
"""
Resource bench: what one desktop, the gui tool, a dashboard tab and a task cost (RES-PLAN Phase 0). Every later change
that claims to save RAM, CPU or tokens is measured against a row of this.

    py -3 bench/resources/run.py --cp http://127.0.0.1:18080 --host-shell "wsl -d Decurix -u root --" \\
        --label "cp 3f2d520-swap, image e1349dd" --eval-run eb5fdd477d1eb6744
    python3 bench/resources/run.py                       # on a Linux host with the docker CLI and the control plane

It creates one desktop through the control plane (as the dashboard's form does: profile basic, the default harness),
measures it, and deletes it at the end. Sections (--only to pick some):

  idle       the desktop idling, with and without Chrome, running and paused: the container's memory and CPU, and each
             process group's anonymous RSS and CPU (chrome, agent, soul, gate, websockify, xfdesktop, panel, X, other)
  gui        p50/p95 latency of about twenty representative gui commands (open, observe, type, click, read, verify) on a
             local form, with the CPU each one took (its own process tree, and the whole container meanwhile)
  dashboard  a scripted viewer holding the dashboard open (viewer.py: the page's own timers), visible then hidden:
             requests/s and bytes/s, and the control plane's and the desktop's CPU meanwhile
  host       host RAM with 0 desktops and with the desktop up (and a second one when it fits): Windows' available
             memory and the WSL VM's share, or MemAvailable on a Linux host; and the docker host's own MemAvailable
  tokens     input tokens per task and the cached share, from /api/llm/calls, for --task ids or an eval run's model rows

Output: one JSON row appended to bench/resources/results/rows.jsonl (git-ignored), and a markdown table printed and
written beside it. Before each desktop it checks the host's available memory: under 1 GB it drops the docker host's
page cache and waits; a desktop that still does not fit is not made (the first one stops the bench, a second is
skipped and said).

Standard library only. The docker host is reached by --host-shell (a prefix such as `wsl -d Decurix -u root --`), or
directly when it is this machine.
"""

import argparse
import json
import os
import shlex
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import viewer  # noqa: E402

SCHEMA = "decurix.resource-bench/v1"
RESULTS = HERE / "results"
MIN_FREE_MB = 1024
SECTIONS = ("idle", "gui", "dashboard", "host", "tokens")

# Process groups, by what the process runs: the first rule a process matches names its group. A rule is
# (group, executable basenames, words of its command line). The executables are the desktop image's own programs.
GROUPS = (
    ("gate", (), ("decurix_agent.gate_server", "gate_server.py")),
    ("soul", ("soul",), ()),
    ("agent", (), ("decurix_agent", "vm_server.py")),
    ("chrome", ("chrome", "chromium", "chromium-browser", "chrome_crashpad_handler"), ()),
    ("websockify", ("websockify",), ("websockify",)),
    ("xfdesktop", ("xfdesktop",), ()),
    ("panel", ("xfce4-panel", "wrapper-2.0", "panel-6-systray", "panel-1-whiskermenu"), ("xfce4/panel/",)),
    ("x", ("Xvnc", "Xtigervnc", "vncserver", "Xtigervnc-session"), ()),
)


def group_of(cmdline):
    """The group a process belongs to, from its command line (argv joined by spaces)."""

    argv = cmdline.split()
    exe = os.path.basename(argv[0]) if argv else ""
    for name, exes, words in GROUPS:
        if exe in exes or any(w in cmdline for w in words):
            return name
    return "other"


# ------------------------------------------------------------------ the docker host

class Host:
    """Commands on the docker host: through a prefix (wsl ... --) or on this machine."""

    def __init__(self, prefix):
        self.prefix = shlex.split(prefix) if prefix else []

    def run(self, argv, timeout=300, stdin=None):
        return subprocess.run(self.prefix + list(argv), capture_output=True, text=True, timeout=timeout, input=stdin,
                              encoding="utf-8", errors="replace")

    def sh(self, script, *args, timeout=300):
        return self.run(["sh", "-c", script, "sh", *args], timeout=timeout)

    def docker(self, *args, timeout=300):
        return self.run(["docker", *args], timeout=timeout)


# One sample of a container: each process (host pid, anonymous RSS in kB, its /proc stat line, its command line), the
# container's cgroup (memory.current, anon bytes, CPU usec), and the host's uptime.
SAMPLE = r"""
n=$1
for p in $(docker top "$n" -eo pid 2>/dev/null | tail -n +2); do
  [ -r /proc/$p/stat ] || continue
  a=$(awk '/^RssAnon:/{print $2}' /proc/$p/status 2>/dev/null)
  s=$(cat /proc/$p/stat 2>/dev/null)
  c=$(tr '\000' ' ' < /proc/$p/cmdline 2>/dev/null | cut -c1-400)
  printf 'P\t%s\t%s\t%s\t%s\n' "$p" "${a:-0}" "$s" "$c"
done
id=$(docker inspect -f '{{.Id}}' "$n" 2>/dev/null)
cg=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" 2>/dev/null | head -1)
if [ -n "$id" ] && [ -n "$cg" ]; then
  printf 'CG\t%s\t%s\t%s\n' "$(cat $cg/memory.current)" "$(awk '/^anon /{print $2}' $cg/memory.stat)" "$(awk '/^usage_usec/{print $2}' $cg/cpu.stat)"
fi
printf 'UP\t%s\n' "$(cut -d' ' -f1 /proc/uptime)"
printf 'TCK\t%s\n' "$(getconf CLK_TCK 2>/dev/null || echo 100)"
"""


def sample(host, container):
    """{procs: {pid: (anon_kb, cpu_ticks, cmdline)}, cg: (mem, anon, usec) or None, up: s, tck: n}"""

    out = host.sh(SAMPLE, container, timeout=120).stdout
    procs, cg, up, tck = {}, None, 0.0, 100
    for line in out.splitlines():
        kind, _, rest = line.partition("\t")
        if kind == "P":
            pid, anon, stat, cmd = (rest.split("\t") + ["", "", "", ""])[:4]
            after = stat.rsplit(")", 1)[-1].split()
            ticks = int(after[11]) + int(after[12]) if len(after) > 12 else 0
            procs[pid] = (int(anon or 0), ticks, cmd.strip())
        elif kind == "CG":
            parts = rest.split("\t")
            cg = tuple(int(x or 0) for x in parts[:3])
        elif kind == "UP":
            up = float(rest or 0)
        elif kind == "TCK":
            tck = int(rest or 100)
    return {"procs": procs, "cg": cg, "up": up, "tck": tck}


def measure(host, container, seconds):
    """A container's memory now and its CPU over seconds: totals and by process group."""

    a = sample(host, container)
    time.sleep(seconds)
    b = sample(host, container)
    dt = max(1e-6, b["up"] - a["up"])
    groups = {}
    for pid, (anon, ticks, cmd) in b["procs"].items():
        g = groups.setdefault(group_of(cmd), {"processes": 0, "anon_mb": 0.0, "cpu_pct": 0.0})
        g["processes"] += 1
        g["anon_mb"] += anon / 1024
        before = a["procs"].get(pid)
        if before:
            g["cpu_pct"] += (ticks - before[1]) / b["tck"] / dt * 100
    for g in groups.values():
        g["anon_mb"], g["cpu_pct"] = round(g["anon_mb"], 1), round(g["cpu_pct"], 2)
    row = {"seconds": round(dt, 1), "groups": dict(sorted(groups.items()))}
    if a["cg"] and b["cg"]:
        row["memory_mb"] = round(b["cg"][0] / 2**20, 1)
        row["anon_mb"] = round(b["cg"][1] / 2**20, 1)
        row["cpu_pct"] = round((b["cg"][2] - a["cg"][2]) / 1e6 / dt * 100, 2)
    return row


def cpu_usec(host, container):
    """The container's cgroup CPU in microseconds and the host's uptime, or (None, uptime)."""

    s = sample(host, container)
    return (s["cg"][2] if s["cg"] else None), s["up"]


# ------------------------------------------------------------------ host memory

def host_memory(host):
    """Available memory: Windows' (and the WSL VM's share) when run there, else this machine's; and the docker
    host's own MemAvailable."""

    out = {}
    if os.name == "nt":
        ps = ("$o=Get-CimInstance Win32_OperatingSystem; $v=Get-Process -Name vmmem,vmmemWSL -ErrorAction SilentlyContinue;"
              "[pscustomobject]@{free_kb=$o.FreePhysicalMemory; total_kb=$o.TotalVisibleMemorySize;"
              "vm_ws=($v|Measure-Object WorkingSet64 -Sum).Sum; vm_private=($v|Measure-Object PrivateMemorySize64 -Sum).Sum}"
              "|ConvertTo-Json")
        r = subprocess.run(["powershell", "-NoProfile", "-Command", ps], capture_output=True, text=True, timeout=60)
        try:
            d = json.loads(r.stdout)
            out.update({"available_mb": int(d["free_kb"]) // 1024, "total_mb": int(d["total_kb"]) // 1024,
                        "vm_working_set_mb": int(d["vm_ws"] or 0) // 2**20, "vm_private_mb": int(d["vm_private"] or 0) // 2**20})
        except (ValueError, KeyError, TypeError):
            out["error"] = "Windows memory could not be read"
    else:
        out.update(meminfo(Path("/proc/meminfo").read_text()))
    docker_host = meminfo(host.run(["cat", "/proc/meminfo"], timeout=60).stdout)
    out["docker_host_available_mb"] = docker_host.get("available_mb")
    out["docker_host_total_mb"] = docker_host.get("total_mb")
    return out


def meminfo(text):
    vals = {}
    for line in text.splitlines():
        k, _, v = line.partition(":")
        if v.strip().endswith("kB"):
            vals[k] = int(v.split()[0])
    return {"available_mb": vals.get("MemAvailable", 0) // 1024, "total_mb": vals.get("MemTotal", 0) // 1024}


def room_for_a_desktop(host, log):
    """Whether the host has MIN_FREE_MB available, after dropping the docker host's page cache and waiting if not."""

    for attempt in range(4):
        free = host_memory(host).get("available_mb", 0)
        if free >= MIN_FREE_MB:
            return True, free
        log(f"  {free} MB available, under {MIN_FREE_MB}: dropping the docker host's page cache and waiting")
        host.sh("sync; echo 3 > /proc/sys/vm/drop_caches", timeout=60)
        time.sleep(30)
    return False, host_memory(host).get("available_mb", 0)


# ------------------------------------------------------------------ control plane

class ControlPlane:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def get(self, path, timeout=60):
        with urllib.request.urlopen(self.base + path, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8") or "null")

    def post(self, path, form, timeout=300):
        data = urllib.parse.urlencode(form).encode()
        with urllib.request.urlopen(urllib.request.Request(self.base + path, data=data, method="POST"), timeout=timeout) as r:
            body = r.read().decode("utf-8")
            return json.loads(body) if body.strip() else {}

    def create(self, name):
        return self.post("/api/desktops/create", {"name": name, "profile": "basic"})

    def wait_ready(self, desktop_id, timeout=600):
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                s = self.get("/api/desktops/status?id=" + urllib.parse.quote(desktop_id))
                if s.get("ready") or (s.get("status") == "running" and s.get("stage") == "ready"):
                    return s
                if s.get("status") == "failed":
                    raise RuntimeError(f"{desktop_id} failed to start: {s}")
            except urllib.error.HTTPError:
                pass
            time.sleep(5)
        raise RuntimeError(f"{desktop_id} was not ready within {timeout}s")


def container_of(desktop_id):
    return "desktop_" + desktop_id


def wait_agent(host, container, timeout=300):
    probe = "import socket,os,sys; socket.create_connection(('127.0.0.1', 8081), 2).close(); sys.exit(0 if os.path.exists('/tmp/.X11-unix/X1') else 1)"
    deadline = time.time() + timeout
    while time.time() < deadline:
        if host.docker("exec", "-u", "headless", container, "python3", "-c", probe, timeout=30).returncode == 0:
            return
        time.sleep(3)
    raise RuntimeError(f"{container}: the agent did not come up")


# ------------------------------------------------------------------ gui

FORM = """<!doctype html><meta charset=utf-8><title>Customers</title>
<h1>Customers</h1>
<label>Customer name <input id=n type=text aria-label="Customer name"></label>
<button id=save onclick="var o=document.getElementById('out');o.textContent='Saved '+document.getElementById('n').value;o.setAttribute('role','status')">Save</button>
<p id=out></p>
"""
NAMES = ("Acme", "Globex", "Initech", "Umbrella")
REQUEST = "Add the customers " + ", ".join(NAMES) + " on the form at ~/work/form.html, one at a time."


def gui_commands():
    """(kind, command): the page opened, then per customer a look, a type, a click, a read and a check."""

    cmds = [("open", "gui open file:///home/headless/work/form.html")]
    for name in NAMES:
        cmds += [("observe", "gui observe"),
                 ("type", f"gui type 'textbox \"Customer name\"' {name}"),
                 ("click", "gui click 'button \"Save\"'"),
                 ("read", "gui read"),
                 ("verify", f"gui verify --text 'Saved {name}'")]
    return cmds


# Run in the desktop as its user: each command through a login shell (as the agent's tools run it), its wall time, the
# CPU of its own process tree (getrusage of the children it waited for), and the container's CPU meanwhile.
GUI_TIMER = r"""
import json, resource, subprocess, sys, time
def cg():
    try:
        for line in open("/sys/fs/cgroup/cpu.stat"):
            if line.startswith("usage_usec"):
                return int(line.split()[1])
    except OSError:
        return None
for kind, cmd in json.loads(sys.argv[1]):
    r0 = resource.getrusage(resource.RUSAGE_CHILDREN); c0 = cg(); t0 = time.perf_counter()
    p = subprocess.run(["bash", "-lc", cmd], capture_output=True, text=True)
    t1 = time.perf_counter(); c1 = cg(); r1 = resource.getrusage(resource.RUSAGE_CHILDREN)
    own = (r1.ru_utime - r0.ru_utime) + (r1.ru_stime - r0.ru_stime)
    print(json.dumps({"kind": kind, "exit": p.returncode, "ms": round((t1 - t0) * 1000, 1), "own_cpu_ms": round(own * 1000, 1),
                      "container_cpu_ms": None if c0 is None or c1 is None else round((c1 - c0) / 1000, 1),
                      "said": (p.stdout + p.stderr).strip()[-160:] if p.returncode else ""}), flush=True)
"""


def pct(values, q):
    v = sorted(values)
    if not v:
        return None
    k = (len(v) - 1) * q
    lo, hi = int(k), min(int(k) + 1, len(v) - 1)
    return round(v[lo] + (v[hi] - v[lo]) * (k - lo), 1)


def put_form(host, container):
    """The local form the gui commands work on, in the worker's ~/work."""

    host.run(["docker", "exec", "-i", "-u", "headless", container, "sh", "-c",
              "mkdir -p /home/headless/work && cat > /home/headless/work/form.html"], stdin=FORM, timeout=60)


def gui_bench(host, container, log):
    put_form(host, container)
    task = "t" + uuid.uuid4().hex[:16]
    host.docker("exec", "-u", "headless", container, "sh", "-c",
                f"printf %s {task} > /tmp/decurix-current-task; printf %s {shlex.quote(REQUEST)} > /tmp/decurix-current-task.request")
    baseline = [("shell", "true")] * 3
    r = host.docker("exec", "-u", "headless", container, "python3", "-c", GUI_TIMER,
                    json.dumps(baseline + gui_commands()), timeout=1800)
    host.docker("exec", "-u", "headless", container, "sh", "-c", "printf %s '' > /tmp/decurix-current-task")
    runs = [json.loads(line) for line in r.stdout.splitlines() if line.startswith("{")]
    if not runs:
        raise RuntimeError(f"the gui timer gave nothing: {r.stderr[-400:]}")
    shell = [x for x in runs if x["kind"] == "shell"]
    cmds = [x for x in runs if x["kind"] != "shell"]
    by_kind = {}
    for x in cmds:
        by_kind.setdefault(x["kind"], []).append(x)

    def summary(xs):
        return {"n": len(xs), "failed": sum(1 for x in xs if x["exit"] != 0),
                "p50_ms": pct([x["ms"] for x in xs], 0.5), "p95_ms": pct([x["ms"] for x in xs], 0.95),
                "own_cpu_ms_p50": pct([x["own_cpu_ms"] for x in xs], 0.5),
                "container_cpu_ms_p50": pct([x["container_cpu_ms"] for x in xs if x["container_cpu_ms"] is not None], 0.5)}

    for x in cmds:
        if x["exit"]:
            log(f"  gui {x['kind']} exited {x['exit']}: {x['said']}")
    return {"shell_overhead_ms_p50": pct([x["ms"] for x in shell], 0.5), "all": summary(cmds),
            "by_kind": {k: summary(v) for k, v in by_kind.items()}}


# ------------------------------------------------------------------ tokens

def tokens(cp, task_ids):
    per = []
    purposes = {}
    for t in task_ids:
        calls = cp.get("/api/llm/calls?task=" + urllib.parse.quote(t)).get("calls") or []
        inp = sum(int(c.get("input_tokens") or 0) for c in calls)
        cached = sum(int(c.get("cached_input_tokens") or 0) for c in calls)
        out = sum(int(c.get("output_tokens") or 0) for c in calls)
        for c in calls:
            p = purposes.setdefault(c.get("purpose") or "untagged", [0, 0])
            p[0] += int(c.get("input_tokens") or 0)
            p[1] += int(c.get("cached_input_tokens") or 0)
        per.append({"task_id": t, "calls": len(calls), "input": inp, "cached": cached, "output": out})
    per = [p for p in per if p["calls"]]
    total_in = sum(p["input"] for p in per)
    total_cached = sum(p["cached"] for p in per)
    return {
        "tasks": len(per),
        "input_per_task_median": int(statistics.median([p["input"] for p in per])) if per else None,
        "input_per_task_p90": int(pct([p["input"] for p in per], 0.9)) if per else None,
        "output_per_task_median": int(statistics.median([p["output"] for p in per])) if per else None,
        "calls_per_task_median": statistics.median([p["calls"] for p in per]) if per else None,
        "cached_share": round(total_cached / total_in, 3) if total_in else None,
        "by_purpose": {k: {"input": v[0], "cached_share": round(v[1] / v[0], 3) if v[0] else None}
                       for k, v in sorted(purposes.items(), key=lambda kv: -kv[1][0])},
        "per_task": per,
    }


def eval_tasks(cp, run_id, arm=None):
    rows = cp.get(f"/api/evals/runs/{urllib.parse.quote(run_id)}/rows").get("rows") or []
    return [r["task_id"] for r in rows if r.get("task_id") and r.get("tokens_in") and (arm is None or r.get("arm") == arm)]


# ------------------------------------------------------------------ the table

def table(row):
    lines = [f"# Resource bench {row['at']}", "", f"Label: {row.get('label') or '-'}; image {row.get('image') or '-'}", ""]
    idle = row.get("idle") or {}
    if idle:
        groups = sorted({g for s in idle.values() for g in (s.get("groups") or {})})
        lines += ["## Idle desktop", "", "| state | memory MB | anon MB | CPU % | " + " | ".join(f"{g} MB / CPU %" for g in groups) + " |",
                  "|---|---|---|---|" + "---|" * len(groups)]
        for state, s in idle.items():
            cells = []
            for g in groups:
                v = (s.get("groups") or {}).get(g)
                cells.append(f"{v['anon_mb']} / {v['cpu_pct']}" if v else "-")
            lines.append(f"| {state} | {s.get('memory_mb', '-')} | {s.get('anon_mb', '-')} | {s.get('cpu_pct', '-')} | " + " | ".join(cells) + " |")
        lines.append("")
    gui = row.get("gui")
    if gui:
        lines += ["## gui", "", f"Shell start (bash -lc true) p50: {gui['shell_overhead_ms_p50']} ms, included below.", "",
                  "| commands | n | failed | p50 ms | p95 ms | own CPU ms p50 | container CPU ms p50 |", "|---|---|---|---|---|---|---|"]
        for k, s in [("all", gui["all"])] + sorted(gui["by_kind"].items()):
            lines.append(f"| {k} | {s['n']} | {s['failed']} | {s['p50_ms']} | {s['p95_ms']} | {s['own_cpu_ms_p50']} | {s['container_cpu_ms_p50']} |")
        lines.append("")
    dash = row.get("dashboard") or []
    if dash:
        lines += ["## Dashboard (scripted viewer, bench/resources/viewer.py)", "",
                  "| tab | desktops | thumbnails a sweep | seconds | req/s | KB/s | errors | control plane CPU % | desktop CPU % |",
                  "|---|---|---|---|---|---|---|---|---|"]
        for d in dash:
            lines.append(f"| {'hidden' if d['hidden'] else 'visible'} | {d['desktops']} | {d['thumbnails_per_sweep']} | {d['seconds']} | "
                         f"{d['requests_per_s']} | {round((d['bytes_per_s'] or 0) / 1024, 1)} | {d['errors']} | "
                         f"{d.get('control_plane_cpu_pct', '-')} | {d.get('desktop_cpu_pct', '-')} |")
        lines.append("")
    host = row.get("host") or {}
    if host:
        lines += ["## Host RAM", "", "| desktops | available MB | VM working set MB | VM private MB | docker host available MB |", "|---|---|---|---|---|"]
        for state, h in host.items():
            lines.append(f"| {state} | {h.get('available_mb', '-')} | {h.get('vm_working_set_mb', '-')} | {h.get('vm_private_mb', '-')} | "
                         f"{h.get('docker_host_available_mb', '-')} |")
        lines.append("")
    tok = row.get("tokens")
    if tok:
        lines += ["## Tokens", "", "| tasks | input/task median | input/task p90 | output/task median | calls/task median | cached share |",
                  "|---|---|---|---|---|---|",
                  f"| {tok['tasks']} | {tok['input_per_task_median']} | {tok['input_per_task_p90']} | {tok['output_per_task_median']} | "
                  f"{tok['calls_per_task_median']} | {tok['cached_share']} |", "", "| purpose | input | cached share |", "|---|---|---|"]
        for k, v in tok["by_purpose"].items():
            lines.append(f"| {k} | {v['input']} | {v['cached_share']} |")
        lines.append("")
    for note in row.get("notes") or []:
        lines.append(f"- {note}")
    return "\n".join(lines) + "\n"


# ------------------------------------------------------------------ main

def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--cp", default=os.environ.get("DECURIX_URL", "http://127.0.0.1:8080"), help="the control plane's primary listener")
    ap.add_argument("--host-shell", default=os.environ.get("DECURIX_BENCH_HOST", ""), help="prefix that runs a command on the docker host")
    ap.add_argument("--only", default=",".join(SECTIONS), help="comma-separated sections: " + ", ".join(SECTIONS))
    ap.add_argument("--settle", type=int, default=60, help="seconds a desktop idles before it is measured")
    ap.add_argument("--window", type=int, default=30, help="seconds each idle measurement lasts")
    ap.add_argument("--viewer-seconds", type=int, default=60)
    ap.add_argument("--shot-width", type=int, default=320)
    ap.add_argument("--simulate", type=int, default=0, help="the viewer sweeps this many thumbnails a pass (stand-in desktops)")
    ap.add_argument("--second", action="store_true", help="also measure host RAM with a second desktop, when it fits")
    ap.add_argument("--task", action="append", default=[], help="a task id for the tokens section (repeatable)")
    ap.add_argument("--eval-run", help="the tokens section reads every row of this eval run that used a model")
    ap.add_argument("--arm", help="with --eval-run: only this arm's rows")
    ap.add_argument("--label", default="", help="what is being measured (builds), kept in the row")
    ap.add_argument("--out", default=str(RESULTS))
    args = ap.parse_args(argv)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    unknown = only - set(SECTIONS)
    if unknown:
        ap.error("unknown sections: " + ", ".join(sorted(unknown)))

    host, cp = Host(args.host_shell), ControlPlane(args.cp)
    log = lambda m: print(m, flush=True)  # noqa: E731
    row = {"schema": SCHEMA, "at": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "label": args.label, "notes": []}
    made = []

    def make_desktop(first):
        ok, free = room_for_a_desktop(host, log)
        if not ok:
            msg = f"a desktop did not fit: {free} MB available after dropping caches (needs {MIN_FREE_MB})"
            if first:
                raise SystemExit(msg)
            row["notes"].append(msg)
            return None
        name = f"resbench-{uuid.uuid4().hex[:6]}"
        log(f"creating {name} ({free} MB available)")
        created = cp.create(name)
        desktop_id = created.get("id") or name
        made.append(desktop_id)
        cp.wait_ready(desktop_id)
        wait_agent(host, container_of(desktop_id))
        return desktop_id

    try:
        needs_desktop = bool(only & {"idle", "gui", "dashboard", "host"})
        if "host" in only:
            listed = cp.get("/api/desktops") or []
            row["host"] = {f"{len(listed)} (before)": host_memory(host)}
        if needs_desktop:
            desktop_id = make_desktop(True)
            container = container_of(desktop_id)
            img = host.docker("inspect", "-f", "{{.Config.Image}} {{.Image}}", container).stdout.strip()
            row["image"] = img
            log(f"idling {args.settle} s")
            time.sleep(args.settle)
            states = {}
            if "idle" in only:
                host.docker("exec", "-u", "headless", container, "sh", "-c", "pkill -u headless -f '[c]hrom' ; true")
                time.sleep(5)
                log("measuring: running, no Chrome")
                states["running, no Chrome"] = measure(host, container, args.window)
                cp.post("/api/desktops/pause", {"id": desktop_id})
                time.sleep(3)
                log("measuring: paused, no Chrome")
                states["paused, no Chrome"] = measure(host, container, args.window)
                cp.post("/api/desktops/unpause", {"id": desktop_id})
                time.sleep(5)
            put_form(host, container)
            opened = host.docker("exec", "-u", "headless", container, "bash", "-lc", "gui open file:///home/headless/work/form.html", timeout=180)
            if opened.returncode:
                row["notes"].append(f"Chrome could not be opened: {(opened.stdout + opened.stderr).strip()[-200:]}")
            time.sleep(args.settle // 2)
            if "idle" in only:
                log("measuring: running, Chrome open")
                states["running, Chrome open"] = measure(host, container, args.window)
            if "gui" in only:
                log("timing gui commands")
                row["gui"] = gui_bench(host, container, log)
            if "dashboard" in only:
                row["dashboard"] = []
                for hidden in (False, True):
                    log(f"holding the dashboard open, {'hidden' if hidden else 'visible'}, {args.viewer_seconds} s")
                    c0, u0 = cpu_usec(host, "decurix")
                    d0, _ = cpu_usec(host, container)
                    seen = viewer.watch(args.cp, args.viewer_seconds, shot_width=args.shot_width, hidden=hidden, simulate=args.simulate)
                    c1, u1 = cpu_usec(host, "decurix")
                    d1, _ = cpu_usec(host, container)
                    dt = max(1e-6, u1 - u0)
                    if None not in (c0, c1):
                        seen["control_plane_cpu_pct"] = round((c1 - c0) / 1e6 / dt * 100, 2)
                    if None not in (d0, d1):
                        seen["desktop_cpu_pct"] = round((d1 - d0) / 1e6 / dt * 100, 2)
                    row["dashboard"].append(seen)
            if "host" in only:
                row["host"]["1"] = host_memory(host)
            if "idle" in only:
                cp.post("/api/desktops/pause", {"id": desktop_id})
                time.sleep(3)
                log("measuring: paused, Chrome open")
                states["paused, Chrome open"] = measure(host, container, args.window)
                cp.post("/api/desktops/unpause", {"id": desktop_id})
                row["idle"] = states
            if "host" in only and args.second:
                second = make_desktop(False)
                if second:
                    time.sleep(args.settle)
                    row["host"]["2"] = host_memory(host)
        if "tokens" in only:
            ids = list(args.task)
            if args.eval_run:
                ids += eval_tasks(cp, args.eval_run, args.arm)
                row["tokens_source"] = {"eval_run": args.eval_run, "arm": args.arm}
            if ids:
                row["tokens"] = tokens(cp, ids)
            else:
                row["notes"].append("tokens: no task named (--task or --eval-run)")
    finally:
        for desktop_id in made:
            try:
                cp.post("/api/desktops/delete", {"id": desktop_id}, timeout=180)
                log(f"deleted {desktop_id}")
            except (urllib.error.URLError, OSError) as exc:
                log(f"could not delete {desktop_id}: {exc}")
                row["notes"].append(f"{desktop_id} could not be deleted: delete it by hand")

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    with open(out / "rows.jsonl", "a", encoding="utf-8") as f:
        f.write(json.dumps(row) + "\n")
    md = table(row)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    (out / f"{stamp}.md").write_text(md, encoding="utf-8")
    (out / f"{stamp}.json").write_text(json.dumps(row, indent=1), encoding="utf-8")
    print("\n" + md)
    print(f"written: {out / 'rows.jsonl'}, {out / (stamp + '.md')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
