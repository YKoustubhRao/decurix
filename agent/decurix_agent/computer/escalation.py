import json
import math
import os
import re

from decurix_agent.computer import detour as detours
from decurix_agent.computer import elements as el
from decurix_agent.computer import healing
from decurix_agent.computer import grounder
from decurix_agent.computer import ocr
from decurix_agent.computer.errors import ComputerError


# ============================================================
# Escalation: what a replay does when it is not sure
#
# A replay used to have two answers to "I cannot find this control": carry on
# or halt and page a person. This gives it a ladder, cheapest and safest rung
# first - the shape Orby describes for its Large Action Model, where a planner
# (the recorded skill) leans on a grounder (something that can read the screen)
# and a human is the last rung, not the first:
#
#   1. Heal    healing.py: score every candidate on name, container, neighbours,
#              position and role; take one only if it clearly stands out. No model.
#   2. Ground  ask a model (harness.ask_model: the hosted model answers first
#              when the control plane has one; otherwise, or when it fails on a
#              desktop whose brain is not the hosted one, the desktop's own
#              engine - Ollama, Claude or TheBot) to choose among the
#              candidates the screen actually has. It may pick from that list
#              or say it is unsure; it cannot invent a control, and a strict
#              step (see healing.strict) never reaches it.
#   3. Ask     hand a person the numbered candidates, what was tried and where
#              the screen is, so the answer is one word - and learn it if it worked.
#
# The same ladder has a second, narrower job: when a step ran and the result it
# was recorded to leave never appeared, the engine may look at the screen and
# say the step's *intent* was met (a page that titles itself differently now).
# That is a judgement, not a check, so a run that leans on it ends
# completed_unverified and says so - replay.py never reports a success it did
# not see.
#
# Everything that can go wrong with a model is treated as "not sure": no
# engine, a timeout, prose instead of JSON, a choice outside the list, low
# confidence. Each drops to the next rung; none of them acts.
#
# Plan rule 3, phase 2 (the second decision moved after the final check's):
# each question is made of evidence (*_evidence: data only - the candidates,
# the screen as the tools render it, the lines of text read off the frames),
# worded by *_prompt and read by decide_*. With DECURIX_SOUL_ESCALATION=1
# (soul_escalates, off by default) soul words and reads it instead
# (agent/thebot internal/escalation, POST /api/llm/escalation), byte for byte
# the same (agent/tests/test_escalation_parity.py); when soul cannot, the
# question is today's.
#
# Approval is not on this ladder. The gate in core.act is not something a
# ladder may climb over - a step it guards is either found strictly or asked
# about, and a grant is still a person's yes, fingerprinted to one action.
# ============================================================

MODEL_CALLS = int(os.environ.get("DECURIX_ESCALATION_MODEL_CALLS", "3"))
GROUND_CONFIDENCE = 0.75
JUDGE_CONFIDENCE = 0.85
DETOUR_CONFIDENCE = 0.7
POINT_LINES = 40
MAX_OPTIONS = 3
SCREEN_CHARS = 1500

HEALED = "healed"
GROUNDED = "grounded"

# Fixed on purpose: Ollama evaluates a prompt prefix once and keeps it, so the
# instructions that never change go first and unchanged (ollama_client's note).
GROUND_SYSTEM = (
    "You are the grounding step of a workflow automation that runs a recorded task again on a computer screen. "
    "One control could not be found the way it was recorded. You are given the candidates that are on the screen "
    "now, numbered. Choose the one that does what the recorded control did, or say none of them is. "
    "Choosing wrongly can cost the person real work, so say 0 when you are not sure. "
    'Reply with exactly one line of JSON and nothing else: {"choice": <number, or 0>, "confidence": <0 to 1>, "reason": "<short>"}'
)

JUDGE_SYSTEM = (
    "You check the work of a workflow automation that runs a recorded task again on a computer screen. "
    "One step ran, but the result it left when it was recorded did not appear exactly as recorded. "
    "Say whether the screen shows the step did what it was meant to do. Say null when you cannot tell - "
    "a wrong yes lets a broken run carry on. "
    'Reply with exactly one line of JSON and nothing else: {"achieved": true, "false" or null, "confidence": <0 to 1>, "reason": "<short>"}'
)

DETOUR_SYSTEM = (
    "You help a workflow automation run a task a person recorded, on a computer screen whose content can differ "
    "from run to run. The recording reached a step that is not on the screen now. You are given the text the "
    "recording's screen showed at that step, the text it showed a few steps later, and the text on the screen now. "
    "Decide whether the steps in between dealt with something that is not here this time - a pop-up, a cookie "
    "banner, a sign-in or robot check, a notice - so the run should leave them out and carry on from the later step. "
    "Say false when the screen now is simply somewhere else, and null when you cannot tell. "
    'Reply with exactly one line of JSON and nothing else: {"skip": true, false or null, "confidence": <0 to 1>, "reason": "<short>"}'
)

POINT_SYSTEM = (
    "You are the grounding step of a workflow automation that runs a task a person recorded, on a screen whose "
    "content differs from run to run (another search, another customer). When recorded, the person clicked a line "
    "of text. You are given that line with the lines around it, and the numbered lines of text on the screen now. "
    "Choose the line that plays the same part in the task now - the same kind of thing in the same role, not the "
    "same words - or 0 when none does. When the person explained what the step is for (\"the first search result\"), "
    "that is what the line must be. A wrong click can cost the person real work, so say 0 when you are not sure. "
    'Reply with exactly one line of JSON and nothing else: {"choice": <number, or 0>, "confidence": <0 to 1>, "reason": "<short>"}'
)

JSON_OBJECT = re.compile(r"\{.*?\}", re.DOTALL)

# The ladder's questions, each its purpose's last part (py.escalation.<kind>).
GROUND, JUDGE, DETOUR, POINT = "ground", "judge", "detour", "point"
PURPOSE = "py.escalation."

# What a choice among numbered things says (ground, point): taken; in the list but not sure enough; none of them.
CHOSEN, DOUBT, NONE = "chosen", "doubt", "none"
# What the detour question says: leave the steps out; they are still needed; cannot tell.
SKIP, KEEP, UNSURE = "skip", "keep", "unsure"

# The bounds of what a prompt shows: the typed pairs, a screen's OCR text, a detour's reason kept for the trail.
TYPED_SHOWN = 3
OCR_CHARS = 700
REASON_CHARS = 80


def soul_escalates():
    """
    Whether soul words the ladder's questions and reads their answers (plan rule 3, phase 2: DECURIX_SOUL_ESCALATION=1,
    off by default; read for each run's ladder, skill_task._ladder). Then the ladder hands soul each question's evidence
    as data at /api/llm/escalation (agent/thebot internal/escalation: the same prompt, byte for byte, and the same
    reading of the answer), and Python keeps the tools: ranking the candidates, rendering the screen, reading the
    frames. When soul cannot (not running, an older image, a refusal), the question is today's: this module's prompt
    and decide_*, through harness.ask_model.
    """

    return os.environ.get("DECURIX_SOUL_ESCALATION", "").strip() == "1"


def parse_reply(text):
    """The JSON object in a model's reply, or None: models wrap it in prose and fences."""

    for found in JSON_OBJECT.finditer(text or ""):
        try:
            value = json.loads(found.group(0))
        except (ValueError, RecursionError):  # nested past Python's limit is no answer, never a crash
            continue
        if isinstance(value, dict):
            return value

    return None


def _confidence(value):
    try:
        confidence = float(value)
        return max(0.0, min(1.0, confidence)) if math.isfinite(confidence) else 0.0
    except (TypeError, ValueError, OverflowError):  # float() of an int past a float's range is no number
        return 0.0


def _choice(answer):
    """int(answer.get("choice") or 0): 0 for anything int() refuses - an infinity included (OverflowError), which once
    escaped the ladder."""

    try:
        return int(answer.get("choice") or 0)
    except (TypeError, ValueError, OverflowError):
        return 0


def _pick(text, count, enough):
    """A choice among `count` numbered things read from the answer: its number (0 when none of them), confidence and
    verdict against the least confidence that is `enough`."""

    answer = parse_reply(text) or {}
    choice = _choice(answer)
    confidence = _confidence(answer.get("confidence"))
    if not 1 <= choice <= count:
        return {"choice": 0, "confidence": confidence, "verdict": NONE}
    return {"choice": choice, "confidence": confidence, "verdict": CHOSEN if confidence >= enough else DOUBT}


def decide_ground(text, evidence):
    """The grounding answer read: which candidate, how sure (soul's internal/escalation DecideGround)."""

    return _pick(text, len(evidence["candidates"]), GROUND_CONFIDENCE)


def decide_point(text, evidence):
    """Which line on the screen now a point click goes to, read from the answer (DecidePoint)."""

    return _pick(text, len(evidence["lines"]), GROUND_CONFIDENCE)


def decide_judge(text, evidence=None):
    """Whether the step did its job: achieved only for a JSON true given with JUDGE_CONFIDENCE (DecideJudge)."""

    answer = parse_reply(text) or {}
    confidence = _confidence(answer.get("confidence"))
    return {"achieved": answer.get("achieved") is True and confidence >= JUDGE_CONFIDENCE, "confidence": confidence}


def decide_detour(text, evidence=None):
    """Whether the steps may be left out: skip for a JSON true, keep for a JSON false, each given with
    DETOUR_CONFIDENCE; unsure otherwise; and the reason given (DecideDetour)."""

    answer = parse_reply(text) or {}
    confidence = _confidence(answer.get("confidence"))
    sure = confidence >= DETOUR_CONFIDENCE
    verdict = (SKIP if answer.get("skip") is True and sure else KEEP if answer.get("skip") is False and sure
               else UNSURE)
    return {"verdict": verdict, "confidence": confidence, "reason": str(answer.get("reason") or "")[:REASON_CHARS]}


def _typing(typed):
    """What the recording typed and what this run typed instead, the first TYPED_SHOWN pairs."""

    return "".join(f"The recording typed {el._quote(was, 80)}; this run typed {el._quote(now, 80)}.\n"
                   for was, now in typed[:TYPED_SHOWN])


def _task(goal):
    return f"Task: {goal}\n" if goal else ""


def ground_prompt(ev):
    """The grounding question (soul's GroundPrompt, byte for byte)."""

    lines = [f"{n}. {c['role']} {el._quote(c['name'])}" + (" [disabled]" if c["disabled"] else "")
             for n, c in enumerate(ev["candidates"], 1)]
    return (
        _task(ev["goal"])
        + f"Step {ev['step']}" + (f" of {ev['total']}" if ev["total"] else "") + f": {ev['summary']}\n"
        + f"Recorded control: {ev['role']} {el._quote(ev['name'])}\n"
        # What the person said the step is (learning, study._aim_spots): a control they named in their own words
        # ("the Save button") is often labelled otherwise on the screen.
        + (f"What the person said this step is for: {' '.join(ev['intent'].split())[:400]}\n" if ev["intent"] else "")
        + f"Window now: {el._quote(ev['window'], 80)}\n"
        + (f"In front: {', '.join(el._quote(n, 60) for n in ev['front'])}\n" if ev["front"] else "")
        + "Candidates:\n" + "\n".join(lines)
    )


def judge_prompt(ev):
    """The judging question (soul's JudgePrompt)."""

    return (
        _task(ev["goal"])
        + f"Step {ev['step']}: {ev['summary']}\n"
        + "It was recorded to leave: " + "; ".join(ev["missing"]) + "\n"
        + f"The screen now:\n{ev['screen']}"
    )


def _ocr(texts):
    return ocr.text([{"text": t} for t in texts], OCR_CHARS)


def detour_prompt(ev):
    """The detour question (soul's DetourPrompt)."""

    return (
        _task(ev["goal"]) + _typing(ev["typed"])
        + f"The run is at step {ev['step']}: {ev['summary']}\n"
        + f"The recording's screen at step {ev['step']}:\n{_ocr(ev['here'])}\n\n"
        + f"The recording's screen at step {ev['later_step']} ({ev['later_summary']}):\n{_ocr(ev['there'])}\n\n"
        + f"The screen now:\n{_ocr(ev['now'])}"
    )


def point_prompt(ev):
    """The question where a point click goes (soul's PointPrompt)."""

    context = "\n".join(("> " if l["clicked"] else "  ") + l["text"][:120] for l in ev["context"])
    return (
        _task(ev["goal"]) + _typing(ev["typed"])
        + (f"The person explained the task: {ev['explanation'][:600]}\n" if ev["explanation"] else "")
        + f"Step {ev['step']}: {ev['summary']}\n"
        + (f"What the person said this step is for: {ev['intent']}\n" if ev["intent"] else "")
        + f"When recorded, the person clicked the line marked >:\n{context}\n\n"
        + "Lines on the screen now:\n"
        + "\n".join(f"{n}. {text[:120]}" for n, text in enumerate(ev["lines"], 1))
    )


SYSTEMS = {GROUND: GROUND_SYSTEM, JUDGE: JUDGE_SYSTEM, DETOUR: DETOUR_SYSTEM, POINT: POINT_SYSTEM}
PROMPTS = {GROUND: ground_prompt, JUDGE: judge_prompt, DETOUR: detour_prompt, POINT: point_prompt}
DECIDE = {GROUND: decide_ground, JUDGE: decide_judge, DETOUR: decide_detour, POINT: decide_point}


def _number(value):
    """A confidence soul gave, as a float, or ValueError: a number from 0 to 1, never a bool or a NaN."""

    if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0.0 <= value <= 1.0:
        raise ValueError(f"soul's confidence is not one: {value!r}"[:200])
    return float(value)


def from_soul(kind, decision, evidence):
    """
    Soul's decision (internal/escalation Decide) in decide_*'s shape, or ValueError for one that is not of it or does
    not hold together (a choice outside the list, a verdict its confidence does not carry): the ladder then reads it as
    an engine that could not be asked - fail closed, nothing made up here.
    """

    if not isinstance(decision, dict):
        raise ValueError("soul's decision is not an object")
    confidence = _number(decision.get("confidence"))
    if kind in (GROUND, POINT):
        count = len(evidence["candidates" if kind == GROUND else "lines"])
        choice, verdict = decision.get("choice"), decision.get("verdict")
        if isinstance(choice, bool) or not isinstance(choice, int) or not 0 <= choice <= count:
            raise ValueError(f"soul's choice is not one of the {count}: {choice!r}"[:200])
        expected = NONE if choice == 0 else CHOSEN if confidence >= GROUND_CONFIDENCE else DOUBT
        if verdict != expected:
            raise ValueError(f"soul's verdict {verdict!r} is not its choice's"[:200])
        return {"choice": choice, "confidence": confidence, "verdict": verdict}
    if kind == JUDGE:
        achieved = decision.get("achieved")
        if not isinstance(achieved, bool) or (achieved and confidence < JUDGE_CONFIDENCE):
            raise ValueError(f"soul's judgement is not one: {json.dumps(decision)[:200]}")
        return {"achieved": achieved, "confidence": confidence}
    verdict, reason = decision.get("verdict"), decision.get("reason")
    if (verdict not in (SKIP, KEEP, UNSURE) or not isinstance(reason, str) or len(reason) > REASON_CHARS
            or (verdict != UNSURE and confidence < DETOUR_CONFIDENCE)):
        raise ValueError(f"soul's detour decision is not one: {json.dumps(decision)[:200]}")
    return {"verdict": verdict, "confidence": confidence, "reason": reason}


def _front(observation):
    """What is asking for attention on the screen: dialogs and alerts, by name."""

    names = [e.get("name") for e in observation.get("elements") or [] if e.get("role") in ("dialog", "alert") and e.get("name")]
    return names[:2]


class Ladder:
    """
    One run's escalation: the rungs a step is taken through, and what the run
    learned on the way (which the skill keeps only if the run then succeeded -
    experience.commit).
    """

    def __init__(self, ask_model=None, goal="", total=0, model_calls=None, explanation="", ask_decided=None):
        self.ask_model = ask_model
        # ask_decided(system, prompt, evidence, *, purpose) -> (text, decision or None): a question soul may word and
        # read itself (soul_escalates; skill_task._ask_decided). None, and every question is today's (ask_model).
        self.ask_decided = ask_decided
        self.goal = goal
        # What the person who taught the skill explained in their own words (learning._answer_explain).
        self.explanation = explanation or ""
        self.total = total
        self.calls_left = MODEL_CALLS if model_calls is None else model_calls
        self.model_calls = 0
        self.report = lambda line: None

        self.healed = []      # repairs made this run
        self.judged = []      # steps carried on past on the engine's say-so
        self.seen = {}        # step id -> where it was found and what stood around it
        self.confirmed = set()
        self.detours = []     # runs of steps left out as not needed this time
        self.grounded = []    # point clicks placed by the engine reading the screen's text
        # What the recording typed and what this run types instead: [(recorded, now)].
        self.typed = []
        self.trail = []       # every rung tried, in order, for whoever has to be asked

    # -- bookkeeping --------------------------------------------------

    @staticmethod
    def key(step):
        return step.get("id") or step["n"]

    def attach(self, report):
        self.report = report

    def found(self, step, element, observation):
        """A step's target was found as recorded: keep where, so a later run can tell when it has moved."""

        self.seen[self.key(step)] = {
            "box": element.get("box"),
            "screen": observation.get("size"),
            "anchors": healing.anchors_near(element, observation.get("elements") or []),
        }

    def confirm(self, step):
        """The screen showed what the step was recorded to leave."""

        self.confirmed.add(self.key(step))

    def repair(self, step, element, observation, how, confidence):
        """Record a control found by other means than its recorded name."""

        target = step.get("target") or {}
        self.found(step, element, observation)
        self.healed.append({
            "id": self.key(step), "n": step["n"], "how": how, "confidence": round(confidence, 2),
            "was": target.get("name"), "now": element.get("name"), "role": element.get("role"),
        })
        self.report(f"Step {step['n']}: {target.get('role', 'control')} \"{target.get('name')}\" is now "
                    f"\"{element.get('name')}\" - found by {how} ({confidence:.2f})")

    def summary(self):
        """A sentence for the end of a run's report: what it repaired itself."""

        parts = []
        if self.healed:
            parts.append("repaired " + "; ".join(f"step {h['n']} (\"{h['was']}\" is now \"{h['now']}\")" for h in self.healed[:4]))
        if self.detours:
            parts.append("left out " + "; ".join(
                (f"steps {d['from']}-{d['to'] - 1}" if d["to"] - d["from"] > 1 else f"step {d['from']}")
                + " (what they dealt with was not on the screen this time)" for d in self.detours[:3]))
        if self.grounded:
            parts.append("chose where to click in step " + ", ".join(
                f"{g['n']} (\"{g['now'][:60]}\" for the recorded \"{g['was'][:60]}\")" for g in self.grounded[:3]))
        if self.judged:
            parts.append("took the screen's word for step " + ", ".join(str(n) for n in self.judged[:4])
                         + " (its recorded result was not there exactly)")

        return ("I " + " and ".join(parts) + ".") if parts else ""

    # -- the ladder -----------------------------------------------------

    def recover(self, step, observation, reason):
        """
        A step's target was not found. Take it up the ladder: {"element", "how",
        "confidence", "reason", "options"} - an element to use, or none and the
        numbered candidates to ask a person about.
        """

        target = step.get("target") or {}
        if not target:
            return self._asked([], reason)

        ranked = healing.rank(step, observation)
        verdict, top = healing.decide(step, ranked)

        if verdict == "confident":
            self.trail.append(f"healing chose {healing.describe(top)}")
            return {"element": top["element"], "how": HEALED, "confidence": top["score"],
                    "reason": "", "options": [], "observation": observation}

        self.trail.append("healing found " + (f"{healing.describe(top)}, not clear enough" if verdict == "doubt" else "nothing close"))

        if ranked and self._may_ground(step):
            picked = self._ground(step, observation, ranked)
            if picked is not None and healing.retargets(step, picked[0]["element"]):
                # The engine's pick is a control that commits or is gated, under another name: a person decides that.
                self.trail.append(f"the engine chose {healing.describe(picked[0])}, which commits - asked instead")
                picked = None
            if picked is not None and not healing.keeps_identity(step, picked[0]["element"]):
                # Another name is another record or party, whoever picks it (D-09): a person decides that.
                self.trail.append(f"the engine chose {healing.describe(picked[0])}, under another name - asked instead")
                picked = None
            if picked is not None:
                return {"element": picked[0]["element"], "how": GROUNDED, "confidence": picked[1],
                        "reason": "", "options": [], "observation": observation}

        return self._asked(ranked, reason)

    def _may_ground(self, step):
        return self.ask_model is not None and self.calls_left > 0 and not healing.strict(step)

    def _consult(self, kind, evidence, prompt):
        """
        One question to the engine: decide_*'s decision of its answer. With ask_decided (DECURIX_SOUL_ESCALATION) soul
        is handed the evidence and may answer with its own decision (from_soul checks it); when it does not, the
        prompt - today's - was asked instead and its answer is read here. Raises as ask_model raises.
        """

        purpose = PURPOSE + kind
        if self.ask_decided is not None:
            text, decision = self.ask_decided(SYSTEMS[kind], prompt, evidence, purpose=purpose)
            if decision is not None:
                return from_soul(kind, decision, evidence)
        else:
            text = self.ask_model(SYSTEMS[kind], prompt, purpose=purpose)
        return DECIDE[kind](text, evidence)

    def _ground_evidence(self, step, observation, ranked):
        """What the grounding question is shown, as data (internal/escalation GroundEvidence)."""

        target = step.get("target") or {}
        return {
            "goal": f"{self.goal}" if self.goal else "",
            "step": f"{step['n']}",
            "total": f"{self.total}" if self.total else "",
            "summary": f"{step.get('summary', '')}",
            "role": f"{target.get('role', 'control')}",
            "name": str(target.get("name") or ""),
            "intent": str(step["intent"]) if step.get("intent") else "",
            "window": str(observation.get("active_title") or ""),
            "front": [str(n) for n in _front(observation)],
            "candidates": [{"role": f"{c['element'].get('role')}", "name": str(c["element"].get("name") or ""),
                            "disabled": "disabled" in (c["element"].get("states") or [])} for c in ranked],
        }

    def _typed(self):
        """What the recording typed and what this run typed instead, as text pairs."""

        return [[str(was), str(now)] for was, now in self.typed]

    def _asked(self, ranked, reason):
        options = [{"n": n, "role": c["element"].get("role"), "name": c["element"].get("name"),
                    "ref": c["element"].get("ref"), "path": c["element"].get("path"), "score": c["score"]}
                   for n, c in enumerate(ranked[:MAX_OPTIONS], 1)]

        return {"element": None, "how": None, "confidence": 0.0, "reason": reason, "options": options, "observation": None}

    def _ground(self, step, observation, ranked):
        """The engine's choice among the candidates: (candidate, confidence) or None."""

        self.calls_left -= 1
        self.model_calls += 1
        evidence = self._ground_evidence(step, observation, ranked)
        prompt = ground_prompt(evidence)

        try:
            decided = self._consult(GROUND, evidence, prompt)
        except Exception as exc:
            self.trail.append(f"the engine could not be asked ({str(exc)[:80]})")
            return None

        choice, confidence = decided["choice"], decided["confidence"]
        if decided["verdict"] == NONE:
            self.trail.append("the engine was not sure which")
            return None
        if decided["verdict"] == DOUBT:
            self.trail.append(f"the engine leaned to {healing.describe(ranked[choice - 1])} but only at {confidence:.2f}")
            return None

        self.trail.append(f"the engine chose {healing.describe(ranked[choice - 1])} at {confidence:.2f}")
        return ranked[choice - 1], confidence

    def judge(self, step, observation, missing):
        """
        Whether the screen shows a step did what it was for although its
        recorded result is not there exactly: True only on a confident yes.
        `missing` is the unmet checks, in words. Never for a step whose wrong
        outcome is not one to carry on past (healing.strict).
        """

        if not self._may_ground(step) or not observation:
            return False

        self.calls_left -= 1
        self.model_calls += 1

        try:
            screen = el.render(observation, budget=SCREEN_CHARS)
        except Exception:
            return False

        evidence = {"goal": f"{self.goal}" if self.goal else "", "step": f"{step['n']}",
                    "summary": f"{step.get('summary', '')}", "missing": list(missing), "screen": screen}
        prompt = judge_prompt(evidence)

        try:
            achieved = self._consult(JUDGE, evidence, prompt)["achieved"]
        except Exception as exc:
            self.trail.append(f"the engine could not be asked to judge step {step['n']} ({str(exc)[:80]})")
            return False

        self.trail.append(f"the engine {'accepted' if achieved else 'would not vouch for'} step {step['n']}")

        if achieved:
            self.judged.append(step["n"])

        return achieved

    # -- when the screen is not the recording's ---------------------------

    def detour(self, steps, index, now, screen_of):
        """
        Whether the steps from steps[index] on dealt with something this run
        does not meet (detour.py): the index to go on from, or None. The
        recording's own screens propose it; the engine, reading their text,
        may overrule a proposal or carry a close one. Without an engine only
        a clear one (detour.SURE_MARGIN) is taken.
        """

        typed = [v for pair in self.typed for v in pair]
        found = detours.find(steps, index, now, screen_of, typed=typed)
        if found is None:
            return None

        first, then = steps[index], steps[found["to"]]
        span = f"steps {first['n']}-{then['n'] - 1}" if then["n"] - first["n"] > 1 else f"step {first['n']}"
        verdict = None

        texts = (found["here"].lines, found["there"].lines, now.lines) if ocr.available() else ((), (), ())
        if self.ask_model is not None and self.calls_left > 0 and all(texts):
            self.calls_left -= 1
            self.model_calls += 1
            evidence = {
                "goal": f"{self.goal}" if self.goal else "", "typed": self._typed(),
                "step": f"{first['n']}", "summary": f"{first.get('summary', '')}",
                "later_step": f"{then['n']}", "later_summary": f"{then.get('summary', '')}",
                "here": [l["text"] for l in texts[0]], "there": [l["text"] for l in texts[1]],
                "now": [l["text"] for l in texts[2]],
            }
            prompt = detour_prompt(evidence)
            try:
                decided = self._consult(DETOUR, evidence, prompt)
            except Exception as exc:
                self.trail.append(f"the engine could not be asked about {span} ({str(exc)[:80]})")
                decided = {"verdict": UNSURE, "confidence": 0.0, "reason": ""}
            if decided["verdict"] == SKIP:
                verdict = "the engine agreed"
            elif decided["verdict"] == KEEP:
                self.trail.append(f"the engine said {span} are still needed ({decided['reason']})")
                return None

        if verdict is None:
            if found["margin"] < detours.SURE_MARGIN:
                self.trail.append(f"the screen looks more like step {then['n']}'s than step {first['n']}'s, "
                                  f"but not clearly enough to leave out {span}")
                return None
            verdict = "the screens say so"

        self.detours.append({"from": first["n"], "to": then["n"], "ids": [self.key(s) for s in steps[index:found["to"]]],
                             "margin": found["margin"], "how": verdict})
        self.trail.append(f"left out {span}: the screen now is what the recording saw at step {then['n']} ({verdict})")
        self.report(f"Left out {span}: they dealt with something that is not on the screen this time - "
                    f"the screen now looks like the recording's at step {then['n']} ({verdict}, {found['margin']:.2f})")
        return found["to"]

    def ground_point(self, step, now_frame, recorded_frame, elements=None):
        """
        Where to click for a point step whose picture is not on the screen:
        the engine reads what the recording clicked (ocr.py) and picks the line
        that plays that part now; failing that, a grounding model looks for
        what the person said the step does (_point_by_eye). (x, y) or None.
        Never for a strict step.
        """

        point = self._point_by_text(step, now_frame, recorded_frame)
        if point is None:
            point = self._point_by_eye(step, now_frame, elements)
        return point

    def _point_by_eye(self, step, now_frame, elements):
        """
        A grounding model (grounder.py) finds what the person's own words for
        the step describe - "clicks the first search result" - on the screen
        now: an icon or a picture has no text for _point_by_text to go by. Only
        a point that lands on a control in the element list is taken; a model
        always answers somewhere, and a click on the wrong thing is not undone.
        """

        intent = " ".join((step.get("intent") or "").split())
        if (not intent or now_frame is None or healing.strict(step) or self.calls_left <= 0
                or step.get("kind") not in ("click", "double_click") or not grounder.available()):
            return None

        self.calls_left -= 1
        self.model_calls += 1
        try:
            point = grounder.locate(now_frame, intent)
        except ComputerError as exc:
            self.trail.append(f"the grounding model could not place step {step['n']} ({str(exc)[:80]})")
            return None

        under = el.at_point(elements or [], point[0], point[1])
        if under is None or under.get("role") not in el.INTERACTIVE_ROLES:
            self.trail.append(f"the grounding model put step {step['n']} at {point[0]},{point[1]}, where there is "
                              "no control - not clicking a guess")
            return None

        self.grounded.append({"id": self.key(step), "n": step["n"], "was": intent, "now": el.describe(under),
                              "how": "grounder"})
        self.trail.append(f"the grounding model found {el.describe(under)} for {el._quote(intent, 60)}")
        self.report(f"Step {step['n']}: what was clicked is not on the screen as recorded - clicking "
                    f"{el.describe(under)} at {point[0]},{point[1]}, which a grounding model found for what you "
                    f"said this step does ({el._quote(intent, 80)})")
        return point

    def _point_by_text(self, step, now_frame, recorded_frame):
        if not self._may_ground(step) or now_frame is None or recorded_frame is None or not ocr.available():
            return None
        if step.get("kind") not in ("click", "double_click"):
            return None

        width, height, rgb = recorded_frame
        before = ocr.read(width, height, rgb)
        clicked = ocr.line_at(before, step.get("point"))
        if clicked is None:
            self.trail.append(f"no text under step {step['n']}'s recorded click to go by")
            return None

        around = sorted(before, key=lambda l: abs(l["box"][1] - clicked["box"][1]))[:7]
        around.sort(key=lambda l: l["box"][1])
        lines = [l for l in ocr.read_frame(now_frame) if len(ocr.WORD_RE.findall(l["text"])) >= 1][:POINT_LINES]
        if not lines:
            return None

        self.calls_left -= 1
        self.model_calls += 1
        evidence = {
            "goal": f"{self.goal}" if self.goal else "", "typed": self._typed(),
            "explanation": self.explanation if self.explanation else "",
            "step": f"{step['n']}", "summary": f"{step.get('summary', '')}",
            "intent": f"{step['intent']}" if step.get("intent") else "",
            "context": [{"text": l["text"], "clicked": l is clicked} for l in around],
            "lines": [l["text"] for l in lines],
        }
        prompt = point_prompt(evidence)

        try:
            decided = self._consult(POINT, evidence, prompt)
        except Exception as exc:
            self.trail.append(f"the engine could not be asked where step {step['n']} clicks ({str(exc)[:80]})")
            return None

        choice, confidence = decided["choice"], decided["confidence"]
        if decided["verdict"] != CHOSEN:
            self.trail.append(f"the engine was not sure where step {step['n']} clicks now")
            return None

        line = lines[choice - 1]
        x, y, w, h = line["box"]
        point = (x + min(w // 2, 60), y + h // 2)
        self.grounded.append({"id": self.key(step), "n": step["n"], "was": clicked["text"], "now": line["text"],
                              "confidence": round(confidence, 2)})
        self.trail.append(f"the engine chose the line {el._quote(line['text'], 60)} at {confidence:.2f}")
        self.report(f"Step {step['n']}: what was clicked is not on the screen as recorded - clicking "
                    f"{el._quote(line['text'], 60)} at {point[0]},{point[1]}, which plays the part the recorded "
                    f"{el._quote(clicked['text'], 60)} did (the engine chose it, {confidence:.2f})")
        return point
