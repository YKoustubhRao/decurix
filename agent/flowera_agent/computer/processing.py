import math


# ============================================================
# Raw input -> the actions a person meant
#
# A human demonstration arrives as raw device events - button down, button
# up, key down, key up - and a replay needs what the person did: a click, a
# double-click, a drag, "typed Acme Ltd", "pressed ctrl+s". These are
# openadapt-capture's merge rules (processing.py), with its thresholds:
#
# - one button's down and up within DRAG_PIXELS is a click; two clicks within
#   DOUBLE_CLICK_SECONDS and DOUBLE_CLICK_PIXELS are a double-click; farther
#   apart it is a drag
# - Control, Alt or Super held with one key is a shortcut, and so is Shift
#   with a key that types nothing (shift+Tab); Shift with a character is just
#   typing
# - wheel clicks in one direction in a row are one scroll
#
# and two of Flowera's own, because a replay types a field's text in one go
# rather than key by key: characters typed in a row are one `type` however
# long the pauses between them, and BackSpace inside that run edits the text
# instead of being replayed as a key press.
#
# Pure functions over plain dicts, so every rule is tested without a screen
# (tests/test_recordings.py). demonstration.py turns X input into the events.
# ============================================================

DOUBLE_CLICK_SECONDS = 0.5
DOUBLE_CLICK_PIXELS = 5
DRAG_PIXELS = 10
SCROLL_GAP_SECONDS = 0.6
REPEAT_KEY_SECONDS = 1.0

SCROLL_BUTTONS = {4: "up", 5: "down", 6: "left", 7: "right"}
CLICK_KINDS = {1: "click", 2: "middle_click", 3: "right_click"}

MODIFIERS = {
    "Shift_L": "shift", "Shift_R": "shift",
    "Control_L": "ctrl", "Control_R": "ctrl",
    "Alt_L": "alt", "Alt_R": "alt", "Meta_L": "alt", "Meta_R": "alt",
    "Super_L": "super", "Super_R": "super", "Hyper_L": "super", "Hyper_R": "super",
}
MODIFIER_ORDER = {"ctrl": 0, "alt": 1, "shift": 2, "super": 3}

# Keys that change how other keys type, and nothing a replay needs on its own.
LOCKS = {"Caps_Lock", "Num_Lock", "Scroll_Lock", "ISO_Level3_Shift", "Mode_switch"}

# Carried from the raw event that starts an action onto the action.
CARRIED = ("frame",)


def _distance(a, b):
    return math.hypot(a[0] - b[0], a[1] - b[1])


def merge(raw):
    """
    Turn raw events into actions, in order.

    Raw events are {"type": "press"|"release", "t", "button", "x", "y"} and
    {"type": "key_down"|"key_up", "t", "key", "char"}: key is the X keysym
    name ("a", "Return", "Shift_L"), char what it typed or None. Actions are
    {"kind", "t", "t_end", ...} with point/end, text, keys or
    direction/amount by kind.
    """

    actions = []
    typing = None
    pressed = {}
    held_keys = set()
    held_modifiers = {}

    def start(kind, event, **fields):
        action = {key: event[key] for key in CARRIED if key in event}
        action.update(kind=kind, t=event["t"], t_end=event["t"], **fields)
        actions.append(action)
        return action

    for event in raw:
        kind = event.get("type")
        t = event["t"]

        if kind == "press":
            button = event.get("button")

            if button in SCROLL_BUTTONS:
                direction = SCROLL_BUTTONS[button]
                last = actions[-1] if actions else None
                if (last is not None and last["kind"] == "scroll" and last["direction"] == direction
                        and t - last["t_end"] <= SCROLL_GAP_SECONDS):
                    last["amount"] += 1
                    last["t_end"] = t
                else:
                    typing = None
                    start("scroll", event, point=[event["x"], event["y"]], direction=direction, amount=1)
                continue

            typing = None
            pressed[button] = event

        elif kind == "release":
            button = event.get("button")
            down = pressed.pop(button, None)

            if down is None or button in SCROLL_BUTTONS:
                continue

            origin = [down["x"], down["y"]]
            end = [event["x"], event["y"]]

            if _distance(origin, end) > DRAG_PIXELS:
                start("drag", down, point=origin, end=end)["t_end"] = t
                continue

            click = CLICK_KINDS.get(button, "click")
            last = actions[-1] if actions else None

            if (click == "click" and last is not None and last["kind"] == "click"
                    and down["t"] - last["t_end"] <= DOUBLE_CLICK_SECONDS
                    and _distance(last["point"], origin) <= DOUBLE_CLICK_PIXELS):
                last["kind"] = "double_click"
                last["t_end"] = t
                continue

            start(click, down, point=origin)["t_end"] = t

        elif kind == "key_down":
            key = event.get("key") or ""
            repeat = key in held_keys
            held_keys.add(key)

            if key in MODIFIERS:
                held_modifiers[key] = MODIFIERS[key]
                continue

            if key in LOCKS or (repeat and not event.get("char") and key != "BackSpace"
                                and not (actions and actions[-1]["kind"] == "key")):
                continue

            modifiers = set(held_modifiers.values())
            char = event.get("char")

            if (modifiers - {"shift"}) or ("shift" in modifiers and not char):
                typing = None
                combo = "+".join(sorted(modifiers, key=MODIFIER_ORDER.get) + [key])
                start("key", event, keys=[combo])
                continue

            if char:
                if typing is None:
                    typing = start("type", event, text="")
                typing["text"] += char
                typing["t_end"] = t
                continue

            if key == "BackSpace" and typing is not None and typing["text"]:
                typing["text"] = typing["text"][:-1]
                typing["t_end"] = t
                continue

            typing = None
            last = actions[-1] if actions else None

            if (last is not None and last["kind"] == "key" and last["keys"][-1] == key
                    and t - last["t_end"] <= REPEAT_KEY_SECONDS):
                last["keys"].append(key)
                last["t_end"] = t
            else:
                start("key", event, keys=[key])

        elif kind == "key_up":
            key = event.get("key") or ""
            held_keys.discard(key)
            held_modifiers.pop(key, None)

    # Typed and then deleted again: nothing happened.
    return [a for a in actions if not (a["kind"] == "type" and not a["text"])]


def idle(raw):
    """Whether every button and key in `raw` has been let go again."""

    buttons = set()
    keys = set()

    for event in raw:
        kind = event.get("type")
        if kind == "press" and event.get("button") not in SCROLL_BUTTONS:
            buttons.add(event.get("button"))
        elif kind == "release":
            buttons.discard(event.get("button"))
        elif kind == "key_down":
            keys.add(event.get("key"))
        elif kind == "key_up":
            keys.discard(event.get("key"))

    return not buttons and not keys
