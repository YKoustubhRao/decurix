"""Nine real native-app learned stages: Calc -> Writer -> Impress, three cycles.

Run INSIDE the same isolated VM after native_transfer.py has taught its lessons.
Every handoff reads the saved preceding file. Independent fixed seed expectations
check each result; a learned skill's self-report is insufficient.
"""
import argparse
import json
import re
import time
import uuid
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

from native_transfer import CASES, odf, reset

LESSONS = {}


def load_lessons(reports):
    """Use exact independently verified teacher lineage, never a mutable case alias."""
    from decurix_agent.computer import skills
    lessons = {}
    for path in reports:
        report = json.loads(Path(path).read_text())
        for row in report.get("results", []):
            if row.get("passed") and row.get("artifact_verified") and row.get("original_preserved"):
                learned = skills.load(row["skill"])
                if row["recording"] not in learned.get("sources", []):
                    raise ValueError("saved skill no longer matches the verified teacher recording")
                lessons[row["task"]] = row["skill"]
    needed = {"calc-values", "writer-form", "impress-replace"}
    if needed - set(lessons):
        raise ValueError("missing verified native lessons: " + ", ".join(sorted(needed - set(lessons))))
    return lessons


def slide_text(path):
    with zipfile.ZipFile(path) as archive:
        return "\n".join(node.text or "" for name in archive.namelist()
                         if re.fullmatch(r"ppt/slides/slide\d+\.xml", name)
                         for node in ET.fromstring(archive.read(name)).iter("{http://schemas.openxmlformats.org/drawingml/2006/main}t"))


def number_text(value):
    return str(int(value)) if float(value).is_integer() else str(value)


def summary(values):
    return "Values " + ",".join(number_text(value) for value in values) + " | Total " + number_text(sum(values))


def cases_by_id():
    return {case["id"]: case for case in CASES}


def expected_values(inputs, cycle):
    return [value + 10 * cycle for value in inputs["seed"]]


def definition(cycles):
    stages = []
    previous = None
    for cycle in range(cycles):
        for app, case, filename, fmt in [("calc", "calc-values", "input.ods", "ods"),
                                          ("writer", "writer-form", "input.odt", "odt"),
                                          ("impress", "impress-replace", "input.pptx", "pptx")]:
            id = f"round{cycle}-{app}"
            stages.append({"id": id, "app": app, "case": case, "cycle": cycle,
                           "instruction": "Run persisted native lesson with checked upstream values",
                           "depends_on": [previous] if previous else [], "retry_safe": True, "max_attempts": 2,
                           "checks": [{"path": f"{id}/{case}/{filename}", "format": fmt}]})
            previous = id
    return {"id": "native-learned-app-chain", "required_inputs": ["seed"], "steps": stages}


def path_for(root, cycle, app):
    case, extension = {"calc": ("calc-values", "ods"), "writer": ("writer-form", "odt"), "impress": ("impress-replace", "pptx")}[app]
    return root / f"round{cycle}-{app}" / case / ("input." + extension)


def calc_values(path):
    grid = odf.read_ods(path)["Sheet1"]
    return [float(odf.cell(grid, reference)["value"]) for reference in ("C2", "C3", "C4")]


def writer_values(path):
    _, tables = odf.read_odt(path)
    return [odf.cell(tables[0], reference)["text"] for reference in ("B1", "B2", "B3")]


def independent_verifier(stage, result, context):
    root = Path(context["work_dir"])
    values = expected_values(context["inputs"], stage["cycle"])
    if stage["app"] == "calc":
        valid = calc_values(path_for(root, stage["cycle"], "calc")) == values
    elif stage["app"] == "writer":
        valid = writer_values(path_for(root, stage["cycle"], "writer")) == ["Checked native rows", f"{sum(values):.2f}", summary(values)]
    else:
        text = slide_text(path_for(root, stage["cycle"], "impress"))
        valid = summary(values) in text and "ORIGINAL" not in text and "Preserve this source content" in text
    print(json.dumps({"event": "artifact_verified", "stage": stage["id"], "passed": valid}), flush=True)
    return valid


def execute(stage, context):
    from decurix_agent import task_context
    from decurix_agent.computer import core, replay, skill_engine, skills
    root, cycle, app = Path(context["work_dir"]), stage["cycle"], stage["app"]
    print(json.dumps({"event": "stage_started", "stage": stage["id"], "app": app}), flush=True)
    case = cases_by_id()[stage["case"]]
    if app == "calc":
        if cycle:
            previous = slide_text(path_for(root, cycle - 1, "impress"))
            values = [float(value) + 10 for value in re.search(r"Values ([\d.,-]+) \| Total", previous).group(1).split(",")]
        else:
            values = list(context["inputs"]["seed"])
        substitutions = dict(zip(case["demo"], [number_text(value) for value in values]))
    elif app == "writer":
        values = calc_values(path_for(root, cycle, "calc"))
        substitutions = dict(zip(case["demo"], ["Checked native rows", f"{sum(values):.2f}", summary(values)]))
    else:
        name, amount, description = writer_values(path_for(root, cycle, "writer"))
        if name != "Checked native rows" or float(amount) != sum(float(value) for value in re.search(r"Values ([\d.,-]+) \| Total", description).group(1).split(",")):
            raise ValueError("upstream Writer fields disagree")
        substitutions = {"Demo slide": description}
    source = reset(case, root / stage["id"])
    skill = skills.load(LESSONS[case["id"]])
    params = {}
    for parameter in skill["params"]:
        if parameter.get("varies"):
            sample = parameter.get("example", parameter.get("default"))
            params[parameter["name"]] = substitutions.get(str(sample), sample)
    recorded_sources = [step["open"] for step in skill["steps"] if step.get("kind") == "open"]
    if len(recorded_sources) != 1:
        raise ValueError("native lesson must expose exactly one recorded source open")
    bindings = {recorded_sources[0]: str(source)}
    task_context.start("t" + uuid.uuid4().hex[:16], "Native app chain " + stage["id"] +
                       "; requested params=" + json.dumps(params) + "; requested sources=" + json.dumps(bindings))
    result = skill_engine.run(skill, params=params, source_files=bindings)
    if result.get("outcome") not in replay.SUCCESS:
        raise RuntimeError(result.get("text", "learned native stage failed"))
    time.sleep(1)
    screen = Path("/home/headless/work/native-transfer-screens") / ("chain-" + stage["id"] + ".png")
    screen.parent.mkdir(parents=True, exist_ok=True)
    screen.write_bytes(core.observe()["frame"].png())
    task_context.start("", "")
    return {"outcome": result["outcome"], "skill": skill["name"], "screenshot": str(screen)}


def main():
    global LESSONS
    from decurix_agent.workflow_runner import WorkflowRunner
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("/home/headless/work/native-chain-runs"))
    parser.add_argument("--cycles", type=int, default=3)
    parser.add_argument("--max-stages", type=int, help="Stop at a durable checkpoint; resume in a fresh worker")
    parser.add_argument("--resume")
    parser.add_argument("--reports", type=Path, nargs="+", required=True,
                        help="Native result reports containing exact verified lesson/recording pairs")
    parser.add_argument("--output", type=Path, default=Path("/home/headless/work/native-chain.json"))
    args = parser.parse_args()
    if not 1 <= args.cycles <= 10:
        parser.error("cycles must be 1..10")
    if args.max_stages is not None and args.max_stages < 1:
        parser.error("max-stages must be positive")
    LESSONS = load_lessons(args.reports)
    runner = WorkflowRunner(args.root, execute, independent_verifier)
    state = runner.status(args.resume) if args.resume else runner.create(definition(args.cycles), {"seed": [101, 202, 303]})
    started = time.monotonic()
    state = runner.advance(state["run_id"], max_steps=args.max_stages)
    intentionally_paused = args.max_stages is not None and state["status"] == "pending"
    report = {"mode": "live_native_learned_multiapp_chain", "live_apps_tested": True,
              "elapsed_seconds": round(time.monotonic() - started, 3), "passed": state["status"] == "completed",
              "run_id": state["run_id"], "status": state["status"], "stages": state["steps"],
              "expected_stages": len(state["spec"]["steps"]),
              "stages_completed": sum(record["status"] == "completed" for record in state["steps"].values()),
              "completed": state["status"] == "completed", "intentionally_paused": intentionally_paused,
              "work_dir": runner.context(state)["work_dir"]}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2))
    print(json.dumps(report), flush=True)
    return 0 if report["passed"] or intentionally_paused else 1


if __name__ == "__main__":
    raise SystemExit(main())
