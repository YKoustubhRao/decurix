"""
The hosted model, reached through the control plane (internal/llm).

The desktop never holds the model's key: it POSTs an OpenAI-shaped chat to
CONTROL_PLANE_URL/api/llm/chat and the control plane adds the key. That is
what lets an agent with a shell and a browser use a strong model without
being able to leak the credential behind it.

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

from decurix_agent import config, usage
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

    for attempt in range(MAX_ATTEMPTS):
        wait = None
        request = urllib.request.Request(
            CHAT_URL, data=data, method="POST",
            headers={"Content-Type": "application/json", **tag_headers(purpose)},
        )

        try:
            with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
                answer = json.loads(response.read().decode("utf-8"))
            break

        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:600]
            last = LLMError(f"the hosted model answered {exc.code}: {detail}")

            if exc.code not in RETRY_STATUSES:
                raise last
            wait = retry_after(exc.headers)

        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            last = LLMError(f"could not reach the hosted model through the control plane: {exc}")

        if wait is None:
            wait = min(2 ** attempt * 2, 60)
        wait += random.uniform(0, 2)  # desktops throttled together do not all retry in the same second

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
        return chat(messages, tools=tools, model=model, max_tokens=EMPTY_RETRY_TOKENS, temperature=temperature,
                    response_format=response_format, purpose=purpose)
    return message, spent


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


def image_part(png_bytes=None, path=None, mime="image/png"):
    """An image as a chat content part, from bytes or a file."""

    if path is not None:
        with open(path, "rb") as f:
            png_bytes = f.read()
        mime = mimetypes.guess_type(str(path))[0] or mime

    encoded = base64.b64encode(png_bytes).decode("ascii")
    return {"type": "image_url", "image_url": {"url": f"data:{mime};base64,{encoded}"}}
