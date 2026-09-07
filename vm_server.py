from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
import threading
import time
import urllib.request
import socket


DESKTOP_ID = os.environ.get("DESKTOP_ID", "")

DESKTOP_NAME = os.environ.get(
    "DESKTOP_NAME",
    socket.gethostname()
)

COMMAND_DIR = "/home/headless/command"
HELPTEXT_DIR = "/home/headless/helptext"

CONTROL_PLANE_URL = os.environ.get(
    "CONTROL_PLANE_URL",
    "http://host.docker.internal:8080"
)

os.makedirs(COMMAND_DIR, exist_ok=True)
os.makedirs(HELPTEXT_DIR, exist_ok=True)


# ------------------------------------------------------------
# Save messages from Flowera
# ------------------------------------------------------------

def save_message(message):
    timestamp = time.strftime("%d%m%Y%H%M%S")

    filename = f"{timestamp}.txt"
    path = os.path.join(COMMAND_DIR, filename)

    # Avoid collision if two messages arrive in the same second.
    counter = 1

    while os.path.exists(path):
        filename = f"{timestamp}_{counter}.txt"
        path = os.path.join(COMMAND_DIR, filename)
        counter += 1

    with open(path, "w", encoding="utf-8") as f:
        f.write(message)

    print(
        f"Message saved: {path}",
        flush=True
    )


# ------------------------------------------------------------
# Send helptext files to Flowera
# ------------------------------------------------------------

def send_helptext(filename, content):

    payload = json.dumps({
        "desktop_id": DESKTOP_ID,
        "vm_name": DESKTOP_NAME,
        "filename": filename,
        "content": content
    }).encode("utf-8")

    url = (
        CONTROL_PLANE_URL.rstrip("/")
        + "/api/helptext"
    )

    request = urllib.request.Request(
        url,
        data=payload,
        headers={
            "Content-Type": "application/json"
        },
        method="POST"
    )

    try:

        with urllib.request.urlopen(
            request,
            timeout=10
        ) as response:

            response_body = response.read().decode(
                "utf-8",
                errors="replace"
            )

            print(
                f"Helptext sent to control plane: "
                f"{filename}",
                flush=True
            )

            print(
                f"Response: {response_body}",
                flush=True
            )

    except Exception as e:

        print(
            f"Failed to send helptext "
            f"{filename}: {e}",
            flush=True
        )


# ------------------------------------------------------------
# Watch /home/headless/helptext
# ------------------------------------------------------------

def helptext_watcher():

    print(
        f"Watching {HELPTEXT_DIR}",
        flush=True
    )

    print(
        f"Control plane: {CONTROL_PLANE_URL}",
        flush=True
    )

    print(
        f"Desktop ID: {DESKTOP_ID}",
        flush=True
    )

    print(
        f"Desktop name: {DESKTOP_NAME}",
        flush=True
    )

    # filename -> (mtime_ns, size)
    known_files = {}

    while True:

        try:

            current_files = {}

            for filename in os.listdir(HELPTEXT_DIR):

                # Only watch .txt files.
                if not filename.endswith(".txt"):
                    continue

                path = os.path.join(
                    HELPTEXT_DIR,
                    filename
                )

                if not os.path.isfile(path):
                    continue

                try:

                    stat = os.stat(path)

                    signature = (
                        stat.st_mtime_ns,
                        stat.st_size
                    )

                except FileNotFoundError:
                    # File may have disappeared between
                    # os.listdir() and os.stat().
                    continue

                current_files[filename] = signature

                previous_signature = known_files.get(
                    filename
                )

                # ------------------------------------------------
                # New file OR modified file
                # ------------------------------------------------
                if previous_signature != signature:

                    print(
                        f"Detected new/modified helptext: "
                        f"{filename}",
                        flush=True
                    )

                    try:

                        with open(
                            path,
                            "r",
                            encoding="utf-8"
                        ) as f:

                            content = f.read()

                        print(
                            f"Helptext content for "
                            f"{filename}:",
                            flush=True
                        )

                        print(
                            content,
                            flush=True
                        )

                        send_helptext(
                            filename,
                            content
                        )

                    except FileNotFoundError:
                        # Editor may temporarily replace the file
                        # while saving.
                        print(
                            f"File disappeared while reading: "
                            f"{path}",
                            flush=True
                        )

                    except Exception as e:

                        print(
                            f"Could not read {path}: {e}",
                            flush=True
                        )

            known_files = current_files

        except Exception as e:

            print(
                f"Helptext watcher error: {e}",
                flush=True
            )

        time.sleep(1)


# ------------------------------------------------------------
# HTTP server
# ------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):

    def do_POST(self):

        if self.path != "/message":

            self.send_response(404)
            self.end_headers()

            return

        try:

            length = int(
                self.headers.get(
                    "Content-Length",
                    "0"
                )
            )

            body = self.rfile.read(length)

            data = json.loads(body)

            message = data.get(
                "content",
                ""
            )

            if not isinstance(message, str):
                message = str(message)

            if not message.strip():

                raise ValueError(
                    "empty message"
                )

            save_message(message)

            print(
                "\n========== MESSAGE RECEIVED ==========",
                flush=True
            )

            print(
                message,
                flush=True
            )

            print(
                "======================================\n",
                flush=True
            )

            response = json.dumps({
                "status": "received"
            }).encode("utf-8")

            self.send_response(200)

            self.send_header(
                "Content-Type",
                "application/json"
            )

            self.send_header(
                "Content-Length",
                str(len(response))
            )

            self.send_header(
                "Connection",
                "close"
            )

            self.end_headers()

            self.wfile.write(response)
            self.wfile.flush()

        except Exception as e:

            print(
                f"Message error: {e}",
                flush=True
            )

            response = json.dumps({
                "error": str(e)
            }).encode("utf-8")

            self.send_response(500)

            self.send_header(
                "Content-Type",
                "application/json"
            )

            self.send_header(
                "Content-Length",
                str(len(response))
            )

            self.send_header(
                "Connection",
                "close"
            )

            self.end_headers()

            self.wfile.write(response)
            self.wfile.flush()

    def log_message(self, format, *args):

        print(
            f"[VM SERVER] {format % args}",
            flush=True
        )


# ------------------------------------------------------------
# Start watcher
# ------------------------------------------------------------

threading.Thread(
    target=helptext_watcher,
    daemon=True
).start()


# ------------------------------------------------------------
# Start HTTP server
# ------------------------------------------------------------

server = HTTPServer(
    ("0.0.0.0", 8081),
    Handler
)

print(
    "VM communication server listening on :8081",
    flush=True
)

server.serve_forever()