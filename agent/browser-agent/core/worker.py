"""HTML cleaning and challenge-page signals for the extension pipeline
(extension_pipeline/ext_server.py), which hands it the HTML the extension
read from its own tab.
"""
import re

from bs4 import BeautifulSoup, Comment

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
