"""Live-ish view of the shared browser for the UI.

Originally used CDP's Page.startScreencast (push frames on repaint), but that
depends on Chrome's compositor occlusion/visibility signals, which didn't fire
reliably in this environment -- zero frames arrived even after real navigation,
while a plain one-shot page.screenshot() (already used for vision mode) worked
fine regardless of window visibility. So this just polls screenshot() on an
interval instead; lower frame rate, but robust.

Runs in a background thread since Playwright's sync API must stay on the
thread that created it. server.py exposes the latest frame as a
multipart/x-mixed-replace HTTP stream, which a plain <img> tag renders
natively with no client-side JS.
"""
import threading
import time

from playwright.sync_api import sync_playwright


class Screencaster:
    def __init__(self, cdp_endpoint, interval=0.5):
        self.cdp_endpoint = cdp_endpoint
        self.interval = interval
        self._cond = threading.Condition()
        self._latest = None
        self._error = None
        self._stopped = False
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self):
        try:
            with sync_playwright() as p:
                browser = p.chromium.connect_over_cdp(self.cdp_endpoint)
                ctx = browser.contexts[0]
                page = ctx.pages[0]
                while not self._stopped:
                    try:
                        data = page.screenshot(type="jpeg", quality=60, timeout=3000)
                        with self._cond:
                            self._latest = data
                            self._cond.notify_all()
                    except Exception:
                        pass  # page mid-navigation or briefly busy; just try again next tick
                    time.sleep(self.interval)
        except Exception as exc:  # noqa: BLE001 - surfaced via get_latest() for the UI
            with self._cond:
                self._error = str(exc)
                self._cond.notify_all()

    def stop(self):
        """Ends the polling loop (its browser is about to be torn down, e.g. on
        /reset) and wakes any blocked get_latest() so a stale stream doesn't
        just hang forever waiting on frames that will never come."""
        self._stopped = True
        with self._cond:
            self._cond.notify_all()

    def get_latest(self, timeout=2.0):
        """Blocks until a new frame is available (or timeout), returns JPEG bytes or None."""
        with self._cond:
            self._cond.wait(timeout=timeout)
            return self._latest
