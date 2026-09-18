"""AI-controlled browser agent, Phase 2 of the local-LLM research.

Devstral (via the llama-server from ../kv-ram/start_server.ps1) doesn't have a
tool-calling jinja template (its GGUF only carries a bare user/system/assistant
template -- see ../kv-ram/start_server.ps1 comments), and llama-server's `tools`
API param hard-requires --jinja with a template that implements tool_calls,
which this model's does not. So instead of OpenAI-style function calling, the
agent uses a plain-text ReAct loop: one action per turn in a fixed line syntax,
parsed with a regex. This also sidesteps relying on a local model's JSON
formatting reliability for structured tool calls.
"""

import argparse
import json
import re
import time
from pathlib import Path

import requests
from playwright.sync_api import sync_playwright

SNAPSHOT_JS = (Path(__file__).parent / "dom_snapshot.js").read_text(encoding="utf-8")

SYSTEM_PROMPT = """You are a browser automation agent. Each turn you are given the task, the \
current page URL/title, the visible text on screen, and a numbered list of interactive \
elements. Read the visible text to find facts needed to answer the task -- don't guess, and \
don't assume the answer is only reachable by clicking something. On every turn, respond with \
EXACTLY ONE action, on its own line, in one of these forms:

CLICK(index)
TYPE(index, "text")
NAVIGATE("url")
SCROLL("down") or SCROLL("up")
FINISH("final answer or short result summary")

Rules:
- Only one action per turn. No explanation, no extra text, just the action line.
- Use FINISH as soon as the task is accomplished, or if it's genuinely impossible.
- TYPE replaces the field's existing content; it does not submit the form. \
Follow it with CLICK on a submit/search button, or navigate, if needed.
- Element indices are only valid for the page state shown in the same turn; a new \
page state means new indices.
- If an element you need isn't listed, try SCROLL first."""

_STR = r'"((?:[^"\\]|\\.)*)"'
CLICK_RE = re.compile(r'CLICK\(\s*(\d+)\s*\)')
TYPE_RE = re.compile(r'TYPE\(\s*(\d+)\s*,\s*' + _STR + r'\s*\)')
NAVIGATE_RE = re.compile(r'NAVIGATE\(\s*' + _STR + r'\s*\)')
SCROLL_RE = re.compile(r'SCROLL\(\s*' + _STR + r'\s*\)')
FINISH_RE = re.compile(r'FINISH\(\s*' + _STR + r'\s*\)', re.DOTALL)


def _unescape(s):
    return s.replace('\\"', '"')


def parse_action(text):
    if m := TYPE_RE.search(text):
        return ("TYPE", int(m.group(1)), _unescape(m.group(2)))
    if m := CLICK_RE.search(text):
        return ("CLICK", int(m.group(1)))
    if m := NAVIGATE_RE.search(text):
        return ("NAVIGATE", _unescape(m.group(1)))
    if m := SCROLL_RE.search(text):
        return ("SCROLL", _unescape(m.group(1)))
    if m := FINISH_RE.search(text):
        return ("FINISH", _unescape(m.group(1)))
    return None


class LlamaClient:
    def __init__(self, base_url):
        self.base_url = base_url.rstrip("/")

    def complete(self, messages, max_tokens=80):
        resp = requests.post(
            f"{self.base_url}/v1/chat/completions",
            json={"messages": messages, "temperature": 0, "max_tokens": max_tokens},
            timeout=300,
        )
        resp.raise_for_status()
        data = resp.json()
        return data["choices"][0]["message"]["content"].strip()


class BrowserAgent:
    def __init__(self, llm, page, log_path, max_steps=15, screenshot_dir=None):
        self.llm = llm
        self.page = page
        self.max_steps = max_steps
        self.log_path = log_path
        self.screenshot_dir = screenshot_dir
        self.messages = [{"role": "system", "content": SYSTEM_PROMPT}]

    def snapshot(self):
        state = self.page.evaluate(SNAPSHOT_JS)
        body = "\n".join(state["elements"]) if state["elements"] else "(no interactive elements found)"
        if state["skippedOffscreen"]:
            body += f"\n... {state['skippedOffscreen']} more elements off-screen or truncated; SCROLL to see them."
        text = state["visibleText"] or "(no visible text)"
        return state["url"], state["title"], body, text

    def log(self, record):
        with open(self.log_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record) + "\n")

    def act(self, action):
        kind = action[0]
        if kind == "CLICK":
            idx = action[1]
            handle = self.page.evaluate_handle("i => window.__agentElements[i]", idx)
            el = handle.as_element()
            if el is None:
                return f"Error: no element at index {idx}."
            el.scroll_into_view_if_needed()
            el.click(timeout=5000)
            self.page.wait_for_timeout(500)
            return f"Clicked [{idx}]."
        if kind == "TYPE":
            idx, text = action[1], action[2]
            handle = self.page.evaluate_handle("i => window.__agentElements[i]", idx)
            el = handle.as_element()
            if el is None:
                return f"Error: no element at index {idx}."
            el.scroll_into_view_if_needed()
            el.fill(text, timeout=5000)
            return f'Typed "{text}" into [{idx}].'
        if kind == "NAVIGATE":
            url = action[1]
            if not re.match(r"^https?://", url):
                url = "https://" + url
            self.page.goto(url, wait_until="domcontentloaded", timeout=20000)
            return f"Navigated to {url}."
        if kind == "SCROLL":
            direction = action[1]
            delta = 800 if direction == "down" else -800
            self.page.mouse.wheel(0, delta)
            self.page.wait_for_timeout(300)
            return f"Scrolled {direction}."
        raise ValueError(f"unhandled action kind {kind}")

    def run(self, task, start_url=None):
        if start_url:
            self.page.goto(start_url, wait_until="domcontentloaded", timeout=20000)

        for step in range(1, self.max_steps + 1):
            url, title, elements, text = self.snapshot()
            user_msg = (
                f"Task: {task}\n\nURL: {url}\nTitle: {title}\n\n"
                f"Visible text:\n{text}\n\n"
                f"Interactive elements:\n{elements}"
            )
            self.messages.append({"role": "user", "content": user_msg})

            raw = self.llm.complete(self.messages)
            self.messages.append({"role": "assistant", "content": raw})

            action = parse_action(raw)
            record = {"step": step, "url": url, "title": title, "raw_response": raw}

            if self.screenshot_dir:
                shot = self.screenshot_dir / f"step{step:02d}.png"
                self.page.screenshot(path=str(shot))
                record["screenshot"] = str(shot)

            if action is None:
                observation = f'Error: could not parse an action from your response: "{raw}"'
                record["parsed_action"] = None
                record["observation"] = observation
                self.log(record)
                self.messages.append({"role": "user", "content": observation})
                continue

            record["parsed_action"] = action
            print(f"[step {step}] {action}")

            if action[0] == "FINISH":
                record["observation"] = None
                self.log(record)
                return action[1]

            try:
                observation = self.act(action)
            except Exception as exc:  # noqa: BLE001 - feed any failure back to the model
                observation = f"Error executing {action[0]}: {exc}"

            record["observation"] = observation
            self.log(record)
            print(f"          -> {observation}")
            self.messages.append({"role": "user", "content": observation})

        return None


def main():
    parser = argparse.ArgumentParser(description="AI-controlled browser agent (Devstral + Playwright)")
    parser.add_argument("task", help="Natural-language task for the agent")
    parser.add_argument("--url", default=None, help="Starting URL")
    parser.add_argument("--server", default="http://127.0.0.1:8081", help="llama-server base URL")
    parser.add_argument("--max-steps", type=int, default=15)
    parser.add_argument("--headless", action="store_true")
    parser.add_argument("--tag", default=None, help="Log/screenshot folder name (default: timestamp)")
    args = parser.parse_args()

    tag = args.tag or time.strftime("%Y%m%d-%H%M%S")
    results_dir = Path(__file__).parent / "results" / tag
    results_dir.mkdir(parents=True, exist_ok=True)
    log_path = results_dir / "trace.jsonl"

    llm = LlamaClient(args.server)

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=args.headless)
        page = browser.new_page(viewport={"width": 1280, "height": 900})
        agent = BrowserAgent(llm, page, log_path, max_steps=args.max_steps, screenshot_dir=results_dir)
        try:
            result = agent.run(args.task, start_url=args.url)
        finally:
            browser.close()

    print("\n=== RESULT ===")
    print(result if result is not None else "(did not finish within max steps)")
    print(f"Trace: {log_path}")


if __name__ == "__main__":
    main()
