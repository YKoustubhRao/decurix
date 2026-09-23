"""
Multi-Application Workflow Demonstration Ingest.

Converts multi-app demonstration logs and task recordings (spanning Mail,
Excel, PDF viewers, ERPs, and web portals) into Decurix native recordings.
"""

from decurix_agent.computer import privacy


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
    """
    if not isinstance(data, dict):
        raise ValueError("Workflow demo data must be a dictionary")

    raw_steps = data.get("steps") or data.get("actions") or []
    if not isinstance(raw_steps, list):
        raise ValueError("Workflow demo must contain a list of steps")

    title = data.get("title") or data.get("workflow") or "Enterprise Workflow Demonstration"
    description = data.get("description") or f"Demonstration of {title}"
    screen = data.get("screen") or [1280, 800]

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

        kind = kind.lower().replace("-", "_")
        target = s.get("target") or s.get("element")
        files = s.get("files") or s.get("paths")

        # Map target to element dict
        element = {}
        if isinstance(target, dict):
            element = {
                "role": target.get("role", "button"),
                "name": target.get("name", ""),
                "source": target.get("source", "cdp" if "http" in current_url else "atspi"),
            }
        elif isinstance(target, str):
            if kind == "open":
                pass
            else:
                element = {
                    "role": "control",
                    "name": target,
                    "source": "cdp" if "http" in current_url else "atspi",
                }

        base_event = {
            "actor": "human",
            "outcome": "done",
            "imported": {"step": idx, "type": kind},
        }

        if kind in ("open", "navigate", "launch"):
            open_target = s.get("target") or s.get("url") or s.get("app")
            if not open_target:
                skipped.append({"step": idx, "why": "Open step has no target"})
                continue
            if "://" in str(open_target) or str(open_target).startswith("www."):
                current_url = str(open_target)
            event = dict(base_event, kind="open", target=open_target, new_tab=bool(s.get("new_tab", False)))
            if current_url:
                event["url"] = current_url
                event["expect"] = {"url": current_url}
            events.append(event)

        elif kind in ("click", "double_click", "right_click"):
            event = dict(base_event, kind=kind, element=element)
            if s.get("point"):
                event["point"] = list(s["point"])
            if current_url:
                event["url"] = current_url
            events.append(event)

        elif kind in ("type", "fill", "input", "change"):
            text = s.get("text") if "text" in s else s.get("value", "")
            event = dict(base_event, kind="type", element=element, text=str(text))
            if current_url:
                event["url"] = current_url
            if element.get("name"):
                event["expect"] = {"values": [{"role": element.get("role", "textbox"), "name": element["name"], "value": str(text)}]}
            events.append(event)

        elif kind == "upload":
            if isinstance(files, str):
                files = [files]
            event = dict(base_event, kind="upload", element=element, files=files or [])
            if current_url:
                event["url"] = current_url
            events.append(event)

        elif kind in ("select", "dropdown"):
            option = s.get("option") or s.get("value")
            event = dict(base_event, kind="select", element=element, option=str(option or ""))
            if current_url:
                event["url"] = current_url
            events.append(event)

        elif kind == "scroll":
            direction = s.get("direction", "down")
            amount = s.get("amount", 3)
            event = dict(base_event, kind="scroll", direction=direction, amount=amount)
            if element:
                event["element"] = element
            if current_url:
                event["url"] = current_url
            events.append(event)

        elif kind == "drag":
            end = s.get("end") or s.get("drop_point")
            event = dict(base_event, kind="drag", element=element, end=list(end) if end else None)
            if current_url:
                event["url"] = current_url
            events.append(event)

        elif kind in ("key", "press"):
            keys = s.get("keys") or [s.get("key")]
            event = dict(base_event, kind="key", keys=[str(k) for k in keys if k])
            if element:
                event["element"] = element
            if current_url:
                event["url"] = current_url
            events.append(event)

        else:
            ignored += 1

    return {
        "title": title,
        "description": description,
        "screen": screen,
        "events": events,
        "skipped": skipped,
        "ignored": ignored,
        "source_steps": len(raw_steps),
    }

