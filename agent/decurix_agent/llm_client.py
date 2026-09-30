"""
The hosted model, reached through the control plane (internal/llm).

The desktop never holds the model's key: it POSTs an OpenAI-shaped chat to
CONTROL_PLANE_URL/api/llm/chat and the control plane adds the key. That is
what lets an agent with a shell and a browser use a strong model without
being able to leak the credential behind it.

`available()` says whether this desktop has a hosted model to ask:
DECURIX_LLM names its provider ("claude" or "azure"; "1", from desktops
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

from decurix_agent import config
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
    return PROVIDER in ("azure", "claude")


def chat(messages, tools=None, model="default", max_tokens=4000, temperature=None, response_format=None):
    """
    One chat completion. Returns (message dict, usage dict). Raises LLMError.
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

    data = json.dumps(body).encode("utf-8")
    answer, last, waited = None, None, 0.0

    for attempt in range(MAX_ATTEMPTS):
        wait = None
        request = urllib.request.Request(
            CHAT_URL, data=data, method="POST",
            headers={"Content-Type": "application/json"},
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

    return choices[0].get("message") or {}, answer.get("usage") or {}


def ask_once(system, prompt, model="default"):
    """One question, no tools, no history - harness.ask_model's shape."""

    message, _ = chat(
        [{"role": "system", "content": system}, {"role": "user", "content": prompt}],
        model=model, max_tokens=1500,
    )

    return (message.get("content") or "").strip()


def image_part(png_bytes=None, path=None, mime="image/png"):
    """An image as a chat content part, from bytes or a file."""

    if path is not None:
        with open(path, "rb") as f:
            png_bytes = f.read()
        mime = mimetypes.guess_type(str(path))[0] or mime

    encoded = base64.b64encode(png_bytes).decode("ascii")
    return {"type": "image_url", "image_url": {"url": f"data:{mime};base64,{encoded}"}}
