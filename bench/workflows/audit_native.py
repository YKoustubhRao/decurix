"""PM audit: exported native files, teacher/skill lineage and GUI task verdicts."""
import argparse
import hashlib
import json
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

TABLE = "{urn:oasis:names:tc:opendocument:xmlns:table:1.0}"
OFFICE = "{urn:oasis:names:tc:opendocument:xmlns:office:1.0}"
TEXT = "{urn:oasis:names:tc:opendocument:xmlns:text:1.0}"


def xml(path, member="content.xml"):
    with zipfile.ZipFile(path) as package:
        return ET.fromstring(package.read(member))


def cells(path):
    table = next(xml(path).iter(TABLE + "table"))
    return [list(row.iter(TABLE + "table-cell")) for row in table.iter(TABLE + "table-row")]


def paragraphs(path):
    return "\n".join("".join(node.itertext()) for node in xml(path).iter(TEXT + "p"))


def audit(root):
    report = json.loads((root / "native-consolidated.json").read_text())
    expected = {"calc-values", "calc-formula", "calc-sheet-name", "writer-form", "writer-replace", "impress-replace", "file-manager-rename"}
    assert report["passed"] and report["completed"] and report["expected_cases"] == 7
    assert len(report["results"]) == 7 and {row["task"] for row in report["results"]} == expected
    for attempt in report["attempt_reports"]:
        assert hashlib.sha256(Path(attempt["path"]).read_bytes()).hexdigest() == attempt["sha256"]
    base = root / "native-qualified"
    for row in report["results"]:
        assert row["passed"] and row["artifact_verified"] and row["original_preserved"]
        assert row["outcome"] == "completed_unverified"  # Saved-file proof is separate from screen-step coverage.
        events = [json.loads(line) for line in (base / "recordings" / row["recording"] / "events.jsonl").read_text().splitlines()]
        assert len(events) == row["recorded_events"]
        assert all(e.get("outcome") == "done" for e in events)
        skill = json.loads((base / "skills" / (row["skill"] + ".json")).read_text())
        assert skill["status"] == "ready" and skill["sources"] == [row["recording"]]
        assert (base / "screens" / Path(row["screenshot"]).name).read_bytes().startswith(b"\x89PNG\r\n\x1a\n")
    artifacts = base / "artifacts"
    for filename, values in (("input.ods", [11,22,33]), ("unseen.ods", [101,202,303])):
        rows = cells(artifacts / "calc-values" / filename)
        assert [float(rows[i][2].get(OFFICE + "value")) for i in (1,2,3)] == values
    for filename, value, formula in (("input.ods", 60, "of:=SUM([.B2:.B4])"), ("unseen.ods", 120, "of:=SUM([.B2:.B4])*2")):
        cell = cells(artifacts / "calc-formula" / filename)[1][2]
        assert float(cell.get(OFFICE + "value")) == value and cell.get(TABLE + "formula") == formula
    for filename, name in (("input.ods", "Demo review"), ("unseen.ods", "Holdout review")):
        assert next(xml(artifacts / "calc-sheet-name" / filename).iter(TABLE + "table")).get(TABLE + "name") == name
    for filename, values in (("input.odt", ["Alex Example","17.50","Demo visit"]), ("unseen.odt", ["Mina Holdout","94.25","Unseen review"])):
        rows = cells(artifacts / "writer-form" / filename)
        assert ["".join(rows[i][1].itertext()) for i in (0,1,2)] == values
    for filename, marker in (("input.odt", "Demo replacement text"), ("unseen.odt", "Holdout replacement text")):
        assert marker in paragraphs(artifacts / "writer-replace" / filename)
    for filename, marker in (("input.pptx", "Demo slide"), ("unseen.pptx", "Holdout slide")):
        tree = xml(artifacts / "impress-replace" / filename, "ppt/slides/slide1.xml")
        text = "\n".join(node.text or "" for node in tree.iter("{http://schemas.openxmlformats.org/drawingml/2006/main}t"))
        assert marker in text and "Preserve this source content" in text
    files = [p.name for p in (artifacts / "file-manager-rename").iterdir() if not p.name.startswith(".")]
    assert files == ["holdout-final.txt"]
    assert (artifacts / "file-manager-rename/holdout-final.txt").read_text() == "Independent original content\n"
    gui_path = sorted((root / "gui-oracle").glob("*.json"))[-1]
    gui = json.loads(gui_path.read_text())["engines"]["oracle"]
    assert gui["passed"] == gui["total"] == len(gui["tasks"]) == 20
    assert len({row["task"] for row in gui["tasks"]}) == 20
    assert all(row["pass"] and row["check"]["pass"] for row in gui["tasks"])
    return {"mode":"independent_native_and_gui_audit", "passed":True, "native_workflows":7,
            "teacher_skill_lineages":7, "saved_artifact_families_reopened":6, "file_rename_verified":True,
            "native_screen_outcome":"completed_unverified; independent saved-file checks passed",
            "gui_scripted_oracle_tasks":20, "gui_apps":sorted({row["app"] for row in gui["tasks"]}),
            "scope":"LibreOffice Calc/Writer/Impress and Thunar transfer; scripted GUI solvability, not LLM autonomy"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).parent / "results")
    args = parser.parse_args()
    result = audit(args.root)
    (args.root / "pm-native-gui-audit.json").write_text(json.dumps(result, indent=2))
    print(json.dumps(result, indent=2))
