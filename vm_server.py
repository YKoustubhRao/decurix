from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
import threading
import time
import urllib.request
import urllib.error
import socket
import subprocess
import shlex
import uuid
from pathlib import Path


# ============================================================
# Configuration
# ============================================================

DESKTOP_ID = os.environ.get("DESKTOP_ID", "")

DESKTOP_NAME = os.environ.get(
    "DESKTOP_NAME",
    socket.gethostname()
)

CONTROL_PLANE_URL = os.environ.get(
    "CONTROL_PLANE_URL",
    "http://host.docker.internal:8080"
)

OLLAMA_URL = os.environ.get(
    "OLLAMA_URL",
    "http://flowera-ollama:11434"
)

OLLAMA_MODEL = os.environ.get(
    "OLLAMA_MODEL",
    "qwen3:0.6b"
)

COMMAND_DIR = Path("/home/headless/command")
HELPTEXT_DIR = Path("/home/headless/helptext")
FINAL_DIR = Path("/home/headless/final")
WORK_DIR = Path("/home/headless/work")

COMMAND_DIR.mkdir(parents=True, exist_ok=True)
HELPTEXT_DIR.mkdir(parents=True, exist_ok=True)
FINAL_DIR.mkdir(parents=True, exist_ok=True)
WORK_DIR.mkdir(parents=True, exist_ok=True)


# ============================================================
# Agent state
# ============================================================

state_lock = threading.Lock()

current_task = None
current_task_file = None
waiting_for_help = False
last_help_id = None

processed_commands = set()


# ============================================================
# Utility
# ============================================================

def timestamp():
    return time.strftime("%Y-%m-%d %H:%M:%S")


def log(message):
    print(
        f"[{timestamp()}] {message}",
        flush=True
    )


def safe_filename(name):
    return "".join(
        c if c.isalnum() or c in "-_." else "_"
        for c in name
    )


# ============================================================
# Existing message functionality
# ============================================================

def save_message(message):
    """
    Save an incoming message/task into command/.
    """

    filename = (
        time.strftime("%Y%m%d_%H%M%S")
        + "_"
        + uuid.uuid4().hex[:8]
        + ".txt"
    )

    path = COMMAND_DIR / filename

    path.write_text(
        message,
        encoding="utf-8"
    )

    log(f"Received message: {message}")
    log(f"Saved to {path}")

    return path


# ============================================================
# Control plane communication
# ============================================================

def send_helptext(filename, content):
    """
    Send a help request to the Flowera control plane.

    The existing Go control plane receives:
        POST /api/helptext

    with:
        desktop_id
        vm_name
        filename
        content
    """

    payload = {
        "desktop_id": DESKTOP_ID,
        "vm_name": DESKTOP_NAME,
        "filename": filename,
        "content": content,
    }

    body = json.dumps(payload).encode("utf-8")

    url = CONTROL_PLANE_URL.rstrip("/") + "/api/helptext"

    request = urllib.request.Request(
        url,
        data=body,
        headers={
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(
            request,
            timeout=10,
        ) as response:

            if response.status < 200 or response.status >= 300:
                raise RuntimeError(
                    f"control plane returned HTTP {response.status}"
                )

            return True

    except Exception as exc:
        log(f"Failed to send help request: {exc}")
        return False


def ask_for_help(problem):
    """
    Ask the human/control plane for help.
    """

    global waiting_for_help
    global last_help_id

    help_id = uuid.uuid4().hex

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    content = f"""
FLOWERA HELP REQUEST

Desktop:
{DESKTOP_NAME}

Desktop ID:
{DESKTOP_ID}

Task:
{current_task or "unknown"}

Problem:
{problem}

Help ID:
{help_id}

The agent is currently blocked.

Please send instructions through the Flowera control plane.
""".strip()

    path = HELPTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    last_help_id = help_id

    log("========================================")
    log("AGENT NEEDS HELP")
    log(content)
    log(f"Help request written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        content,
    )

    if sent:
        log("Help request sent to control plane.")
    else:
        log("WARNING: could not contact control plane.")

    waiting_for_help = True

    return help_id

# ============================================================
# Final task message
# ============================================================

def write_final_message(message):
    """
    Write the final result into helptext/ so the existing
    helptext watcher sends it to the Flowera control plane.
    """

    global current_task
    global current_task_file
    global waiting_for_help

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    final_message = f"""
FLOWERA TASK COMPLETED

Desktop:
{DESKTOP_NAME}

Desktop ID:
{DESKTOP_ID}

Task:
{current_task or "unknown"}

Status:
COMPLETED

Final message:
{message}

Completed at:
{timestamp()}
""".strip()

    path = HELPTEXT_DIR / filename

    path.write_text(
        final_message,
        encoding="utf-8"
    )

    log("========================================")
    log("TASK COMPLETED")
    log(final_message)
    log(f"Final message written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        final_message,
    )

    if sent:
        log("Final message sent to control plane.")
    else:
        log("WARNING: could not send final message to control plane.")

    current_task = None
    current_task_file = None
    waiting_for_help = False


# ============================================================
# Ollama
# ============================================================

def ollama_request(messages):
    """
    Call the local Ollama server.
    """

    payload = {
        "model": OLLAMA_MODEL,
        "messages": messages,
        "stream": False,
    }

    body = json.dumps(payload).encode("utf-8")

    url = OLLAMA_URL.rstrip("/") + "/api/chat"

    request = urllib.request.Request(
        url,
        data=body,
        headers={
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(
            request,
            timeout=300,
        ) as response:

            raw = response.read()

            result = json.loads(
                raw.decode("utf-8")
            )

            return result["message"]["content"]

    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"Could not reach Ollama at {OLLAMA_URL}: {exc}"
        )

    except Exception as exc:
        raise RuntimeError(
            f"Ollama request failed: {exc}"
        )


def wait_for_ollama():
    """
    Wait until Ollama becomes available.
    """

    url = OLLAMA_URL.rstrip("/") + "/api/tags"

    while True:

        try:
            request = urllib.request.Request(
                url,
                method="GET",
            )

            with urllib.request.urlopen(
                request,
                timeout=5,
            ) as response:

                if response.status == 200:
                    log(
                        f"Ollama available: "
                        f"{OLLAMA_URL}"
                    )
                    return

        except Exception:
            pass

        log("Waiting for Ollama...")

        time.sleep(2)


# ============================================================
# Command execution
# ============================================================

def run_command(command, timeout=120):
    """
    Execute a shell command inside the desktop container.

    Commands run as the headless user.
    """

    log(f"Executing command: {command}")

    try:
        process = subprocess.run(
            command,
            shell=True,
            cwd=str(WORK_DIR),
            capture_output=True,
            text=True,
            timeout=timeout,
        )

        output = ""

        if process.stdout:
            output += process.stdout

        if process.stderr:
            output += "\n" + process.stderr

        output = output.strip()

        log(
            f"Command exit code: "
            f"{process.returncode}"
        )

        if output:
            log(output[-4000:])

        return {
            "success": process.returncode == 0,
            "exit_code": process.returncode,
            "output": output,
        }

    except subprocess.TimeoutExpired:
        return {
            "success": False,
            "exit_code": -1,
            "output": (
                "Command timed out after "
                f"{timeout} seconds."
            ),
        }

    except Exception as exc:
        return {
            "success": False,
            "exit_code": -1,
            "output": str(exc),
        }


# ============================================================
# Agent reasoning
# ============================================================

SYSTEM_PROMPT = """
You are the autonomous agent running inside a Flowera desktop VM.

You receive a task from a human.

Your job is to actually perform the task.

You have access to a Flowera shell.

IMPORTANT OUTPUT RULE:

Every response MUST begin with exactly ONE of:

COMMAND:
DONE:
HELP:

Do not write anything before the control signal.

Valid response formats:

COMMAND:
<one shell command>

DONE:
<short final answer>

HELP:
<specific blocking problem>

Rules:

1. Use COMMAND when you need to perform an action on the Flowera desktop.

2. After a COMMAND is executed, you will receive its output.

3. If the command fails, reason about the failure and try to fix it.

4. Do not repeatedly execute the same failed command.

5. Use HELP ONLY when you are genuinely blocked and cannot
continue without information or action from the human/control plane.

6. A normal question, greeting, conversation, explanation, or
identity question is NOT a HELP situation.

7. If the human asks something like:
   "hello"
   "who are you?"
   "what can you do?"
   "how are you?"
   answer using DONE.

8. HELP must ONLY be used when you need something from the human
to continue the task.

9. Examples of valid HELP situations:
   - You need the user to choose between two files.
   - You need a password or confirmation.
   - A required resource is unavailable.
   - You cannot determine what the user wants and cannot safely
     proceed.
   - An external system requires human intervention.

10. When the task is complete, use DONE.

11. Do not claim a task is complete unless it actually is.

12. Prefer small, verifiable commands.

13. Never invent command output.

14. Never use HELP merely to ask a conversational question.

15. Never use HELP merely because the task is a question.

16. Never output both DONE and HELP.

17. The control signal must be the FIRST thing in your response.

Examples:

Human:
hello, who are you?

Correct:
DONE:
I am a virtual machine assistant running inside your Flowera desktop VM.

Human:
List the files in /tmp.

Correct:
COMMAND:
ls -la /tmp

Human:
Delete the project.

If you need clarification about which project:
HELP:
Which project directory should I delete?

Human:
Delete /home/headless/work/project.

Correct:
COMMAND:
rm -rf /home/headless/work/project

After verifying deletion:
DONE:
The project was deleted successfully.
"""


def ask_agent(task, history):
    messages = [
        {
            "role": "system",
            "content": SYSTEM_PROMPT,
        }
    ]

    messages.extend(history)

    messages.append(
        {
            "role": "user",
            "content": task,
        }
    )

    return ollama_request(messages)


# ============================================================
# Agent parser
# ============================================================

def parse_agent_response(response):
    """
    Parse the agent's control signal.

    The model is instructed to put COMMAND, DONE, or HELP
    first, but small models sometimes add text before it.
    We therefore search for the control signal rather than
    requiring it to be the first characters.
    """

    response = response.strip()

    # Prefer the first control signal that appears.
    command_pos = response.find("COMMAND:")
    done_pos = response.find("DONE:")
    help_pos = response.find("HELP:")

    positions = [
        (pos, "command")
        for pos in [command_pos]
        if pos != -1
    ] + [
        (pos, "done")
        for pos in [done_pos]
        if pos != -1
    ] + [
        (pos, "help")
        for pos in [help_pos]
        if pos != -1
    ]

    if not positions:
        return ("invalid", response)

    # Use whichever control signal appears first.
    _, action = min(positions, key=lambda x: x[0])

    if action == "command":
        value = response[
            command_pos + len("COMMAND:"):
        ].strip()

        return ("command", value)

    if action == "done":
        value = response[
            done_pos + len("DONE:"):
        ].strip()

        return ("done", value)

    if action == "help":
        value = response[
            help_pos + len("HELP:"):
        ].strip()

        return ("help", value)

    return ("invalid", response)


# ============================================================
# Task execution
# ============================================================

def perform_task(task, task_file):
    global current_task
    global current_task_file
    global waiting_for_help

    current_task = task
    current_task_file = task_file
    waiting_for_help = False

    log("========================================")
    log("STARTING TASK")
    log(task)
    log("========================================")

    history = []

    # Limit the number of autonomous steps.
    #
    # This prevents an agent from looping forever.
    max_steps = 50

    for step in range(max_steps):

        log(
            f"Agent step "
            f"{step + 1}/{max_steps}"
        )

        try:
            response = ask_agent(
                task,
                history,
            )

        except Exception as exc:

            help_id = ask_for_help(
                f"""
Ollama/agent execution failed.

Error:
{exc}
""".strip()
            )

            # Wait until a human response arrives.
            return

        log("Agent response:")
        log(response)

        action, value = parse_agent_response(
            response
        )

        if action == "done":

            write_final_message(value)

            return

        if action == "help":

            ask_for_help(value)

            return

        if action == "command":

            command = value

            if not command:
                ask_for_help(
                    "The agent generated an empty COMMAND."
                )
                return

            result = run_command(
                command
            )

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": (
                        "Command execution result:\n\n"
                        f"exit_code: "
                        f"{result['exit_code']}\n\n"
                        f"output:\n"
                        f"{result['output']}"
                    ),
                }
            )

            continue

        if action == "invalid":

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": """
Your response did not follow the required format.

Respond with exactly one of:

COMMAND:
<shell command>

DONE:
<final result>

HELP:
<specific blocking issue>
""".strip(),
                }
            )

            continue

    # Agent exhausted its step limit.
    ask_for_help(
        f"""
The agent reached its maximum number of
autonomous steps ({max_steps}) without completing
the task.

It may be stuck in a loop.

Please inspect the task and provide guidance.
""".strip()
    )


# ============================================================
# Help response detection
# ============================================================

def is_help_response(filename, content):
    """
    A message received while waiting for help is treated
    as the human's response to the current blocked task.
    """

    if not waiting_for_help:
        return False

    lower_name = filename.lower()

    # Ignore our own generated result/help files.
    if lower_name.startswith("help_"):
        return False

    if lower_name.startswith("result_"):
        return False

    return True


def continue_after_help(help_response):
    """
    Continue the current task after a human provides help.
    """

    global waiting_for_help

    if not current_task:
        log(
            "Received help response but no "
            "active task exists."
        )
        return

    task = current_task

    waiting_for_help = False

    log("========================================")
    log("RECEIVED HUMAN HELP")
    log(help_response)
    log("========================================")

    # Continue the same task with the human's
    # response appended as context.
    #
    # We run this in the same task thread.
    history = [
        {
            "role": "user",
            "content": (
                "The human has provided help:\n\n"
                + help_response
            ),
        }
    ]

    max_steps = 50

    for step in range(max_steps):

        try:
            response = ollama_request(
                [
                    {
                        "role": "system",
                        "content": SYSTEM_PROMPT,
                    },
                    {
                        "role": "user",
                        "content": (
                            f"Original task:\n\n"
                            f"{task}\n\n"
                            f"Human help:\n\n"
                            f"{help_response}\n\n"
                            f"Continue the task."
                        ),
                    },
                    *history,
                ]
            )

        except Exception as exc:

            ask_for_help(
                f"""
The agent tried to continue after human help,
but Ollama failed again.

Error:
{exc}
""".strip()
            )

            return

        log("Agent response:")
        log(response)

        action, value = parse_agent_response(
            response
        )

        if action == "done":

            write_final_message(value)
            return

        if action == "help":

            ask_for_help(value)
            return

        if action == "command":

            result = run_command(value)

            history.append(
                {
                    "role": "assistant",
                    "content": response,
                }
            )

            history.append(
                {
                    "role": "user",
                    "content": (
                        "Command execution result:\n\n"
                        f"exit_code: "
                        f"{result['exit_code']}\n\n"
                        f"output:\n"
                        f"{result['output']}"
                    ),
                }
            )

            continue

        history.append(
            {
                "role": "assistant",
                "content": response,
            }
        )

        history.append(
            {
                "role": "user",
                "content": """
Invalid response.

Use COMMAND, DONE, or HELP.
""".strip(),
            }
        )

    ask_for_help(
        "The agent exhausted its steps after receiving human help."
    )


# ============================================================
# Command watcher
# ============================================================

def command_watcher():
    """
    Continuously watch command/ for new task files.
    """

    global processed_commands

    log(
        f"Watching command directory: "
        f"{COMMAND_DIR}"
    )

    while True:

        try:

            files = sorted(
                COMMAND_DIR.glob("*.txt"),
                key=lambda p: p.stat().st_mtime,
            )

            for path in files:

                if path.name in processed_commands:
                    continue

                # Ignore files generated by the agent.
                if path.name.startswith("result_"):
                    processed_commands.add(
                        path.name
                    )
                    continue

                try:
                    content = path.read_text(
                        encoding="utf-8"
                    ).strip()

                except Exception as exc:
                    log(
                        f"Could not read {path}: "
                        f"{exc}"
                    )
                    continue

                if not content:
                    processed_commands.add(
                        path.name
                    )
                    continue

                processed_commands.add(
                    path.name
                )

                # If the agent is waiting for human help,
                # this new command is the help response.
                if waiting_for_help:

                    thread = threading.Thread(
                        target=continue_after_help,
                        args=(content,),
                        daemon=True,
                    )

                    thread.start()

                    continue

                # Do not start another task while one
                # is already running.
                if current_task is not None:

                    log(
                        "Task already running. "
                        f"Leaving {path.name} "
                        "for later."
                    )

                    processed_commands.discard(
                        path.name
                    )

                    continue

                thread = threading.Thread(
                    target=perform_task,
                    args=(
                        content,
                        path,
                    ),
                    daemon=True,
                )

                thread.start()

        except Exception as exc:
            log(
                f"Command watcher error: {exc}"
            )

        time.sleep(1)


# ============================================================
# HTTP API
# ============================================================

class Handler(BaseHTTPRequestHandler):

    def log_message(self, format, *args):
        log(
            "HTTP "
            + format % args
        )

    def do_POST(self):

        if self.path != "/message":
            self.send_response(404)
            self.end_headers()
            return

        try:

            content_length = int(
                self.headers.get(
                    "Content-Length",
                    "0",
                )
            )

            raw = self.rfile.read(
                content_length
            )

            data = json.loads(
                raw.decode("utf-8")
            )

            message = data.get(
                "content",
                "",
            )

            if not message:
                raise ValueError(
                    "missing content"
                )

            path = save_message(
                message
            )

            self.send_response(200)

            self.send_header(
                "Content-Type",
                "application/json",
            )

            self.end_headers()

            self.wfile.write(
                json.dumps(
                    {
                        "status": "received",
                        "file": path.name,
                    }
                ).encode("utf-8")
            )

        except Exception as exc:

            log(
                f"POST /message failed: "
                f"{exc}"
            )

            self.send_response(400)
            self.send_header(
                "Content-Type",
                "application/json",
            )
            self.end_headers()

            self.wfile.write(
                json.dumps(
                    {
                        "status": "error",
                        "error": str(exc),
                    }
                ).encode("utf-8")
            )


# ============================================================
# Help text watcher (defunct, useful to have)
# ============================================================

def helptext_watcher():
    """
    Existing functionality:
    watches helptext/ and sends newly-created or
    modified .txt files to the control plane.

    This remains useful for agent-generated help requests.
    """

    log(
        f"Watching helptext directory: "
        f"{HELPTEXT_DIR}"
    )

    seen = {}

    while True:

        try:

            for path in HELPTEXT_DIR.glob("*.txt"):

                try:
                    stat = path.stat()

                except FileNotFoundError:
                    continue

                signature = (
                    stat.st_mtime_ns,
                    stat.st_size,
                )

                if seen.get(path.name) == signature:
                    continue

                seen[path.name] = signature

                try:
                    content = path.read_text(
                        encoding="utf-8"
                    )

                except Exception as exc:
                    log(
                        f"Could not read help file "
                        f"{path}: {exc}"
                    )
                    continue

                send_helptext(
                    path.name,
                    content,
                )

        except Exception as exc:
            log(
                f"Helptext watcher error: "
                f"{exc}"
            )

        time.sleep(1)


# ============================================================
# Startup
# ============================================================

def main():

    log("========================================")
    log("Flowera VM Agent")
    log("========================================")

    log(
        f"Desktop ID: {DESKTOP_ID}"
    )

    log(
        f"Desktop name: {DESKTOP_NAME}"
    )

    log(
        f"Control plane: "
        f"{CONTROL_PLANE_URL}"
    )

    log(
        f"Ollama: "
        f"{OLLAMA_URL}"
    )

    log(
        f"Model: "
        f"{OLLAMA_MODEL}"
    )

    # Wait until the shared Ollama instance is alive.
    wait_for_ollama()

    # Start command watcher.
    threading.Thread(
        target=command_watcher,
        daemon=True,
    ).start()

    server = HTTPServer(
        ("0.0.0.0", 8081),
        Handler,
    )

    log(
        "VM agent listening on "
        "0.0.0.0:8081"
    )

    server.serve_forever()


if __name__ == "__main__":
    main()
