"""Owns the one long-lived Chromium instance for the whole session.

Launched once as its own subprocess by server.py. Playwright's Python bindings
don't expose launch_server() (Node-only), so instead this launches a normal
Chromium with a CDP debug port open, prints that endpoint as its first stdout
line, then idles -- worker.py attaches via chromium.connect_over_cdp() from a
fresh short-lived process per step, so browser/page state (cookies, DOM,
navigation) survives across steps while each step's generated code still runs
in its own disposable, hard-killable process.

Killing this process (server.py does so on shutdown, and on every /reset to
give a genuinely fresh browser rather than just blanking the current page)
tears down the browser. Each launch gets its own temporary, unshared profile
(no user_data_dir given), so a new process is already cookie/storage-free --
--port just avoids racing the previous instance for the same debug port
during a reset, since the old process may not have released it yet.
"""
import argparse
import time

from playwright.sync_api import sync_playwright

DEFAULT_CDP_PORT = 9333


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=DEFAULT_CDP_PORT)
    args = ap.parse_args()

    with sync_playwright() as p:
        browser = p.chromium.launch(
            headless=False, args=[f"--remote-debugging-port={args.port}", "--start-maximized"],
        )
        # Create the one shared context/page up front so worker.py's connects
        # always find something to reuse instead of racing to create it.
        browser.new_context(no_viewport=True).new_page()
        print(f"http://127.0.0.1:{args.port}", flush=True)

        try:
            while True:
                time.sleep(3600)
        except KeyboardInterrupt:
            pass
        browser.close()


if __name__ == "__main__":
    main()
