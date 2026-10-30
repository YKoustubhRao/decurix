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


def get_shared_page(browser):
    """The most recently created page/tab, not always the first -- a click on a
    link with target="_blank" (very common on e-commerce product links) opens a
    new tab that the ORIGINAL page object never follows; without this, every
    subsequent snapshot/exec call would keep looking at the tab left behind
    instead of where the action actually landed."""
    ctx = browser.contexts[0] if browser.contexts else browser.new_context(no_viewport=True)
    return ctx.pages[-1] if ctx.pages else ctx.new_page()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cdp", required=True, help="CDP endpoint printed by browser_host.py")
    ap.add_argument("--mode", choices=["snapshot", "exec"], required=True)
    ap.add_argument("--code-file", default=None)
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
            result["html"] = get_full_html(page)
            result["captcha"] = detect_captcha(page)
            if args.vision:
                png = page.screenshot(type="jpeg", quality=70, full_page=False)
                result["screenshot_b64"] = base64.b64encode(png).decode("ascii")
        else:
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

        print(json.dumps(result))


if __name__ == "__main__":
    main()
