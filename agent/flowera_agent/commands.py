import subprocess

from flowera_agent import config
from flowera_agent.logging_utils import log


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
            cwd=str(config.WORK_DIR),
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
