"""
Handing part of a task to another worker, or asking another worker a question: the `handoff` shell command.

    handoff delegate --to WORKER|--role ROLE|--skill SKILL --request TEXT [--input k=v]... [--file PATH]...
                     [--expect TEXT] [--wait SECONDS]
    handoff ask --skill SKILL [--wait SECONDS] QUESTION
    handoff status ID
    handoff cancel ID
    handoff who

`handoff who` lists the other workers the skill registry knows, with the roles they carry and the skills it records
them holding (GET /api/skillhub): the names `--to`, `--role` and `--skill` take, so a worker need not guess them.

The control plane (internal/handoff) sends the request to the receiving worker as an ordinary task, and that worker's
own final check decides whether it is done. What comes back is printed as one block: only a result the receiving
worker's check confirmed reads "DONE (verified)"; anything else reads NOT DONE, FAILED or STILL RUNNING and must not be
reported as done. A question's answer is what the other worker was taught, not something anyone saw: it is advice.

Secrets the request gave are withheld before anything leaves this desktop (computer/privacy.py): the receiving task's
request is recorded like any other.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

from decurix_agent import config, task_context

TIMEOUT = 20
POLL_SECONDS = 10
DEFAULT_WAIT = 1800
DEFAULT_ASK_WAIT = 300
# How long one ask request holds the line open on the control plane before this polls.
ASK_HOLD = 50

# Exit codes: the result was verified done / it was not / still running when the wait ran out / it could not be sent.
EXIT_DONE, EXIT_NOT_DONE, EXIT_RUNNING, EXIT_ERROR = 0, 1, 2, 3

OPEN = ("queued", "running")


class HandoffError(Exception):
    """The control plane could not be reached, or refused the request (its own words)."""


def _withhold(text):
    """text with the request's secrets withheld before it leaves the desktop; fails closed (computer/privacy.py)."""

    try:
        from decurix_agent.computer import privacy
        return privacy.withhold_known(text)
    except Exception:  # noqa: BLE001 - nothing that could not be checked leaves the desktop as it was
        return "[held back: this text could not be checked for secrets]" if text else text


def _files(paths):
    """The files to carry, each checked to be a file of the work (computer/privacy.file_may_leave): never one under a
    hidden folder (~/.claude/.credentials.json, ~/.ssh) or outside the work and shared folders. Raises HandoffError,
    nothing sent, for the first that is not."""

    from decurix_agent.computer import privacy

    out = []
    for path in paths or ():
        allowed, why = privacy.file_may_leave(path)
        if allowed is None:
            raise HandoffError(f"{path} is not sent: {why}. Nothing was handed off.")
        out.append(str(allowed))
    return out


def _url(path, query=None):
    url = config.CONTROL_PLANE_URL.rstrip("/") + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    return url


def _call(method, path, body=None, query=None, timeout=TIMEOUT):
    """One JSON request to the control plane; the decoded answer, or HandoffError with the reason it gave."""

    data = json.dumps(body, ensure_ascii=False).encode("utf-8") if body is not None else None
    request = urllib.request.Request(_url(path, query), data=data, method=method,
                                     headers={"Content-Type": "application/json"} if data is not None else {})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        reason = ""
        try:
            reason = json.loads(exc.read().decode("utf-8")).get("error", "")
        except Exception:  # noqa: BLE001
            pass
        raise HandoffError(reason or f"the control plane answered HTTP {exc.code}") from None
    except (urllib.error.URLError, OSError, ValueError) as exc:
        raise HandoffError(f"the control plane could not be reached: {exc}") from None


def _pairs(items):
    inputs = {}
    for item in items or ():
        key, sep, value = item.partition("=")
        if not sep or not key.strip():
            raise HandoffError(f"--input takes name=value, not {item!r}")
        inputs[key.strip()] = _withhold(value)
    return inputs


def create(target, request, inputs=None, files=None, expect=""):
    """POST a new handoff; the record the control plane made. target is {"to_worker"|"to_role"|"to_skill": value}."""

    body = {"desktop_id": config.DESKTOP_ID, "from_task": task_context.current(), "request": _withhold(request),
            "inputs": inputs or {}, "files": _files(files), "expected": _withhold(expect or "")}
    body.update(target)
    return _call("POST", "/api/handoffs", body)


def get(handoff_id):
    return _call("GET", "/api/handoffs/get", query={"id": handoff_id})


def cancel(handoff_id):
    return _call("POST", "/api/handoffs/cancel", {"id": handoff_id, "desktop_id": config.DESKTOP_ID})


def wait_for(record, wait, poll=POLL_SECONDS, sleep=None, clock=None):
    """Poll an open handoff until it closes or `wait` seconds pass; the last record seen."""

    sleep, clock = sleep or time.sleep, clock or time.monotonic
    deadline = clock() + max(0, wait)
    while record.get("status") in OPEN and clock() < deadline:
        sleep(max(0.0, min(poll, deadline - clock())))
        record = get(record["id"])
    return record


def ask(skill, question, wait=DEFAULT_ASK_WAIT, sleep=None, clock=None):
    """Put a question to a free worker taught `skill`; the handoff record once answered, failed or out of time."""

    clock = clock or time.monotonic
    start = clock()
    answer = _call("POST", "/api/handoffs/ask",
                   {"desktop_id": config.DESKTOP_ID, "from_task": task_context.current(), "skill": skill,
                    "question": _withhold(question), "wait": min(max(0, wait), ASK_HOLD)},
                   timeout=TIMEOUT + ASK_HOLD)
    record = answer.get("handoff") or {}
    return wait_for(record, wait - (clock() - start), sleep=sleep, clock=clock)


# Bounds on what `handoff who` prints, whatever the registry holds: it goes into the model's context.
MAX_WORKERS = 60
MAX_PER_WORKER = 40


def who():
    """The other workers the skill registry knows: [{"worker", "roles", "skills": [(name, version)]}], by worker id.
    Raises HandoffError when the registry cannot be read."""

    answer = _call("GET", "/api/skillhub")
    if not isinstance(answer, dict):
        raise HandoffError("the control plane's skill registry answered something unreadable")
    me = config.DESKTOP_ID or ""
    workers = {}
    for worker, roles in (answer.get("workers") or {}).items():
        if isinstance(worker, str) and isinstance(roles, list):
            workers.setdefault(worker, {"roles": [], "skills": {}})["roles"] = [r for r in roles if isinstance(r, str)]
    for skill in answer.get("skills") or []:
        if not isinstance(skill, dict) or not isinstance(skill.get("name"), str):
            continue
        for install in skill.get("installs") or []:
            worker = install.get("worker") if isinstance(install, dict) else None
            if isinstance(worker, str):
                held = workers.setdefault(worker, {"roles": [], "skills": {}})["skills"]
                held[skill["name"]] = install.get("version") or 0
    return [{"worker": w, "roles": sorted(v["roles"]), "skills": sorted(v["skills"].items())}
            for w, v in sorted(workers.items()) if w and w != me]


def render_who(workers):
    """who()'s list as the block a worker reads before `handoff delegate` or `handoff ask`."""

    lines = ["=== WORKERS ==="]
    if not workers:
        lines.append("No other worker is in the skill registry: no role is given to one and no skill is installed on "
                     "one. There is no worker to hand work to or ask; ask the person.")
    for entry in workers[:MAX_WORKERS]:
        roles = ", ".join(entry["roles"][:MAX_PER_WORKER]) or "none"
        skills = ", ".join(f"{name} (v{version})" if version else name
                           for name, version in entry["skills"][:MAX_PER_WORKER]) or "none"
        lines.append(f"worker {entry['worker']}: roles: {roles}; skills: {skills}")
    if len(workers) > MAX_WORKERS:
        lines.append(f"({len(workers) - MAX_WORKERS} more workers not shown)")
    if workers:
        lines.append("note: use these names as they are: handoff delegate --to WORKER, --role ROLE or --skill SKILL; "
                     "handoff ask --skill SKILL. Whether a worker is free is found when the handoff is sent. A skill "
                     "taught on a desktop and never published to the registry is not listed and cannot be asked for.")
    lines.append("=== END WORKERS ===")
    return "\n".join(lines)


def _verdict(record):
    status = record.get("status", "")
    verified = bool((record.get("result") or {}).get("verified"))
    if status == "done" and verified:
        return "DONE (verified)"
    if status in OPEN:
        return "STILL RUNNING (no result yet)"
    if status == "not_done":
        return "NOT DONE (not verified)"
    return status.upper().replace("_", " ") or "UNKNOWN"


def _list(lines, title, items):
    lines.append(f"{title}:" + ("" if items else " none"))
    lines.extend(f"  - {item}" for item in items or ())


def render(record, kind="handoff"):
    """The record as the block the soul's report review reads (agent/thebot handoff_result.go): one fixed shape."""

    result = record.get("result") or {}
    verified = bool(result.get("verified")) and record.get("status") == "done"
    hid = record.get("id", "?")
    tag = "ANSWER" if kind == "ask" else "HANDOFF"
    lines = [f"=== {tag} {hid}: {_verdict(record)} ===",
             f"status: {record.get('status', '')}",
             f"verified: {'yes' if verified else 'no'}",
             f"worker: {record.get('to_worker') or '(none yet)'}"]
    if record.get("task_id"):
        lines.append(f"task: {record['task_id']}")
    if record.get("reason"):
        lines.append(f"reason: {record['reason']}")
    report = (result.get("report") or "").strip()
    lines.append("report:" + ("" if report else " none"))
    lines.extend("  " + line for line in report.splitlines())
    _list(lines, "evidence", result.get("evidence"))
    _list(lines, "files", result.get("files"))
    _list(lines, "open questions", result.get("open_questions"))
    if kind == "ask":
        lines.append("note: an answer is what the other worker was taught, not something anyone checked; "
                     "use it as advice, and still check the work it leads to.")
    elif verified:
        lines.append("note: the receiving worker's final check confirmed this result.")
    elif record.get("status") in OPEN:
        lines.append(f"note: no result yet - run `handoff status {hid}` later. Do not report this part as done.")
    else:
        lines.append("note: this part is NOT verified done. Do not report it as done: do it yourself, hand it on "
                     "again, or say in your report that it is not done and why.")
    lines.append(f"=== END {tag} {hid} ===")
    return "\n".join(lines)


def exit_code(record):
    status = record.get("status")
    if status == "done" and (record.get("result") or {}).get("verified"):
        return EXIT_DONE
    return EXIT_RUNNING if status in OPEN else EXIT_NOT_DONE


def _parser():
    parser = argparse.ArgumentParser(prog="handoff", description="Hand part of a task to another worker, or ask one.")
    sub = parser.add_subparsers(dest="command", required=True)

    d = sub.add_parser("delegate", help="hand a piece of work to another worker and wait for its checked result")
    target = d.add_mutually_exclusive_group(required=True)
    target.add_argument("--to", help="the worker's ID")
    target.add_argument("--role", help="any free worker holding this role")
    target.add_argument("--skill", help="any free worker taught this skill")
    d.add_argument("--request", required=True, help="what the other worker must do, complete on its own")
    d.add_argument("--input", action="append", default=[], metavar="NAME=VALUE", help="a value the work needs")
    d.add_argument("--file", action="append", default=[], metavar="PATH",
                   help="a file of the work to carry (under ~/work, the shared folder or ~/Downloads; never a hidden folder)")
    d.add_argument("--expect", default="", help="what the result must show when it is done")
    d.add_argument("--wait", type=float, default=DEFAULT_WAIT, help="seconds to wait for the result (default 1800)")

    a = sub.add_parser("ask", help="ask a free worker taught a skill, before asking a person")
    a.add_argument("--skill", required=True)
    a.add_argument("--wait", type=float, default=DEFAULT_ASK_WAIT)
    a.add_argument("question", nargs="+")

    s = sub.add_parser("status", help="the current state of a handoff")
    s.add_argument("id")
    c = sub.add_parser("cancel", help="stop waiting for a handoff")
    c.add_argument("id")
    sub.add_parser("who", help="the other workers, their roles and the skills they hold")
    return parser


def main(argv=None, out=sys.stdout):
    args = _parser().parse_args(argv)
    if args.command == "who":
        try:
            print(render_who(who()), file=out)
        except HandoffError as exc:
            print(f"handoff: {exc}", file=out)
            return EXIT_ERROR
        return EXIT_DONE
    try:
        if args.command == "delegate":
            target = ({"to_worker": args.to} if args.to else {"to_role": args.role} if args.role
                      else {"to_skill": args.skill})
            record = create(target, args.request, _pairs(args.input), args.file, args.expect)
            record = wait_for(record, args.wait)
            kind = "handoff"
        elif args.command == "ask":
            record, kind = ask(args.skill, " ".join(args.question), wait=args.wait), "ask"
        elif args.command == "status":
            record = get(args.id)
            kind = record.get("kind") or "handoff"
            kind = "ask" if kind == "ask" else "handoff"
        else:
            record, kind = cancel(args.id), "handoff"
    except HandoffError as exc:
        print(f"handoff: {exc}", file=out)
        return EXIT_ERROR
    print(render(record, kind), file=out)
    return exit_code(record)


if __name__ == "__main__":
    sys.exit(main())
