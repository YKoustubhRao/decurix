"""
A scripted viewer of the dashboard (web/templates/index.html): it asks the control plane for what one browser tab with
the page open asks for, on the page's own timers, and counts requests and bytes.

How it mirrors the page (the schedule below is checked against the templates by bench/test_benches.py, so a timer
changed there fails that test until it is changed here too):

  - It loads "/" once, as the tab does, and reads the cards from the served HTML: each card's desktop id, its status
    (data-base-status), and which of the polled parts it has (data-shot, data-activity, data-mem-list, data-task-pick).
    Only what the page holds is polled.
  - Thumbnails (shotLoop): one sweep fetches /api/desktops/screenshot?id=..&w=..&t=.. for each card in turn, one at a
    time, waiting for each; a paused card is skipped once its picture is live. After a sweep it waits
    SHOT_INTERVAL_MS. w is the card's width in device pixels, clamped to 120..960 (--shot-width; 320 is a card at 1x).
  - Each Decurix.poll(fn, ms[, {hidden}]) is a timer of its own: fn runs every ms while the tab is visible (every
    hidden ms, or never, while it is hidden), and a run that is still waiting skips the next tick (the page's
    "...Polling" flags). Polls the page also calls once at load are called at load here too.
  - Requests are sent with the browser's Accept-Encoding (gzip), and bytes are counted as they come over the wire:
    the status line, the headers and the body as sent (compressed JSON stays compressed).
  - Not mirrored, because they ask nothing while the page sits open: paintCreated and paintActivity (local repaints),
    refreshTunnel (asks only while its menu is open), refreshQueryBadge (off on this page: data-query-poll="own"),
    and checkProvisioningDesktops (asks only for a card still being created; none in a steady state).

Standard library only.
"""

import gzip
import json
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

# The page's thumbnail pause between sweeps (index.html SHOT_INTERVAL_MS).
SHOT_INTERVAL_MS = 5000

# Every timer the dashboard keeps that asks the control plane something: (the page's function, ms while visible, ms
# while hidden (0: never), called at load, what it asks, which card part it needs ("" for none)). The function names
# and intervals are the templates' own (index.html, theme.html), checked by bench/test_benches.py.
POLLS = (
    ("pollMemory", 4000, 0, False, "/api/desktops/memory", "mem"),
    ("pollTaskPicks", 4000, 0, True, "/api/desktops/tasks?ids={ids}", "task"),
    ("pollActivity", 2500, 0, True, "/api/desktops/activity?ids={ids}", "activity"),
    ("loadNotes", 2500, 30000, True, "/api/querynotes", ""),
    ("pollResourceAlerts", 4000, 0, True, "/api/desktops/resource-alerts", ""),
    ("refreshModel", 60000, 0, True, "/api/model", ""),
    ("refreshDocker", 10000, 60000, True, "/api/docker/status", ""),
    ("refreshFinished", 5000, 30000, True, "/api/tasks/finished{after}", ""),
)

# Timers the page keeps that ask nothing while it sits open (see the module's docstring): named so the check against
# the templates accounts for every one.
SILENT = ("refreshTunnel", "refreshQueryBadge", "checkProvisioningDesktops")

_CARD = re.compile(r'<article class="dcard" data-desktop-id="([^"]+)">(.*?)</article>', re.S)


def cards(html):
    """[{id, status, parts}] for each desktop card in the served page; parts is the set of polled parts it holds."""

    out = []
    for desktop_id, body in _CARD.findall(html):
        status = re.search(r'data-base-status="([^"]*)"', body)
        parts = set()
        for attr, part in (("data-shot", "shot"), ("data-activity", "activity"), ("data-mem-list", "mem"),
                           ("data-task-pick", "task")):
            if re.search(attr + r'="', body):
                parts.add(part)
        out.append({"id": desktop_id, "status": status.group(1) if status else "", "parts": parts})
    return out


class Counter:
    """Requests and wire bytes by endpoint, safe across the viewer's timers."""

    def __init__(self):
        self.lock = threading.Lock()
        self.by = {}
        self.errors = 0

    def add(self, endpoint, nbytes, ok):
        with self.lock:
            n, b = self.by.get(endpoint, (0, 0))
            self.by[endpoint] = (n + 1, b + nbytes)
            if not ok:
                self.errors += 1

    def totals(self):
        with self.lock:
            return sum(n for n, _ in self.by.values()), sum(b for _, b in self.by.values()), dict(self.by), self.errors


def fetch(base, path, counter, timeout=30):
    """GET path as the tab would; counts it under its path without the query. Returns the body (decompressed) or None."""

    endpoint = path.split("?", 1)[0]
    request = urllib.request.Request(base + path, headers={"Accept-Encoding": "gzip", "Cache-Control": "no-store"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as r:
            raw = r.read()
            head = f"HTTP/1.1 {r.status} {r.reason}\r\n" + str(r.headers)
            counter.add(endpoint, len(head.encode()) + len(raw), True)
            if r.headers.get("Content-Encoding") == "gzip":
                raw = gzip.decompress(raw)
            return raw
    except urllib.error.HTTPError as e:
        body = e.read()
        counter.add(endpoint, len(str(e.headers).encode()) + len(body), False)
    except (urllib.error.URLError, OSError):
        counter.add(endpoint, 0, False)
    return None


def watch(base, seconds, shot_width=320, hidden=False, simulate=0):
    """Hold the dashboard open for seconds; returns the counts. hidden mirrors a tab in the background. simulate N
    sweeps N thumbnails a pass (the real cards, cycled) to stand in for N desktops when fewer exist."""

    counter = Counter()
    page = fetch(base, "/", counter)
    page_requests, page_bytes, _, _ = counter.totals()
    counter = Counter()
    held = cards((page or b"").decode("utf-8", "replace"))
    stop = time.monotonic() + seconds
    finished = {"after": None}

    def ids(part):
        return ",".join(c["id"] for c in held if part in c["parts"])

    def poll_loop(name, ms, hidden_ms, at_load, path, part):
        if part and not ids(part):
            return
        every = (hidden_ms if hidden else ms) / 1000.0
        if every <= 0:
            return
        next_at = time.monotonic() + (0 if at_load and not hidden else every)
        while next_at < stop:
            time.sleep(max(0.0, next_at - time.monotonic()))
            after = "" if finished["after"] is None else f"?after={finished['after']}"
            body = fetch(base, path.format(ids=urllib.parse.quote(ids(part) if part else ""), after=after), counter)
            if name == "refreshFinished" and body:
                try:
                    finished["after"] = json.loads(body).get("latest") or 0
                except ValueError:
                    pass
            # setInterval keeps its beat; a run still waiting at the next tick skips it (the page's busy flags).
            next_at += every
            while next_at < time.monotonic():
                next_at += every

    def shot_loop():
        if hidden:
            return
        shots = [c for c in held if "shot" in c["parts"]]
        if not shots:
            return
        sweep = shots if not simulate else [shots[i % len(shots)] for i in range(simulate)]
        live = set()
        while time.monotonic() < stop:
            for i, card in enumerate(sweep):
                if time.monotonic() >= stop:
                    return
                if card["status"] == "paused" and i in live:
                    continue
                width = min(960, max(120, shot_width))
                path = (f"/api/desktops/screenshot?id={urllib.parse.quote(card['id'])}&w={width}"
                        f"&t={int(time.time() * 1000)}")
                if fetch(base, path, counter) is not None:
                    live.add(i)
            time.sleep(min(SHOT_INTERVAL_MS / 1000.0, max(0.0, stop - time.monotonic())))

    threads = [threading.Thread(target=shot_loop, daemon=True)]
    threads += [threading.Thread(target=poll_loop, args=p, daemon=True) for p in POLLS]
    started = time.monotonic()
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=seconds + 60)
    elapsed = time.monotonic() - started
    n, b, by, errors = counter.totals()
    return {
        "seconds": round(elapsed, 1),
        "hidden": hidden,
        "desktops": len(held),
        "thumbnails_per_sweep": (simulate or len([c for c in held if "shot" in c["parts"]])) if not hidden else 0,
        "page_load": {"requests": page_requests, "bytes": page_bytes},
        "requests": n,
        "bytes": b,
        "requests_per_s": round(n / elapsed, 2) if elapsed else None,
        "bytes_per_s": round(b / elapsed) if elapsed else None,
        "errors": errors,
        "by_endpoint": {k: {"requests": v[0], "bytes": v[1]} for k, v in sorted(by.items())},
    }
