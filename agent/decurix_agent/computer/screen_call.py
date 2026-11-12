"""
The agent's own screen calls in broker mode (docs/gate-boundary.md, "Next: the input broker").

The agent process (vm_server) is the worker's user. In broker mode (DECURIX_SCREEN_USER=1) that user cannot reach the
screen: its X cookie is untrusted (no XTEST, nothing of the screen user's windows) and the browser has no DevTools
port. So the few screen calls the agent makes in its own process - a taught skill's replay, a look before it starts,
a host document's viewer, a question put to a web service, a demonstration being recorded, the browser warmed for a
task - go to the screen broker the way `gui` does: as a command, `gui __screen_call NAME`, handed to the broker by
the same thin client (gui_client.py) and run by the broker's child as the screen user. Nothing here ever runs one in
the worker's process instead: a broker that is missing or refuses is an error, as it is for `gui`.

Outside broker mode nothing changes: every routed function runs in its caller's process as it always did (`routed`
calls it directly; remote() is False).

What goes over: one JSON line with the call's arguments to the child's stdin, then JSON lines back on its stdout -
{"result": ...} or {"error": {...}} at the end, and on the way, for a replay, {"report": line} and {"ask": {...}} (the
escalation ladder asking this desktop's engine, which the caller answers with {"answer": text} on the child's stdin;
an ask that carries its question's "evidence", from a ladder whose caller lets soul word and read it
(escalation.soul_escalates), may be answered with soul's "decision" beside the text).
Values that JSON has no form for are tagged ({"__dx__": kind, ...}): a screenshot Frame, bytes, a set, a tuple, a dict
whose keys are not all text.

What the broker takes is only what each call site needs (SERVED): a name the broker knows, and arguments of the
kinds it allows. None of it is more than a `gui` command can do already - a click on a ref, keys, a look, a page
opened, a recording replayed through the gate - and some of it is less: a web service is read with the screen side's
own script for it, never one the caller wrote.
"""

import base64
import functools
import inspect
import json
import os
import re
import subprocess
import sys
import threading

from decurix_agent.computer import broker
from decurix_agent.computer.errors import ComputerError

# The hidden `gui` command the broker's child runs a call as (computer/__main__.py).
CALL_COMMAND = "__screen_call"

TAG = "__dx__"

# How much of a failed call's stderr is kept for its error.
STDERR_KEPT = 4000


class ScreenCallError(ComputerError):
    """The broker could not run a screen call, or the call failed with an error that is not one of the agent's."""


def remote():
    """Whether this process's screen calls go to the broker: broker mode, and this is the worker's side of it."""

    return broker.requested() and not broker.screen_side()


# ------------------------------------------------------------
# Values over JSON
# ------------------------------------------------------------

def encode(value):
    """`value` in a form json.dumps takes, with the tags decode() reverses."""

    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, list):
        return [encode(v) for v in value]
    if isinstance(value, tuple):
        return {TAG: "tuple", "v": [encode(v) for v in value]}
    if isinstance(value, (set, frozenset)):
        return {TAG: "set", "v": [encode(v) for v in value]}
    if isinstance(value, (bytes, bytearray)):
        return {TAG: "bytes", "v": base64.b64encode(bytes(value)).decode("ascii")}
    if isinstance(value, dict):
        if all(isinstance(k, str) for k in value) and TAG not in value:
            return {k: encode(v) for k, v in value.items()}
        return {TAG: "pairs", "v": [[encode(k), encode(v)] for k, v in value.items()]}
    if type(value).__name__ == "Frame" and hasattr(value, "rgb") and hasattr(value, "width"):
        return {TAG: "frame", "w": value.width, "h": value.height,
                "v": base64.b64encode(bytes(value.rgb)).decode("ascii")}
    # Anything else is said as text: a value no caller reads as an object (a path, an enum).
    return {TAG: "text", "v": str(value)}


def decode(value):
    """The value encode() was given."""

    if isinstance(value, list):
        return [decode(v) for v in value]
    if not isinstance(value, dict):
        return value
    kind = value.get(TAG)
    if kind is None:
        return {k: decode(v) for k, v in value.items()}
    data = value.get("v")
    if kind == "tuple":
        return tuple(decode(v) for v in data)
    if kind == "set":
        return {_hashable(decode(v)) for v in data}
    if kind == "bytes":
        return base64.b64decode(data)
    if kind == "pairs":
        return {_hashable(decode(k)): decode(v) for k, v in data}
    if kind == "frame":
        from decurix_agent.computer import screen
        return screen.Frame(int(value["w"]), int(value["h"]), base64.b64decode(data))
    if kind == "text":
        return str(data)
    raise ValueError(f"not a value of a screen call: {kind!r}")


def _hashable(value):
    return tuple(_hashable(v) for v in value) if isinstance(value, list) else value


def dumps(message):
    """One protocol line (bytes)."""

    return json.dumps(encode(message), separators=(",", ":")).encode("utf-8") + b"\n"


def loads(line):
    """A protocol line's message."""

    return decode(json.loads(line.decode("utf-8")))


# ------------------------------------------------------------
# What the broker takes
# ------------------------------------------------------------

def _text(limit=100000):
    return lambda v: isinstance(v, str) and len(v) <= limit and "\0" not in v


def _flag(v):
    return isinstance(v, bool)


def _number(v):
    return isinstance(v, int) and not isinstance(v, bool) and 0 <= v <= 100000


def _plain(v):
    """A JSON value (no tag decoded into something else): what a recording's program and its values are."""

    if v is None or isinstance(v, (bool, int, float, str)):
        return True
    if isinstance(v, (list, tuple)):
        return all(_plain(x) for x in v)
    if isinstance(v, dict):
        return all(isinstance(k, (str, int)) and _plain(x) for k, x in v.items())
    return False


def _optional(check):
    return lambda v: v is None or check(v)


def _keys(v):
    return isinstance(v, list) and 0 < len(v) <= 32 and all(_text(200)(k) for k in v)


TARGET_ID = re.compile(r"[A-Za-z0-9._-]{1,128}")


def _target_id(v):
    return isinstance(v, str) and bool(TARGET_ID.fullmatch(v))


# name -> (module, function, {parameter: check}). A parameter the call site never passes is not taken: its default
# is the screen side's. A ref, a key, a page or a skill's program is only what `gui` takes from the worker anyway.
SERVED = {
    "core.observe": ("decurix_agent.computer.core", "observe", {"remember": _flag}),
    "core.act": ("decurix_agent.computer.core", "act", {"kind": lambda v: v in ("click", "key"),
                                                        "ref": _optional(lambda v: _text(64)(v) or _number(v)), "keys": _optional(_keys)}),
    "core.open_target": ("decurix_agent.computer.core", "open_target",
                         {"target": _text(8192), "new_tab": _flag, "reuse": _flag}),
    "core.forget_failed_search": ("decurix_agent.computer.core", "forget_failed_search", {}),
    "screen.grab": ("decurix_agent.computer.screen", "grab", {}),
    "cdp.page_list": ("decurix_agent.computer.cdp", "page_list", {}),
    "cdp.activate_page": ("decurix_agent.computer.cdp", "activate_page", {"target_id": _target_id}),
    "browser.warm": ("decurix_agent.computer.browser", "warm", {}),
    "launchers.close": ("decurix_agent.computer.launchers", "close_recorded", {"title": _text(1000)}),
    "ask_task.read_service": ("decurix_agent.ask_task", "read_service", {"key": _text(64)}),
    "demonstration.start": ("decurix_agent.computer.demonstration", "start",
                            {"title": _text(1000), "description": _text(10000), "task_id": _text(200)}),
    "demonstration.pause": ("decurix_agent.computer.demonstration", "pause", {"reason": _text(1000)}),
    "demonstration.resume": ("decurix_agent.computer.demonstration", "resume", {}),
    "demonstration.note": ("decurix_agent.computer.demonstration", "note", {"text": _text(10000)}),
    "demonstration.stop": ("decurix_agent.computer.demonstration", "stop", {}),
    "demonstration.status": ("decurix_agent.computer.demonstration", "status", {}),
    "demonstration.recover": ("decurix_agent.computer.demonstration", "recover", {}),
    "replay.run": ("decurix_agent.computer.replay", "run",
                   {"program": lambda v: isinstance(v, dict) and _plain(v), "params": _optional(_plain),
                    "start": _number}),
}


def bound(name, args, kwargs):
    """(function, {parameter: value}) of a call the broker takes; ScreenCallError for one it does not."""

    if name not in SERVED:
        raise ScreenCallError(f"the screen broker does not take {name!r}")
    module_name, attr, checks = SERVED[name]
    import importlib
    fn = getattr(importlib.import_module(module_name), attr)
    try:
        arguments = inspect.signature(fn).bind(*args, **kwargs).arguments
    except TypeError as exc:
        raise ScreenCallError(f"{name}: {exc}") from exc
    for parameter, value in arguments.items():
        check = checks.get(parameter)
        if check is None or not check(value):
            raise ScreenCallError(f"{name}: the screen broker does not take {parameter!r} as given")
    return fn, arguments


# The parameters of a call that may name a file of the caller's: the file is handed to the broker as a descriptor,
# as `gui` hands the files its command names (gui_client.input_files) - the screen user may not open the worker's.
FILE_PARAMETERS = {"core.open_target": ("target",)}


# ------------------------------------------------------------
# The caller's side
# ------------------------------------------------------------

def routed(name):
    """A function the worker's side runs through the broker in broker mode, and in its own process otherwise."""

    def wrap(fn):
        @functools.wraps(fn)
        def inner(*args, **kwargs):
            if remote():
                return call(name, *args, **kwargs)
            return fn(*args, **kwargs)

        inner.local = fn
        return inner

    return wrap


def command(name, files=()):
    """The command a call runs as: the `gui` thin client, as agent/gui runs it in broker mode. `files`: the paths it
    names, after the call's name, so the client hands them over (they are not the call's arguments)."""

    from decurix_agent.computer import gui_client
    return [sys.executable, "-I", "-S", os.path.abspath(gui_client.__file__), CALL_COMMAND, name] + \
        [str(f) for f in files if isinstance(f, str) and f]


def named_files(name, args, kwargs):
    """The values of a call's FILE_PARAMETERS, by name or position."""

    wanted = FILE_PARAMETERS.get(name, ())
    if not wanted:
        return []
    module_name, attr, _ = SERVED[name]
    import importlib
    fn = getattr(importlib.import_module(module_name), attr)
    try:
        bound_args = inspect.signature(getattr(fn, "local", fn)).bind_partial(*args, **kwargs).arguments
    except TypeError:
        return []
    return [bound_args[p] for p in wanted if isinstance(bound_args.get(p), str)]


def wait_for_broker(seconds=120.0, pause=0.5):
    """Broker mode, the worker's side: wait until the screen broker serves (the agent starts beside the screen's
    session, and its first screen call at boot came before the broker was up). Returns whether it does."""

    import time
    from decurix_agent.computer import gui_client
    deadline = time.monotonic() + seconds
    while True:
        if gui_client.broker_ready():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(pause)


def call(name, *args, **kwargs):
    """Run a SERVED call through the broker; its result, or the error it ended with raised here."""

    return exchange(name, {"args": list(args), "kwargs": kwargs}, files=named_files(name, args, kwargs))["result"]


def exchange(name, request, on_ask=None, on_report=None, files=(), on_decide=None):
    """
    One call: `request` to the broker's child, the messages it sends answered (on_ask, or on_decide for an ask with
    its evidence) or passed on (on_report), and the last message returned: {"result": ...} and, for a replay, the
    ladder's state.
    """

    try:
        process = subprocess.Popen(command(name, files), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, cwd="/")
    except OSError as exc:
        raise ScreenCallError(f"could not reach the screen broker: {exc.strerror or exc}") from exc
    errors = []
    reader = threading.Thread(target=_drain, args=(process.stderr, errors), daemon=True)
    reader.start()
    final = None
    try:
        try:
            process.stdin.write(dumps(request))
            process.stdin.flush()
        except OSError:
            pass  # the client exited at once; what it said is on stderr
        for line in process.stdout:
            message = loads(line)
            if "ask" in message:
                _answer(process, message["ask"], on_ask, on_decide)
            elif "report" in message:
                if on_report is not None:
                    on_report(message["report"])
            elif "result" in message or "error" in message:
                final = message
                break
    finally:
        for stream in (process.stdin, process.stdout):
            try:
                stream.close()
            except OSError:
                pass
        process.wait()
        reader.join(timeout=5)
        try:
            process.stderr.close()
        except OSError:
            pass
    if final is None:
        said = b"".join(errors).decode("utf-8", "replace").strip()[-STDERR_KEPT:]
        raise ScreenCallError(f"the screen call {name} ended without an answer (exit {process.returncode})"
                              + (f": {said}" if said else ""))
    if "error" in final:
        raise rebuilt(final["error"])
    return final


def _drain(stream, into):
    kept = 0
    for chunk in iter(lambda: stream.read(4096), b""):
        into.append(chunk)
        kept += len(chunk)
        while kept > STDERR_KEPT * 2 and len(into) > 1:
            kept -= len(into.pop(0))


def _answer(process, ask, on_ask, on_decide=None):
    if on_ask is None:
        reply = {"error": "this caller has no engine to ask"}
    else:
        try:
            system, prompt, purpose = ask.get("system", ""), ask.get("prompt", ""), ask.get("purpose") or "py.skill.ask"
            if on_decide is not None and isinstance(ask.get("evidence"), dict):
                text, decision = on_decide(system, prompt, ask["evidence"], purpose=purpose)
                reply = {"answer": text} if decision is None else {"answer": text, "decision": decision}
            else:
                reply = {"answer": on_ask(system, prompt, purpose)}
        except Exception as exc:  # noqa: BLE001 - the ladder reads a failed ask as "the engine could not be asked"
            reply = {"error": str(exc)[:500] or type(exc).__name__}
    process.stdin.write(dumps(reply))
    process.stdin.flush()


def rebuilt(error):
    """The exception a call ended with, as this side can raise it: the agent's own error classes as themselves, a
    built-in one as itself, anything else as ScreenCallError."""

    module, name, message = error.get("module") or "", error.get("type") or "", error.get("message") or ""
    if module.startswith("decurix_agent."):
        try:
            import importlib
            cls = getattr(importlib.import_module(module), name)
            if isinstance(cls, type) and issubclass(cls, ComputerError):
                return cls(message)
        except (ImportError, AttributeError, TypeError):
            pass
    if module == "builtins":
        import builtins
        cls = getattr(builtins, name, None)
        if isinstance(cls, type) and issubclass(cls, Exception) and not issubclass(cls, (SystemExit,)):
            try:
                return cls(message)
            except TypeError:
                pass
    return ScreenCallError(message or name)


# ------------------------------------------------------------
# A replay, with its escalation ladder
# ------------------------------------------------------------

def ladder_state(ladder):
    """What a ladder holds (no callables): what the screen side's ladder starts from, and what comes back."""

    state = {k: v for k, v in vars(ladder).items() if not callable(v) and k != "ask_decided"}
    state["ask"] = ladder.ask_model is not None
    state["decided"] = getattr(ladder, "ask_decided", None) is not None
    return state


def apply_state(ladder, state):
    """Set what the screen side's ladder came back holding onto the caller's (only fields a ladder has)."""

    for key, value in (state or {}).items():
        if key not in ("ask", "decided", "ask_decided") and key in vars(ladder) and not callable(getattr(ladder, key)):
            setattr(ladder, key, value)


def new_ladder(state, ask_model, ask_decided=None):
    """The screen side's ladder for a replay: a fresh one with the caller's fields (and its questions handed to soul
    only when the caller's ladder hands them, "decided")."""

    from decurix_agent.computer import escalation
    ladder = escalation.Ladder()
    apply_state(ladder, state)
    ladder.ask_model = ask_model if (state or {}).get("ask") else None
    ladder.ask_decided = ask_decided if (state or {}).get("ask") and (state or {}).get("decided") is True else None
    return ladder


def program_files(program, params=None, start=1):
    """
    The files of the caller's that a replay of `program` from step `start` may open or upload: each open step's
    target and each upload's files (this run's value for an upload that takes one, as replay._run_step reads it).
    They are handed over as `gui` hands the files its command names (gui_client.input_files keeps only regular files
    the worker can read, at most MAX_INPUTS); the screen side reads none by its name. Found live (2026-11-07): a
    taught skill whose first step opens ~/work/form.html stopped at that step in broker mode.
    """

    found = []
    for step in (program or {}).get("steps") or []:
        if not isinstance(step, dict):
            continue
        n = step.get("n")
        if isinstance(n, int) and isinstance(start, int) and n < start:
            continue
        kind = step.get("kind")
        if kind == "open":
            named = [step.get("open")]
        elif kind == "upload":
            named = step.get("files") or []
            if step.get("param"):
                value = (params or {}).get(step["param"])
                named = value if isinstance(value, list) else [f.strip() for f in str(value or "").split(";")]
        else:
            continue
        for path in named if isinstance(named, list) else []:
            if isinstance(path, str) and path and path not in found:
                found.append(path)
    return found


def run_replay(program, params=None, start=1, report=None, ladder=None):
    """replay.run through the broker: the ladder's questions to the engine are asked here, its state comes back.
    The files its steps open or upload are handed over with it (program_files)."""

    request = {"args": [program], "kwargs": {"params": params, "start": start}, "report": report is not None,
               "ladder": ladder_state(ladder) if ladder is not None else None}
    ask = ladder.ask_model if ladder is not None else None
    decide = getattr(ladder, "ask_decided", None) if ladder is not None else None
    final = exchange("replay.run", request, on_ask=ask, on_report=report,
                     files=program_files(program, params, start), on_decide=decide)
    if ladder is not None:
        apply_state(ladder, final.get("ladder"))
    return final.get("result")


# ------------------------------------------------------------
# The broker's side
# ------------------------------------------------------------

class _Channel:
    """The protocol's two descriptors, kept apart from the command's own stdio."""

    def __init__(self, read_fd, write_fd):
        self.read_fd, self.write_fd, self.buffer = read_fd, write_fd, b""

    def send(self, message):
        view = memoryview(dumps(message))
        while view:
            view = view[os.write(self.write_fd, view):]

    def receive(self):
        while b"\n" not in self.buffer:
            chunk = os.read(self.read_fd, 1 << 16)
            if not chunk:
                raise EOFError("the caller went away")
            self.buffer += chunk
        line, self.buffer = self.buffer.split(b"\n", 1)
        return loads(line)


def _isolate_stdio():
    """The protocol keeps fds 0 and 1 for itself; the call's own prints and its children's go to stderr, and no
    child reads the caller's answers. Returns the channel."""

    channel = _Channel(os.dup(0), os.dup(1))
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    os.close(null)
    os.dup2(2, 1)
    sys.stdout = sys.stderr
    return channel


def _error(exc):
    return {"module": type(exc).__module__, "type": type(exc).__name__, "message": str(exc)}


def serve(argv):
    """`gui __screen_call NAME`: read the call, run it here, answer. Exit 0 once an answer was sent."""

    channel = _isolate_stdio()
    if len(argv) < 1:
        channel.send({"error": _error(ScreenCallError("a screen call names one function"))})
        return 0
    name = argv[0]  # (what follows it are files the caller handed over, command())
    try:
        request = channel.receive()
    except (EOFError, OSError, ValueError) as exc:
        sys.stderr.write(f"gui: the screen call was not read: {exc}\n")
        return 1
    try:
        if name == "replay.run":
            result, extra = _serve_replay(channel, request)
        else:
            fn, arguments = bound(name, request.get("args") or [], request.get("kwargs") or {})
            result, extra = fn(**arguments), {}
    except Exception as exc:  # noqa: BLE001 - the caller raises it as its own
        channel.send({"error": _error(exc)})
        return 0
    channel.send(dict(extra, result=result))
    return 0


def _serve_replay(channel, request):
    fn, arguments = bound("replay.run", request.get("args") or [], request.get("kwargs") or {})
    state = request.get("ladder")
    if state is not None and not isinstance(state, dict):
        raise ScreenCallError("replay.run: the ladder is not a ladder's state")

    def ask_model(system, prompt, purpose="py.skill.ask"):
        channel.send({"ask": {"system": system, "prompt": prompt, "purpose": purpose}})
        reply = channel.receive()
        if "answer" not in reply:
            raise RuntimeError(reply.get("error") or "the engine could not be asked")
        return reply["answer"]

    def ask_decided(system, prompt, evidence, *, purpose):
        channel.send({"ask": {"system": system, "prompt": prompt, "purpose": purpose, "evidence": evidence}})
        reply = channel.receive()
        if "answer" not in reply:
            raise RuntimeError(reply.get("error") or "the engine could not be asked")
        return reply["answer"], reply.get("decision")

    if request.get("report"):
        arguments["report"] = lambda line: channel.send({"report": str(line)})
    ladder = None
    if state is not None:
        ladder = arguments["ladder"] = new_ladder(state, ask_model, ask_decided)
    result = fn(**arguments)
    return result, ({"ladder": ladder_state(ladder)} if ladder is not None else {})
