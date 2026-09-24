import json
import re
import time
import traceback
import urllib.parse

from decurix_agent import progress, state, usage
from decurix_agent.computer import cdp, core, privacy
from decurix_agent.computer.errors import ComputerError
from decurix_agent.computer.mimic import SECOND_JOB
from decurix_agent.logging_utils import log
from decurix_agent.messaging import write_final_reply


# ============================================================
# Asking a web app, as a task
#
# `chatgpt "where is addis ababa"`, `ask chatgpt ...`, `google best pizza near
# me`, `search youtube for lo-fi` name a service the desktop's browser can
# reach and something to put to it. The desktop's own model is small, and the
# live bug that made this module: TheBot took `chatgpt "..."` for a chat
# question, rerouted it to the operator, which tried
# `curl https://www.google.com/search?q=` on an image with no curl, and a
# person was paged to "install curl". What the request says is already the
# whole plan - open the service with the question in its address (every one
# here takes it there), wait for the answer, read it off the page - so no
# model is asked anything.
#
# The page is opened in the desktop's Chromium like any other `open` (audited,
# recorded, on the screen for a person to see and carry on in); what the
# reply says is read from that page over DevTools. Dispatched by the command
# watcher before the engine, after a recorded task (computer/mimic.py): a way
# a person showed is still preferred over this one.
# ============================================================

ENGINE = "web"

# How long a page may take, on a 1-vCPU desktop: ChatGPT answered in ~20 s,
# a Google page with its AI overview in ~12 s, YouTube's results in ~15 s.
ASSISTANT_SECONDS = 120
SEARCH_SECONDS = 45
# An answer that streams in is done when it has stopped changing for this long.
STABLE_SECONDS = 3
# A reply still marked as being written that has not changed for this long is
# taken as it stands.
STALLED_SECONDS = 12
POLL_SECONDS = 1.5
MAX_RESULTS = 5
MAX_ANSWER_CHARS = 4000


# What a page says when it will not answer an automated browser. Shared by
# every reader: a challenge page is the same few phrases everywhere.
BLOCKED_JS = r"""
function decurixBlocked(text) {
  var m = /(verify you are human|are you a robot|not a robot|unusual traffic|just a moment\.\.\.|checking your browser|complete the security check|captcha)/i.exec(text || "");
  return m ? m[1] : "";
}
"""

# ChatGPT (checked 2026-09 against the logged-out page, whose classes are
# generated): the reply is li[data-message-role=assistant], which gains
# data-message-complete when it has finished; inside it the source pills
# ("W Wikipedia +1") are [data-assistant-content-reference] and the action bar
# [data-assistant-message-actions]. The reply is read from a copy with those
# taken out, laid out off screen so innerText keeps its line breaks - the page
# a person may be looking at is never changed. data-message-author-role is
# what the page used before; the screen-reader heading "ChatGPT said:" in its
# text is the last resort.
CHATGPT_JS = BLOCKED_JS + r"""
(function () {
  var text = document.body ? document.body.innerText : "";
  var blocked = decurixBlocked(text);
  if (blocked) return JSON.stringify({state: "blocked", why: blocked, excerpt: text.slice(0, 400)});

  var turns = document.querySelectorAll('li[data-message-role="assistant"], [data-message-author-role="assistant"]');
  var turn = turns.length ? turns[turns.length - 1] : null;
  var answer = "", complete = false;

  if (turn) {
    complete = turn.hasAttribute("data-message-complete");
    var copy = turn.cloneNode(true);
    // An entity chip ("Mount Kilimanjaro" as a button) sits beside the words
    // it names, or stands in for them: keep its name only when the paragraph
    // does not already say it.
    copy.querySelectorAll("[data-assistant-entity-reference]").forEach(function (chip) {
      var name = ((chip.querySelector("[data-assistant-entity-name]") || chip).textContent || "").trim();
      var block = chip.closest("p, li, h1, h2, h3, h4, td") || chip.parentElement;
      var rest = block ? block.textContent.replace(chip.textContent, "") : "";
      if (name && rest.indexOf(name) < 0) chip.replaceWith(document.createTextNode(name));
      else chip.remove();
    });
    copy.querySelectorAll('[data-assistant-content-reference], [data-assistant-message-actions], [data-message-attribution], button, [aria-hidden="true"], .sr-only').forEach(function (n) { n.remove(); });
    var box = document.createElement("div");
    // Not visibility:hidden - innerText leaves out whatever is not visible.
    box.style.cssText = "position:fixed;left:-20000px;top:0;width:760px;opacity:0;pointer-events:none";
    box.appendChild(copy);
    document.body.appendChild(box);
    answer = copy.innerText;
    box.remove();
  }

  if (!answer.trim()) {
    var marker = "ChatGPT said:";
    var at = text.lastIndexOf(marker);
    if (at >= 0) {
      answer = text.slice(at + marker.length);
      ["ChatGPT is AI and can make mistakes", "ChatGPT can make mistakes", "\nChat with ChatGPT", "\nMessage ChatGPT", "\nAsk anything"].forEach(function (end) {
        var cut = answer.indexOf(end);
        if (cut >= 0) answer = answer.slice(0, cut);
      });
    }
  }

  answer = answer.replace(/\n{3,}/g, "\n\n").trim();
  var limit = /(you've reached (our|the|your) (limit|usage)|too many requests)/i.exec(text);
  // A reply ChatGPT has not marked complete is still being written (its
  // source and entity chips are only marked up at the end).
  var streaming = turn ? !complete : !!document.querySelector('[data-testid="stop-button"], button[aria-label^="Stop"]');
  return JSON.stringify({
    state: answer ? (streaming ? "streaming" : "answer") : (limit ? "blocked" : "pending"),
    why: limit ? limit[1] : "",
    text: answer,
    complete: complete,
    excerpt: text.slice(0, 400)
  });
})()
"""

# Google: result titles are h3 inside a link, the site is the cite beside it
# (the link itself is a /goto redirect that means nothing to a person). The AI
# overview, when there is one, is the text under its heading.
GOOGLE_JS = BLOCKED_JS + r"""
(function () {
  var text = document.body ? document.body.innerText : "";
  if (location.pathname.indexOf("/sorry") === 0) return JSON.stringify({state: "blocked", why: "unusual traffic", excerpt: text.slice(0, 400)});
  var blocked = decurixBlocked(text);
  if (blocked) return JSON.stringify({state: "blocked", why: blocked, excerpt: text.slice(0, 400)});

  var results = [], seen = {};
  document.querySelectorAll("#search a h3").forEach(function (h) {
    var title = h.innerText.trim();
    if (!title || seen[title]) return;
    seen[title] = 1;
    var box = h.closest("[data-hveid], .g, [jscontroller]") || h.parentElement;
    var cite = box ? box.querySelector("cite") : null;
    var site = cite ? cite.innerText.split(/\s+›/)[0].trim() : "";
    results.push({title: title, site: site});
  });

  var overview = "";
  var at = text.indexOf("AI Overview\n");
  if (at >= 0) {
    overview = text.slice(at + 12, at + 2500);
    ["AI responses may include mistakes", "\nShow more", "\nShow all", "\nDive deeper", "\nPeople also ask"].forEach(function (end) {
      var cut = overview.indexOf(end);
      if (cut >= 0) overview = overview.slice(0, cut);
    });
    var lines = overview.split("\n").map(function (line) { return line.trim(); });
    // Its source cards follow the text: a title, then a snippet cut off with
    // "..." - the overview ends at the first of them.
    for (var i = 1; i < lines.length; i++) {
      if (/(\.\.\.|\u2026)$/.test(lines[i]) && /\s[-|\u2013]\s/.test(lines[i - 1])) { lines = lines.slice(0, i - 1); break; }
    }
    overview = lines.filter(function (line) {
      return line && !/^\+\d+$/.test(line) && !/^(Wikipedia|Britannica|YouTube|Reddit)$/.test(line);
    }).join("\n");
  }

  return JSON.stringify({state: results.length ? "answer" : "pending", text: overview.trim(), results: results.slice(0, 8), excerpt: text.slice(0, 400)});
})()
"""

DUCKDUCKGO_JS = BLOCKED_JS + r"""
(function () {
  var text = document.body ? document.body.innerText : "";
  var blocked = decurixBlocked(text);
  if (blocked) return JSON.stringify({state: "blocked", why: blocked, excerpt: text.slice(0, 400)});
  var results = [];
  document.querySelectorAll('article[data-testid="result"]').forEach(function (a) {
    var link = a.querySelector("h2 a");
    var snippet = a.querySelector('[data-result="snippet"]');
    if (!link || !link.innerText.trim()) return;
    results.push({title: link.innerText.trim(), url: link.href, snippet: snippet ? snippet.innerText.trim() : ""});
  });
  return JSON.stringify({state: results.length ? "answer" : "pending", results: results.slice(0, 8), excerpt: text.slice(0, 400)});
})()
"""

# Wikipedia's search goes straight to an article whose title matches, and to
# a results list otherwise. The lead paragraph is the answer.
WIKIPEDIA_JS = r"""
(function () {
  var text = document.body ? document.body.innerText : "";
  // Paragraphs sit in <section>s now (checked 2026-09), directly under the
  // parser output before that; an infobox or a note box is not the lead.
  var paragraphs = document.querySelectorAll("#mw-content-text p");
  for (var i = 0; i < paragraphs.length; i++) {
    if (paragraphs[i].closest("table, .infobox, .hatnote, .ambox, figure, .navbox")) continue;
    var p = paragraphs[i].innerText.replace(/\[\d+\]|\[[a-z]\]|\[note \d+\]/g, "").trim();
    if (p.length > 60) {
      var heading = document.getElementById("firstHeading");
      return JSON.stringify({state: "answer", title: heading ? heading.innerText.trim() : document.title, text: p, url: location.href});
    }
  }
  var results = [];
  document.querySelectorAll(".mw-search-result-heading a").forEach(function (a) {
    results.push({title: a.innerText.trim(), url: a.href});
  });
  if (results.length) return JSON.stringify({state: "answer", results: results.slice(0, 8)});
  if (document.querySelector(".mw-search-nonefound")) return JSON.stringify({state: "answer", results: []});
  return JSON.stringify({state: "pending", excerpt: text.slice(0, 400)});
})()
"""

YOUTUBE_JS = r"""
(function () {
  var text = document.body ? document.body.innerText : "";
  var results = [];
  document.querySelectorAll("a#video-title").forEach(function (a) {
    var title = (a.innerText || a.title || "").trim();
    if (title) results.push({title: title, url: a.href.replace(/&pp=[^&]*/, "")});
  });
  return JSON.stringify({state: results.length ? "answer" : "pending", results: results.slice(0, 8), excerpt: text.slice(0, 400)});
})()
"""


def _q(question):
    return urllib.parse.quote_plus(question)


# Each service: what it is called (`names`, a regex over the request), where a
# question goes, how long to wait, how to read the answer. `kind` decides the
# reply's shape: an assistant answers in its own words, a search lists what
# it found. `not_query` is what, straight after the name, means another
# product and not a question ("google docs ...").
SERVICES = [
    {
        "key": "chatgpt", "name": "ChatGPT", "kind": "assistant",
        "names": r"chat\s?-?gpt",
        "host": "chatgpt.com",
        "home": "https://chatgpt.com/",
        "url": lambda q: "https://chatgpt.com/?q=" + urllib.parse.quote(q),
        "seconds": ASSISTANT_SECONDS, "js": CHATGPT_JS,
    },
    {
        "key": "google", "name": "Google", "kind": "search",
        "names": r"google",
        "not_query": r"(docs?|drive|sheets?|slides|forms|maps|calendar|mail|gmail|meet|chrome|cloud|photos|play|translate|analytics|account|earth|keep|colab|workspace|ads|news|scholar|flights|lens|home|pay|fit|assistant|gemini|pixel|images?)\b|'s\b",
        "host": "google.com",
        "home": "https://www.google.com/",
        "url": lambda q: "https://www.google.com/search?q=" + _q(q),
        "seconds": SEARCH_SECONDS, "js": GOOGLE_JS,
    },
    {
        "key": "duckduckgo", "name": "DuckDuckGo", "kind": "search",
        "names": r"duck\s?duck\s?go|ddg",
        "host": "duckduckgo.com",
        "home": "https://duckduckgo.com/",
        "url": lambda q: "https://duckduckgo.com/?q=" + _q(q),
        "seconds": SEARCH_SECONDS, "js": DUCKDUCKGO_JS,
    },
    {
        "key": "wikipedia", "name": "Wikipedia", "kind": "reference",
        "names": r"wikipedia|wiki",
        "host": "wikipedia.org",
        "home": "https://en.wikipedia.org/",
        "url": lambda q: "https://en.wikipedia.org/w/index.php?search=" + _q(q),
        "seconds": SEARCH_SECONDS, "js": WIKIPEDIA_JS,
    },
    {
        "key": "youtube", "name": "YouTube", "kind": "video",
        "names": r"you\s?tube",
        "host": "youtube.com",
        "home": "https://www.youtube.com/",
        "url": lambda q: "https://www.youtube.com/results?search_query=" + _q(q),
        "seconds": SEARCH_SECONDS, "js": YOUTUBE_JS,
    },
]

_NAMES = "|".join(f"(?P<s_{s['key']}>{s['names']})" for s in SERVICES)

_POLITE = r"(?:(?:hey|hi|ok|okay)[,!\s]+)?(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+(?:please\s+)?)?"

# "chatgpt ...", "ask chatgpt ...", "use chatgpt to ...", "search google for
# ...", "on youtube, ...". The name has to lead: "what does google say about
# x" is a question for the engine, not a search.
LEADING = re.compile(
    rf"^\s*{_POLITE}"
    r"(?:(?:go\s+)?(?:ask|use|open|search|query|check|try|go\s+to|visit|look\s+(?:it\s+)?up\s+(?:on|in))\s+)?"
    r"(?:(?:on|in|at|with|using)\s+)?"
    rf"(?:{_NAMES})\b"
    r"(?P<rest>.*)$",
    re.IGNORECASE | re.DOTALL,
)

# What joins the name to the question: "chatgpt: x", "ask chatgpt to x",
# "search google for x", "youtube - x".
JOINER = re.compile(
    r"^\s*(?:(?:and|to)\s+(?:ask|search(?:\s+for)?|find(?:\s+out)?|look\s+up|type|tell\s+me|answer|show\s+me|play)\b|"
    r"to\b|for\b|about\b|what\s+it\s+says\s+about\b|[:,\-–—]+)?\s*",
    re.IGNORECASE,
)

# "search for x on google", "ask where x is in chatgpt", "look up x on wiki".
TRAILING = re.compile(
    rf"^\s*{_POLITE}"
    r"(?:ask|search(?:\s+for)?|look\s+up|find|query|google)\s+(?P<question>.+?)\s+"
    r"(?:on|in|with|using|via|at)\s+"
    rf"(?:{_NAMES})\s*[.!]*\s*$",
    re.IGNORECASE | re.DOTALL,
)

# A job for the desktop after the service has answered ("... and save it to
# notes.txt"), which only an engine can do. An assistant's own prompt may say
# "and summarise it" - that is for the assistant - so what counts is doing
# something with the result here.
DESKTOP_JOB = re.compile(
    r"\b(?:and|then)\s+(?:also\s+)?(?:save|send|email|mail|write|copy|paste|put|store|download|message|share|post|print|open|click)\b"
    r"[^.]*\b(?:it|that|this|them|the\s+(?:answer|result|reply|response|output|first|top|video|link))\b"
    r"|(?:^|\s)(?:~/|/[a-z_.])|\b[\w-]+\.(?:txt|md|csv|json|docx?|xlsx?|pdf|html?)\b|;",
    re.IGNORECASE,
)

QUOTES = "\"'“”‘’`"


def _service(match):
    for service in SERVICES:
        if match.group(f"s_{service['key']}"):
            return service
    return None


def _clean_question(text):
    question = (text or "").strip()
    # A question wrapped in quotes is the question; the quotes are not.
    if len(question) >= 2 and question[0] in QUOTES and question[-1] in QUOTES:
        question = question[1:-1].strip()
    return re.sub(r"\s+", " ", question)


def parse(content):
    """
    (service, question) when a request is something to put to a web app the
    desktop's browser can reach, or None. The question is "" for a request
    that only names the service ("open youtube").
    """

    text = (content or "").strip()
    if not text or text.startswith("/") or "\n" in text.strip():
        return None

    match = TRAILING.match(text)
    if match:
        service, question = _service(match), _clean_question(match.group("question"))
    else:
        match = LEADING.match(text)
        if not match:
            return None
        service = _service(match)
        rest = match.group("rest")
        if service.get("not_query") and re.match(r"\s*(?:" + service["not_query"] + ")", rest, re.IGNORECASE):
            return None
        joined = JOINER.match(rest)
        question = _clean_question(rest[joined.end():] if joined else rest)

    if service is None:
        return None

    if question and (DESKTOP_JOB.search(question) or (service["kind"] != "assistant" and SECOND_JOB.search(question))):
        # A job for the desktop after the answer: the engine does the whole thing.
        return None

    return service, question


def handles(content):
    """Whether the command watcher should hand this request to perform()."""

    try:
        return parse(content) is not None
    except Exception as exc:
        log(f"Could not read {content!r} as a question for a web app: {exc}")
        return False


def _begin(content, task_file):
    state.current_task = content
    state.current_task_file = task_file
    state.waiting_for_query = False

    # Known, not unknown: no model reads anything here.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)


def perform(content, task_file=None):
    """Put the request's question to the web app it names, and reply with what it said."""

    _begin(content, task_file)

    try:
        found = parse(content)
        if found is None:
            write_final_reply("That is not something to ask a web app.", kind="error", engine=ENGINE)
            return

        service, question = found
        reply, kind = _ask(service, question)
        write_final_reply(reply, kind=kind, engine=ENGINE)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"That stopped on an internal error: {exc}", kind="error", engine=ENGINE)


def _ask(service, question):
    name = service["name"]

    secret = privacy.credential_in(question)
    if secret:
        # A web app keeps what it is asked. A question that carries a
        # password or key is not sent anywhere.
        article = "an" if secret[:1].lower() in "aeiou" else "a"
        return (f"I did not send that to {name}: it contains what looks like {article} {secret}, "
                f"and {name} would keep it. Ask again without it."), "error"

    if not question:
        progress.emit(f"Opening {name}")
        core.open_target(service["home"], new_tab=True)
        return f"{name} is open in the browser on the desktop.", "final"

    if service["kind"] == "assistant":
        progress.emit(f"Asking {name}: {question}")
    else:
        progress.emit(f"Searching {name} for: {question}")

    try:
        core.open_target(service["url"](question), new_tab=True)
    except ComputerError as exc:
        return f"I could not open {name} in the browser: {exc}", "error"

    progress.emit(f"Waiting for {name} to answer" if service["kind"] == "assistant" else f"Reading {name}'s results")

    result = _read(service)
    return render(service, question, result)


def _page_for(service):
    """A DevTools session on the newest tab showing the service."""

    for target in cdp._page_targets():
        host = urllib.parse.urlparse(target.get("url") or "").hostname or ""
        if host == service["host"] or host.endswith("." + service["host"]):
            return cdp.Page(target)
    return None


def _read(service, sleep=time.sleep, clock=time.monotonic, page_for=None):
    """
    Poll the page until the service has answered and the answer has stopped
    changing, it refuses, or its time is up. Returns the reader's last
    result, with "timed_out" set when the time ran out.
    """

    page_for = page_for or _page_for
    deadline = clock() + service["seconds"]
    last, since, page = None, None, None

    try:
        while True:
            try:
                if page is None:
                    page = page_for(service)
                result = json.loads(page.evaluate(service["js"]) or "{}") if page else {}
            except (ComputerError, OSError, ValueError) as exc:
                # A page that navigates (ChatGPT moves to /uc/<id> once it has
                # the question) drops the session: attach again next time.
                log(f"Reading {service['name']}: {exc}")
                result = {}
                if page is not None:
                    page.close()
                    page = None

            status = result.get("state")
            if status == "blocked":
                return result

            if status in ("answer", "streaming"):
                key = json.dumps([result.get("text"), result.get("results")], sort_keys=True)
                if key != last:
                    last, since = key, clock()
                elif clock() - since >= (STABLE_SECONDS if status == "answer" else STALLED_SECONDS):
                    # Still "streaming" but unchanged this long: the page did
                    # not mark it finished (its markup moved on), and the
                    # words are all there is.
                    result["state"] = "answer"
                    return result
            else:
                last, since = None, None

            if clock() >= deadline:
                if status in ("answer", "streaming") and result.get("text"):
                    result["state"] = "answer"
                    return result
                if status == "answer":
                    return result
                result["timed_out"] = True
                return result

            sleep(POLL_SECONDS)
    finally:
        if page is not None:
            page.close()


# A source pill read as text: its favicon placeholder (the source's first
# letter, alone on a line), the source, and "+N" for more of them - "W /
# Wikipedia / +1", "E / education.nationalgeographic.org". Found live: ChatGPT
# marks a reply complete a moment before it marks its pills up, so a reply
# read in that moment carries them as words. The letter has to be the
# source's own first letter, so an answer that is just "B" is left alone.
# The source is a site ("en.wikipedia.org") or a short name in title case
# ("Wikipedia", "The Guardian") - never a sentence.
_PILL = re.compile(
    r"(?m)^[ \t]*([A-Z])[ \t]*\n[ \t]*"
    r"([\w-]+(?:\.[\w-]+)*\.[a-z]{2,}|[A-Z][\w&'.-]*(?: [A-Z][\w&'.-]*){0,3})"
    r"[ \t]*\n(?:[ \t]*\+\d+[ \t]*(?:\n|$))?"
)
_SOURCES_FOOTER = re.compile(r"(?:\n[ \t]*[A-Z][ \t]*)*\n[ \t]*Sources[ \t]*$")


def strip_source_pills(text):
    """An assistant's reply without the source pills its page shows as words."""

    text = _SOURCES_FOOTER.sub("", (text or "").rstrip())
    text = _PILL.sub(lambda m: "" if m.group(2)[:1].upper() == m.group(1) else m.group(0), text + "\n")
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def _clip(text, limit=MAX_ANSWER_CHARS):
    text = (text or "").strip()
    return text if len(text) <= limit else text[:limit].rstrip() + "…"


def _excerpt(result):
    words = " ".join((result.get("excerpt") or "").split())
    return words[:300]


def render(service, question, result):
    """(reply, kind) for what the page said."""

    name = service["name"]
    where = f"It is open in the browser on the desktop."

    if result.get("state") == "blocked":
        why = result.get("why") or "it would not answer"
        return (f"{name} did not answer: it says \"{why}\". The page is open in the browser on the desktop - "
                f"deal with it there, then send the request again."), "error"

    if result.get("timed_out"):
        shown = _excerpt(result)
        seen = f" The page shows: \"{shown}\"" if shown else ""
        return (f"{name} did not answer within {service['seconds']} seconds.{seen} "
                f"It is still open in the browser on the desktop."), "error"

    if service["kind"] == "assistant":
        return f"{name} says:\n\n{_clip(strip_source_pills(result.get('text')))}\n\n{where}", "final"

    if service["kind"] == "reference" and result.get("text"):
        title = result.get("title") or question
        return f"From Wikipedia, \"{title}\":\n\n{_clip(result['text'], 1500)}\n\n{result.get('url', '')}".rstrip(), "final"

    results = (result.get("results") or [])[:MAX_RESULTS]
    if not results and not result.get("text"):
        return f"{name} found nothing for \"{question}\". {where}", "final"

    lines = [f"{name}, for \"{question}\":"]

    if result.get("text"):
        lines += ["", "AI overview:", _clip(result["text"], 1500)]

    if results:
        lines += ["", "Top results:" if service["kind"] != "video" else "Top videos:"]
        for n, item in enumerate(results, 1):
            where_from = item.get("site") or item.get("url") or ""
            line = f"{n}. {item['title']}"
            if where_from:
                line += f" - {where_from}"
            lines.append(line)
            if item.get("snippet") and n <= 2:
                lines.append("   " + _clip(item["snippet"], 240))

    lines += ["", where]
    return "\n".join(lines), "final"
