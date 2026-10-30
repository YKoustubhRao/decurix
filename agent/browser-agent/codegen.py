"""Ask the selected model to write one short Playwright snippet for one
sub-instruction, grounded in the page's actual full HTML (and, when vision mode
is on, a screenshot -- Devstral-only, per instruction)."""
import re

import models

SYSTEM_PROMPT = """You write short Playwright Python snippets that operate on an already-open \
Playwright `page` object (sync API) to carry out ONE browser instruction.

Available in scope, nothing else -- no imports, no launching/closing a browser or context, no \
asyncio/await:
- `page` -- the current Playwright page (sync API).
- `re` -- Python's re module.

Rules:
- You are shown the page's actual (cleaned) HTML below. Read it before writing any code -- \
find the real element the instruction refers to and ground your selector in an attribute or \
text you can actually see there (id, name, role, aria-label, placeholder, href, visible text). \
Never guess a plausible-looking selector pattern (a URL path fragment, a class name) that isn't \
grounded in something you actually saw in the HTML -- if you can't find the element, look again \
rather than inventing one. Prefer an id via page.locator('#id') when present; otherwise \
get_by_role(role, name=...), get_by_placeholder, get_by_text, or an attribute-selector on href.
- Loops are fine for a genuinely repetitive instruction (e.g. "open each of the first 5 \
results", "scroll down a few times") -- write the loop, don't ask for it to be split up.
- For "the Nth search result/item" on a listing page, don't rely on matching a result's exact \
visible title text with get_by_role/get_by_text -- e-commerce/search-result cards routinely \
nest that text across multiple spans, so the computed accessible name often doesn't match what \
you saw and the locator resolves to nothing even after adding .first. A CSS attribute-selector \
on the href (something you can see in the actual HTML, e.g. '/dp/' for an Amazon product link) \
is usually more robust.
- If you were told a previous attempt's click/interaction didn't work, don't just retry the \
same click, and don't guess a different pattern blindly -- go back to the HTML you were shown \
and find the real element again, fixing the selector to match what's actually there.
- A shown href can be a long tracking/redirect URL (common on search-result pages) -- don't \
hand-copy one into page.goto("...") unless it's a URL you constructed yourself (e.g. a search \
URL you built from a query); it may be incomplete. Click the actual element instead.
- Call methods directly (.click(), .fill(...), .press(...)); don't wrap in try/except -- \
let errors surface, the caller handles retries.
- For navigation use page.goto("https://...", wait_until="domcontentloaded") -- NOT the \
default wait_until="load", which waits for every last tracker/ad/analytics request and \
routinely times out or hangs on heavy commercial sites even though the page is already usable. \
Never pass timeout=0 (waits forever) -- if a goto times out, that's just a slow site; the \
default per-call timeout is enough once you're using domcontentloaded.
- Output ONLY the Python code. No explanation, no markdown fences, no comments."""

SYSTEM_PROMPT_VISION = SYSTEM_PROMPT + """

You are also shown a screenshot of the current page. Use it to understand layout, visual \
state (e.g. a dropdown that's open, an error message shown), and to disambiguate elements \
that look similar in the HTML -- but still write selectors only from attributes/text you \
actually saw in the HTML, not visual position."""

CODE_FENCE_RE = re.compile(r"```(?:python)?\s*(.*?)```", re.DOTALL)


def extract_code(raw):
    m = CODE_FENCE_RE.search(raw)
    return (m.group(1) if m else raw).strip()


def generate_step_code(model_key, instruction, url, page_html, prior_error=None,
                        screenshot_b64=None, max_tokens=500, on_chunk=None):
    """on_chunk(partial_text), if given, is called after every token so a UI can
    show generation happening live instead of a silent wait for the full reply.
    prior_error covers both a raised exception's traceback and (from runner.py's
    verification step) a plain-English reason the last attempt didn't actually
    achieve what was expected -- either way, the model needs to try differently."""
    use_vision = screenshot_b64 and models.MODELS[model_key]["vision"]

    user = f"Current URL: {url}\n\nPage HTML:\n{page_html}\n\nInstruction: {instruction}"
    if prior_error:
        user += f"\n\nThe previous attempt at this instruction did not work -- fix it:\n{prior_error}"

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT_VISION if use_vision else SYSTEM_PROMPT},
        {"role": "user", "content": user},
    ]
    full = ""
    for evt in models.chat_stream(model_key, messages,
                                   images=[screenshot_b64] if use_vision else None,
                                   max_tokens=max_tokens):
        full = evt["text"]
        if evt["type"] == "chunk" and on_chunk:
            on_chunk(full)
    return extract_code(full)
