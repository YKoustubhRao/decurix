#!/usr/bin/env python3
"""Smoke-test a built control plane against Docker with a temporary database.

Usage: python3 scripts/testing/control_plane_smoke.py /path/to/decurix
Needs Docker and the desktop images. Does not create or alter desktops.
"""

import gzip
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    binary = str(Path(sys.argv[1]).resolve())
    admin, lan = free_port(), free_port()
    while lan == admin:
        lan = free_port()
    base = f"http://127.0.0.1:{admin}"
    with tempfile.TemporaryDirectory(prefix="decurix-smoke-") as directory:
        env = dict(os.environ, DECURIX_DATA_DIR=directory,
                   DECURIX_ADMIN_ADDR=f"127.0.0.1:{admin}", DECURIX_LAN_PORT=str(lan),
                   DECURIX_IDLE_STOP_MINUTES="0", OLLAMA_URL="http://127.0.0.1:9",
                   OLLAMA_MODEL="smoke-test")
        proc = None

        def request(path, form=None, encoding="gzip", headers=None):
            data = urllib.parse.urlencode(form).encode() if form is not None else None
            hdr = {"Accept-Encoding": encoding, "Connection": "close", **(headers or {})}
            req = urllib.request.Request(base + path, data=data, headers=hdr)
            with urllib.request.urlopen(req, timeout=10) as response:
                body = response.read()
                if response.headers.get("Content-Encoding") == "gzip":
                    body = gzip.decompress(body)
                return response.headers, body

        def start(log):
            process = subprocess.Popen([binary], cwd=directory, env=env, stdout=log, stderr=log)
            try:
                for _ in range(600):
                    if process.poll() is not None:
                        raise RuntimeError("control plane exited during startup")
                    try:
                        request("/api/dags")
                        return process
                    except (OSError, urllib.error.URLError):
                        time.sleep(0.1)
                raise RuntimeError("control plane did not become ready")
            except BaseException:
                stop(process)
                raise

        def stop(process):
            if process is not None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()

        with open(Path(directory) / "server.log", "w+") as log:
            try:
                proc = start(log)
                for path in ("/", "/requests", "/tasks", "/dags", "/dashboard", "/favicon.svg",
                             "/api/desktops", "/api/tasks", "/api/schedules", "/api/querynotes"):
                    request(path)
                headers, body = request("/")
                assert headers.get("Content-Encoding") == "gzip" and b"<html" in body.lower()
                headers, body = request("/", encoding="gzip;q=0")
                assert not headers.get("Content-Encoding") and b"<html" in body.lower()
                try:
                    request("/api/desktops", headers={"X-Forwarded-For": "192.0.2.1"})
                    raise AssertionError("primary listener accepted a forwarded request")
                except urllib.error.HTTPError as exc:
                    assert exc.code == 403
                    exc.close()
                with urllib.request.urlopen(f"http://127.0.0.1:{lan}/", timeout=10) as response:
                    assert b"<form" in response.read().lower(), "LAN listener did not show login"
                _, body = request("/api/dags/create", {"name": "Smoke persistence"})
                dag = json.loads(body)
                stop(proc)
                admin, lan = free_port(), free_port()
                while lan == admin:
                    lan = free_port()
                base = f"http://127.0.0.1:{admin}"
                env["DECURIX_ADMIN_ADDR"] = f"127.0.0.1:{admin}"
                env["DECURIX_LAN_PORT"] = str(lan)
                proc = start(log)
                _, body = request("/api/dags")
                assert any(item["dag"]["id"] == dag["id"] for item in json.loads(body))
                request("/api/dags/delete", {"id": dag["id"]})
                _, body = request("/api/dags")
                assert json.loads(body) == []
                print("PASS: pages, APIs, gzip negotiation, listener gating, DAG persistence and deletion")
            except BaseException:
                log.flush()
                log.seek(0)
                print(log.read(), file=sys.stderr)
                raise
            finally:
                stop(proc)


if __name__ == "__main__":
    main()
