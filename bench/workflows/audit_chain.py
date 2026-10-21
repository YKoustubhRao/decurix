"""PM audit of nine saved native-app stages and non-repeating checkpoint resume."""
import argparse
import hashlib
import json
from pathlib import Path
from audit_native import OFFICE, cells, xml


def events(path):
    raw = path.read_bytes()
    text = raw.decode("utf-16") if raw.startswith(b"\xff\xfe") else raw.decode("utf-8")
    return [json.loads(line) for line in text.splitlines() if line.startswith("{")]


def audit(root):
    before = json.loads((root / "native-chain-checkpoint.json").read_text())
    final = json.loads((root / "native-chain-final.json").read_text())
    saved = json.loads((root / "native-chain-run/checkpoint.json").read_text())
    stage_ids = [f"round{cycle}-{app}" for cycle in range(3) for app in ("calc", "writer", "impress")]
    assert before["run_id"] == final["run_id"] == saved["run_id"]
    assert before["status"] == "pending" and before["intentionally_paused"] is True
    assert before["stages_completed"] == 3 and before["completed"] is False and before["passed"] is False
    assert all(before["stages"][key]["status"] == "completed" and before["stages"][key]["attempts"] == 1 for key in stage_ids[:3])
    assert all(before["stages"][key]["status"] == "pending" and before["stages"][key]["attempts"] == 0 for key in stage_ids[3:])
    assert final["status"] == saved["status"] == "completed" and final["passed"] and final["completed"]
    assert final["stages_completed"] == final["expected_stages"] == 9
    assert set(final["stages"]) == set(saved["steps"]) == set(stage_ids)
    assert saved["inputs"]["seed"] == [101,202,303]
    artifacts = root / "native-chain-run/artifacts"
    for key in stage_ids:
        record = final["stages"][key]
        assert record == saved["steps"][key]
        assert record["status"] == "completed" and record["attempts"] == 1 and len(record["evidence"]) == 1
        if key in stage_ids[:3]:
            assert record["evidence"] == before["stages"][key]["evidence"], "completed stage changed after resume"
        evidence = record["evidence"][0]
        relative = Path(evidence["path"])
        assert not relative.is_absolute() and ".." not in relative.parts
        data = (artifacts / relative).read_bytes()
        assert len(data) == evidence["bytes"] and hashlib.sha256(data).hexdigest() == evidence["sha256"]
    for cycle, values in enumerate(([101,202,303], [111,212,313], [121,222,323])):
        grid = cells(artifacts / f"round{cycle}-calc/calc-values/input.ods")
        assert [float(grid[i][2].get(OFFICE + "value")) for i in (1,2,3)] == values
        summary = "Values " + ",".join(str(x) for x in values) + " | Total " + str(sum(values))
        table = cells(artifacts / f"round{cycle}-writer/writer-form/input.odt")
        assert ["".join(table[i][1].itertext()) for i in (0,1,2)] == ["Checked native rows", f"{sum(values):.2f}", summary]
        tree = xml(artifacts / f"round{cycle}-impress/impress-replace/input.pptx", "ppt/slides/slide1.xml")
        text = "\n".join(node.text or "" for node in tree.iter("{http://schemas.openxmlformats.org/drawingml/2006/main}t"))
        assert summary in text and "Preserve this source content" in text and "ORIGINAL" not in text
    resumed = events(root / "native-chain-resume-worker.log")
    starts = [row["stage"] for row in resumed if row.get("event") == "stage_started"]
    assert starts == stage_ids[3:], "resume replayed completed stages or missed downstream work"
    verdicts = [row for row in resumed if row.get("event") == "artifact_verified"]
    assert [row["stage"] for row in verdicts] == stage_ids[3:] and all(row["passed"] for row in verdicts)
    screenshots = list((root / "native-chain-run/screens").glob("chain-*.png"))
    if screenshots:
        assert len(screenshots) == 9 and all(path.read_bytes().startswith(b"\x89PNG\r\n\x1a\n") for path in screenshots)
    return {"mode":"independent_nine_stage_native_chain_audit", "passed":True, "stages":9,
            "apps":["LibreOffice Calc","LibreOffice Writer","LibreOffice Impress"],
            "pre_resume_completed_stages":3, "resumed_stages":6,
            "all_stage_attempts":1, "unchanged_pre_resume_artifact_hashes":3,
            "reopened_and_hashed_artifacts":9, "final_totals":[606,636,666],
            "active_seconds":round(before["elapsed_seconds"] + final["elapsed_seconds"], 3),
            "saved_screenshots":len(screenshots),
            "scope":"Three cycles of real learned native-app stages; prior stages not repeated on resumed worker"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).parent / "results")
    args = parser.parse_args()
    result = audit(args.root)
    (args.root / "pm-chain-audit.json").write_text(json.dumps(result, indent=2))
    print(json.dumps(result, indent=2))
