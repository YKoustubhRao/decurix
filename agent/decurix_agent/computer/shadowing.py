import json
import os
import time
from pathlib import Path

from decurix_agent import config
from decurix_agent.computer import modes


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
# ============================================================

SHADOWING_DIR = config.DECURIX_DIR / "shadowing"
OBSERVATIONS_FILE = SHADOWING_DIR / "observations.jsonl"
METRICS_FILE = SHADOWING_DIR / "metrics.json"

MAX_RECENT_OBSERVATIONS = 100


def _ensure_dir():
    SHADOWING_DIR.mkdir(parents=True, exist_ok=True)


def predict_action(goal, screen_context=None, step_index=0):
    """
    Generate an internal prediction for the next action given goal and context.

    Returns a dict with action details:
      {
        "kind": "click" | "type" | "key" | "open" | "done",
        "target": {"role": "button", "name": "Submit"},
        "value": "search query" | "Return" | "",
        "reasoning": "Click the submit button after typing query",
        "predicted_at": timestamp
      }
    """

    context = screen_context or {}
    window = context.get("active_window", "")
    focused = context.get("focused_element", {})
    url = context.get("url", "")

    # Heuristic baseline prediction based on intent and visible context
    goal_lower = (goal or "").lower()

    if step_index == 0 and ("open" in goal_lower or "browse" in goal_lower or "navigate" in goal_lower):
        # Look for a url in goal or context
        words = goal.split()
        target_url = next((w for w in words if w.startswith("http://") or w.startswith("https://") or ".com" in w), url)
        return {
            "kind": "open",
            "target": {"role": "browser", "name": "address_bar"},
            "value": target_url or "https://www.google.com",
            "reasoning": "Navigate to the initial destination URL",
            "predicted_at": time.time(),
        }

    if "search" in goal_lower or "find" in goal_lower:
        if focused and focused.get("role") in ("entry", "textfield", "searchbox"):
            return {
                "kind": "type",
                "target": {"role": focused.get("role"), "name": focused.get("name", "search")},
                "value": goal_lower.replace("search for", "").replace("search", "").strip(),
                "reasoning": "Type search keywords into focused search input",
                "predicted_at": time.time(),
            }
        return {
            "kind": "click",
            "target": {"role": "searchbox", "name": "Search"},
            "value": "",
            "reasoning": "Focus search box",
            "predicted_at": time.time(),
        }

    if "submit" in goal_lower or "save" in goal_lower or "send" in goal_lower:
        button_name = "Send" if "send" in goal_lower else ("Save" if "save" in goal_lower else "Submit")
        return {
            "kind": "click",
            "target": {"role": "button", "name": button_name},
            "value": "",
            "reasoning": f"Click {button_name} button to commit action",
            "predicted_at": time.time(),
        }

    # Fallback prediction
    return {
        "kind": "click",
        "target": {"role": focused.get("role", "control"), "name": focused.get("name", window)},
        "value": "",
        "reasoning": "Interact with active context element",
        "predicted_at": time.time(),
    }


def compute_delta(predicted, observed):
    """
    Compare predicted action against observed human action and return delta metrics.
    """

    pred_kind = (predicted or {}).get("kind", "").lower()
    obs_kind = (observed or {}).get("kind", "").lower()

    kind_match = (pred_kind == obs_kind)

    pred_target = (predicted or {}).get("target") or {}
    obs_target = (observed or {}).get("target") or {}

    pred_role = str(pred_target.get("role", "")).lower()
    obs_role = str(obs_target.get("role", "")).lower()
    role_match = (not pred_role or not obs_role or pred_role == obs_role)

    pred_name = str(pred_target.get("name", "")).lower().strip()
    obs_name = str(obs_target.get("name", "")).lower().strip()
    name_match = (not pred_name or not obs_name or pred_name in obs_name or obs_name in pred_name)

    target_match = role_match and name_match

    pred_val = str((predicted or {}).get("value", "")).strip().lower()
    obs_val = str((observed or {}).get("value", "")).strip().lower()
    val_match = (pred_val == obs_val) if (pred_val or obs_val) else True

    # Compute a similarity score between 0.0 and 1.0
    score = 0.0
    if kind_match:
        score += 0.4
    if role_match:
        score += 0.2
    if name_match:
        score += 0.2
    if val_match:
        score += 0.2

    delta_notes = []
    if not kind_match:
        delta_notes.append(f"Predicted action kind '{pred_kind}' but observed '{obs_kind}'")
    if not name_match:
        delta_notes.append(f"Predicted target '{pred_name}' but observed '{obs_name}'")
    if not val_match:
        delta_notes.append(f"Predicted value '{pred_val}' but observed '{obs_val}'")

    return {
        "score": round(score, 2),
        "kind_match": kind_match,
        "target_match": target_match,
        "value_match": val_match,
        "delta_notes": delta_notes or ["Predicted action matched observed human behavior"],
    }


def record_observation(task_id, goal, predicted, observed, context=None):
    """
    Record an observation step comparing prediction to human demonstration.
    Persists to observations.jsonl and updates aggregate metrics.
    """

    _ensure_dir()
    delta = compute_delta(predicted, observed)

    entry = {
        "timestamp": time.time(),
        "task_id": str(task_id),
        "goal": goal,
        "predicted": predicted,
        "observed": observed,
        "context": context or {},
        "delta": delta,
    }

    try:
        with open(OBSERVATIONS_FILE, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry) + "\n")
    except OSError:
        pass

    _update_metrics(delta)
    return entry


def _update_metrics(delta):
    """Update running accuracy metrics."""

    metrics = get_metrics()
    total = metrics.get("total_observations", 0) + 1
    exact_matches = metrics.get("exact_matches", 0) + (1 if delta["score"] >= 0.9 else 0)
    kind_matches = metrics.get("kind_matches", 0) + (1 if delta["kind_match"] else 0)
    target_matches = metrics.get("target_matches", 0) + (1 if delta["target_match"] else 0)
    cumulative_score = metrics.get("cumulative_score", 0.0) + delta["score"]

    updated = {
        "total_observations": total,
        "exact_matches": exact_matches,
        "kind_matches": kind_matches,
        "target_matches": target_matches,
        "cumulative_score": round(cumulative_score, 2),
        "average_accuracy": round(cumulative_score / max(1, total), 3),
        "last_updated": time.time(),
    }

    try:
        tmp = METRICS_FILE.with_name(METRICS_FILE.name + ".tmp")
        tmp.write_text(json.dumps(updated, indent=2), encoding="utf-8")
        tmp.replace(METRICS_FILE)
    except OSError:
        pass


def get_metrics():
    """Retrieve aggregate shadowing accuracy metrics."""

    if not METRICS_FILE.exists():
        return {
            "total_observations": 0,
            "exact_matches": 0,
            "kind_matches": 0,
            "target_matches": 0,
            "cumulative_score": 0.0,
            "average_accuracy": 0.0,
            "last_updated": 0,
        }

    try:
        return json.loads(METRICS_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {
            "total_observations": 0,
            "exact_matches": 0,
            "kind_matches": 0,
            "target_matches": 0,
            "cumulative_score": 0.0,
            "average_accuracy": 0.0,
            "last_updated": 0,
        }


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

