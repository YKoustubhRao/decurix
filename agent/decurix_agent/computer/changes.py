"""
What differs between two looks at the screen, said for an engine and kept for a recording.

Split out of core.py (follow-up O-10): core observes, acts and verifies; this module holds the comparison of two
observations (compare), the short paragraph an engine is told (summarize, text_change) and the same as data a
recording keeps and replay mines (expectations). Nothing here looks at the screen: both observations are given.
"""

from decurix_agent.computer import privacy, screen, volatility
from decurix_agent.computer import elements as el


def text_change(old, new, spans=3, context=18):
    """
    What an edit did to a field's text, for an engine to check: the changed
    spans with a little of what stands around them. A whole-value quote cut
    at 40 characters showed the first line of a document and hid every change
    after it (found by the GUI benchmark: a word replaced in the third line of
    a file was invisible, and the engine reported every one replaced).
    """

    old, new = old or "", new or ""
    if len(old) <= 40 and len(new) <= 40:
        return f"{el._quote(old, 40)} -> {el._quote(new, 40)}"

    import difflib
    changes = [op for op in difflib.SequenceMatcher(None, old, new, autojunk=False).get_opcodes() if op[0] != "equal"]
    if not changes:
        return "unchanged"

    def quoted(text, i, j):
        start, end = max(0, i - context), min(len(text), j + context)
        piece = " ".join(text[start:end].replace("\n", " / ").split())
        return '"' + ("…" if start else "") + piece.replace('"', "'") + ("…" if end < len(text) else "") + '"'

    shown = []
    for tag, i1, i2, j1, j2 in changes[:spans]:
        line = old.count("\n", 0, i1) + 1
        shown.append(f"line {line}: {quoted(old, i1, i2)} -> {quoted(new, j1, j2)}")
    more = f" (+{len(changes) - spans} more changes)" if len(changes) > spans else ""
    return f"changed in {len(changes)} place{'s' if len(changes) != 1 else ''}: " + "; ".join(shown) + more


def compare(before, after):
    """What differs between two observations."""

    old = {e["path"]: e for e in before["elements"]}
    new = {e["path"]: e for e in after["elements"]}

    # A clock is not something an action did: the panel's ticking over from
    # 10:41 to 10:42 would otherwise be reported, and recorded as an expected
    # result, under every action (volatility.py).
    appeared = [e for p, e in new.items() if p not in old and not volatility.only_a_moment(e.get("name"))]
    disappeared = [e for p, e in old.items() if p not in new and not volatility.only_a_moment(e.get("name"))]
    changed = []

    for p, e in new.items():
        prior = old.get(p)
        if prior is None:
            continue
        fields = []
        old_value, new_value = prior.get("value") or "", e.get("value") or ""
        if old_value != new_value and not (volatility.only_a_moment(old_value) and volatility.only_a_moment(new_value)):
            fields.append(("value", old_value, new_value))
        before_states = set(prior.get("states", [])) - {"offscreen"}
        after_states = set(e.get("states", [])) - {"offscreen"}
        if before_states != after_states:
            fields.append(("states", ", ".join(sorted(before_states)) or "-", ", ".join(sorted(after_states)) or "-"))
        if fields:
            changed.append((e, fields))

    old_windows = {w.get("id", w["title"]): w["title"] for w in before["windows"]}
    new_windows = {w.get("id", w["title"]): w["title"] for w in after["windows"]}

    if "frame" in before and isinstance(before["frame"], screen.Frame):
        pixels = screen.changed_fraction(before["frame"], after["frame"])
    else:
        pixels = None

    focused = next((e for e in after["elements"] if "focused" in e.get("states", [])), None)

    return {
        "pixels": pixels,
        "active_before": before.get("active_title"),
        "active_after": after.get("active_title"),
        "windows_opened": sorted(t for i, t in new_windows.items() if i not in old_windows),
        "windows_closed": sorted(t for i, t in old_windows.items() if i not in new_windows),
        "url_before": before.get("page_url"),
        "url_after": after.get("page_url"),
        "appeared": appeared,
        "disappeared": disappeared,
        "changed": changed,
        "focused": focused,
    }


def summarize(change, limit=6):
    """One short paragraph: what an engine needs to know about the result."""

    parts = []

    for title in change["windows_opened"]:
        parts.append(f"window {el._quote(title, 60)} opened")
    for title in change["windows_closed"]:
        parts.append(f"window {el._quote(title, 60)} closed")

    if change["active_before"] != change["active_after"] and not change["windows_opened"]:
        parts.append(f"active window is now {el._quote(change['active_after'] or '(none)', 60)}")

    if change.get("url_after") and change.get("url_before") != change.get("url_after"):
        parts.append(f"page is now {(privacy.reduce_url(change['url_after']) or '')[:100]}")

    for element, fields in change["changed"][:limit]:
        for field, old, new in fields:
            if field == "value" and privacy.secret_field(element):
                parts.append(f"{el.describe(element)} value changed")
            elif field == "value":
                parts.append(f"{el.describe(element)} value " + text_change(old, new))
            else:
                parts.append(f"{el.describe(element)} [{old}] -> [{new}]")

    if change["appeared"]:
        appeared = change["appeared"]
        # What a Save or a Submit answers with is a few words appearing - "Successfully
        # Saved", "Should be a valid date in yyyy-dd-mm format" - usually plain text
        # beside a close button. Listing only the button ("button × (+2 more)") hid the
        # validation errors of a form that never saved, and the engine reported its
        # fields as filled. So messages come first, and a handful of new elements is
        # listed whole; only a new page's hundreds are cut to the controls.
        messages = [e for e in appeared if e["role"] in ("alert", "status", "dialog")]
        texts = [e for e in appeared if e["role"] in ("text", "paragraph", "heading", "label")
                 and (e.get("name") or "").strip()]
        controls = [e for e in appeared if e["role"] in el.INTERACTIVE_ROLES]
        if len(appeared) <= SMALL_CHANGE:
            ordered = messages + texts + [e for e in appeared if e not in messages and e not in texts]
            shown_list = ordered
        elif len(appeared) <= PANEL_CHANGE and change.get("url_before") == change.get("url_after") \
                and not change["windows_opened"]:
            # A panel opened in place (a ribbon tab, a toolbar, a sidebar): every control in it is listed, so the
            # next press can be made by a ref that was shown. Found live (2026-10-16): after Excel's Insert tab,
            # six of 35 new controls were listed "(+29 more)", and the engine guessed a ref from their numbering -
            # it pressed "Insert Pie or Doughnut Chart" for a column chart.
            shown_list = messages + [e for e in texts if e["role"] == "heading"] + controls or appeared[:limit]
        else:
            shown_list = (messages + [e for e in texts if e["role"] == "heading"] + controls)[:limit] or appeared[:limit]
        shown = ", ".join(el.describe(e) for e in shown_list)
        more = len(appeared) - len(shown_list)
        parts.append(f"{len(appeared)} element(s) appeared: {shown}" + (f" (+{more} more)" if more > 0 else ""))

    if change["disappeared"]:
        parts.append(f"{len(change['disappeared'])} element(s) disappeared")

    if change.get("focused") is not None:
        parts.append(f"focus: {el.describe(change['focused'])}")

    if change["pixels"] is not None:
        if change["pixels"] < 0.001 and len(parts) <= 1:
            return "nothing visible changed" + (f"; {parts[0]}" if parts else "") + " - check whether the action reached its target."
        percent = change["pixels"] * 100
        parts.append(("under 1%" if percent < 1 else f"{round(percent)}%") + " of the screen changed")

    return "; ".join(parts) if parts else "nothing visible changed"


# Up to this many new elements are all listed in an action's result.
SMALL_CHANGE = 10
# Up to this many, appearing in place (no new page or window), every new control is listed.
PANEL_CHANGE = 60


# Roles worth checking for on a replay, besides the ones that can be acted on.
NOTABLE_ROLES = ("dialog", "alert", "status", "heading")


def expectations(change, limit=8):
    """
    What an action left on the screen, as data rather than a sentence: the
    recording keeps it with the action, and replay.py mines what to check
    from it. Secret fields, and values past privacy.MAX_TEXT, are left out.
    """

    def brief(element):
        return {
            "role": element["role"],
            "name": privacy.kept_text(element.get("name")),
            "path": privacy.kept_text(element.get("path")),
        }

    def notable(elements):
        return [
            brief(e) for e in elements
            if e.get("name") and not privacy.secret_field(e)
            and (e["role"] in el.INTERACTIVE_ROLES or e["role"] in NOTABLE_ROLES)
        ][:limit]

    values = []
    for element, fields in change["changed"]:
        for field, _, new in fields:
            if field == "value" and not privacy.secret_field(element) and privacy.kept_text(new) is not None:
                values.append(dict(brief(element), value=new))

    url_changed = change.get("url_after") and change.get("url_before") != change.get("url_after")
    window_changed = change["active_before"] != change["active_after"]

    return {
        "url": privacy.reduce_url(change["url_after"]) if url_changed else None,
        "active_window": privacy.kept_text(change["active_after"]) if window_changed else None,
        "windows_opened": [privacy.kept_text(t) for t in change["windows_opened"][:limit]],
        "windows_closed": [privacy.kept_text(t) for t in change["windows_closed"][:limit]],
        "appeared": notable(change["appeared"]),
        "disappeared": notable(change["disappeared"]),
        "values": values[:limit],
        "pixels": round(change["pixels"], 4) if change["pixels"] is not None else None,
    }
