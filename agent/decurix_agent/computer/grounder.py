"""
Where something is on the screen, by asking a GUI-grounding model.

A brain that plans in words is poor at reading pixel positions off a
screenshot (pointing.py's ruler is the other half of the answer to that): it
clicked (68,171) for a box at (100,248), and the taskbar for a row below a
page's edge. A grounding model (Qwen3-VL, UGround, GTA1, Holo1.5, UI-TARS) is
trained for exactly one thing - a screenshot and "the Save button in the
dialog" in, a point out. So the brain says WHAT, and this says WHERE.

The model sits behind the control plane (internal/grounder, set up in its .env
with DECURIX_GROUNDER_MODEL), like the hosted brain: this desktop never knows
which model it is or holds a key, and changing the model needs no new image.

Only the screen point comes from here. What is clicked is decided as before:
core.act takes the element under the point, so the approval gate and privacy
judge the real control, never the description.
"""

import base64
import json
import os
import urllib.error
import urllib.request

from decurix_agent import config
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log

BASE_URL = config.CONTROL_PLANE_URL.rstrip("/") + "/api/grounder"

# A grounding model on a CPU reads a 1280x720 screen in tens of seconds; on a GPU, in one.
TIMEOUT_SECONDS = float(os.environ.get("DECURIX_GROUNDER_TIMEOUT_SECONDS", "240"))

_status = None


def available():
    """Whether the control plane has a grounding model set up. Asked once per process."""

    global _status
    if _status is None:
        try:
            with urllib.request.urlopen(BASE_URL, timeout=2) as response:
                _status = json.loads(response.read() or b"{}")
        except (OSError, ValueError) as exc:
            log(f"grounder: status unavailable ({exc})")
            _status = {}
    return bool(_status.get("configured"))


def model():
    return (_status or {}).get("model") or "the grounding model"


def locate(frame, description):
    """
    (x, y) on the screen where `description` is, read off `frame` (the screen
    as it is now). Raises ComputerError when there is no model or no answer.
    """

    description = " ".join((description or "").split())
    if not description:
        raise ComputerError("say what to look for: gui click --see \"the Save button in the dialog\"")
    if not available():
        raise ComputerError("there is no grounding model set up to look at the screen with "
                            "(DECURIX_GROUNDER_MODEL in the control plane's .env). Use a ref from gui observe.")

    body = json.dumps({
        "image": base64.b64encode(frame.png()).decode("ascii"),
        "description": description,
    }).encode()
    request = urllib.request.Request(BASE_URL + "/locate", data=body, method="POST",
                                     headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_SECONDS) as response:
            answer = json.loads(response.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 422:
            raise ComputerError(f"{model()} could not find {description!r} on the screen")
        raise ComputerError(f"the grounding model could not be asked (HTTP {exc.code})")
    except (OSError, ValueError) as exc:
        raise ComputerError(f"the grounding model could not be asked ({exc})")

    try:
        x, y = int(answer["x"]), int(answer["y"])
    except (KeyError, TypeError, ValueError):
        raise ComputerError("the grounding model's answer had no position")
    if not (0 <= x < frame.width and 0 <= y < frame.height):
        raise ComputerError("the grounding model put it off the screen")
    log(f"grounder: {description!r} -> {x},{y} ({answer.get('model')}, {answer.get('seconds', 0):.1f}s)")
    return x, y
