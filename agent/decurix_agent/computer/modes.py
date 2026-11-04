import json
import os

from decurix_agent import config


# ============================================================
# Worker Replication Stages (Zoral AI Lifecycle)
#
# Zoral AI's 3-stage worker replication progression:
#
#   Stage 1: Observation ("observation")
#            Shadows human activity. Before an observed action occurs, the
#            worker predicts the action, observes what the human did, and
#            calculates the prediction delta without mutating external systems.
#
#   Stage 2: Supervised Execution ("supervised", default)
#            Internal actions (local file editing, system inspection, reading
#            DOM, local document drafting) run freely. Externally visible
#            actions (outbound emails, messaging, public posts, external API
#            writes, payments) are gated for human approval.
#
#   Stage 3: Autonomous Execution ("autonomous")
#            Unsupervised execution for trusted operations, compounding
#            experience into memory with critical hard safety boundaries.
# ============================================================

STAGE_OBSERVATION = "observation"
STAGE_SUPERVISED = "supervised"
STAGE_AUTONOMOUS = "autonomous"

VALID_STAGES = {STAGE_OBSERVATION, STAGE_SUPERVISED, STAGE_AUTONOMOUS}

STAGE_LABELS = {
    STAGE_OBSERVATION: "Stage 1: Observation (Shadowing)",
    STAGE_SUPERVISED: "Stage 2: Supervised Execution",
    STAGE_AUTONOMOUS: "Stage 3: Autonomous Execution",
}

STAGE_DESCRIPTIONS = {
    STAGE_OBSERVATION: "Shadows human workflows, predicts actions, records difference deltas, and makes no external changes.",
    STAGE_SUPERVISED: "Internal actions run freely; outbound messages, payments, and external writes require approval.",
    STAGE_AUTONOMOUS: "Operates autonomously with continuous learning and core system safety gates.",
}

STATE_FILE = config.DECURIX_DIR / "computer" / "worker_mode.json"


def get_stage():
    """Current replication stage: 'observation', 'supervised' (default), or 'autonomous'."""

    env_stage = os.environ.get("DECURIX_WORKER_STAGE", "").strip().lower()
    if env_stage in VALID_STAGES:
        return env_stage

    if not STATE_FILE.exists():
        return STAGE_SUPERVISED

    try:
        data = json.loads(STATE_FILE.read_text(encoding="utf-8"))
        stage = data.get("stage", STAGE_SUPERVISED)
        return stage if stage in VALID_STAGES else STAGE_SUPERVISED
    except (OSError, ValueError):
        return STAGE_SUPERVISED


def set_stage(stage):
    """Set the worker replication stage."""

    stage = str(stage).strip().lower()
    if stage not in VALID_STAGES:
        raise ValueError(f"invalid worker stage: {stage!r}, must be one of {sorted(VALID_STAGES)}")

    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(STATE_FILE, {"stage": stage}, atomic=True, json_kw={"indent": 2})
    return stage


def stage_summary():
    """Human-readable summary of the current replication stage."""

    stage = get_stage()
    return {
        "stage": stage,
        "label": STAGE_LABELS.get(stage, stage),
        "description": STAGE_DESCRIPTIONS.get(stage, ""),
    }
