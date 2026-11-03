#!/usr/bin/env python3
"""
Run an eval suite on the control plane's matrix runner, and read its results (docs/evals.md).

    py -3 evals/run.py run    --suite evals/suites/core-v1 --arm default --arm fast+expert [--arm default@eb15981]
                              [--arm oracle] [--arm nothing] --repeats 5 [--cases a,b] [--cp http://127.0.0.1:8080]
                              [--detach]
    py -3 evals/run.py status <run-id>
    py -3 evals/run.py table  <run-id>      # prints the markdown and writes evals/results/<id>/table.md
    py -3 evals/run.py rows   <run-id>      # writes evals/results/<id>/rows.jsonl

An arm is <model>[+expert][@<git-rev>], or oracle, nothing or ask:
  - <model> is default or fast (the hub's two deployments), on azure; claude:default (or claude:fast) uses claude;
  - +expert answers the worker's questions with the scripted expert;
  - @<rev> runs the agent tree of that git revision (git archive of agent/decurix_agent), not the image's;
  - oracle runs each case's scripted solution; nothing claims "done" and does nothing; ask asks one question about
    nothing and stops (calibration: oracle must pass every case, nothing and ask none).

Standard library only, Python 3.11+, run from the repository root. The control plane's eval routes are on its primary
listener only (127.0.0.1:8080), never the gated one.
"""

import argparse
import io
import json
import os
import re
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RESULTS = ROOT / "evals" / "results"
DEFAULT_CP = os.environ.get("DECURIX_URL", "http://127.0.0.1:8080")
POLL_SECONDS = 30

ARM = re.compile(r"^(?:(?P<provider>azure|claude):)?(?P<model>default|fast)(?P<expert>\+expert)?(?:@(?P<rev>[A-Za-z0-9._/~^-]{1,80}))?$")


class Usage(SystemExit):
    """A command line this tool cannot run: said, and exit 2."""

    def __init__(self, message):
        super().__init__(2)
        self.message = message


def parse_arm(text):
    """One --arm as the runner takes it: {name, kind, provider, model, expert, agent_tree_rev}."""

    text = text.strip()
    if text in ("oracle", "nothing", "ask"):
        return {"name": text, "kind": text, "expert": False}
    m = ARM.match(text)
    if not m:
        raise Usage(f"--arm {text!r}: an arm is <model>[+expert][@<git-rev>] (model default or fast, optionally "
                    "claude: before it), or oracle, nothing or ask")
    arm = {"name": text, "kind": "model", "provider": m.group("provider") or "azure", "model": m.group("model"),
           "expert": bool(m.group("expert"))}
    if m.group("rev"):
        arm["agent_tree_rev"] = m.group("rev")
    return arm


def archive_command(rev):
    """The git command that writes an agent tree tar at rev, rooted at decurix_agent/ as the practice desktop copies it
    into /opt (desktop.PracticeAgentTree reads /opt/decurix_agent as a tar rooted there)."""

    return ["git", "-C", str(ROOT), "archive", "--format=tar", "--prefix=decurix_agent/", f"{rev}:agent/decurix_agent"]


def resolve_rev(rev, run=subprocess.run):
    """The full commit id a revision names (HEAD, a branch, a short id): a run records what it ran, not a moving name."""

    p = run(["git", "-C", str(ROOT), "rev-parse", "--verify", "--quiet", rev + "^{commit}"], capture_output=True, timeout=60)
    sha = p.stdout.decode(errors="replace").strip() if isinstance(p.stdout, bytes) else str(p.stdout or "").strip()
    if p.returncode != 0 or not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise Usage(f"{rev!r} is not a commit of this repository")
    return sha


def agent_tree(rev, run=subprocess.run):
    """The agent tree tar of a git revision."""

    p = run(archive_command(rev), capture_output=True, timeout=300)
    if p.returncode != 0:
        raise Usage(f"git archive of agent/decurix_agent at {rev!r} failed: {p.stderr.decode(errors='replace').strip()[:300]}")
    return p.stdout


def suite_tar(suite_dir):
    """The suite as the control plane takes it: the suite folder at the root, and mocks/<name>/mock_app.py for each
    mock it names (shared mocks live in evals/mocks). Sorted, fixed modes and times: the same files give the same tar."""

    suite_dir = Path(suite_dir)
    doc = json.loads((suite_dir / "suite.json").read_text(encoding="utf-8"))
    files = {}
    for path in sorted(suite_dir.rglob("*")):
        if path.is_file() and "__pycache__" not in path.parts:
            files[path.relative_to(suite_dir).as_posix()] = path.read_bytes()
    mocks_root = suite_dir.parent.parent / "mocks"
    for name in sorted(doc.get("mocks") or {}):
        source = mocks_root / name / "mock_app.py"
        if not source.is_file():
            raise Usage(f"the suite names the mock {name!r}, and {source} does not exist")
        files[f"mocks/{name}/mock_app.py"] = source.read_bytes()
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for name in sorted(files):
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(files[name]), 0o644, 0
            tar.addfile(info, io.BytesIO(files[name]))
    return buf.getvalue()


def multipart(fields, files):
    """A multipart/form-data body: (content type, bytes)."""

    boundary = "decurix-eval-" + uuid.uuid4().hex
    out = io.BytesIO()
    for name, value in fields.items():
        out.write(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n".encode())
        out.write(str(value).encode() + b"\r\n")
    for name, (filename, body) in files.items():
        out.write(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"; filename=\"{filename}\"\r\n"
                  "Content-Type: application/x-tar\r\n\r\n".encode())
        out.write(body + b"\r\n")
    out.write(f"--{boundary}--\r\n".encode())
    return f"multipart/form-data; boundary={boundary}", out.getvalue()


class ControlPlane:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def request(self, path, data=None, content_type=None, timeout=120):
        req = urllib.request.Request(self.base + path, data=data, method="POST" if data is not None else "GET")
        if content_type:
            req.add_header("Content-Type", content_type)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.read()
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")
            try:
                said = json.loads(body)
            except ValueError:
                said = {"error": body.strip()[:400]}
            problems = said.get("problems") or []
            raise SystemExit(f"the control plane answered {e.code}: {said.get('error') or body[:400]}"
                             + "".join(f"\n  - {p}" for p in problems)) from None
        except urllib.error.URLError as e:
            raise SystemExit(f"cannot reach the control plane at {self.base}: {e.reason}") from None

    def json(self, path, **kw):
        return json.loads(self.request(path, **kw) or b"{}")


def start(cp, suite_dir, arms, repeats, cases):
    fields = {"arms": json.dumps([{k: v for k, v in a.items() if k != "tree"} for a in arms]), "repeats": repeats,
              "cases": ",".join(cases)}
    files = {"suite": ("suite.tar", suite_tar(suite_dir))}
    for a in arms:
        if a.get("tree"):
            files["tree_" + a["name"]] = ("agent.tar", a["tree"])
    content_type, body = multipart(fields, files)
    return cp.json("/api/evals/runs", data=body, content_type=content_type, timeout=600)["run"]


def describe(status):
    run, p = status.get("run") or {}, status.get("progress") or {}
    line = f"{run.get('id')}: {run.get('status')}"
    if status.get("active"):
        line += f" - {p.get('done', 0)}/{p.get('total', 0)} rows; arm {p.get('arm')}, repeat {p.get('repeat')}, case {p.get('case')}"
    if run.get("note"):
        line += f" ({run['note']})"
    return line


def write_table(cp, run_id, out=None):
    out = RESULTS if out is None else out
    md = cp.request(f"/api/evals/runs/{run_id}/table?format=md").decode("utf-8")
    folder = Path(out) / run_id
    folder.mkdir(parents=True, exist_ok=True)
    (folder / "table.md").write_text(md, encoding="utf-8")
    return md, folder / "table.md"


def write_rows(cp, run_id, out=None):
    out = RESULTS if out is None else out
    rows = cp.json(f"/api/evals/runs/{run_id}/rows").get("rows") or []
    folder = Path(out) / run_id
    folder.mkdir(parents=True, exist_ok=True)
    path = folder / "rows.jsonl"
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        for row in rows:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")
    return rows, path


def wait(cp, run_id, poll=POLL_SECONDS, sleep=time.sleep):
    while True:
        status = cp.json(f"/api/evals/runs/{run_id}")
        print(time.strftime("%H:%M:%S"), describe(status), flush=True)
        if (status.get("run") or {}).get("status") != "running" and not status.get("active"):
            return status
        sleep(poll)


def main(argv=None):
    parser = argparse.ArgumentParser(prog="evals/run.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--cp", default=DEFAULT_CP, help="the control plane's primary listener (default %(default)s)")
    sub = parser.add_subparsers(dest="command", required=True)
    r = sub.add_parser("run", help="start a matrix and, unless --detach, wait for its table")
    r.add_argument("--suite", required=True, help="a suite folder, e.g. evals/suites/core-v1")
    r.add_argument("--arm", action="append", required=True, help="repeatable: default, fast+expert, default@<rev>, claude:default, oracle, nothing, ask")
    r.add_argument("--repeats", type=int, default=5)
    r.add_argument("--cases", default="", help="comma-separated case ids (default: every case)")
    r.add_argument("--detach", action="store_true", help="print the run id and return at once")
    r.add_argument("--cp", default=argparse.SUPPRESS, help=argparse.SUPPRESS)
    for name in ("status", "table", "rows"):
        p = sub.add_parser(name)
        p.add_argument("run_id")
        p.add_argument("--cp", default=argparse.SUPPRESS, help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    cp = ControlPlane(getattr(args, "cp", None) or DEFAULT_CP)

    try:
        if args.command == "run":
            arms = [parse_arm(a) for a in args.arm]
            names = [a["name"] for a in arms]
            if len(set(names)) != len(names):
                raise Usage("each --arm is named once")
            if args.repeats == 1:
                print("note: one repeat shows what happened once; rates need at least 5 (n=1: not a result).", flush=True)
            for a in arms:
                if a.get("agent_tree_rev"):
                    a["agent_tree_rev"] = resolve_rev(a["agent_tree_rev"])
                    a["tree"] = agent_tree(a["agent_tree_rev"])
            cases = [c.strip() for c in args.cases.split(",") if c.strip()]
            run = start(cp, args.suite, arms, args.repeats, cases)
            print(f"started {run['id']}: suite {run.get('suite_name')} ({run.get('suite_sha', '')[:12]}), "
                  f"{len(arms)} arm(s) x {args.repeats} repeat(s)", flush=True)
            if args.detach:
                print(run["id"])
                return 0
            wait(cp, run["id"])
            md, path = write_table(cp, run["id"])
            _, rows_path = write_rows(cp, run["id"])
            print("\n" + md)
            print(f"written: {path} and {rows_path}")
            return 0
        if args.command == "status":
            print(describe(cp.json(f"/api/evals/runs/{args.run_id}")))
            return 0
        if args.command == "table":
            md, path = write_table(cp, args.run_id)
            print(md)
            print(f"written: {path}")
            return 0
        if args.command == "rows":
            rows, path = write_rows(cp, args.run_id)
            print(f"{len(rows)} rows written: {path}")
            return 0
    except Usage as u:
        print(u.message, file=sys.stderr)
        return 2
    return 2


if __name__ == "__main__":
    sys.exit(main())
