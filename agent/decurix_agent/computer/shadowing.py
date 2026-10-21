import json

from decurix_agent import config


# ============================================================
# Shadowing: Zoral AI Stage 1 Observation Loop
#
# Predict -> Observe -> Learn
#
# When a worker is in observation/shadowing mode, it runs alongside
# human demonstrations or workflows without mutating external systems.
# For each step:
#   1. Predict: Infer the next action from current context and task goal.
#   2. Observe: Capture what the human actually executed.
#   3. Learn: Compare predicted vs observed, compute the difference delta,
#      and compound the delta into memory/experience.
#
# Only the read side is built: nothing records observations yet, so the
# endpoints below serve empty metrics and no traces.
# ============================================================

SHADOWING_DIR = config.DECURIX_DIR / "shadowing"
OBSERVATIONS_FILE = SHADOWING_DIR / "observations.jsonl"
METRICS_FILE = SHADOWING_DIR / "metrics.json"


def _empty_metrics():
    return {
        "total_observations": 0,
        "exact_matches": 0,
        "kind_matches": 0,
        "target_matches": 0,
        "cumulative_score": 0.0,
        "average_accuracy": 0.0,
        "last_updated": 0,
    }


def get_metrics():
    """Retrieve aggregate shadowing accuracy metrics."""

    if not METRICS_FILE.exists():
        return _empty_metrics()

    try:
        return json.loads(METRICS_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return _empty_metrics()


def get_recent_observations(limit=50):
    """Retrieve recent observation traces."""

    if not OBSERVATIONS_FILE.exists():
        return []

    entries = []
    try:
        lines = OBSERVATIONS_FILE.read_text(encoding="utf-8").splitlines()
        for line in lines[-limit:]:
            if line.strip():
                try:
                    entries.append(json.loads(line))
                except ValueError:
                    continue
    except OSError:
        return []

    return entries

