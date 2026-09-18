"""Expands compound/complex instructions into atomic steps before the per-step
codegen loop runs. Always uses Devstral for this regardless of which model is
selected to execute the steps -- planning quality matters more here than
sticking to whatever's cheapest to keep resident."""
import re

import models

PLANNER_MODEL = "devstral"

SYSTEM_PROMPT = """You are a task planner for a browser automation agent. Each resulting step \
will either perform ONE atomic browser action (navigate, click one thing, type into one field, \
download one thing) or ask a question about the current page (e.g. "extract X as JSON", "what \
does Y say") -- you are only splitting up instructions, never answering them or producing the \
data/JSON/answer yourself. You are given a list of instructions, one per line. For each line:
- If it is already a single atomic action or a single question/extraction request, output it \
UNCHANGED, verbatim -- this includes any request that mentions JSON or another data format; \
that is a request to answer that way later, not something for you to construct now.
- If it is compound (multiple actions joined by "and", "then", commas, or otherwise), split it \
into separate instructions in the same order, preserving the original intent and any specific \
values (search terms, names, etc) exactly.
- When splitting "open/click the first N [links/results/items]" that follows a search or \
listing action, make each resulting step say what kind of thing it means (e.g. "open the first \
search result", not the ambiguous "open the first link") -- each step is executed alone, \
without seeing the rest of this list, so it can't infer from context that "link" was short for \
"search result link" and might instead click the first ANY link on the page (nav bar, logo, \
skip-to-content), which is never what's meant.
Output ONE resulting instruction per line, in execution order. No numbering, no blank lines, no \
markdown, no explanation, no JSON, no braces -- only the instruction lines themselves, in plain \
English.

Example:
Input: "go to google.com and search for cats"
Output:
go to google.com
search for cats

Example (already atomic -- passes through unchanged, even though it mentions JSON):
Input: "extract the product price as JSON"
Output:
extract the product price as JSON

Example (disambiguating "links" -> "search results" so each step stands alone):
Input: "search amazon.in for 5090 laptops and open the first 3 links one by one"
Output:
search amazon.in for 5090 laptops
open the first search result
open the second search result
open the third search result"""

# A real decomposition of English instructions should never contain markdown fences,
# bare braces/brackets, or a `"key": value` pair -- those mean the model tried to
# fabricate the requested data/JSON itself instead of just splitting instructions up.
_MALFORMED_LINE_RE = re.compile(r'^(```|[{}\[\]]+,?$|"[^"]+"\s*:\s*)')


def decompose(instructions, on_chunk=None):
    """on_chunk(partial_text), if given, is called after every token so a UI can
    show the expansion happening live. Falls back to the original instructions
    unchanged if the model call fails, returns nothing usable, or produces
    output that looks malformed (see _MALFORMED_LINE_RE)."""
    user = "\n".join(instructions)
    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": user},
    ]
    full = ""
    for evt in models.chat_stream(PLANNER_MODEL, messages, max_tokens=500):
        full = evt["text"]
        if evt["type"] == "chunk" and on_chunk:
            on_chunk(full)

    lines = [line.strip() for line in full.splitlines() if line.strip()]
    if not lines or any(_MALFORMED_LINE_RE.match(line) for line in lines):
        return instructions
    return lines
