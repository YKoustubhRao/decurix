"""Short-lived process that connects to the shared browser (browser_host.py)
to either take a page snapshot or execute one piece of LLM-generated code.

Run as its own subprocess per call (not imported) so a runaway generated
snippet can be hard-killed via subprocess timeout without taking the shared
browser down with it.
"""
import argparse
import base64
import json
import re
import sys
import traceback
from pathlib import Path

from bs4 import BeautifulSoup, Comment
from playwright.sync_api import sync_playwright

# How much cleaned HTML to hand the model, in characters (not exposed to generated
# code -- this is us preparing the model's own input, not a sandbox capability).
# ~100k chars is roughly 25k tokens, leaving headroom in the 40960 context budget
# for the system prompt, instruction, and completion.
FULL_HTML_MAX_CHARS = 100_000
KEEP_ATTRS = {"id", "class", "name", "type", "role", "placeholder", "aria-label",
              "aria-expanded", "aria-selected", "title", "alt", "href", "value", "for"}

# Only human-facing phrases and actually-rendered elements -- raw HTML text search
# is too noisy (e.g. Wikipedia ships a dormant wgConfirmEditHCaptchaSiteKey config
# variable on every article for its edit-protection feature, which isn't a live
# challenge and shouldn't halt a run that never tried to edit anything).
CAPTCHA_TEXT_PATTERNS = [
    r"verify you are human", r"are you a robot", r"unusual traffic",
    r"complete the security check", r"prove you.?re not a robot",
    r"i'?m not a robot",
]
CAPTCHA_TITLES = ["just a moment", "attention required", "access denied"]

def detect_captcha(page):
    try:
        title = (page.title() or "").lower()
    except Exception:
        title = ""
    for t in CAPTCHA_TITLES:
        if t in title:
            return f"page title suggests a challenge page: {title!r}"

    try:
        frame_loc = page.locator(
            'iframe[src*="recaptcha"], iframe[src*="hcaptcha"], iframe[src*="turnstile"]'
        )
        for idx in range(frame_loc.count()):
            fl = frame_loc.nth(idx)
            if fl.is_visible():
                return f"visible captcha iframe: {(fl.get_attribute('src') or '')[:150]}"
    except Exception:
        pass

    try:
        body_text = page.inner_text("body").lower()
    except Exception:
        body_text = ""
    for pat in CAPTCHA_TEXT_PATTERNS:
        if re.search(pat, body_text):
            return f"visible page text matched captcha pattern: {pat!r}"
    return None


def get_full_html(page, max_chars=FULL_HTML_MAX_CHARS):
    """The page's own real HTML, cleaned of noise (scripts/styles/comments) and
    with attributes pruned to the ones useful for writing a selector -- but every
    real element stays, unlike the old filtered interactive-elements-only list,
    which could silently omit the exact element an instruction needed (that's
    what caused the model to guess an ungrounded URL pattern on a Bing page)."""
    html = page.content()
    soup = BeautifulSoup(html, "html.parser")
    for tag in soup(["script", "style", "svg", "noscript", "path", "head", "link", "meta"]):
        tag.decompose()
    for c in soup.find_all(string=lambda t: isinstance(t, Comment)):
        c.extract()
    for tag in soup.find_all(True):
        for attr in list(tag.attrs):
            if attr not in KEEP_ATTRS:
                del tag.attrs[attr]
            elif isinstance(tag.attrs.get(attr), str) and len(tag.attrs[attr]) > 300:
                tag.attrs[attr] = tag.attrs[attr][:300]
    text = str(soup.body if soup.body else soup)
    text = re.sub(r"\n\s*\n+", "\n", text)
    if len(text) > max_chars:
        text = text[:max_chars] + "\n... (truncated -- page is larger than the context budget)"
    return text


def get_live_field_values(page, max_fields=30):
    """A browser's serialized outerHTML always reflects an <input>/<textarea>'s
    ORIGINAL markup value, never a value set later via .fill() or typing --
    that's tracked as a separate live DOM property that never syncs back into
    the text node. So a static HTML snapshot can NEVER show "the search box
    now contains X" after a fill, no matter how the page is cleaned -- this
    queries the live .value directly so codegen/verify can actually see it
    (found via a real false-negative: a correct page.fill() into Google's
    search box was reported NOT_MET because the snapshot's HTML still showed
    the textarea as empty)."""
    try:
        return page.eval_on_selector_all(
            "input, textarea, select",
            """(els, max) => els.slice(0, max).map(e => {
                const val = e.tagName === 'SELECT'
                    ? (e.selectedOptions[0] ? e.selectedOptions[0].textContent.trim() : '')
                    : e.value;
                return val ? {tag: e.tagName.toLowerCase(), id: e.id, name: e.name, value: val} : null;
            }).filter(Boolean)""",
            max_fields,
        )
    except Exception:
        return []


def get_shared_page(browser):
    """The most recently created page/tab, not always the first -- a click on a
    link with target="_blank" (very common on e-commerce product links) opens a
    new tab that the ORIGINAL page object never follows; without this, every
    subsequent snapshot/exec call would keep looking at the tab left behind
    instead of where the action actually landed."""
    ctx = browser.contexts[0] if browser.contexts else browser.new_context(no_viewport=True)
    return ctx.pages[-1] if ctx.pages else ctx.new_page()


# Mirrors extension/background.js's domAction/pick(): resolve (selector, near)
# to ONE element, preferring a candidate whose own text matches `near`, else
# climbing each candidate to the smallest ancestor that still contains only
# that one candidate (its "item container") and matching THAT text instead --
# needed for e.g. an "Add to cart" button whose own text never contains the
# product name, only its surrounding card does. Kept as fixed JS (never
# model-authored) for the same reason ext_codegen.py gives: this is the FAST
# path, deliberately not running arbitrary generated code.
_PICK_JS = """([selector, near]) => {
  const nodes = Array.from(document.querySelectorAll(selector));
  if (!nodes.length) return null;
  if (!near) return nodes[0];
  const needle = String(near).toLowerCase();
  const textOf = (n) => (n.innerText || n.textContent || "").toLowerCase();
  for (const n of nodes) if (textOf(n).includes(needle)) return n;
  for (const n of nodes) {
    let ctx = n;
    while (ctx.parentElement && nodes.filter((o) => ctx.parentElement.contains(o)).length === 1) {
      ctx = ctx.parentElement;
    }
    if (textOf(ctx).includes(needle)) return n;
  }
  return null;
}"""


def do_action(page, action):
    """Deterministic execution of one fixed action dict -- the Playwright-side
    counterpart to extension/background.js's execAction/domAction. Raises on
    failure (caught by the worker.py CLI, same as a generated-code exception),
    letting runner.py's existing pause-on-failure handling cover this path
    unchanged."""
    kind = action["type"]
    if kind == "navigate":
        url = action["url"]
        if not re.match(r"^https?://", url):
            url = "https://" + url
        page.goto(url, wait_until="domcontentloaded", timeout=20000)
        return
    if kind == "scroll":
        delta = 800 if action.get("direction", "down") != "up" else -800
        page.mouse.wheel(0, delta)
        page.wait_for_timeout(300)
        return

    selector = action["selector"]
    handle = page.evaluate_handle(_PICK_JS, [selector, action.get("near") or None])
    el = handle.as_element()
    if el is None:
        near = action.get("near")
        detail = f' belongs to an item matching near="{near}"' if near else ""
        raise RuntimeError(f'no element matches selector: {selector}{detail}')
    el.scroll_into_view_if_needed()

    if kind == "click":
        el.click(timeout=8000)
        page.wait_for_timeout(300)
    elif kind == "type":
        el.fill(action["text"], timeout=8000)
    elif kind == "search":
        el.fill(action["text"], timeout=8000)
        el.press("Enter")
        page.wait_for_timeout(500)
    elif kind == "select":
        want = action["text"]
        try:
            el.select_option(label=want, timeout=8000)
        except Exception:
            # Fuzzy fallback, mirroring domAction's substring match: exact
            # label/value failed, so pick the first option whose text contains it.
            value = el.evaluate(
                """(sel, want) => {
                    const w = want.toLowerCase();
                    const opt = Array.from(sel.options).find(
                        (o) => (o.textContent || '').trim().toLowerCase().includes(w));
                    return opt ? opt.value : null;
                }""",
                want,
            )
            if value is None:
                raise RuntimeError(f'no option matching "{want}" in {selector}')
            el.select_option(value=value, timeout=8000)
    else:
        raise RuntimeError(f"unknown action type: {kind}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cdp", required=True, help="CDP endpoint printed by browser_host.py")
    ap.add_argument("--mode", choices=["snapshot", "exec", "action"], required=True)
    ap.add_argument("--code-file", default=None)
    ap.add_argument("--action-file", default=None)
    ap.add_argument("--vision", action="store_true", help="also capture a screenshot (snapshot mode only)")
    args = ap.parse_args()

    with sync_playwright() as p:
        browser = p.chromium.connect_over_cdp(args.cdp)
        page = get_shared_page(browser)
        # Keep well under runner.py's subprocess kill timeout so a bad selector
        # raises a normal, informative Playwright TimeoutError we can feed back
        # to the model, instead of the whole process getting hard-killed first.
        page.set_default_timeout(8000)
        page.set_default_navigation_timeout(15000)

        result = {}
        if args.mode == "snapshot":
            result["url"] = page.url
            try:
                result["title"] = page.title()
            except Exception:
                result["title"] = ""
            html = get_full_html(page)
            live_fields = get_live_field_values(page)
            if live_fields:
                lines = [f"{f['tag']}#{f['id'] or f['name']}: {f['value']!r}" for f in live_fields]
                html += (
                    "\n\n--- LIVE FORM FIELD VALUES (not visible in the HTML above -- browsers "
                    "never reflect a value set via .fill()/typing in outerHTML) ---\n" + "\n".join(lines)
                )
            result["html"] = html
            result["captcha"] = detect_captcha(page)
            if args.vision:
                png = page.screenshot(type="jpeg", quality=70, full_page=False)
                result["screenshot_b64"] = base64.b64encode(png).decode("ascii")
        elif args.mode == "exec":
            prev_url = page.url
            code = open(args.code_file, encoding="utf-8").read()
            try:
                exec(compile(code, "<generated>", "exec"), {"page": page, "re": re})
                result["success"] = True
                result["error"] = None
            except Exception:
                result["success"] = False
                result["error"] = traceback.format_exc(limit=4)
            result["prev_url"] = prev_url
            result["url"] = page.url
            result["captcha"] = detect_captcha(page)
        else:  # action
            prev_url = page.url
            action = json.loads(open(args.action_file, encoding="utf-8").read())
            try:
                do_action(page, action)
                result["success"] = True
                result["error"] = None
            except Exception:
                result["success"] = False
                result["error"] = traceback.format_exc(limit=4)
            result["prev_url"] = prev_url
            result["url"] = page.url
            result["captcha"] = detect_captcha(page)

        print(json.dumps(result))


if __name__ == "__main__":
    main()
