import ast
import re

from decurix_agent.computer import ingest, privacy


# ============================================================
# Ingest: what other agents did, read as a recording
#
# ingest.py reads a browser flow a PERSON recorded. This reads what an AGENT did,
# in the two forms the open-source web-agent projects leave behind:
#
#   BrowserGym / AgentLab   a step is an action in BrowserGym's action language
#                           (`click('19')`, `fill('14', 'text')`) and the
#                           observation it was chosen from, whose accessibility
#                           tree is text: `[19] button 'submit'`
#   OpenHands               an event stream: `browse_interactive` actions
#                           carrying the same action language, each answered by a
#                           browse observation whose extras hold the tree as a
#                           Chrome accessibility object
#
# Both name a control by a `bid` - a number BrowserGym stamps on the page's
# elements, which means nothing to anyone else. What ties it back to something
# a replay can find again is the tree of the observation the action was chosen
# from: `[19] button 'submit'` is a button named submit. That is why a step
# whose tree is missing is kept with no target rather than guessed at - and
# OpenHands strips `axtree_object` when it saves a trajectory
# (`DELETE_FROM_TRAJECTORY_EXTRAS`), so a stripped one says so.
#
# Two things the real trees showed (MiniWoB++, through BrowserGym 0.14.3):
#
# - a great many controls have NO name: `textbox ''` twice in a login form. A
#   role and a name cannot tell them apart, and a replay does not guess, so the
#   target carries `nth` - its place among controls with the same role and name -
#   and a `hint`, the label text beside it ("Username"), which is what a person
#   would call it and what tells the privacy layer that "Password" is a secret.
# - a password field is a plain textbox to the accessibility tree
#   (`editable: plaintext`, nothing marks it protected), so the label is the only
#   signal, and it is used.
#
# The action strings are read with `ast`, never evaluated: a trajectory is data
# from somewhere else and must not be able to run anything here.
# ============================================================

# The grammar of BrowserGym's flatten_axtree_to_str (browsergym/utils/obs.py):
#
#   <tabs>[bid] role 'name' value='v', flag, key='v', key2=True
#
# - the tabs are the depth; `[bid]` is missing on nodes BrowserGym did not number
# - the name is Python's repr(), so quoted with either kind of quote, and absent
#   for an unnamed `generic`
# - `value='v'` follows the name after a SPACE; every other property follows a
#   comma, and `focused` / `required` / `atomic` are bare flags when true
#
# A first version of this took the properties to start at a comma and lost every
# control that had a value - a textbox once it was filled in, and every dropdown.
QUOTED = r"'(?:[^'\\]|\\.)*'|\"(?:[^\"\\]|\\.)*\""
TREE_LINE = re.compile(
    r"^(?P<indent>\t*)(?:\[(?P<bid>[^\]]+)\] )?(?P<role>[^\s']+)"
    rf"(?: (?P<name>{QUOTED}))?"
    rf"(?: value=(?P<value>{QUOTED}|[^,]+))?"
    r"(?P<props>(?:, .*)?)$"
)
PROP = re.compile(rf"(\w+)=({QUOTED}|[^,]+)|(\w+)")

TEXT_ROLES = {"StaticText", "LabelText"}
MAX_SKIPPED_LISTED = 100

# BrowserGym key names -> the names a replay presses.
MODIFIER_NAMES = {"Control": "ctrl", "ControlOrMeta": "ctrl", "Alt": "alt", "Shift": "shift", "Meta": "super"}


# ============================================================
# The accessibility tree
# ============================================================

def _unquote(literal):
    try:
        return ast.literal_eval(literal)
    except (ValueError, SyntaxError):
        return literal.strip("'\"")


def parse_axtree_text(text):
    """
    The nodes of BrowserGym's accessibility tree text, in order: [{"index",
    "depth", "bid", "role", "name", "props"}]. A line that is not one (a blank
    line, an unknown shape) is left out.
    """

    nodes = []

    for line in (text or "").splitlines():
        match = TREE_LINE.match(line)
        if not match:
            continue

        props = {}
        if match.group("value") is not None:
            props["value"] = _unquote(match.group("value")) if match.group("value")[:1] in "'\"" else match.group("value")
        for key, value, flag in PROP.findall(match.group("props") or ""):
            if flag:
                props[flag] = True
            else:
                props[key] = _unquote(value) if value[:1] in "'\"" else value.strip()

        nodes.append({
            "index": len(nodes),
            "depth": len(match.group("indent")),
            "bid": match.group("bid"),
            "role": match.group("role"),
            "name": _unquote(match.group("name")) if match.group("name") else "",
            "props": props,
        })

    return nodes


def nodes_from_object(ax):
    """
    The same nodes from a Chrome accessibility object, the form OpenHands keeps
    in a browse observation's extras: {"nodes": [{"nodeId", "role": {"value"},
    "name": {"value"}, "childIds", "browsergym_id"}]}, walked in tree order.
    """

    raw = {str(n.get("nodeId")): n for n in (ax or {}).get("nodes") or [] if isinstance(n, dict)}
    roots = [nid for nid, n in raw.items() if str(n.get("parentId")) not in raw]
    nodes = []

    def visit(nid, depth):
        node = raw.get(nid)
        if node is None:
            return

        role = ((node.get("role") or {}).get("value")) or ""
        name = ((node.get("name") or {}).get("value")) or ""
        props = {p.get("name"): (p.get("value") or {}).get("value") for p in node.get("properties") or []
                 if isinstance(p, dict)}
        # A field's value is a key of the node itself, not one of its properties.
        if isinstance((node.get("value") or {}).get("value"), str):
            props["value"] = node["value"]["value"]

        if not node.get("ignored") or node.get("browsergym_id"):
            nodes.append({"index": len(nodes), "depth": depth, "bid": node.get("browsergym_id"),
                          "role": str(role), "name": str(name).strip(), "props": props})

        for child in node.get("childIds") or []:
            visit(str(child), depth + 1)

    for root in roots:
        visit(root, 0)

    return nodes


def _hint(nodes, at):
    """
    What a control with no name is called: the label beside it. That is the text of
    the nearest earlier SIBLING that is a label (or bare text) - not a descendant of
    an earlier sibling, which for a field is the value of the one before it.
    """

    depth = nodes[at]["depth"]
    i = at - 1

    while i >= 0 and nodes[i]["depth"] >= depth:
        node = nodes[i]

        if node["depth"] == depth and node["role"] in TEXT_ROLES:
            if node["name"]:
                return node["name"]
            # A LabelText's words are its StaticText child, which follows it.
            for below in nodes[i + 1:at]:
                if below["depth"] <= depth:
                    break
                if below["role"] == "StaticText" and below["name"]:
                    return below["name"]

        i -= 1

    return ""


def identify(nodes, bid):
    """
    What a `bid` stands for as a replay can find it: {"role", "name", "nth", "hint",
    "source"} or None. `nth` is set only when other controls share its role and
    name (the two `textbox ''` of a login form) and counts from 1 in tree order.
    """

    if not nodes or bid is None:
        return None

    at = next((i for i, n in enumerate(nodes) if str(n["bid"]) == str(bid)), None)
    if at is None:
        return None

    node = nodes[at]
    same = [n for n in nodes if n["bid"] and n["role"] == node["role"] and n["name"] == node["name"]]

    element = {"role": node["role"], "name": node["name"], "source": "cdp"}
    if len(same) > 1:
        element["nth"] = same.index(node) + 1
    if not node["name"]:
        hint = _hint(nodes, at)
        if hint:
            element["hint"] = hint

    return element


def _appeared(before, after):
    """(role, name) of what the tree shows after an action that it did not before."""

    seen = {n["bid"] for n in before or [] if n["bid"]}

    return [{"role": n["role"], "name": n["name"]} for n in after or []
            if n["bid"] and n["bid"] not in seen and n["name"]]


# ============================================================
# BrowserGym's action language
# ============================================================

def parse_actions(text):
    """
    [(function, args, kwargs)] for the statements of an action string. Each
    must be a call of a plain name with literal arguments - anything else is
    refused, since this text came from somewhere else.
    """

    source = re.sub(r"^```\w*\s*|\s*```$", "", (text or "").strip())
    if not source:
        return []

    try:
        tree = ast.parse(source, mode="exec")
    except SyntaxError as exc:
        raise ingest.IngestError(f"not an action BrowserGym can read: {exc.msg}")

    actions = []

    for statement in tree.body:
        call = statement.value if isinstance(statement, ast.Expr) else None

        if not isinstance(call, ast.Call) or not isinstance(call.func, ast.Name):
            raise ingest.IngestError("an action that is not a call of one of BrowserGym's functions")

        try:
            args = [ast.literal_eval(a) for a in call.args]
            kwargs = {k.arg: ast.literal_eval(k.value) for k in call.keywords if k.arg}
        except (ValueError, SyntaxError):
            raise ingest.IngestError(f"{call.func.id}() has an argument that is not a plain value")

        actions.append((call.func.id, args, kwargs))

    return actions


def _keys(combo):
    parts = [p for p in str(combo).split("+") if p]
    if not parts:
        return None

    *mods, key = parts
    named = ingest.KEYS.get(key, key)
    if len(named) == 1 and named.isalpha():
        named = named.lower()

    return "+".join([MODIFIER_NAMES.get(m, m.lower()) for m in mods] + [named])


def _scroll(dx, dy):
    if abs(dy) >= abs(dx) and dy:
        return ("down" if dy > 0 else "up"), max(1, round(abs(dy) / 100))
    if dx:
        return ("right" if dx > 0 else "left"), max(1, round(abs(dx) / 100))
    return None, None


# What each function becomes: the kind of step, and what is lost on the way.
NOTES = {
    "focus": "focusing a control is done by clicking it",
    "check": "made sure it was checked by clicking it; a replay clicks whether or not it already is",
    "uncheck": "made sure it was unchecked by clicking it; a replay clicks whether or not it already is",
}

IGNORED = {"noop", "send_msg_to_user", "report_infeasible", "go_forward"}

UNSUPPORTED = {
    "hover": "a replay has no step for hovering",
    "mouse_move": "a replay has no step for moving the pointer",
    "mouse_down": "a replay has no step for holding a button",
    "mouse_up": "a replay has no step for holding a button",
    "drag_and_drop": "a replay cannot drag one control to another",
    "mouse_drag_and_drop": "a replay cannot drag between two positions it was not shown",
    "tab_focus": "a replay does not switch tabs",
    "tab_close": "a replay does not close tabs",
    "new_tab": "a replay does not open tabs",
    "upload_file": "a replay cannot choose a file",
    "mouse_upload_file": "a replay cannot choose a file",
    "clear": "a replay cannot empty a field on its own",
    "scroll_at": "a replay scrolls where it is, not at a position",
    "keyboard_down": "a replay cannot hold a key",
    "keyboard_up": "a replay cannot hold a key",
}


def _judged(element):
    """A field as the privacy layer judges it: by its own label when it has no name."""

    return {"role": (element or {}).get("role") or "textbox",
            "name": (element or {}).get("name") or (element or {}).get("hint") or ""}


def _step_event(name, args, kwargs, nodes, note_sink):
    """
    One action as an event of a recording, or (None, why not). `nodes` is the tree
    of the observation it was chosen from, or None when the trajectory kept none.
    """

    def element_for(bid):
        return identify(nodes, bid) if nodes else None

    def targeted(kind, bid, **fields):
        element = element_for(bid)
        event = {"kind": kind, "element": dict(element or {}), "imported": {"bid": str(bid)}}
        event.update(fields)
        if element is None:
            event["imported"]["why"] = ("no accessibility tree came with this step, so the control it names cannot "
                                        "be found again" if not nodes else f"the tree has no control numbered {bid}")
        return event

    if name in ("click", "dblclick"):
        bid = args[0] if args else kwargs.get("bid")
        if kwargs.get("modifiers") or (len(args) > 2 and args[2]):
            return None, "a click with a modifier key held is not one a replay takes"

        button = args[1] if len(args) > 1 else kwargs.get("button", "left")
        kind = "double_click" if name == "dblclick" else {"right": "right_click", "middle": "middle_click"}.get(button, "click")
        return targeted(kind, bid), None

    if name in ("focus", "check", "uncheck"):
        event = targeted("click", args[0] if args else kwargs.get("bid"))
        event["imported"] = dict(event.get("imported") or {}, note=NOTES[name])
        note_sink.append(NOTES[name])
        return event, None

    if name == "fill":
        bid = args[0] if args else kwargs.get("bid")
        value = args[1] if len(args) > 1 else kwargs.get("value")
        if not isinstance(value, str):
            return None, "fill() has no text to type"

        event = targeted("type", bid)
        text, why = ingest._typed(_judged(event["element"]), value)
        event["text"] = text
        if why:
            event["withheld"] = {"text": why}
            if privacy.secret_field(_judged(event["element"])):
                event["element"]["secret"] = why
        elif event["element"].get("role"):
            event["expect"] = {"values": [{"role": event["element"]["role"], "name": event["element"]["name"],
                                           "value": text}]}
        return event, None

    if name == "select_option":
        bid = args[0] if args else kwargs.get("bid")
        options = args[1] if len(args) > 1 else kwargs.get("options")
        if isinstance(options, (list, tuple)):
            if len(options) != 1:
                return None, "choosing several options at once is not a step a replay takes"
            options = options[0]
        if not isinstance(options, str):
            return None, "select_option() has no option to choose"

        return targeted("select", bid, option=options), None

    if name == "press":
        bid = args[0] if args else kwargs.get("bid")
        combo = _keys(args[1] if len(args) > 1 else kwargs.get("key_comb"))
        return (targeted("key", bid, keys=[combo]), None) if combo else (None, "press() has no key")

    if name == "keyboard_press":
        combo = _keys(args[0] if args else kwargs.get("key"))
        return ({"kind": "key", "keys": [combo], "element": {}}, None) if combo else (None, "no key")

    if name == "keyboard_type":
        text = args[0] if args else kwargs.get("text")
        if not isinstance(text, str):
            return None, "keyboard_type() has no text"
        kept, why = ingest._typed({"role": "textbox", "name": ""}, text)
        event = {"kind": "type", "element": {}, "text": kept}
        if why:
            event["withheld"] = {"text": why}
        return event, None

    if name == "goto":
        target = args[0] if args else kwargs.get("url")
        if not isinstance(target, str) or not target:
            return None, "goto() has no address"
        reduced = privacy.reduce_url(target)
        return {"kind": "open", "target": reduced, "new_tab": False, "url": reduced, "expect": {"url": reduced}}, None

    if name == "go_back":
        return {"kind": "key", "keys": ["alt+Left"], "element": {}}, None

    if name == "scroll":
        numbers = [v for v in list(args) + [kwargs.get("delta_x"), kwargs.get("delta_y")][len(args):] if v is not None]
        dx, dy = (list(numbers) + [0, 0])[:2]
        direction, amount = _scroll(dx, dy) if all(isinstance(v, (int, float)) for v in (dx, dy)) else (None, None)
        return ({"kind": "scroll", "direction": direction, "amount": amount, "element": {}}, None) if direction \
            else (None, "a scroll of nothing")

    if name in ("mouse_click", "mouse_dblclick"):
        if len(args) < 2 or not all(isinstance(v, (int, float)) for v in args[:2]):
            return None, f"{name}() has no position"
        button = args[2] if len(args) > 2 else kwargs.get("button", "left")
        kind = "double_click" if name == "mouse_dblclick" else {"right": "right_click"}.get(button, "click")
        return {"kind": kind, "point": [int(args[0]), int(args[1])], "element": {}}, None

    if name in IGNORED:
        return "ignored", None

    return None, UNSUPPORTED.get(name, f"{name}() is not one of the actions a replay takes")


def _finish(events, skipped, ignored, title, source_steps, warnings):
    title = privacy.withhold_spoken_secrets(title)

    for event in events:
        event.setdefault("actor", "agent")
        event.setdefault("outcome", "done")
        event["summary"] = ingest._summary(event)

        expect = event.get("expect")
        if expect and not any(expect.values()):
            event.pop("expect")

    return {"title": title, "screen": None, "events": events, "skipped": skipped, "ignored": ignored,
            "source_steps": source_steps, "warnings": warnings}


MASK = set("\u2022*\u25cf\u00b7")


def _masked(node):
    """A field whose value is bullets: the tree's own way of saying it is a password."""

    value = (node or {}).get("props", {}).get("value")
    return isinstance(value, str) and bool(value) and set(value) <= MASK


def _after(events, before, after):
    """
    What the tree after a step adds to the events it made: a typed field whose
    value came back as bullets was a password field whatever its label said
    (and its text is never kept), and what appeared is what the step should leave.
    """

    if after is None:
        return

    for event in events:
        bid = (event.get("imported") or {}).get("bid")
        node = next((n for n in after if bid is not None and str(n["bid"]) == str(bid)), None)

        if event["kind"] == "type" and _masked(node):
            event["text"] = None
            event["withheld"] = {"text": "password field"}
            event["element"]["secret"] = "password field"
            event.pop("expect", None)
        elif not event.get("expect") and before is not None:
            appeared = _appeared(before, after)
            if appeared:
                event["expect"] = {"appeared": appeared}


# ============================================================
# BrowserGym / AgentLab
# ============================================================

def _step_nodes(step):
    """The tree a step was chosen from, whichever way the trajectory kept it."""

    obs = step.get("obs") if isinstance(step.get("obs"), dict) else {}

    text = step.get("axtree_txt") or obs.get("axtree_txt")
    if isinstance(text, str) and text.strip():
        return parse_axtree_text(text)

    ax = step.get("axtree_object") or obs.get("axtree_object")
    if isinstance(ax, dict):
        return nodes_from_object(ax)

    return None


def from_browsergym(data):
    """
    A BrowserGym / AgentLab trajectory: {"goal", "steps": [{"action", "axtree_txt" |
    "axtree_object" | "obs": {...}}], "final": {...}} or the bare list of steps.
    `final` is the tree after the last action, when there is one.
    """

    steps = data.get("steps") if isinstance(data, dict) else data
    if not isinstance(steps, list) or not steps:
        raise ingest.IngestError("that trajectory has no steps in it")
    if len(steps) > ingest.MAX_STEPS:
        raise ingest.IngestError(f"that trajectory has more than {ingest.MAX_STEPS} steps")

    trees = [_step_nodes(s) if isinstance(s, dict) else None for s in steps]
    if isinstance(data, dict) and isinstance(data.get("final"), dict):
        trees.append(_step_nodes(data["final"]))
    else:
        trees.append(None)

    events, skipped, ignored, warnings = [], [], 0, []

    for i, step in enumerate(steps):
        if not isinstance(step, dict) or not isinstance(step.get("action"), str):
            skipped.append({"step": i + 1, "type": "step", "why": "it has no action"})
            continue

        before, after = trees[i], trees[i + 1]

        try:
            actions = parse_actions(step["action"])
        except ingest.IngestError as exc:
            skipped.append({"step": i + 1, "type": "action", "why": str(exc)})
            continue

        made = []
        for name, args, kwargs in actions:
            event, why = _step_event(name, args, kwargs, before, warnings)

            if event == "ignored":
                ignored += 1
            elif event is None:
                skipped.append({"step": i + 1, "type": name, "why": why})
            else:
                event["imported"] = dict(event.get("imported") or {}, step=i + 1, type=name)
                if isinstance(step.get("url"), str):
                    event["url"] = privacy.reduce_url(step["url"])
                made.append(event)

        _after(made, before, after)
        events.extend(made)

    title = str(data.get("goal") or "").strip()[:200] if isinstance(data, dict) else ""
    if all(t is None for t in trees[:-1]):
        warnings.append("no step came with an accessibility tree, so no control can be found again")

    return _finish(events, skipped, ignored, title, len(steps), sorted(set(warnings)))


# ============================================================
# OpenHands
# ============================================================

def _events(data):
    if isinstance(data, list):
        return data
    if isinstance(data, dict):
        for key in ("history", "trajectory", "events"):
            if isinstance(data.get(key), list):
                return data[key]
    return None


def from_openhands(data):
    """
    An OpenHands trajectory: its events, each an action ({"action", "args"}) or an
    observation ({"observation", "content", "extras"}). Only what happened in the
    browser is a step; commands, file edits and the rest are listed as skipped.
    """

    events_in = _events(data)
    if not events_in:
        raise ingest.IngestError("that trajectory has no events in it")
    if len(events_in) > ingest.MAX_STEPS * 4:
        raise ingest.IngestError("that trajectory is too long to import")

    events, skipped, ignored, warnings = [], [], 0, []
    nodes, url, title, hidden = None, None, "", 0
    pending = []          # the events made since the last observation, and the tree they were chosen from

    def skip(index, kind, why):
        if len(skipped) < MAX_SKIPPED_LISTED:
            skipped.append({"step": index + 1, "type": kind, "why": why})

    for index, event in enumerate(events_in):
        if not isinstance(event, dict):
            continue

        if "observation" in event:
            if event.get("observation") == "browse":
                extras = event.get("extras") or {}
                url = extras.get("url") or url
                if isinstance(extras.get("axtree_object"), dict) and extras["axtree_object"].get("nodes"):
                    fresh = nodes_from_object(extras["axtree_object"])
                    _after(pending, nodes, fresh)
                    nodes = fresh
                else:
                    nodes = None
                    hidden += 1
                pending = []
            continue

        kind, args = event.get("action"), event.get("args") or {}

        if kind == "message" and event.get("source") == "user":
            words = str(args.get("content") or "").strip()
            if words and not title:
                title = words.splitlines()[0][:200]
            continue

        if kind == "browse":
            target = args.get("url")
            if isinstance(target, str) and target:
                reduced = privacy.reduce_url(target)
                events.append({"kind": "open", "target": reduced, "new_tab": False, "url": reduced,
                               "expect": {"url": reduced}, "imported": {"step": index + 1, "type": "browse"}})
            continue

        if kind == "browse_interactive":
            try:
                actions = parse_actions(args.get("browser_actions"))
            except ingest.IngestError as exc:
                skip(index, "browse_interactive", str(exc))
                continue

            for name, a, kw in actions:
                made, why = _step_event(name, a, kw, nodes, warnings)
                if made == "ignored":
                    ignored += 1
                elif made is None:
                    skip(index, name, why)
                else:
                    made["imported"] = dict(made.get("imported") or {}, step=index + 1, type=name)
                    events.append(made)
                    pending.append(made)
            continue

        if kind in ("finish", "think", "system", "recall", "change_agent_state", "null"):
            ignored += 1
        elif kind in ("run", "run_ipython", "read", "write", "edit", "delegate", "mcp"):
            skip(index, kind, f"a {kind} is not a step on a screen")
        elif kind:
            skip(index, kind, f"{kind} is not a step a replay takes")

    if hidden and any((e.get("imported") or {}).get("bid") for e in events):
        warnings.append("OpenHands removes the accessibility tree when it saves a trajectory, so the controls its "
                        "browser actions name (by number) cannot be found again - those steps stop a replay")

    return _finish(events, skipped, ignored, title, len(events_in), sorted(set(warnings)))
