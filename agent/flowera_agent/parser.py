import re


# ============================================================
# Agent parser
# ============================================================

# The contract as written: an uppercase marker, which may appear anywhere
# in the response because small models sometimes put a word or two in
# front of it. The trailing [*_`]* swallows the closing half of a
# marker the model bolded ("**DONE:**") or a backtick it opened the
# command with, neither of which belongs in the extracted value; it
# cannot cross a newline, so a DONE whose answer is a "* bullet" list
# keeps its bullet.
_MARKER_RE = re.compile(r"\b(COMMAND|DONE|HELP):[*_`]*")

# The same three markers as the model actually tends to mistype them:
# lowercase or title case ("Help:"), wrapped in markdown emphasis
# ("**DONE:**"), or spaced off the colon ("HELP :"). Anchored to the start
# of a line, which the strict pattern above deliberately is not: an
# uppercase COMMAND: mid-sentence is unambiguous, but a lowercase "help:"
# is an ordinary English word followed by a colon, and treating one inside
# a real answer as a control signal would truncate that answer at it.
_LOOSE_MARKER_RE = re.compile(
    r"^[\s*_`#>-]*(COMMAND|DONE|HELP)\s*:[*_`]*",
    re.IGNORECASE | re.MULTILINE,
)

_ACTIONS = {
    "command": "command",
    "done": "done",
    "help": "help",
}


def _matches(response):
    """
    Find the control signals in a response, strictly if possible.

    Two passes rather than one permissive pattern, so the loose spellings
    are only ever a recovery path for a response that has no properly
    formed marker at all - never a way for the word "help:" inside a
    perfectly good DONE answer to hijack it.
    """

    strict = list(_MARKER_RE.finditer(response))

    if strict:
        return strict

    return list(_LOOSE_MARKER_RE.finditer(response))


def parse_agent_response(response):
    """
    Parse the agent's control signal.

    A response is bounded from its control signal to the START of the
    NEXT control signal (of any kind), not to the end of the string. A
    model that trails off after its real answer - repeating the marker,
    echoing the prompt's own example, or appending a stray DONE/HELP -
    would otherwise have all of that garbage folded into the extracted
    command/message, which for a COMMAND gets executed as shell input.
    Seen live on the model this ships with: "how much disk space is
    free?" answered with a correct `COMMAND: df -h ...` immediately
    followed by an invented `DONE: 10G Available` for output no command
    had produced yet.
    """

    response = response.strip()

    matches = _matches(response)

    if not matches:
        return ("invalid", response)

    first = matches[0]
    end = matches[1].start() if len(matches) > 1 else len(response)

    action = _ACTIONS[first.group(1).strip().lower()]
    value = response[first.end():end].strip()

    return (action, value)
