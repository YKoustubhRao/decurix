#!/usr/bin/env python3
"""
Has BrowserGym solve MiniWoB++ tasks, and keeps what it did - real actions on
real pages, chosen from real accessibility trees - in the two shapes other
agents leave behind: a BrowserGym / AgentLab trajectory, and the event stream
OpenHands saves. bench/agents/e2e.py replays them in a Decurix desktop.

    python3 bench/agents/gen_trajectories.py <out dir> [--seeds 1,2,3] [--tasks a,b]

Needs BrowserGym (a Python 3.12 environment; the pages are the `miniwob`
wheel's html):

    uv venv --python 3.12 venv && . venv/bin/activate
    uv pip install browsergym-miniwob miniwob && playwright install chromium
    MINIWOB_URL=file:///.../site-packages/miniwob/html/miniwob/ python gen_trajectories.py out

The solvers read the goal and the tree the way an agent would; each step's
observation is recorded *before* the action, which is what an action's bids
mean. It uses Decurix's own tree parser (agent/decurix_agent/computer/
ingest_agents.py) to find controls, so a tree it cannot read fails here first.
"""

import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agent"))

import gymnasium as gym  # noqa: E402

import browsergym.miniwob  # noqa: E402,F401
from browsergym.miniwob import base  # noqa: E402
from browsergym.utils.obs import flatten_axtree_to_str  # noqa: E402

from decurix_agent.computer import ingest_agents  # noqa: E402


def find(nodes, role, name=None, nth=1):
    """The bid of the nth control with a role (and a name, when given)."""

    hits = [n for n in nodes if n["bid"] and n["role"] == role and (name is None or n["name"] == name)]
    if len(hits) < nth:
        raise LookupError(f"no {role} {name!r} #{nth} in the tree")
    return hits[nth - 1]["bid"]


def click(role, name=None, nth=1):
    return lambda nodes, goal: f"click({find(nodes, role, name, nth)!r})"


def fill(role, text, nth=1):
    return lambda nodes, goal: f"fill({find(nodes, role, None, nth)!r}, {text!r})"


def goal_words(pattern, goal):
    match = re.search(pattern, goal)
    if not match:
        raise LookupError(f"the goal {goal!r} is not one this solver knows")
    return match.groups()


# task -> a function from the goal to the steps that solve it; each step is a function
# of the current tree that returns one action string.
def solve_click_button(goal):
    (label,) = goal_words(r'Click on the "(.+)" button\.', goal)
    return [click("button", label)]


def solve_click_link(goal):
    (label,) = goal_words(r'Click on the link "(.+)"\.', goal)
    return [click("link", label)]


def solve_enter_text(goal):
    (text,) = goal_words(r'Enter "(.*)" into the text field and press Submit\.', goal)
    return [fill("textbox", text), click("button", "Submit")]


def solve_enter_password(goal):
    (text,) = goal_words(r'Enter the password "(.*)" into both text fields and press submit\.', goal)
    return [fill("textbox", text, 1), fill("textbox", text, 2), click("button", "Submit")]


def solve_login_user(goal):
    user, password = goal_words(r'Enter the username "(.*)" and the password "(.*)" into the text fields and press login\.', goal)
    return [fill("textbox", user, 1), fill("textbox", password, 2), click("button", "Login")]


def solve_choose_list(goal):
    (choice,) = goal_words(r"Select (.+) from the list and click Submit\.", goal)
    return [lambda nodes, g: f"select_option({find(nodes, 'combobox')!r}, {choice!r})", click("button", "Submit")]


def solve_click_option(goal):
    (choice,) = goal_words(r"Select (.+) and click Submit\.", goal)
    return [click("radio", choice), click("button", "Submit")]


def solve_click_checkboxes(goal):
    (chosen,) = goal_words(r"Select (.+) and click Submit\.", goal)
    words = [] if chosen == "nothing" else re.split(r", | and ", chosen)
    return [click("checkbox", w) for w in words if w] + [click("button", "Submit")]


def solve_focus_text(goal):
    return [lambda nodes, g: f"focus({find(nodes, 'textbox')!r})"]


SOLVERS = {
    "click-button": solve_click_button,
    "click-link": solve_click_link,
    "enter-text": solve_enter_text,
    "enter-password": solve_enter_password,
    "login-user": solve_login_user,
    "choose-list": solve_choose_list,
    "click-option": solve_click_option,
    "click-checkboxes": solve_click_checkboxes,
    "focus-text": solve_focus_text,
}


# The seed BrowserGym hands the page's own random generator (`Math.seedrandom`) is
# drawn inside setup(); reading it there is how a replay elsewhere gets the same
# instance of the task.
captured = {}
_setup = base.AbstractMiniwobTask.setup


def _setup_and_remember(self, page):
    state = self.random.get_state()
    captured["seed"] = int(self.random.randint(0, 1000000))
    self.random.set_state(state)
    return _setup(self, page)


base.AbstractMiniwobTask.setup = _setup_and_remember


def episode(task, seed):
    env = gym.make(f"browsergym/miniwob.{task}", headless=True)

    try:
        obs, info = env.reset(seed=seed)
        goal, page_url = obs["goal"], obs["url"]
        steps, reward, done = [], 0.0, False

        for make in SOLVERS[task](goal):
            nodes = ingest_agents.parse_axtree_text(flatten_axtree_to_str(obs["axtree_object"]))
            action = make(nodes, goal)
            steps.append({
                "action": action,
                "axtree_txt": flatten_axtree_to_str(obs["axtree_object"]),
                "axtree_object": obs["axtree_object"],
                "url": obs["url"],
            })
            obs, reward, terminated, truncated, info = env.step(action)
            steps[-1]["after_object"] = obs["axtree_object"]
            done = terminated or truncated
            if done:
                break

        final = flatten_axtree_to_str(obs["axtree_object"])
        return {"task": task, "seed": seed, "miniwob_seed": captured["seed"], "goal": goal, "url": page_url,
                "reward": reward, "done": done, "steps": steps, "final": {"axtree_txt": final}}
    finally:
        env.close()


def as_browsergym(ep):
    """AgentLab-style: a step is an action and the observation it was chosen from."""

    return {"format": "browsergym", "goal": ep["goal"], "task": ep["task"], "miniwob_seed": ep["miniwob_seed"],
            "steps": [{"action": s["action"], "axtree_txt": s["axtree_txt"], "url": s["url"]} for s in ep["steps"]],
            "final": ep["final"]}


def as_openhands(ep):
    """OpenHands' event stream: a user message, a browse, then an action and its observation each step."""

    events = [{"id": 0, "source": "user", "action": "message", "args": {"content": ep["goal"]}},
              {"id": 1, "source": "agent", "action": "browse", "args": {"url": ep["url"]}}]

    def browse_observation(cause, step, tree):
        return {"id": len(events), "source": "agent", "cause": cause, "observation": "browse",
                "content": "(page text)", "message": "Visited " + step["url"],
                "extras": {"url": step["url"], "axtree_object": step[tree], "error": False}}

    # The page as it first loads, then each action followed by what the page looked like after it.
    events.append(browse_observation(1, ep["steps"][0], "axtree_object"))

    for step in ep["steps"]:
        events.append({"id": len(events), "source": "agent", "action": "browse_interactive",
                       "args": {"browser_actions": step["action"], "thought": "", "browsergym_send_msg_to_user": ""}})
        events.append(browse_observation(len(events) - 1, step, "after_object"))

    events.append({"id": len(events), "source": "agent", "action": "finish", "args": {"outputs": {}, "thought": ""}})
    return events


def as_openhands_saved(events):
    """What `--save-trajectory` writes: the same events without the accessibility trees."""

    saved = json.loads(json.dumps(events))
    for e in saved:
        (e.get("extras") or {}).pop("axtree_object", None)
    return saved


def main():
    args = sys.argv[1:]
    out = Path(args[0])
    seeds = [int(s) for s in (args[args.index("--seeds") + 1] if "--seeds" in args else "1,2,3").split(",")]
    tasks = (args[args.index("--tasks") + 1].split(",") if "--tasks" in args else list(SOLVERS))
    out.mkdir(parents=True, exist_ok=True)

    manifest = []
    for task in tasks:
        for seed in seeds:
            try:
                ep = episode(task, seed)
            except LookupError as exc:
                print(f"skip {task} seed {seed}: {exc}")
                continue

            name = f"{task}-{seed}"
            (out / f"{name}.browsergym.json").write_text(json.dumps(as_browsergym(ep), indent=1))
            oh = as_openhands(ep)
            (out / f"{name}.openhands.json").write_text(json.dumps(oh))
            (out / f"{name}.openhands-saved.json").write_text(json.dumps(as_openhands_saved(oh)))
            manifest.append({"name": name, "task": task, "seed": seed, "miniwob_seed": ep["miniwob_seed"], "goal": ep["goal"],
                             "url_file": os.path.basename(ep["url"]), "reward": ep["reward"], "steps": len(ep["steps"]),
                             "actions": [s["action"] for s in ep["steps"]]})
            print(f"{name:22s} reward={ep['reward']}  {ep['goal'][:70]}")

    (out / "manifest.json").write_text(json.dumps(manifest, indent=1))
    solved = sum(1 for m in manifest if m["reward"] > 0)
    print(f"\n{solved}/{len(manifest)} episodes solved by BrowserGym itself")


main()
