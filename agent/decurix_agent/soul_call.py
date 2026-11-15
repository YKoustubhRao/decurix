"""
One model call made through soul (plan rule 3, phase 1: one path for every worker-side call).

The agent's own questions - the final check, a taught skill's values, the grounder and the escalation ladder - used
to go straight to the control plane's proxy (llm_client) or to Ollama (ollama_client), so a run had two paths and only
soul's left a line in its trace. Soul's POST /api/llm/call makes the call through its own provider - the same
concurrency limit, proxy headers, task and turn, prompt-cache key and trace line as its own calls - with the agent's
purpose (py.*) recorded verbatim, and answers {text, truncated, usage}.

Who may ask: the agent holds the desktop's agent token; a command soul runs (the worker's `gui`) does not (soul keeps
it out of its children's environment), so it reads soul's call key from ~/.decurix/soul-call.key, which opens this one
route and nothing else.

When soul cannot make a call - it is not running (an old desktop, a test), its image has no such route, it refuses
this call, there is no token or key - the caller goes the way it always went, and that is logged once per process.

Phase 2 moves decisions, purpose by purpose: judge() hands soul the final check's evidence as data at /api/llm/judge,
and soul words the prompt and reads the answer (agent/thebot internal/judge; verify.soul_judges, DECURIX_SOUL_JUDGE);
escalation() does the same for the escalation ladder's questions at /api/llm/escalation (internal/escalation;
escalation.soul_escalates, DECURIX_SOUL_ESCALATION).

The approval gate never asks soul (in_gate): it is another user's process, and it must not depend on, or believe an
answer from, a process of the worker's user. Its calls (approval.read_choice under IN_GATE) go straight to the proxy
as DECURIX_CALLER=gate.
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

from decurix_agent import config
from decurix_agent.logging_utils import log

CALL_PATH = "/api/llm/call"
# The final check, decided by soul from its evidence (cmd/soul judge.go).
JUDGE_PATH = "/api/llm/judge"
# The escalation ladder's questions, decided by soul from their evidence (cmd/soul escalation.go).
ESCALATION_PATH = "/api/llm/escalation"
# The review pass after a task checked done, made by soul from the run it kept (cmd/soul review.go; review_pass.py).
REVIEW_PATH = "/api/llm/review"
REVIEW_PURPOSE = "py.learn.review"
# Soul writes its call key here at start (agent/thebot cmd/soul llmcall.go: callKeyPath), 0600, the worker's own.
KEY_FILE = "soul-call.key"
# After soul was found not running, how long calls go their own way before soul is tried again (it may start later).
RECHECK_SECONDS = 30
# How much longer than the call's own deadline the agent waits for soul's answer: soul's deadline covers its wait for a
# free model slot as well as the call.
ANSWER_SLACK_SECONDS = 30
# What soul takes as a call's secret values (withheld.MaxValues, withheld.MaxValueLen).
MAX_SECRETS = 1000
MAX_SECRET_BYTES = 64 * 1024


class Unusable(Exception):
    """Soul cannot make this call: the caller goes its own way."""


class Failed(Exception):
    """The provider behind soul failed the call: its HTTP status, message and headers, mapped as a direct call's."""

    def __init__(self, status, detail, headers=None):
        super().__init__(f"{status}: {detail}")
        self.status = status
        self.detail = detail
        self.headers = headers


_state = {"down_until": 0.0, "absent": False, "noted": False, "judge_absent": False, "escalation_absent": False,
          "review_absent": False}


def in_gate():
    """Whether this process is the approval gate: approval.IN_GATE (set only inside gate_server, never from the
    environment) or a process that says it calls as the gate. Either way soul is never asked."""

    approval = sys.modules.get("decurix_agent.computer.approval")
    if approval is not None and getattr(approval, "IN_GATE", False):
        return True
    return os.environ.get("DECURIX_CALLER", "").strip() == "gate"


def credential():
    """What this process shows soul: the desktop's agent token, else soul's call key from its file, else ""."""

    token = config.agent_token()
    if token:
        return token
    try:
        return (config.DECURIX_DIR / KEY_FILE).read_text(encoding="utf-8").strip()
    except OSError:
        return ""


def _note(why):
    """Say once per process that model calls go their own way, and why."""

    if not _state["noted"]:
        _state["noted"] = True
        log(f"soul_call: this call goes straight to the model, not through soul ({why}); later ones are not logged")


def usable():
    """Whether to try soul for a call now: not the gate, a token or key to show, and soul not known to be away."""

    if in_gate():
        return False
    if _state["absent"]:
        return False
    if time.monotonic() < _state["down_until"]:
        return False
    if not credential():
        _note("no agent token or soul call key in this process")
        return False
    return True


def _secrets():
    """The task's secret values for soul to withhold from what it writes of the call; Unusable when they cannot be
    read or soul could not take them all (then nothing of the call is handed to soul)."""

    try:
        from decurix_agent import task_context
        from decurix_agent.computer import privacy
        values = privacy.plain_values(task_context.secret_values())
    except Exception as exc:  # unknown secrets are never a reason to hand soul a call
        _note("the task's secret values could not be read")
        raise Unusable(f"the task's secret values could not be read: {exc}") from exc
    if len(values) > MAX_SECRETS or any(len(v.encode("utf-8")) > MAX_SECRET_BYTES for v in values):
        _note("the task's secret values are more than soul takes")
        raise Unusable("the task's secret values are more than soul takes")
    return values


def _refused(exc, absent="absent"):
    """Unusable for soul's own refusal of a call (an HTTPError whose body does not say the provider failed). `absent`
    is the flag that keeps an older soul with no such route from being asked again this process."""

    if exc.code in (404, 405):
        _state[absent] = True
    if absent == "absent":
        _note(f"soul refused the call with HTTP {exc.code}")
    else:
        log(f"soul_call: soul refused {absent.replace('_absent', '')} with HTTP {exc.code}; today's path is taken")
    return Unusable(f"soul refused the call ({exc.code})")


def _connection_refused(exc):
    reason = getattr(exc, "reason", exc)
    return isinstance(reason, (ConnectionRefusedError, FileNotFoundError))


def call(brain, messages, *, purpose, max_tokens, timeout, temperature=None, response_format=None, stop=None,
         model=None):
    """
    One call through soul: {"text", "truncated", "usage": {"prompt_tokens", "completion_tokens", "cached_tokens"}}.

    `brain` is "hosted" (llm_client) or "local" (ollama_client): a soul with the other brain refuses. Raises Unusable
    when soul cannot make the call (the caller goes its own way), Failed when the provider behind soul failed it, and
    URLError/TimeoutError/OSError when soul could not be reached mid-call.
    """

    payload = _base(brain, purpose, max_tokens, timeout, model)
    payload.update({"messages": messages, "temperature": temperature, "response_format": response_format,
                    "stop": list(stop or [])})
    return _post(CALL_PATH, payload, timeout, "absent")


def judge(evidence, *, purpose, max_tokens, timeout, look=(), closer="", model=None):
    """
    The final check made by soul (plan rule 3, phase 2): soul words the prompt from `evidence` (verify.judge_evidence)
    - with, for a second look, the terms the judge asked for (`look`) and the lines found (`closer`) - makes the call
    as /api/llm/call does, and reads the answer (internal/judge Decide). Answers as call() does plus "decision": the
    verdict ({"verdict", "confidence", "seen", "why", "look"[, "put_back"]}) or {"error", "cut_off"} for an answer it
    could not read. Raises as call() does; Unusable when soul has no such route (asked again only by a new process).
    """

    if _state["judge_absent"]:
        raise Unusable("soul has no judge route")
    payload = _base("hosted", purpose, max_tokens, timeout, model)
    # The call's own fields as verify.judge's chat() sends them (no temperature, format or stop), so the provider is
    # sent the same request as through /api/llm/call (cmd/soul judge_test.go compares the bytes).
    payload.update({"temperature": None, "response_format": None, "stop": [],
                    "evidence": evidence, "look": list(look or []), "closer": closer or ""})
    answer = _post(JUDGE_PATH, payload, timeout, "judge_absent")
    if not isinstance(answer.get("decision"), dict):
        raise Failed(502, f"soul answered no decision: {json.dumps(answer)[:200]}")
    return answer


def escalation(brain, evidence, *, purpose, max_tokens, timeout, temperature=None, stop=None, model=None):
    """
    One of the escalation ladder's questions made by soul (plan rule 3, phase 2): soul words the prompt from
    `evidence` (escalation.py's *_evidence; the question is the purpose's, py.escalation.<kind>), makes the call as
    /api/llm/call does on `brain` with these limits, and reads the answer (internal/escalation Decide). Answers as
    call() does plus "decision" (escalation.from_soul checks it). Raises as call() does; Unusable when soul has no such
    route (asked again only by a new process).
    """

    if _state["escalation_absent"]:
        raise Unusable("soul has no escalation route")
    payload = _base(brain, purpose, max_tokens, timeout, model)
    # The call's own fields as today's question sends them through call() (llm_client.ask_once: no temperature,
    # format or stop; ollama_client.ask_once: its temperature and stop sequences), so the provider is sent the same
    # request (cmd/soul escalation_test.go compares the bytes).
    payload.update({"temperature": temperature, "response_format": None, "stop": list(stop or []),
                    "evidence": evidence})
    answer = _post(ESCALATION_PATH, payload, timeout, "escalation_absent")
    if not isinstance(answer.get("decision"), dict):
        raise Failed(502, f"soul answered no decision: {json.dumps(answer)[:200]}")
    return answer


def review(task_id, secrets, *, notes="", max_tokens, timeout, model=None):
    """
    The review pass of a finished task, made by soul (review_pass.py): soul sends the run it kept of `task_id` - its
    last transcript, or its jobs' logs with `notes` (the person's words on the task) when the transcript is gone - with
    one review instruction appended, and reads the answer. Answers as call() does plus "decision": {"proposals":
    [{"kind", "text", "why", "evidence", "replaces", "in_run"}], "dropped", "error"?, "source"}. The task and its secret
    values are given, not read from task_context: the review runs after the task let go of both. Raises as call() does;
    Unusable when soul keeps nothing of the task or has no such route (an older soul: not asked again this process).
    """

    if _state["review_absent"]:
        raise Unusable("soul has no review route")
    if in_gate():
        raise Unusable("the approval gate never asks soul")
    if not credential():
        raise Unusable("no agent token or soul call key")
    payload = {"brain": "hosted", "purpose": REVIEW_PURPOSE, "task": str(task_id or ""), "turn": "",
               "model": model or "", "max_tokens": int(max_tokens), "timeout_ms": int(timeout * 1000),
               "secrets": list(secrets or ()), "temperature": None, "response_format": None, "stop": [],
               "notes": str(notes or "")}
    # (Only a 404 or 405 marks the route absent: a 409, "nothing kept of that task", is about this task.)
    answer = _post(REVIEW_PATH, payload, timeout, "review_absent")
    if not isinstance(answer.get("decision"), dict):
        raise Failed(502, f"soul answered no decision: {json.dumps(answer)[:200]}")
    return answer


def _base(brain, purpose, max_tokens, timeout, model):
    """What every call to soul carries: the brain, purpose, task, turn, model, limits and the task's secret values.
    Unusable in the gate (never, by design: the gate does not depend on, or believe, a process of the worker's user)."""

    if in_gate():
        raise Unusable("the approval gate never asks soul")
    if not credential():
        raise Unusable("no agent token or soul call key")
    from decurix_agent import task_context

    return {
        "brain": brain, "purpose": purpose, "task": task_context.current() or "",
        "turn": os.environ.get("DECURIX_TURN_ID", "").strip(), "model": model or "",
        "max_tokens": int(max_tokens), "timeout_ms": int(timeout * 1000), "secrets": _secrets(),
    }


def _post(path, payload, timeout, absent):
    """POST one call to soul and read its answer (see call())."""

    key = credential()
    if not key:
        raise Unusable("no agent token or soul call key")
    request = urllib.request.Request(
        config.THEBOT_URL.rstrip("/") + path, data=json.dumps(payload).encode("utf-8"), method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout + ANSWER_SLACK_SECONDS) as response:
            answer = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        try:
            body = json.loads(raw)
        except ValueError:
            body = {}
        if not isinstance(body, dict) or body.get("where") != "provider":
            raise _refused(exc, absent) from exc
        raise Failed(exc.code, str(body.get("error") or "")[:600], exc.headers) from exc
    except urllib.error.URLError as exc:
        if _connection_refused(exc):
            _state["down_until"] = time.monotonic() + RECHECK_SECONDS
            _note("soul is not running on this desktop")
            raise Unusable("soul is not running") from exc
        raise
    if not isinstance(answer, dict) or not isinstance(answer.get("text"), str):
        raise Failed(502, f"soul answered no text: {json.dumps(answer)[:200]}")
    return answer
