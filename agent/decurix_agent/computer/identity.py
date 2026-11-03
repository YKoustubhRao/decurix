"""
Is this the same control, record, window or value? One module with two answers, for every part of the computer that
acts, passes a check or calls a run verified.

- same(a, b) / holds(text, value): exact. Two names are the same when they are equal apart from case, spaces and the
  punctuation between words; a value is in a text when its whole tokens stand there, in order, with no letter or digit
  glued on at either end. "Paid" is not in "Unpaid", "Ravi" is not in "Ravindra Kumar", INV-1042 is not INV-1043.
- candidates(target, pool): ranked near matches, with the reason each was offered. They are evidence for a person or a
  model judge, never something to act on, pass a check with, or call verified.

Found in review (2026-11-03, D-09 to D-17, D-20, D-35): a dozen places each had their own idea of "close enough" -
a substring, a word subset, a startswith, letters squashed together - and each acted on or passed the wrong record.
Acting, passing a check and VERIFIED use only same() and holds().
"""

import re

_TOKEN = re.compile(r"[^\W_]+", re.UNICODE)


def tokens(text):
    """A text's words and numbers, lower case, in order: "INV-1042 / Ravi Kumar" is [inv, 1042, ravi, kumar]."""

    return [t.casefold() for t in _TOKEN.findall(str(text or ""))]


def digit_runs(text):
    """The runs of digits in a text, in order: an identifier's or an amount's."""

    return re.findall(r"\d+", str(text or ""))


def same(a, b):
    """Whether two names are the same name: equal tokens, in the same order (case, spaces and the punctuation between
    words aside). Two empty names are not the same name."""

    ta, tb = tokens(a), tokens(b)
    return bool(ta) and ta == tb


def holds(text, value):
    """Whether `value` stands in `text` whole: its tokens, in order and next to each other, as whole tokens."""

    want, have = tokens(value), tokens(text)
    if not want or len(want) > len(have):
        return False
    n = len(want)
    return any(have[i:i + n] == want for i in range(len(have) - n + 1))


def holds_identifier(text, identifier):
    """Whether an identifier ("INV-1234", "GSTIN 27AAAA...", "B9") stands in `text` as a whole token: its letters and
    digits in order, with only punctuation or spaces between them, and no letter or digit glued on at either end
    ("INV-1234" is not in "INV-12345")."""

    key = re.sub(r"[^0-9a-z]", "", str(identifier or "").casefold())
    if not key:
        return False
    pattern = r"(?<![0-9a-z])" + r"[^0-9a-z]*".join(re.escape(c) for c in key) + r"(?![0-9a-z])"
    return bool(re.search(pattern, str(text or "").casefold()))


def same_control(a, b):
    """Whether two controls are the same one: the same role, the same name (same) and the same window title."""

    if not a or not b:
        return False
    return (str(a.get("role") or "") == str(b.get("role") or "") and same(a.get("name"), b.get("name"))
            and " ".join(str(a.get("window_title") or "").split()) == " ".join(str(b.get("window_title") or "").split()))


def candidates(target, pool, key=None, limit=5):
    """
    The entries of `pool` nearest `target`, best first, each as {"item", "score", "reason"}: for a person or a judge to
    choose from, never to act on. `key` gives an entry's text (default: the entry itself, or its "name").
    """

    def text_of(item):
        if key is not None:
            return key(item)
        return item.get("name") if isinstance(item, dict) else item

    want = tokens(target)
    if not want:
        return []
    found = []
    for item in pool or ():
        have = tokens(text_of(item))
        if not have:
            continue
        shared = [t for t in want if t in have]
        if not shared:
            continue
        score = len(shared) / max(len(want), len(have))
        reasons = []
        if same(target, text_of(item)):
            reasons.append("the same name")
        else:
            missing = [t for t in want if t not in have]
            extra = [t for t in have if t not in want]
            if missing:
                reasons.append("lacks " + " ".join(missing))
            if extra:
                reasons.append("also has " + " ".join(extra))
            if digit_runs(target) != digit_runs(text_of(item)):
                reasons.append("its numbers differ")
        found.append({"item": item, "score": round(score, 3), "reason": "; ".join(reasons)})
    found.sort(key=lambda c: -c["score"])
    return found[:limit]


def changes_identity(old, new):
    """Whether going from name `old` to name `new` would be another record or party, not the same one drawn
    differently: their digit runs differ, or their whole tokens do (a word added, dropped or changed). Only case,
    spacing and punctuation may differ (D-09: a repair may move to another role, container or position, never to
    another name)."""

    return digit_runs(old) != digit_runs(new) or tokens(old) != tokens(new)
