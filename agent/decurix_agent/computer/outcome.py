"""
What an action's result shows of its effect, as data: an effect and the decision it leaves the engine (technique from
Hermes' computer-use tool, `_classify_action_result` in tools/computer_use/tool.py).

effect    confirmed       what the screen reads back shows the action took hold: the field holds the text, the box
                          is ticked, the field has the keyboard, the screen's controls changed
          unverifiable    something may have happened, and nothing read back says what: only the picture changed, a
                          secret field (never read back), a field that shows another form of the text, an app whose
                          copy is still being read
          suspected_noop  nothing read back changed: the field holds what it held, the box is as it was, nothing on
                          the screen moved
decision  done                           confirmed
          look again before any retry    the input may have landed: look first, never repeat it blind
          escalate                       the same input will not do it (it did not reach the field): do it another
                                         way - another control, the app's own command - or ask

Classified from the two looks an action already makes (core.act), by code only: no model, no word lists - a field's
value against what was typed, a state against a state, a count of what changed. It is advice the result carries, never
a change to `gui`'s exit codes; the one thing enforced is the repeat: after an action that was not confirmed, the same
input on the same target is refused until the engine has looked again (observe, verify or read), with exit 1.
"""

import hashlib
import json
import time

from decurix_agent import config

CONFIRMED = "confirmed"
UNVERIFIABLE = "unverifiable"
SUSPECTED_NOOP = "suspected_noop"

DONE = "done"
LOOK_AGAIN = "look again before any retry"
ESCALATE = "escalate"

CHECKED_ROLES = ("checkbox", "radio", "switch", "menuitemcheckbox", "menuitemradio")
FIELD_ROLES = ("textbox", "searchbox", "combobox", "spinbutton", "password", "password text")
SECRET_SHOWN = "[withheld]"
DATE_STATES = {"date", "date and time", "time", "month", "week"}


def _norm(text):
    return " ".join(str(text or "").split()).casefold()


def _number(text):
    try:
        from decurix_agent import values
        return values.parse_number(str(text or ""))
    except Exception:  # noqa: BLE001 - not a number this can read
        return None


def holds_text(value, text):
    """Whether a field reading `value` holds `text`: its first line within it, or the same number in another format."""

    line = (str(text or "").split("\n")[0]).strip()
    if not line:
        return False
    value = str(value or "")
    if line in value or _norm(line) in _norm(value):
        return True
    a, b = _number(line), _number(value)
    return a is not None and a == b


def same_element(element, observation):
    """The element `element` was, in a later look: the same ref with the same role and name (a ref is carried only to
    the same control: elements.assign_refs), else the one at the same path with the same role; or None."""

    if not element or not observation:
        return None
    found = observation.get("elements") or []
    ref = element.get("ref")
    if ref:
        for e in found:
            if e.get("ref") == ref and e.get("role") == element.get("role") and e.get("name") == element.get("name"):
                return e
    path = element.get("path")
    if path:
        same = [e for e in found if e.get("path") == path and e.get("role") == element.get("role")]
        if len(same) == 1:
            return same[0]
    return None


def _focused(observation):
    return next((e for e in (observation or {}).get("elements") or [] if "focused" in (e.get("states") or [])), None)


def _structural(change):
    """Whether the screen's controls changed, not only its picture: a window, the address, a control come or gone, a
    value or a state changed, the keyboard moved."""

    if not change:
        return False
    return bool(change.get("windows_opened") or change.get("windows_closed")
                or (change.get("active_before") != change.get("active_after"))
                or (change.get("url_after") and change.get("url_before") != change.get("url_after"))
                or change.get("appeared") or change.get("disappeared") or change.get("changed"))


def _pixels(change):
    pixels = (change or {}).get("pixels")
    return pixels is not None and pixels >= 0.001


def _result(effect, decision, why):
    return {"effect": effect, "decision": decision, "why": why}


def _states(element):
    return set((element or {}).get("states") or []) - {"offscreen", "focused"}


def classify(kind, element, before, after, change, text=None, option=None, taken=None, unverified=(),
             untargeted_typing_lost=False, set_directly=None):
    """
    {"effect", "decision", "why"} for one action from what it saw: `element` the target as the first look had it (None
    for keys or typing aimed at no control), `before`/`after` the looks, `change` their comparison (changes.compare),
    `text`/`option` what was typed or chosen, `taken` the entry a pick took, `unverified` the cells of a typed block
    that read back otherwise, `untargeted_typing_lost` typing aimed at nothing that changed nothing, `set_directly` the
    way a field's value was set when typed keys did not land ("cdp" or "atspi").
    """

    if (after or {}).get("behind"):
        return _result(UNVERIFIABLE, LOOK_AGAIN, "the app's controls are still being read again after the action")
    if set_directly:
        return _result(CONFIRMED, DONE, f"typed keys did not reach the field, so its value was set directly through "
                                        f"{'the page' if set_directly == 'cdp' else 'the accessibility interface'} "
                                        "and reads back")
    if unverified:
        return _result(UNVERIFIABLE, LOOK_AGAIN, f"{len(unverified)} cell(s) of the block do not read back what was "
                                                 "typed")

    now = same_element(element, after)
    secret = False
    if element is not None:
        try:
            from decurix_agent.computer import privacy
            secret = bool(privacy.secret_field(element))
        except Exception:  # noqa: BLE001 - not known to be readable: treated as secret
            secret = True

    if kind == "type":
        if element is None:
            if untargeted_typing_lost:
                return _result(SUSPECTED_NOOP, ESCALATE, "no field had the keyboard: nothing on the screen changed")
            field = _focused(after)
            if field is not None and holds_text(field.get("value"), text):
                return _result(CONFIRMED, DONE, "the field with the keyboard reads back what was typed")
            if _structural(change) or _pixels(change):
                return _result(UNVERIFIABLE, LOOK_AGAIN, "it was typed where the keyboard was, and no field reads it "
                                                         "back")
            return _result(SUSPECTED_NOOP, LOOK_AGAIN, "nothing on the screen changed")
        if secret:
            return _result(UNVERIFIABLE, LOOK_AGAIN, "a secret field is never read back")
        if now is None:
            return _result(UNVERIFIABLE, LOOK_AGAIN, "the field is no longer on the screen to read back")
        value = now.get("value")
        if value in (None, "") and now.get("role") in ("paragraph", "heading", "cell", "document"):
            value = now.get("name")
        if holds_text(value, text):
            return _result(CONFIRMED, DONE, "the field reads back what was typed")
        if any(c in str(text or "") for c in "\n\t"):
            # Enter or Tab in the text may have sent the field's form or moved on: an emptied field proves nothing.
            if _structural(change) or _pixels(change):
                return _result(UNVERIFIABLE, LOOK_AGAIN, "the text held Enter or Tab, and the field does not read it "
                                                         "back")
            return _result(SUSPECTED_NOOP, LOOK_AGAIN, "nothing on the screen changed")
        if _states(now) & DATE_STATES and value:
            return _result(UNVERIFIABLE, LOOK_AGAIN, "the date field shows the date in its own form")
        if (value or "") == (element.get("value") or ""):
            return _result(SUSPECTED_NOOP, ESCALATE, f"none of the {len(str(text or ''))} characters landed: the "
                                                     "field reads back what it held before")
        return _result(UNVERIFIABLE, LOOK_AGAIN, "the field changed, and does not read back what was typed")

    if kind == "select":
        if now is not None:
            shown = {_norm(now.get("value")), _norm(now.get("name"))}
            if (option and _norm(option) in shown) or (taken and _norm(taken) in shown):
                return _result(CONFIRMED, DONE, "the field reads back the entry chosen")
            if now.get("role") == "radio" and "checked" in _states(now):
                return _result(CONFIRMED, DONE, "the choice is checked")
        if _structural(change):
            return _result(UNVERIFIABLE, LOOK_AGAIN, "the screen changed, and the field does not read back the entry")
        return _result(SUSPECTED_NOOP, LOOK_AGAIN, "nothing on the screen changed")

    if kind in ("click", "double_click", "right_click") and element is not None:
        role = element.get("role")
        if role in CHECKED_ROLES and now is not None:
            if _states(now) != _states(element):
                return _result(CONFIRMED, DONE, "its checked state changed")
            if kind == "click":
                return _result(SUSPECTED_NOOP, ESCALATE, "its checked state did not change")
        if role in FIELD_ROLES and now is not None and "focused" in (now.get("states") or []):
            return _result(CONFIRMED, DONE, "the field has the keyboard")

    if kind == "scroll":
        if _structural(change) or _pixels(change) or _moved(before, after):
            return _result(CONFIRMED, DONE, "the view moved")
        return _result(SUSPECTED_NOOP, LOOK_AGAIN, "the view did not move (at its end, or nothing there scrolls)")

    if kind == "upload" and now is not None and (now.get("value") or "") != ((element or {}).get("value") or ""):
        return _result(CONFIRMED, DONE, "the file control reads back a file")

    if _structural(change) or _focus_moved(before, after):
        return _result(CONFIRMED, DONE, "the screen's controls changed")
    if _pixels(change):
        return _result(UNVERIFIABLE, LOOK_AGAIN, "only the picture changed; no control reads back a change")
    return _result(SUSPECTED_NOOP, LOOK_AGAIN, "nothing on the screen changed")


def _focus_moved(before, after):
    a, b = _focused(before), _focused(after)
    return (a or {}).get("path") != (b or {}).get("path")


def _moved(before, after):
    """Whether the controls of the looks stand elsewhere (a scroll moves boxes, not values)."""

    old = {e.get("path"): e.get("box") for e in (before or {}).get("elements") or []}
    return any(e.get("path") in old and old[e.get("path")] != e.get("box") for e in (after or {}).get("elements") or [])


def line(result):
    """The result's one line for the engine: `Outcome: effect=E; decision=D - why`."""

    return f"Outcome: effect={result['effect']}; decision={result['decision']} - {result['why']}."


# ============================================================
# Look first: no blind repeat of an input that was not confirmed
# ============================================================

MAX_MARKS = 8
MARK_SECONDS = 1800


def _file():
    return config.DECURIX_DIR / "computer" / "unconfirmed.json"


def _task():
    try:
        from decurix_agent import task_context
        return task_context.current() or ""
    except Exception:  # noqa: BLE001
        return ""


def _digest(value):
    """A short hash: what was typed is never kept (it may be a secret), only whether the next input is the same."""

    return hashlib.sha256(json.dumps(value, sort_keys=True, default=str).encode("utf-8")).hexdigest()[:20]


def target_key(element, point=None, cell=None, window=None):
    """What an action was aimed at, as a hash: the control (its group and path), a cell, a point, or - for keys aimed
    at nothing - the window that had the keyboard."""

    if element is not None:
        return _digest(["element", element.get("group") or "", element.get("path") or "", element.get("ref") or ""])
    if cell:
        return _digest(["cell", str(cell).upper()])
    if point is not None:
        return _digest(["point", list(point)])
    return _digest(["keyboard", window or ""])


def input_key(kind, text=None, keys=None, option=None, direction=None, files=None):
    """What the action put in, as a hash."""

    return _digest([kind, text, list(keys or []), option, direction, list(files or [])])


def _read():
    try:
        record = json.loads(_file().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return record if isinstance(record, dict) else {}


def _save(record):
    from decurix_agent.computer import privacy

    path = _file()
    path.parent.mkdir(parents=True, exist_ok=True)
    privacy.write_scrubbed(path, record, atomic=True)


def remember(result, kind, target, given):
    """Keep an action that was not confirmed (for the task being worked on), so its blind repeat is refused; a
    confirmed one on the same target ends that target's mark. Never raises."""

    task = _task()
    if not task:
        return
    try:
        record = _read()
        marks = record.get("marks") if record.get("task") == task else []
        marks = [m for m in marks or [] if isinstance(m, dict) and m.get("target") != target]
        if result.get("effect") != CONFIRMED:
            marks.append({"kind": kind, "target": target, "input": given, "effect": result.get("effect"),
                          "at": time.time()})
        _save({"task": task, "marks": marks[-MAX_MARKS:]})
    except Exception:  # noqa: BLE001 - a note for the next command; the action stands either way
        pass


def looked():
    """The engine looked at the screen again (observe, verify, read): every mark is answered. Never raises."""

    task = _task()
    try:
        record = _read()
        if record.get("marks"):
            _save({"task": task, "marks": []})
    except Exception:  # noqa: BLE001
        pass


def repeat_refusal(kind, target, given):
    """Why this input may not be sent now, or "": it is the very input an earlier action of this task put on the same
    target, whose effect was not confirmed, and the screen has not been looked at since."""

    task = _task()
    if not task:
        return ""
    record = _read()
    if record.get("task") != task:
        return ""
    now = time.time()
    for mark in record.get("marks") or []:
        if not isinstance(mark, dict) or now - float(mark.get("at") or 0) > MARK_SECONDS:
            continue
        if mark.get("kind") == kind and mark.get("target") == target and mark.get("input") == given:
            effect = mark.get("effect") or UNVERIFIABLE
            return (f"the same {kind.replace('_', ' ')} on the same target was just done and its effect was not "
                    f"confirmed ({effect}), and the screen has not been looked at since - nothing was done. Look "
                    "again first (gui observe, or gui read on the field) and act on what it shows: the first one may "
                    "have landed, and a second would do it twice.")
    return ""
