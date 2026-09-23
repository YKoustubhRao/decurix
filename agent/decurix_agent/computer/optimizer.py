import subprocess
import time
from pathlib import Path

from decurix_agent import config
from decurix_agent.logging_utils import log


# ============================================================
# Replicate First. Optimize Second. (Zoral AI Principle)
#
# "Replicate first, optimize second: Replicate human UI steps first;
# once verified, optimize via terminal commands, direct API calls,
# or specialized vertical tools."
#
# A skill or subtask begins by replicating the exact human demonstration
# on screen (GUI clicks, typing, screen element interactions).
#
# Once an operation has proven reliable or has a defined deterministic
# fast-path, the optimizer offers a direct execution route (CLI / API / script).
#
# Dual-route policy:
#   1. Attempt the optimized route (fast-path).
#   2. Validate outcome against completion criteria (expected result).
#   3. If optimization fails or cannot be verified, immediately fall back
#      to exact UI replication.
# ============================================================

ROUTE_GUI_REPLAY = "gui_replay"
ROUTE_OPTIMIZED = "optimized_fastpath"


def get_available_routes(skill_or_subtask):
    """
    List routes available for a skill or subtask:
    Always includes 'gui_replay'; includes 'optimized_fastpath' if an
    optimized command or script is registered.
    """

    routes = [ROUTE_GUI_REPLAY]
    opt = (skill_or_subtask or {}).get("optimization") or {}
    if opt.get("command") or opt.get("script"):
        routes.insert(0, ROUTE_OPTIMIZED)

    return routes


def build_optimization(command=None, script=None, timeout=60, post_check=None):
    """
    Define an optimized fast-path route for a skill or step.

    `post_check` is an optional validation condition (e.g. file must exist, exit code 0).
    """

    return {
        "command": command,
        "script": script,
        "timeout": timeout,
        "post_check": post_check or {},
        "created_at": time.time(),
        "runs": 0,
        "successes": 0,
        "fallbacks": 0,
    }


def execute_with_fallback(skill_or_step, gui_replay_fn, work_dir=None):
    """
    Execute with dual-route strategy:
      1. If an optimized route is configured, run it first.
      2. If it succeeds and satisfies checks, return the fast-path result.
      3. Otherwise, log fallback and execute exact UI replication (gui_replay_fn).
    """

    optimization = (skill_or_step or {}).get("optimization")
    work_path = Path(work_dir) if work_dir else (config.WORK_DIR if config.WORK_DIR.exists() else Path.cwd())

    if optimization and (optimization.get("command") or optimization.get("script")):
        log("Attempting optimized fast-path route...")
        success, result = _run_optimized(optimization, work_path)

        if success:
            log("Optimized fast-path succeeded.")
            optimization["runs"] = optimization.get("runs", 0) + 1
            optimization["successes"] = optimization.get("successes", 0) + 1
            return {
                "route": ROUTE_OPTIMIZED,
                "status": "success",
                "result": result,
                "fallback_used": False,
            }

        log("Optimized route failed or unverified - falling back to GUI replication.")
        optimization["runs"] = optimization.get("runs", 0) + 1
        optimization["fallbacks"] = optimization.get("fallbacks", 0) + 1

    # Baseline: Replicate first
    replay_result = gui_replay_fn()
    return {
        "route": ROUTE_GUI_REPLAY,
        "status": "success" if (isinstance(replay_result, dict) and replay_result.get("status") != "failed") else "completed",
        "result": replay_result,
        "fallback_used": bool(optimization),
    }


def _run_optimized(optimization, work_path):
    """Execute the command or script and verify post-check."""

    cmd = optimization.get("command")
    timeout = optimization.get("timeout", 60)

    if not cmd and optimization.get("script"):
        cmd = f"python3 {optimization['script']}"

    if not cmd:
        return False, "no command or script provided"

    try:
        proc = subprocess.run(
            cmd,
            shell=True,
            cwd=str(work_path),
            capture_output=True,
            text=True,
            timeout=timeout,
        )

        if proc.returncode != 0:
            return False, f"exit {proc.returncode}: {proc.stderr.strip()[:200]}"

        output = proc.stdout.strip()

        # Check post-check criteria if specified
        post = optimization.get("post_check") or {}
        if post.get("file_exists"):
            target_file = work_path / post["file_exists"]
            if not target_file.exists():
                return False, f"post-check failed: {post['file_exists']} not found"

        if post.get("contains_text") and post["contains_text"] not in output:
            return False, f"post-check failed: expected text {post['contains_text']!r} not in output"

        return True, output
    except Exception as exc:
        return False, str(exc)
