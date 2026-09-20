import json
import os
import re

from flowera_agent.computer import elements as el
from flowera_agent.computer import healing


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
#   2. Ground  ask the desktop's own engine (harness.ask_model - Ollama, Claude
#              or TheBot, whichever this desktop runs) to choose among the
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
# Approval is not on this ladder. The gate in core.act is not something a
# ladder may climb over - a step it guards is either found strictly or asked
# about, and a grant is still a person's yes, fingerprinted to one action.
# ============================================================

MODEL_CALLS = int(os.environ.get("FLOWERA_ESCALATION_MODEL_CALLS", "3"))
GROUND_CONFIDENCE = 0.75
JUDGE_CONFIDENCE = 0.85
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

JSON_OBJECT = re.compile(r"\{.*?\}", re.DOTALL)


def parse_reply(text):
    """The JSON object in a model's reply, or None: models wrap it in prose and fences."""

    for found in JSON_OBJECT.finditer(text or ""):
        try:
            value = json.loads(found.group(0))
        except ValueError:
            continue
        if isinstance(value, dict):
            return value

    return None


def _confidence(value):
    try:
        return max(0.0, min(1.0, float(value)))
    except (TypeError, ValueError):
        return 0.0


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

    def __init__(self, ask_model=None, goal="", total=0, model_calls=None):
        self.ask_model = ask_model
        self.goal = goal
        self.total = total
        self.calls_left = MODEL_CALLS if model_calls is None else model_calls
        self.model_calls = 0
        self.report = lambda line: None

        self.healed = []      # repairs made this run
        self.judged = []      # steps carried on past on the engine's say-so
        self.seen = {}        # step id -> where it was found and what stood around it
        self.confirmed = set()
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
            return self._asked(step, [], reason)

        ranked = healing.rank(step, observation)
        verdict, top = healing.decide(step, ranked)

        if verdict == "confident":
            self.trail.append(f"healing chose {healing.describe(top)}")
            return {"element": top["element"], "how": HEALED, "confidence": top["score"],
                    "reason": "", "options": [], "observation": observation}

        self.trail.append("healing found " + (f"{healing.describe(top)}, not clear enough" if verdict == "doubt" else "nothing close"))

        if ranked and self._may_ground(step):
            picked = self._ground(step, observation, ranked)
            if picked is not None:
                return {"element": picked[0]["element"], "how": GROUNDED, "confidence": picked[1],
                        "reason": "", "options": [], "observation": observation}

        return self._asked(step, ranked, reason)

    def _may_ground(self, step):
        return self.ask_model is not None and self.calls_left > 0 and not healing.strict(step)

    def _asked(self, step, ranked, reason):
        options = [{"n": n, "role": c["element"].get("role"), "name": c["element"].get("name"),
                    "ref": c["element"].get("ref"), "path": c["element"].get("path"), "score": c["score"]}
                   for n, c in enumerate(ranked[:MAX_OPTIONS], 1)]

        return {"element": None, "how": None, "confidence": 0.0, "reason": reason, "options": options, "observation": None}

    def _ground(self, step, observation, ranked):
        """The engine's choice among the candidates: (candidate, confidence) or None."""

        self.calls_left -= 1
        self.model_calls += 1
        target = step.get("target") or {}
        window = el._quote(observation.get("active_title") or "", 80)

        lines = [f"{n}. {c['element'].get('role')} {el._quote(c['element'].get('name') or '')}"
                 + (" [disabled]" if "disabled" in (c["element"].get("states") or []) else "")
                 for n, c in enumerate(ranked, 1)]
        front = _front(observation)

        prompt = (
            (f"Task: {self.goal}\n" if self.goal else "")
            + f"Step {step['n']}" + (f" of {self.total}" if self.total else "") + f": {step.get('summary', '')}\n"
            + f"Recorded control: {target.get('role', 'control')} {el._quote(target.get('name') or '')}\n"
            + f"Window now: {window}\n"
            + (f"In front: {', '.join(el._quote(n, 60) for n in front)}\n" if front else "")
            + "Candidates:\n" + "\n".join(lines)
        )

        try:
            answer = parse_reply(self.ask_model(GROUND_SYSTEM, prompt))
        except Exception as exc:
            self.trail.append(f"the engine could not be asked ({str(exc)[:80]})")
            return None

        try:
            choice = int((answer or {}).get("choice") or 0)
        except (TypeError, ValueError):
            choice = 0
        confidence = _confidence((answer or {}).get("confidence"))

        if not 1 <= choice <= len(ranked):
            self.trail.append("the engine was not sure which")
            return None
        if confidence < GROUND_CONFIDENCE:
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

        prompt = (
            (f"Task: {self.goal}\n" if self.goal else "")
            + f"Step {step['n']}: {step.get('summary', '')}\n"
            + "It was recorded to leave: " + "; ".join(missing) + "\n"
            + f"The screen now:\n{screen}"
        )

        try:
            answer = parse_reply(self.ask_model(JUDGE_SYSTEM, prompt)) or {}
        except Exception as exc:
            self.trail.append(f"the engine could not be asked to judge step {step['n']} ({str(exc)[:80]})")
            return False

        achieved = answer.get("achieved") is True and _confidence(answer.get("confidence")) >= JUDGE_CONFIDENCE
        self.trail.append(f"the engine {'accepted' if achieved else 'would not vouch for'} step {step['n']}")

        if achieved:
            self.judged.append(step["n"])

        return achieved
