"""
The hosted model, reached through the control plane (internal/llm).

The desktop never holds the model's key: it POSTs an OpenAI-shaped chat to
CONTROL_PLANE_URL/api/llm/chat and the control plane adds the key. That is
what lets an agent with a shell and a browser use a strong model without
being able to leak the credential behind it.

Soul makes the call when it can (soul_call, plan rule 3): through its own
provider, so the call has soul's trace line, tags and prompt-cache key like
soul's own. Straight to the proxy only when soul cannot (not running, an
older image, a test) - and always in the approval gate, which never depends
on a process of the worker's user.

`available()` says whether this desktop has a hosted model to ask:
DECURIX_LLM names its provider ("claude", "azure" or "openai" - any
OpenAI-compatible server the control plane was given; "1", from desktops
created before there was a choice, is Azure), set by buildDesktopEnv only for
a desktop whose brain is that hosted model.
"""

import base64
import json
import mimetypes
import os
import random
import time
import urllib.error
import urllib.request

from decurix_agent import config, soul_call, usage
from decurix_agent.logging_utils import log

PROVIDER = {"1": "azure"}.get(
    os.environ.get("DECURIX_LLM", "").strip().lower(),
    os.environ.get("DECURIX_LLM", "").strip().lower(),
)
CHAT_URL = config.CONTROL_PLANE_URL.rstrip("/") + f"/api/llm/{PROVIDER or 'azure'}/chat"

# One answer can take a while when it reads several pages of a scanned PDF.
TIMEOUT_SECONDS = int(os.environ.get("DECURIX_LLM_TIMEOUT_SECONDS", "240"))

# Retried: the endpoint's rate limit (429) and the gateway failing (5xx) -
# both pass. A 400 is our mistake and is not retried. A rate limit is per
# minute of tokens, and a task working a screen sends a screenshot with every
# call: live, a 30-second retry budget failed a task that the next minute would
# have served. So a 429 waits as long as the endpoint says (Retry-After, passed
# through by the control plane), within RETRY_BUDGET_SECONDS in all.
RETRY_STATUSES = {429, 500, 502, 503, 504}
MAX_ATTEMPTS = 8
RETRY_BUDGET_SECONDS = float(os.environ.get("DECURIX_LLM_RETRY_SECONDS", "240"))
# The budget an empty answer cut off by its limit is asked again with (a reasoning model's thinking counts in it).
EMPTY_RETRY_TOKENS = 16000
# The models (provider and the name asked for) seen to spend a short budget thinking and answer nothing: their later
# calls start with EMPTY_RETRY_TOKENS. A budget is a cap, not a cost, so the room is free; the call it saves was a
# whole prompt sent for nothing and up to a minute (found in the learning curves, 2026-11-15: every run's request
# reading, value fill and check did it again). Kept on disk so a restarted agent - between a series' tasks - knows.
ROOMY_FILE = None  # (None: config.DECURIX_DIR / "llm-roomy-models.json", read when used)


def _roomy_file():
    return ROOMY_FILE or (config.DECURIX_DIR / "llm-roomy-models.json")


def _roomy_key(model):
    return f"{PROVIDER or 'azure'}:{model or 'default'}"


def _roomy():
    try:
        with open(_roomy_file(), encoding="utf-8") as f:
            return set(json.loads(f.read()))
    except (OSError, ValueError, TypeError):
        return set()


def _remember_roomy(model):
    from pathlib import Path
    from decurix_agent.computer import privacy
    try:
        Path(_roomy_file()).parent.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(Path(_roomy_file()), sorted(_roomy() | {_roomy_key(model)}), atomic=True, shapes=False)
    except OSError as exc:
        log(f"llm: could not keep that {_roomy_key(model)} needs room to answer: {exc}")


def forget_roomy():
    """Forget which models needed room to answer (a test's clean slate)."""

    try:
        os.remove(_roomy_file())
    except OSError:
        pass


class LLMError(RuntimeError):
    pass


def retry_after(headers):
    """Seconds a 429's headers say to wait (Retry-After-Ms, Retry-After in seconds), or None."""

    if headers is None:
        return None
    for name, scale in (("Retry-After-Ms", 1000.0), ("Retry-After", 1.0)):
        value = headers.get(name)
        if value:
            try:
                return max(0.0, float(value) / scale)
            except ValueError:
                continue
    return None


def available():
    """Whether this desktop has a hosted model to ask: Azure, Claude, or any OpenAI-compatible server ("openai")."""

    return PROVIDER in ("azure", "claude", "openai")


def _reasoning_effort():
    """What a person set for this worker in ~/.decurix/reasoning-effort: low, medium or high; None otherwise."""

    try:
        effort = (config.DECURIX_DIR / "reasoning-effort").read_text(encoding="utf-8").strip()
    except OSError:
        return None
    return effort if effort in ("low", "medium", "high") else None


# What a call is for travels in headers (PLAN-P2 C1), never in the body: the prompt bytes - and with them the
# provider's cache - stay the same. The control plane's proxy stores every call with them (R2, R3).
PURPOSE_HEADER = "X-Decurix-Purpose"
TASK_HEADER = "X-Decurix-Task"
CALLER_HEADER = "X-Decurix-Caller"


def tag_headers(purpose):
    """The headers that say what a call is for, which task it works on and which process makes it (C1)."""

    from decurix_agent import task_context

    headers = {PURPOSE_HEADER: str(purpose), CALLER_HEADER: os.environ.get("DECURIX_CALLER", "").strip() or "agent"}
    task = task_context.current()
    if task:
        headers[TASK_HEADER] = task
    return headers


def chat(messages, tools=None, model="default", max_tokens=4000, temperature=None, response_format=None, *, purpose):
    """
    One chat completion. Returns (message dict, usage dict). Raises LLMError.

    `purpose` names what the call is for ("py.judge", ...): keyword-only and required, so a call site that says
    nothing fails in its tests instead of reaching the proxy untagged (agent/tests/test_purpose_tags.py).
    """

    return _chat(messages, tools, model, max_tokens, temperature, response_format, purpose, None)


def chat_decided(via_soul, messages, *, max_tokens, purpose):
    """
    chat() for a decision soul makes itself (plan rule 3, phase 2): `via_soul(max_tokens)` is a soul route that makes
    the call from its evidence and reads the answer (soul_call.judge), answering as soul_call.call does plus its
    "decision", which comes back in the message (message["decision"]). Retries, the cut-off retry and the usage count
    are chat()'s. When soul is not up, or that route cannot make the call (soul_call.Unusable), `messages` - today's
    prompt - go the way chat() sends them: through /api/llm/call, else straight to the proxy, with no decision.
    """

    return _chat(messages, None, "default", max_tokens, None, None, purpose, via_soul)


def _chat(messages, tools, model, max_tokens, temperature, response_format, purpose, via_soul):

    if max_tokens < EMPTY_RETRY_TOKENS and _roomy_key(model) in _roomy():
        max_tokens = EMPTY_RETRY_TOKENS
    body = {
        "model": model,
        "messages": messages,
        "max_completion_tokens": max_tokens,
    }

    if tools:
        body["tools"] = tools
        body["tool_choice"] = "auto"

    if temperature is not None:
        body["temperature"] = temperature

    if response_format is not None:
        body["response_format"] = response_format

    effort = _reasoning_effort()
    if effort:
        body["reasoning_effort"] = effort
    data = json.dumps(body).encode("utf-8")
    answer, last, waited = None, None, 0.0
    # Soul has no tools to offer a call (no caller passes any): such a call goes straight to the proxy.
    through_soul = not tools and soul_call.usable()

    for attempt in range(MAX_ATTEMPTS):
        wait = None
        began = time.monotonic()

        try:
            if through_soul:
                try:
                    if via_soul is not None:
                        try:
                            answer = _shaped(via_soul(max_tokens))
                            break
                        except soul_call.Unusable:
                            via_soul = None  # this call, from here on, is today's: its messages through soul's call
                    answer = _soul_answer(messages, model, max_tokens, temperature, response_format, purpose)
                    break
                except soul_call.Unusable:
                    through_soul = False  # this call, from here on, goes the way it always went

            request = urllib.request.Request(
                CHAT_URL, data=data, method="POST",
                headers={"Content-Type": "application/json", **tag_headers(purpose)},
            )
            with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
                answer = json.loads(response.read().decode("utf-8"))
            break

        except soul_call.Failed as exc:
            # The provider failed behind soul: mapped exactly as the proxy's own answer is below.
            last = LLMError(f"the hosted model answered {exc.status}: {exc.detail}")
            if exc.status not in RETRY_STATUSES:
                raise last
            wait = retry_after(exc.headers)

        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:600]
            last = LLMError(f"the hosted model answered {exc.code}: {detail}")

            if exc.code not in RETRY_STATUSES:
                raise last
            wait = retry_after(exc.headers)

        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            via = "soul" if through_soul else "the control plane"
            last = LLMError(f"could not reach the hosted model through {via}: {exc}")

        if wait is None:
            wait = min(2 ** attempt * 2, 60)
        wait += random.uniform(0, 2)  # desktops throttled together do not all retry in the same second
        if through_soul:
            # Soul already asked again within the call's deadline (a 429 as long as it allowed, a 5xx a few times):
            # the time it spent counts against this budget too, so a call is never retried twice over.
            waited += time.monotonic() - began

        if waited + wait > RETRY_BUDGET_SECONDS:
            break
        log(f"llm: {last} - trying again in {wait:.0f}s")
        time.sleep(wait)
        waited += wait

    if answer is None:
        raise last

    choices = answer.get("choices") or []
    if not choices:
        raise LLMError(f"the hosted model gave no answer: {json.dumps(answer)[:400]}")

    # Counted here, for every caller: reading a request, filling a skill's
    # values, the final check and a kept lesson are model calls like the
    # engine's own, and a task's record used to show only some of them.
    spent = answer.get("usage") or {}
    usage.add(input_tokens=spent.get("prompt_tokens"), output_tokens=spent.get("completion_tokens"),
              cached_input_tokens=cached_tokens(spent))

    message = choices[0].get("message") or {}
    if (choices[0].get("finish_reason") == "length" and not message.get("tool_calls")
            and max_tokens < EMPTY_RETRY_TOKENS):
        # A reasoning model can spend the whole budget thinking and answer nothing - or answer half. Found live
        # (2026-11-01): reading a request against the taught tasks got an empty answer at 800 tokens, and a taught task
        # went to the engine as a request it had never seen. Found in an eval (2026-11-04, review round 9): the final
        # check's answer was cut off mid-JSON, so the check was not made, and the task was run again and saved a
        # second bill. An answer cut off at the limit is no answer: once more, with room to answer.
        cut = "answered nothing" if not (message.get("content") or "").strip() else "was cut off"
        log(f"llm: the model used all {max_tokens} tokens and {cut} - asking again with {EMPTY_RETRY_TOKENS}")
        _remember_roomy(model)
        return _chat(messages, tools, model, EMPTY_RETRY_TOKENS, temperature, response_format, purpose, via_soul)
    return message, spent


def _soul_answer(messages, model, max_tokens, temperature, response_format, purpose):
    """One call through soul (soul_call), as the proxy's OpenAI-shaped answer: the rest of chat() reads it unchanged.
    Counted once, by chat() (usage.add): soul counts a call it makes for the agent in no job of its own."""

    return _shaped(soul_call.call("hosted", messages, purpose=purpose, max_tokens=max_tokens, timeout=TIMEOUT_SECONDS,
                                  temperature=temperature, response_format=response_format, model=model))


def _shaped(got):
    """Soul's answer {text, truncated, usage[, decision]} as the proxy's OpenAI-shaped one; a decision soul made of the
    answer (a decision route's) rides in the message."""

    spent = got.get("usage") or {}
    message = {"role": "assistant", "content": got.get("text") or ""}
    if "decision" in got:
        message["decision"] = got["decision"]
    return {
        "choices": [{"message": message, "finish_reason": "length" if got.get("truncated") else "stop"}],
        "usage": {"prompt_tokens": spent.get("prompt_tokens"), "completion_tokens": spent.get("completion_tokens"),
                  "prompt_tokens_details": {"cached_tokens": spent.get("cached_tokens")}},
    }


def cached_tokens(spent):
    """The part of an answer's prompt tokens the provider read from its prompt cache, as the proxy passes the answer
    on: OpenAI's prompt_tokens_details.cached_tokens, or an Anthropic-shaped cache_read_input_tokens. None when the
    answer says nothing of it (unknown, not 0). Counted so the tokens-per-task measure shows the cached share
    (resource plan B6: it was dropped)."""

    details = (spent or {}).get("prompt_tokens_details")
    found = details.get("cached_tokens") if isinstance(details, dict) else None
    if found is None:
        found = (spent or {}).get("cache_read_input_tokens")
    try:
        return max(0, int(found)) if found is not None else None
    except (TypeError, ValueError):
        return None


def ask_once(system, prompt, *, purpose):
    """One question, no tools, no history - harness.ask_model's shape."""

    message, _ = chat([{"role": "system", "content": system}, {"role": "user", "content": prompt}], max_tokens=1500,
                      purpose=purpose)

    return (message.get("content") or "").strip()


def ask_decided(system, prompt, via_soul, *, purpose):
    """
    ask_once for a question soul words and reads itself (plan rule 3, phase 2; harness.ask_decided): (text, decision).
    `via_soul(max_tokens)` is soul's decision route for it; when soul cannot make the call, ask_once's messages go as
    ask_once sends them (chat_decided) and the decision is None - the caller reads the text itself.
    """

    message, _ = chat_decided(via_soul, [{"role": "system", "content": system}, {"role": "user", "content": prompt}],
                              max_tokens=1500, purpose=purpose)
    return (message.get("content") or "").strip(), message.get("decision")


def image_part(png_bytes=None, path=None, mime="image/png"):
    """An image as a chat content part, from bytes or a file."""

    if path is not None:
        with open(path, "rb") as f:
            png_bytes = f.read()
        mime = mimetypes.guess_type(str(path))[0] or mime

    encoded = base64.b64encode(png_bytes).decode("ascii")
    return {"type": "image_url", "image_url": {"url": f"data:{mime};base64,{encoded}"}}
