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
import re
import urllib.error
import urllib.request

from decurix_agent import config
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log

BASE_URL = config.CONTROL_PLANE_URL.rstrip("/") + "/api/grounder"

# A grounding model on a CPU reads a 1280x720 screen in tens of seconds; on a GPU, in one.
TIMEOUT_SECONDS = float(os.environ.get("DECURIX_GROUNDER_TIMEOUT_SECONDS", "240"))

_status = None


def _specialist():
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


def _brain_sees():
    """Whether this worker's own hosted model reads pictures (it can then say where something is, off the ruler)."""

    if os.environ.get("DECURIX_BRAIN_VISION", "").strip().lower() not in ("1", "true", "yes"):
        return False
    try:
        from decurix_agent import llm_client
        return llm_client.available()
    except Exception:
        return False


def available():
    """
    Whether something can look at the screen for a description: a grounding
    model, or failing that the worker's own hosted model when it reads
    pictures. Found live: with a vision brain and no grounding model, three
    tasks in a row began with `gui click --see ...`, each answered "there is no
    grounding model set up" - the model that had asked was itself able to look.
    """

    return _specialist() or _brain_sees()


def model():
    return (_status or {}).get("model") or "the grounding model"


POINT_SYSTEM = (
    "You are shown a screenshot of a computer screen with a ruler drawn on it: dotted lines every 100 pixels, each "
    "numbered at the top and left edges with its real screen position. You are asked where one thing is. Read its "
    "position off the ruler's numbers (a point between the 300 and 400 lines is 300-and-something) - the middle "
    "of the thing, where a person would click it. If it is not on the screen, or two things fit the description "
    "equally, say so instead of choosing. Reply with JSON only: {\"found\": true, \"x\": 0, \"y\": 0, \"what\": "
    "\"the thing at that point, in a few words\"} or {\"found\": false, \"why\": \"one sentence\"}."
)


def _ask_brain(frame, description):
    """(x, y) for `description`, asked of the worker's own hosted model with the ruled screenshot."""

    from decurix_agent import llm_client
    from decurix_agent.computer import pointing

    try:
        message, _spent = llm_client.chat([
            {"role": "system", "content": POINT_SYSTEM},
            {"role": "user", "content": [{"type": "text", "text": f"Where is: {description}"},
                                         llm_client.image_part(pointing.ruled(frame).png())]},
        ], max_tokens=200, purpose="py.ground")
        found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
        answer = json.loads(found.group(0)) if found else {}
    except (llm_client.LLMError, ValueError) as exc:
        raise ComputerError(f"the model could not be asked where that is ({exc})")

    if not answer.get("found"):
        raise ComputerError(f"looking at the screen found no {description!r}"
                            + (f": {' '.join(str(answer['why']).split())[:200]}" if answer.get("why") else ""))
    try:
        x, y = int(answer["x"]), int(answer["y"])
    except (KeyError, TypeError, ValueError):
        raise ComputerError("the model's answer had no position")
    if not (0 <= x < frame.width and 0 <= y < frame.height):
        raise ComputerError("the model put it off the screen")
    log(f"grounder: {description!r} -> {x},{y} (the worker's own model; it saw: {str(answer.get('what'))[:80]})")
    return x, y


def locate(frame, description):
    """
    (x, y) on the screen where `description` is, read off `frame` (the screen
    as it is now). Raises ComputerError when there is no model or no answer.
    """

    description = " ".join((description or "").split())
    if not description:
        raise ComputerError("say what to look for: gui click --see \"the Save button in the dialog\"")
    if not _specialist():
        if _brain_sees():
            return _ask_brain(frame, description)
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
