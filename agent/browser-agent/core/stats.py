"""VRAM/RAM/model-context stats for the UI's status bar."""
import subprocess

import psutil
import requests

import models

OLLAMA = models.OLLAMA  # single source of truth (models.py is also OLLAMA_URL-driven in the decurix deployment)


def gpu_vram():
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=memory.used,memory.total", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=5,
        )
        used, total = (int(x.strip()) for x in out.stdout.strip().split(","))
        return {"used_mb": used, "total_mb": total}
    except Exception:
        return None


def system_ram():
    vm = psutil.virtual_memory()
    return {"used_mb": (vm.total - vm.available) // (1024 * 1024), "total_mb": vm.total // (1024 * 1024)}


def loaded_models():
    """What Ollama currently has resident, with each model's own VRAM footprint."""
    try:
        resp = requests.get(f"{OLLAMA}/api/ps", timeout=5)
        resp.raise_for_status()
        return [
            {
                "name": m["name"],
                "size_vram_mb": m.get("size_vram", 0) // (1024 * 1024),
                "context": m.get("context_length"),
            }
            for m in resp.json().get("models", [])
        ]
    except Exception:
        return []


def snapshot():
    return {
        "vram": gpu_vram(),
        "ram": system_ram(),
        "ollama_models": loaded_models(),
        "last_request": dict(models.LAST_USAGE),
    }
