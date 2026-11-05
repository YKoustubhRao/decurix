"""Provision an isolated desktop for live learning-transfer or GUI qualification.

Uses the actual desktop image and current source mounted read-only. Existing
desktops are never stopped, reused or removed. Only this run's container is
removed unless --keep is supplied.
"""
import argparse
import json
import subprocess
import sys
import time
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from agent_token import PY_AGENT, docker_env as token_env  # noqa: E402

REPO = Path(__file__).resolve().parents[2]


def command(args, **kwargs):
    kwargs.setdefault("timeout", 180)
    return subprocess.run(args, check=True, **kwargs)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--image", default="decurix-desktop:basic")
    p.add_argument("--suite", choices=["transfer", "native", "gui"], default="transfer")
    p.add_argument("--only")
    p.add_argument("--variants", default="holdout,moved-layout,renamed-control")
    p.add_argument("--keep", action="store_true")
    p.add_argument("--output", type=Path, default=REPO / "bench/workflows/results")
    args = p.parse_args()
    name = "decurix-test-" + args.suite + "-" + uuid.uuid4().hex[:10]
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = {"container": name, "image": args.image, "suite": args.suite, "status": "starting"}
    manifest_path = args.output / (name + ".json")
    manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")
    created = False
    try:
        command(["docker", "run", "-d", "--name", name, "--label", "decurix.benchmark=workflow-qualification",
            "--add-host", "host.docker.internal:host-gateway", "--shm-size=1g",
            "--security-opt", f"seccomp={REPO / 'internal/desktop/seccomp-desktop.json'}", "--ulimit", "core=0",
            "-e", "VNC_PW=local-test-only", "-e", "VNC_RESOLUTION=1280x720", "-e", "DESKTOP_ID=" + name,
            "-e", "DESKTOP_NAME=Workflow qualification", "-e", "AGENT_HARNESS=ollama",
            "-e", "CONTROL_PLANE_URL=http://127.0.0.1:18080", "-e", "OLLAMA_URL=http://127.0.0.1:11434",
            "-e", "DECURIX_ESCALATION_MODEL_CALLS=0", "-e", "DECURIX_SKILL_TRUST_RUNS=0", *token_env(),
            "-v", f"{REPO / 'agent/decurix_agent'}:/opt/decurix_agent:ro",
            "-v", f"{REPO / 'bench/workflows'}:/home/headless/bench-workflows:ro",
            "-v", f"{REPO / 'bench/gui/fixtures'}:/home/headless/gui/fixtures:ro", args.image])
        created = True
        print(f"Created isolated desktop {name}", flush=True)
        deadline = time.monotonic() + 300
        readiness_error = "agent has not answered"
        while time.monotonic() < deadline:
            try:
                ready = subprocess.run(["docker", "exec", name, "python3", "-c",
                    PY_AGENT + "agent('/skills', timeout=2)"],
                    capture_output=True, text=True, timeout=10)
            except (subprocess.TimeoutExpired, OSError) as exc:
                readiness_error = str(exc)
                time.sleep(2)
                continue
            if ready.returncode == 0:
                break
            readiness_error = ready.stderr[-1000:]
            time.sleep(2)
        else:
            raise RuntimeError("Desktop agent did not become ready within 300 seconds: " + readiness_error)
        manifest["status"] = "running"
        manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")
        if args.suite in {"transfer", "native"}:
            cmd = ["docker", "exec", "-u", "headless", "-e", "DISPLAY=:1", "-e", "PYTHONPATH=/opt", name,
                   "python3", "-u", "/home/headless/bench-workflows/" +
                   ("live_transfer.py" if args.suite == "transfer" else "native_transfer.py")]
            if args.suite == "transfer":
                cmd += ["--variants", args.variants]
            if args.only:
                cmd += ["--only", args.only]
        else:
            cmd = [sys.executable, str(REPO / "bench/gui/run.py"), "--engine", "oracle", "--container", name,
                   "--out", str(args.output / name / "gui")]
            if args.only:
                cmd += ["--tasks", args.only]
        result = subprocess.run(cmd)
        manifest.update(status="passed" if result.returncode == 0 else "failed", exit_code=result.returncode)
        return result.returncode
    except Exception as exc:
        manifest.update(status="failed", error=str(exc))
        raise
    finally:
        if created:
            # Keep reports even when the test failed, timed out or was cancelled.
            target = args.output / name
            target.mkdir(exist_ok=True)
            def collect(argv):
                try:
                    return subprocess.run(argv, capture_output=True, text=True, timeout=30)
                except (OSError, subprocess.TimeoutExpired) as exc:
                    manifest.setdefault("collection_errors", []).append(str(exc))
                    return None
            for remote, local in [("/home/headless/work/live-transfer.json", "transfer.json"),
                                  ("/home/headless/work/native-transfer.json", "native-transfer.json"),
                                  ("/home/headless/work/transfer-screens", "screens"),
                                  ("/home/headless/work/native-transfer-screens", "native-screens"),
                                  ("/home/headless/.decurix/skills", "skills"),
                                  ("/home/headless/.decurix/recordings", "recordings")]:
                collect(["docker", "cp", f"{name}:{remote}", str(target / local)])
            logs = collect(["docker", "logs", "--tail", "100", name])
            if logs is not None:
                (target / "container.log").write_text(logs.stdout + logs.stderr, encoding="utf-8")
            manifest["kept"] = args.keep
            if not args.keep:
                collect(["docker", "rm", "-f", name])
        manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")
        print(json.dumps(manifest), flush=True)


if __name__ == "__main__":
    raise SystemExit(main())
