"""
Multi-Application Workflow Demonstration Ingest.

Converts multi-app demonstration logs and task recordings (spanning Mail,
Excel, PDF viewers, ERPs, and web portals) into Decurix native recordings.

Held to the same rules as every other importer (computer/ingest.py). Found in review (2026-11-03, D-03): this one
applied no privacy at all - typed text and web addresses with their tokens were stored as given - had no step cap, and
a malformed step raised an error the import route answered with a 500. Now typed text goes through ingest._typed (a
password field's value or a credential is withheld), every web address through privacy.reduce_url, the step count is
capped like the Chrome Recorder's, and anything malformed is an IngestError.
"""

from decurix_agent.computer import privacy


def _error(message):
    from decurix_agent.computer import ingest
    return ingest.IngestError(message)


def _text(value, what):
    if value is None:
        return ""
    if not isinstance(value, (str, int, float)) or isinstance(value, bool):
        raise _error(f"{what} is not text")
    return str(value)


def _point(value, what):
    if (not isinstance(value, (list, tuple)) or len(value) != 2
            or not all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in value)):
        raise _error(f"{what} is not a point (two numbers)")
    return [value[0], value[1]]


def _url(value):
    text = str(value)
    if "://" in text or text.startswith("www."):
        return privacy.reduce_url(text) or ""
    return text


def from_workflow_demo(data):
    """
    Parse a workflow demonstration data dictionary.
    Returns:
      {
        "title": str,
        "description": str,
        "screen": [int, int],
        "events": [dict],
        "skipped": [dict],
        "ignored": int,
        "source_steps": int,
      }
    Raises ingest.IngestError for anything that is not such a demonstration.
    """
    from decurix_agent.computer import ingest

    if not isinstance(data, dict):
        raise _error("a workflow demonstration is a JSON object")

    raw_steps = data.get("steps") or data.get("actions") or []
    if not isinstance(raw_steps, list):
        raise _error("a workflow demonstration's steps are a list")
    if len(raw_steps) > ingest.MAX_STEPS:
        raise _error(f"that recording has more than {ingest.MAX_STEPS} steps")

    title = privacy.safe_text(_text(data.get("title") or data.get("workflow"), "the title")
                              or "Enterprise Workflow Demonstration")
    description = privacy.safe_text(_text(data.get("description"), "the description") or f"Demonstration of {title}")
    screen = data.get("screen") or [1280, 800]
    screen = _point(screen, "the screen size")

    events = []
    skipped = []
    ignored = 0
    current_url = ""

    for idx, s in enumerate(raw_steps, start=1):
        if not isinstance(s, dict):
            skipped.append({"step": idx, "why": "Step is not a dictionary"})
            continue

        kind = s.get("kind") or s.get("action") or s.get("type")
        if not kind:
            skipped.append({"step": idx, "why": "Step has no kind or action"})
            continue
        if not isinstance(kind, str):
            raise _error(f"step {idx}'s kind is not text")

        kind = kind.lower().replace("-", "_")
        target = s.get("target") or s.get("element")
        files = s.get("files") or s.get("paths")

        # Map target to element dict
        element = {}
        if isinstance(target, dict):
            element = {
                "role": _text(target.get("role", "button"), f"step {idx}'s role") or "button",
                "name": _text(target.get("name", ""), f"step {idx}'s name"),
                "source": _text(target.get("source", "cdp" if "http" in current_url else "atspi"),
                                f"step {idx}'s source"),
            }
        elif isinstance(target, str) and kind != "open":
            element = {
                "role": "control",
                "name": target,
                "source": "cdp" if "http" in current_url else "atspi",
            }
        element = privacy.public_element(element) if element else {}
        if element:
            element = {k: v for k, v in element.items() if v is not None or k == "withheld"}

        base_event = {
            "actor": "human",
            "outcome": "done",
            "imported": {"step": idx, "type": kind},
        }
        expect = None

        if kind in ("open", "navigate", "launch"):
            open_target = s.get("target") or s.get("url") or s.get("app")
            if not open_target:
                skipped.append({"step": idx, "why": "Open step has no target"})
                continue
            open_target = _text(open_target, f"step {idx}'s target")
            if "://" in open_target or open_target.startswith("www."):
                current_url = _url(open_target)
                open_target = current_url
            event = dict(base_event, kind="open", target=open_target, new_tab=bool(s.get("new_tab", False)))
            if current_url:
                expect = {"url": current_url}

        elif kind in ("click", "double_click", "right_click"):
            event = dict(base_event, kind=kind, element=element)
            if s.get("point"):
                event["point"] = _point(s["point"], f"step {idx}'s point")

        elif kind in ("type", "fill", "input", "change"):
            value = _text(s.get("text") if "text" in s else s.get("value", ""), f"step {idx}'s text")
            text, why = ingest._typed(element, value)
            event = dict(base_event, kind="type", element=dict(element), text=text)
            if text is not None and element.get("name"):
                expect = {"values": [{"role": element.get("role", "textbox"), "name": element["name"], "value": text}]}
            if why:
                event["withheld"] = {"text": why}
                if privacy.secret_field(dict(element, role=element.get("role") or "textbox")):
                    event["element"]["secret"] = why

        elif kind == "upload":
            if isinstance(files, str):
                files = [files]
            if files is not None and not (isinstance(files, list) and all(isinstance(f, str) for f in files)):
                raise _error(f"step {idx}'s files are not a list of paths")
            event = dict(base_event, kind="upload", element=element, files=files or [])

        elif kind in ("select", "dropdown"):
            option = _text(s.get("option") or s.get("value"), f"step {idx}'s option")
            event = dict(base_event, kind="select", element=element, option=privacy.kept_text(privacy.safe_text(option)))

        elif kind == "scroll":
            direction = _text(s.get("direction", "down"), f"step {idx}'s direction")
            amount = s.get("amount", 3)
            if not isinstance(amount, (int, float)) or isinstance(amount, bool):
                raise _error(f"step {idx}'s amount is not a number")
            event = dict(base_event, kind="scroll", direction=direction, amount=amount)
            if element:
                event["element"] = element

        elif kind == "drag":
            end = s.get("end") or s.get("drop_point")
            event = dict(base_event, kind="drag", element=element,
                         end=_point(end, f"step {idx}'s end") if end else None)

        elif kind in ("key", "press"):
            keys = s.get("keys") or [s.get("key")]
            if not isinstance(keys, list):
                raise _error(f"step {idx}'s keys are not a list")
            event = dict(base_event, kind="key", keys=[_text(k, f"step {idx}'s key") for k in keys if k])
            if element:
                event["element"] = element

        else:
            ignored += 1
            continue

        if current_url:
            event["url"] = current_url
        if expect:
            event["expect"] = expect
        events.append(event)

    return {
        "title": title,
        "description": description,
        "screen": screen,
        "events": events,
        "skipped": skipped,
        "ignored": ignored,
        "source_steps": len(raw_steps),
    }
