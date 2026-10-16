"""Reproduce the PM audit of one-demo API teaching, correction and resumed replay."""
import argparse
import json
from pathlib import Path


def audit(root):
    skill = json.loads((root / "api-skills/add-a-customer.json").read_text())
    assert skill["status"] == "ready" and skill["version"] == 3 and skill["runs"] == 3
    assert skill["sources"] == ["task-t00000000000000a1"] and skill["demos"] == 1
    assert skill["params"][0]["varies"] is True and skill["params"][0]["default"] is None
    assert skill["steps"][2]["target"]["name"] == "Go"
    source = root / "api-recordings" / skill["sources"][0]
    events = [json.loads(line) for line in (source / "events.jsonl").read_text().splitlines()]
    assert len(events) == 3 and [e["kind"] for e in events] == ["open", "type", "click"]
    assert events[1]["text"] == "Acme" and events[2]["element"]["name"] == "Save"
    runs = [[json.loads(line) for line in path.read_text().splitlines()]
            for path in (root / "api-recordings").glob("*/events.jsonl") if path.parent.name != skill["sources"][0]]
    assert len(runs) == 3
    names = set()
    for rows in runs:
        typed = [event["text"] for event in rows if event["kind"] == "type"]
        assert len(typed) == 1
        name = typed[0]
        names.add(name)
        assert any("Saved " + name in json.dumps(event.get("expect", {})) for event in rows)
        if name == "Wayne":
            assert len(rows) == 3 and [event["replay_of"]["step"] for event in rows] == [1, 2, 3]
            assert rows[-1]["element"]["name"] == "Go" and rows[-1]["outcome"] == "done"
    assert names == {"Initech", "Umbrella", "Wayne"}
    raw = (root / "skill-api-verified.log").read_bytes()
    log = raw.decode("utf-16") if raw.startswith(b"\xff\xfe") else raw.decode("utf-8")
    assert "END TO END: OK" in log and "steps 3-3 ran" in log
    return {"mode":"independent_api_skill_lineage_audit", "passed":True, "demonstrations":1,
            "ready_skill_version":3, "successful_runs":3, "teacher_actions":3,
            "learned_final_button":"Go", "resumed_wayne_typed_once":True,
            "scope":"API teaching, live Chromium, agent-driven audit capture; source record metadata is task-kind"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).parent / "results")
    args = parser.parse_args()
    result = audit(args.root)
    (args.root / "pm-api-audit.json").write_text(json.dumps(result, indent=2))
    print(json.dumps(result, indent=2))
