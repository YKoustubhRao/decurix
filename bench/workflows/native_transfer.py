"""Live native-app record/learn/holdout tests, executed INSIDE an isolated VM.

Uses actual core/audit actions and persisted learned skills; only fixtures and
independent artifact readers use file APIs. No model or fabricated action log.
Run after copying bench/gui/fixtures/odf.py beside the benchmark tree.
"""
import argparse
import json
import subprocess
import sys
import time
import uuid
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "gui" / "fixtures"))
import odf


CASES = [
    {"id": "calc-values", "app": "calc", "demo": {"11": "101", "22": "202", "33": "303"}},
    {"id": "calc-formula", "app": "calc", "demo": {"=SUM(B2:B4)": "=SUM(B2:B4)*2"}},
    {"id": "calc-sheet-name", "app": "calc", "demo": {"Demo review": "Holdout review"}},
    {"id": "writer-form", "app": "writer", "demo": {"Alex Example": "Mina Holdout", "17.50": "94.25", "Demo visit": "Unseen review"}},
    {"id": "writer-replace", "app": "writer", "demo": {"Demo replacement": "Holdout replacement"}},
    {"id": "impress-replace", "app": "impress", "demo": {"Demo slide": "Holdout slide"}},
    {"id": "file-manager-rename", "app": "thunar", "demo": {"demo-final.txt": "holdout-final.txt"}},
]


def wait_for(predicate, seconds=25):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.25)
    raise TimeoutError("native app did not reach the expected state")


def action(kind, **kwargs):
    from decurix_agent.computer import core
    result = core.act(kind, **kwargs)
    if result.get("status") != "done":
        raise RuntimeError(json.dumps(result, default=str)[:1500])
    return result


def key(*keys):
    return action("key", keys=list(keys))


def named(kind, names, roles, text=None):
    """Resolve only a unique accessible native control, never click by guessed position."""
    from decurix_agent.computer import core
    def locate():
        elements = core.observe().get("elements", [])
        candidates = [e for e in elements if e.get("role") in roles and
                      str(e.get("name", "")).strip().rstrip(":").lower() in {n.lower() for n in names}]
        return candidates[0] if len(candidates) == 1 else None
    target = wait_for(locate)
    return action(kind, path=target["path"], **({"text": text} if text is not None else {}))


def reset(case, root, holdout=False):
    """Replace only this benchmark's generated fixture after closing its native app."""
    app = case["app"]
    executables = ["soffice.bin"] if app in {"calc", "writer", "impress"} else ["Thunar", "thunar"]
    for executable in executables:
        subprocess.run(["pkill", "-TERM", "-x", executable], capture_output=True, timeout=10)
    time.sleep(0.75)
    folder = root / case["id"]
    folder.mkdir(parents=True, exist_ok=True)
    if app == "calc":
        path = folder / ("unseen.ods" if holdout else "input.ods")
        odf.write_ods(path, "Sheet1", [["Item", "Amount", "Output"], ["A", 10, ""], ["B", 20, ""], ["C", 30, ""]])
    elif app == "writer":
        path = folder / ("unseen.odt" if holdout else "input.odt")
        odf.write_odt(path, "Native review", "ORIGINAL text for independent verification", [["Name", ""], ["Amount", ""], ["Purpose", ""]])
    elif app == "impress":
        from pptx import Presentation
        path = folder / ("unseen.pptx" if holdout else "input.pptx")
        deck = Presentation()
        slide = deck.slides.add_slide(deck.slide_layouts[1])
        slide.shapes.title.text = "ORIGINAL"
        slide.placeholders[1].text = "Preserve this source content"
        deck.save(path)
    else:
        path = folder
        for name in ("draft.txt", "demo-final.txt", "holdout-final.txt"):
            item = folder / name
            if item.exists():
                item.unlink()
        (folder / "draft.txt").write_text("Independent original content\n")
    return path


def demonstrate(case):
    id = case["id"]
    if id == "calc-values":
        for cell, value in zip(("C2", "C3", "C4"), ("11", "22", "33")):
            action("type", role="cell", name=cell, text=value)
        key("ctrl+s")
    elif id == "calc-formula":
        action("type", role="cell", name="C2", text="=SUM(B2:B4)")
        key("ctrl+s")
    elif id == "calc-sheet-name":
        action("double_click", role="tab", name="Sheet1")
        named("type", ["Name"], ["textbox"], "Demo review")
        key("enter")
        key("ctrl+s")
    elif id == "writer-form":
        for cell, value in zip(("B1", "B2", "B3"), ("Alex Example", "17.50", "Demo visit")):
            action("type", role="cell", name=cell, text=value)
        key("ctrl+s")
    elif id in {"writer-replace", "impress-replace"}:
        key("ctrl+h")
        named("type", ["Find", "Search for"], ["textbox", "combobox"], "ORIGINAL")
        named("type", ["Replace", "Replace with"], ["textbox", "combobox"], "Demo replacement" if id == "writer-replace" else "Demo slide")
        named("click", ["Replace All"], ["button"])
        # LibreOffice can open a replacement-count information dialog.
        from decurix_agent.computer import core
        if any(e.get("role") == "button" and e.get("name") == "OK" for e in core.observe().get("elements", [])):
            action("click", role="button", name="OK")
        named("click", ["Close"], ["button"])
        key("ctrl+s")
        if id == "impress-replace":
            time.sleep(1)
            for e in core.observe().get("elements", []):
                if e.get("role") == "button" and "Use PowerPoint" in e.get("name", ""):
                    action("click", path=e["path"])
                    break
    else:
        action("click", role="cell", name="draft.txt")
        key("F2")
        named("type", ["Name", ""], ["textbox"], "demo-final.txt")
        named("click", ["Rename"], ["button"])
    time.sleep(1.5)


def verify(case, path, holdout):
    id = case["id"]
    if case["app"] == "calc":
        sheets = odf.read_ods(path)
        if id == "calc-sheet-name":
            return list(sheets) == ["Holdout review" if holdout else "Demo review"], list(sheets)
        grid = sheets["Sheet1"]
        if id == "calc-values":
            cells = [odf.cell(grid, ref) for ref in ("C2", "C3", "C4")]
            actual = [float(cell.get("value")) for cell in cells]
            expected = [101, 202, 303] if holdout else [11, 22, 33]
            return actual == expected, actual
        cell = odf.cell(grid, "C2")
        return float(cell.get("value", "nan")) == (120 if holdout else 60) and "SUM" in cell.get("formula", ""), cell
    if case["app"] == "writer":
        paragraphs, tables = odf.read_odt(path)
        if id == "writer-form":
            values = [odf.cell(tables[0], ref).get("text") for ref in ("B1", "B2", "B3")]
            return values == (list(case["demo"].values()) if holdout else list(case["demo"])), values
        text = "\n".join(paragraphs)
        expected = "Holdout replacement" if holdout else "Demo replacement"
        return expected in text and "ORIGINAL" not in text, text
    if case["app"] == "impress":
        with zipfile.ZipFile(path) as archive:
            text = "\n".join(node.text or "" for name in archive.namelist() if name.startswith("ppt/slides/slide") and name.endswith(".xml")
                             for node in ET.fromstring(archive.read(name)).iter("{http://schemas.openxmlformats.org/drawingml/2006/main}t"))
        expected = "Holdout slide" if holdout else "Demo slide"
        return expected in text and "ORIGINAL" not in text and "Preserve this source content" in text, text
    output = path / ("holdout-final.txt" if holdout else "demo-final.txt")
    valid = output.is_file() and output.read_text() == "Independent original content\n" and not (path / "draft.txt").exists()
    return valid, sorted(item.name for item in path.iterdir())


def learn(case, session):
    from decurix_agent.computer import recording, learning, skills
    meta, events = recording.load(session)
    skill = learning.build([(meta, events)])
    for _ in range(100):
        question = learning.next_question(skill)
        if question is None:
            break
        answers = {"goal": "the generated native document is saved with the requested values", "start": "no",
                   "variable": "changes", "variables": "all change", "confirm": "yes", "optional": "always",
                   "coordinates": "keep", "unsupported": "manual", "loop": "fixed", "approval": "go ahead"}
        learning.answer(skill, question, answers[question["kind"]])
    skill["name"] = "native-transfer-" + case["id"]
    skills.save(skill)
    return skills.load(skill["name"]), len(events)


def run_case(case, root):
    from decurix_agent import task_context
    from decurix_agent.computer import core, recording, replay, skill_engine
    path = reset(case, root)
    task_context.start("t" + uuid.uuid4().hex[:16], "Native transfer " + case["id"] +
                       "; source=" + str(path) + "; demonstrated values=" + json.dumps(list(case["demo"])))
    core.open_target(str(path))
    wait_for(lambda: core.observe().get("elements"))
    demonstrate(case)
    baseline_ok, baseline = verify(case, path, False)
    if not baseline_ok:
        raise AssertionError("teacher failed independent artifact verification: " + str(baseline))
    session = recording.session_for_task(task_context.current())
    recording.update(session, status="complete", title=case["id"])
    task_context.start("", "")
    skill, events = learn(case, session)
    if skill["status"] != "ready":
        raise AssertionError("lesson is not ready: " + skill["status"])
    demonstration_path = path
    path = reset(case, root, holdout=True)
    params = {}
    for parameter in skill["params"]:
        if parameter.get("varies"):
            sample = parameter.get("example", parameter.get("default"))
            params[parameter["name"]] = case["demo"].get(str(sample), sample)
    if not any(str(value) in case["demo"].values() for value in params.values()):
        raise AssertionError("recorded skill exposes none of the intended changed values")
    bindings = {str(demonstration_path): str(path)} if case["app"] != "thunar" else {}
    task_context.start("t" + uuid.uuid4().hex[:16], "Native holdout " + case["id"] +
                       "; requested params=" + json.dumps(params) + "; requested sources=" + json.dumps(bindings))
    outcome = skill_engine.run(skill, params=params, source_files=bindings)
    time.sleep(1.5)
    passed, actual = verify(case, path, True)
    original_preserved = verify(case, demonstration_path, False)[0] if bindings else True
    screenshot = Path("/home/headless/work/native-transfer-screens") / (case["id"] + ".png")
    screenshot.parent.mkdir(parents=True, exist_ok=True)
    screenshot.write_bytes(core.observe()["frame"].png())
    task_context.start("", "")
    return {"task": case["id"], "app": case["app"], "recording": session, "recorded_events": events,
            "skill": skill["name"], "outcome": outcome.get("outcome"), "detail": outcome.get("text", "")[:1500],
            "artifact_verified": passed, "actual": actual, "teacher_actual": baseline, "source_file_bindings": bindings,
            "original_preserved": original_preserved,
            "passed": passed and original_preserved and outcome.get("outcome") in replay.SUCCESS, "screenshot": str(screenshot)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--only", help="Comma-separated native case IDs")
    parser.add_argument("--root", type=Path, default=Path("/home/headless/work") / ("native-transfer-" + uuid.uuid4().hex[:8]))
    parser.add_argument("--output", type=Path, default=Path("/home/headless/work/native-transfer.json"))
    args = parser.parse_args()
    cases = [case for case in CASES if not args.only or case["id"] in args.only.split(",")]
    if not cases:
        parser.error("No selected native cases")
    args.root.mkdir(parents=True, exist_ok=True)
    results = []
    for case in cases:
        try:
            row = run_case(case, args.root)
        except Exception as exc:
            row = {"task": case["id"], "app": case["app"], "passed": False, "error": str(exc)}
            try:
                from decurix_agent.computer import core
                observation = core.observe()
                folder = Path("/home/headless/work/native-transfer-screens")
                folder.mkdir(parents=True, exist_ok=True)
                image = folder / (case["id"] + "-failure.png")
                image.write_bytes(observation["frame"].png())
                tree = folder / (case["id"] + "-failure.json")
                tree.write_text(json.dumps({key: value for key, value in observation.items() if key != "frame"}, default=str, indent=2))
                row.update(screenshot=str(image), observation=str(tree))
            except Exception as evidence_error:
                row["evidence_error"] = str(evidence_error)
        results.append(row)
        print(json.dumps(row, default=str), flush=True)
        report = {"mode": "live_native_record_learn_holdout", "live_apps_tested": True,
                  "root": str(args.root), "passed": all(row["passed"] for row in results), "results": results}
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2, default=str))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
