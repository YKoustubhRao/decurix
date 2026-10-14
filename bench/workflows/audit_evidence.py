"""Independent PM audit of exported live-browser report/recording/skill lineage."""
import argparse
import json
from pathlib import Path
from transfer import SCENARIOS


def audit(root):
    report = json.loads((root / "live-transfer-full.json").read_text(encoding="utf-8"))
    cases = {case["id"]:case for case in SCENARIOS}
    assert report["mode"] == "live_chromium_fixture_record_learn_transfer"
    assert report["live_apps_tested"] is True
    assert report["families"] == len(cases) == 25
    assert {row["task"] for row in report["results"]} == set(cases)
    assert len(report["results"]) == 25
    variants_checked, frames_checked = 0, set()
    for row in report["results"]:
        case = cases[row["task"]]
        directory = root / "browser-recordings" / row["recording"]
        meta = json.loads((directory / "meta.json").read_text(encoding="utf-8"))
        events = [json.loads(line) for line in (directory / "events.jsonl").read_text(encoding="utf-8").splitlines()]
        assert meta["id"] == row["recording"]
        assert meta["actors"] == ["agent"] and meta["status"] == "complete"
        assert meta["event_count"] == row["recorded_events"] == len(events) == 3
        assert [event["kind"] for event in events] == ["open", "type", "click"]
        assert all(event["outcome"] == "done" and event["actor"] == "agent" for event in events)
        assert events[1]["text"] == case["example"] == row["teacher_baseline"]["input"]
        assert case["example"] != case["holdout"]
        for event in events:
            for label in ("before", "after"):
                path = directory / event[label]
                assert path.read_bytes().startswith(b"\x89PNG\r\n\x1a\n"), path
                frames_checked.add(str(path))
        skill = json.loads((root / "browser-skills" / (row["skill"] + ".json")).read_text(encoding="utf-8"))
        assert skill["sources"] == [row["recording"]], (row["task"], skill["sources"], row["recording"])
        assert skill["status"] == "ready" and skill["demos"] == 1
        assert len(skill["steps"]) == 3
        assert skill["params"][0]["example"] == case["example"]
        assert skill["params"][0]["varies"] is True and skill["params"][0]["default"] is None
        assert skill["steps"][1]["target"]["name"] == "Source data", "healing mutated the saved lesson"
        assert {v["variant"] for v in row["variants"]} == {"holdout", "moved-layout", "renamed-control"}
        assert len(row["variants"]) == 3
        for variant in row["variants"]:
            assert variant["passed"] is True and variant["ui_verified"] is True
            assert variant["outcome"] == "verified", variant
            assert variant["actual"]["input"] == case["holdout"]
            assert variant["actual"]["result"] == variant["expected"] == case["expected"]
            remote = Path(variant["screenshot"])
            parts = remote.parts
            relative = Path(*parts[parts.index("transfer-screens") + 1:]) if "transfer-screens" in parts else Path(remote.name)
            assert ".." not in relative.parts and not relative.is_absolute()
            path = root / "browser-screens" / relative
            assert path.read_bytes().startswith(b"\x89PNG\r\n\x1a\n"), path
            variants_checked += 1
    assert variants_checked == 75 and report["passed"] is True
    return {"mode":"independent_exported_live_browser_audit", "passed":True, "families":25,
            "variants":variants_checked, "recordings":25, "matched_ready_skills":25,
            "recording_frames_checked":len(frames_checked), "screenshots_checked":75,
            "elapsed_live_seconds":report["elapsed_seconds"],
            "scope":"Real Chromium/CDP, local deterministic fixtures, agent-driven audit capture; no native Office claim"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).parent/"results")
    args = parser.parse_args()
    result = audit(args.root)
    (args.root / "pm-browser-audit.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
    print(json.dumps(result, indent=2))
