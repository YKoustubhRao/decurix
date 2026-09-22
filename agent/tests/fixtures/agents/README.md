Real trajectories: BrowserGym 0.14.3 solving MiniWoB++ tasks in Playwright's Chromium,
made by bench/agents/gen_trajectories.py. Nothing here is hand-written.

- `*.browsergym.json`      an AgentLab-style trajectory: each action with the accessibility tree it was chosen from
- `*.openhands.json`       the same run as OpenHands' event stream, with each browse observation's `axtree_object`
- `*.openhands-saved.json` what OpenHands' --save-trajectory keeps: the same events with the trees removed
