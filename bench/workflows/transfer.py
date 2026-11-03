"""Record -> learn -> persist -> replay holdouts against scripted application adapters.

No live application success is implied. Every family has a distinct business
operation, UI route, unseen input and independently specified expected result.
"""
import argparse
import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agent"))


def scenario(id, title, apps, route, example, holdout, expected, operation):
    example, holdout = example.replace("\n", "\\n"), holdout.replace("\n", "\\n")
    return {"id": id, "title": title, "apps": apps, "operations": route,
            "example": example, "holdout": holdout, "expected": expected, "operation": operation}


SCENARIOS = [
    scenario("reconcile", "Reconcile statement amounts", ["spreadsheet", "bank export"], ["open", "type", "select", "click"], "[10,20,-5]", "[105,20,-15]", 110, "sum"),
    scenario("deduplicate", "Deduplicate customer IDs", ["spreadsheet", "CRM export"], ["click", "type", "key", "click"], '["a","a","b"]', '["x","y","x","z","y"]', ["x","y","z"], "unique"),
    scenario("group-summary", "Group sales by region", ["spreadsheet", "report"], ["type", "select", "key", "click"], '[["N",2],["S",3]]', '[["E",4],["W",7],["E",6]]', {"E":10,"W":7}, "group"),
    scenario("variance", "Compare actual versus budget", ["spreadsheet", "presentation"], ["open", "type", "key", "select", "click"], "[20,15]", "[145,120]", 25, "difference"),
    scenario("mail-priority", "Classify urgent messages", ["mail", "ticket queue"], ["click", "type", "select", "click"], '["hello","urgent outage"]', '["invoice","URGENT access loss","urgent payment","thanks"]', ["URGENT access loss","urgent payment"], "urgent"),
    scenario("mail-merge", "Prepare personalized letter draft", ["contacts", "word processor", "mail"], ["open", "click", "type", "key", "click"], '["Alex","Welcome"]', '["Mina","Your renewal is ready"]', "Dear Mina: Your renewal is ready", "letter"),
    scenario("schedule", "Find first unoccupied calendar slot", ["calendar", "mail"], ["type", "click", "select", "key", "click"], "[[9,10],9]", "[[9,10,11,13],9]", 12, "slot"),
    scenario("document-template", "Fill document template", ["word processor", "spreadsheet"], ["open", "type", "key", "click", "click"], '["Offer for {name}","Alex"]', '["Appointment: {name}","Noor"]', "Appointment: Noor", "template"),
    scenario("redaction", "Redact personal email from document", ["word processor", "PDF"], ["click", "type", "select", "key", "click", "key"], '"Call a@example.test"', '"Contact mina@example.test today"', "Contact [REDACTED] today", "redact"),
    scenario("pdf-bundle", "Order document pages into review bundle", ["PDF viewer", "file manager"], ["open", "type", "select", "click", "key", "click"], '[[2,"B"],[1,"A"]]', '[[3,"Terms"],[1,"Cover"],[2,"Invoice"]]', ["Cover","Invoice","Terms"], "pages"),
    scenario("pdf-extraction", "Extract numbered clauses", ["PDF viewer", "document editor"], ["type", "key", "click", "select", "click"], '"1. one\nnotes"', '"intro\n1. Delivery\n2. Payment\nfooter"', ["1. Delivery","2. Payment"], "clauses"),
    scenario("slide-outline", "Create section slide outline", ["document editor", "presentation"], ["open", "type", "key", "click", "select", "key", "click"], '"# Intro\ntext"', '"# Results\nnotes\n# Risks\n# Decisions"', ["Results","Risks","Decisions"], "headings"),
    scenario("chart-series", "Prepare percentage chart series", ["spreadsheet", "presentation"], ["click", "type", "select", "key", "click", "click"], "[1,1]", "[20,30,50]", [20.0,30.0,50.0], "percent"),
    scenario("crm-match", "Match requested customer to CRM record", ["CRM", "browser"], ["open", "click", "type", "select", "click"], '[{"A":11},"A"]', '[{"B":22,"C":33},"C"]', 33, "lookup"),
    scenario("onboarding", "Identify missing joiner documents", ["HR app", "file manager"], ["click", "type", "key", "select", "click", "key", "click"], '[["id","offer"],["id"]]', '[["id","offer","tax"],["id","tax"]]', ["offer"], "missing"),
    scenario("ticket-escalation", "Escalate tickets beyond SLA", ["helpdesk", "mail"], ["open", "type", "select", "key", "click", "key"], '[[["a",5]],4]', '[[["T1",2],["T2",14],["T3",9]],8]', ["T2","T3"], "overdue"),
    scenario("quote-comparison", "Choose minimum comparable supplier quote", ["browser", "spreadsheet"], ["open", "click", "type", "key", "select", "click"], '[["A",10],["B",12]]', '[["N",25],["M",18],["P",22]]', "M", "cheapest"),
    scenario("file-rename", "Prepare deterministic batch rename manifest", ["file manager", "spreadsheet"], ["click", "type", "key", "click", "select", "click"], '["jan.pdf","feb.pdf"]', '["signed.pdf","draft.docx"]', {"signed.pdf":"001_signed.pdf","draft.docx":"002_draft.docx"}, "rename"),
    scenario("archive-manifest", "Summarize archive size by extension", ["archive manager", "spreadsheet"], ["open", "type", "click", "key", "select", "click"], '[["a.pdf",2]]', '[["a.pdf",7],["b.txt",3],["c.pdf",8]]', {"pdf":15,"txt":3}, "extensions"),
    scenario("csv-conversion", "Convert delimited rows to named records", ["spreadsheet", "database import"], ["type", "select", "click", "key", "click", "key"], '"id,name\n1,Alex"', '"id,name\n2,Mina\n3,Noor"', [{"id":"2","name":"Mina"},{"id":"3","name":"Noor"}], "csv"),
    scenario("database-filter", "Select active database records", ["database client", "spreadsheet"], ["open", "type", "key", "click", "key", "click"], '[["a",true]]', '[["B",false],["C",true],["D",true]]', ["C","D"], "active"),
    scenario("inventory", "Calculate reorder quantities", ["inventory app", "purchase draft"], ["click", "type", "select", "click", "key", "click"], '[["A",2,5]]', '[["SKU1",4,10],["SKU2",12,8]]', {"SKU1":6}, "reorder"),
    scenario("translation-review", "Check required terminology in translation", ["document editor", "review queue"], ["open", "click", "type", "key", "click", "select", "click"], '["hello world",["hello"]]', '["invoice payment pending",["invoice","refund"]]', ["refund"], "terms"),
    scenario("payroll-controls", "Detect pay above approved hours", ["payroll", "spreadsheet"], ["type", "key", "select", "click", "key", "click", "click"], '[["A",40,40]]', '[["E1",45,40],["E2",35,40],["E3",50,48]]', ["E1","E3"], "hours"),
    scenario("knowledge-draft", "Normalize FAQ into knowledge draft", ["support app", "knowledge editor"], ["open", "type", "click", "select", "key", "click", "key"], '[["Q?","A"]]', '[["Reset password?","Use settings"],["Cancel plan?","Contact support"]]', "## Reset password?\nUse settings\n\n## Cancel plan?\nContact support", "faq"),
]


def evaluate(operation, raw):
    """Scripted application behavior; holdout answers are separately fixed above."""
    import csv
    import io
    import re
    value = json.loads(raw)
    if operation == "sum": return sum(value)
    if operation == "unique": return list(dict.fromkeys(value))
    if operation == "group":
        out = {}
        for key, amount in value: out[key] = out.get(key, 0) + amount
        return out
    if operation == "difference": return value[0] - value[1]
    if operation == "urgent": return [x for x in value if "urgent" in x.lower()]
    if operation == "letter": return "Dear " + value[0] + ": " + value[1]
    if operation == "slot":
        busy, start = value
        while start in busy: start += 1
        return start
    if operation == "template": return value[0].replace("{name}", value[1])
    if operation == "redact": return re.sub(r"[\w.+-]+@[\w.-]+", "[REDACTED]", value)
    if operation == "pages": return [x[1] for x in sorted(value)]
    if operation == "clauses": return [x for x in value.splitlines() if re.match(r"^\d+\. ", x)]
    if operation == "headings": return [x[2:] for x in value.splitlines() if x.startswith("# ")]
    if operation == "percent": return [round(x * 100 / sum(value), 2) for x in value]
    if operation == "lookup": return value[0].get(value[1])
    if operation == "missing": return sorted(set(value[0]) - set(value[1]))
    if operation == "overdue": return [key for key, age in value[0] if age > value[1]]
    if operation == "cheapest": return min(value, key=lambda x: x[1])[0]
    if operation == "rename": return {name: f"{i:03}_{name}" for i, name in enumerate(value, 1)}
    if operation == "extensions":
        out = {}
        for name, size in value:
            ext = name.rsplit(".", 1)[-1]
            out[ext] = out.get(ext, 0) + size
        return out
    if operation == "csv": return list(csv.DictReader(io.StringIO(value)))
    if operation == "active": return [key for key, active in value if active]
    if operation == "reorder": return {key: target-stock for key, stock, target in value if stock < target}
    if operation == "terms": return [term for term in value[1] if term not in value[0]]
    if operation == "hours": return [key for key, paid, approved in value if paid > approved]
    if operation == "faq": return "\n\n".join("## " + q + "\n" + a for q, a in value)
    raise ValueError(operation)


def element(role, name, offset=0):
    return {"role": role, "name": name, "value": "", "states": [], "box": [10+offset,20+offset,160,24],
            "path": f'workspace > {role} "{name}"', "source": "cdp", "group": "workspace", "window_title": "Workflow Lab"}


def demonstration(case):
    events = []
    for index, kind in enumerate(case["operations"]):
        item = {"i": index, "kind": kind, "actor": "human", "outcome": "done", "window": "Workflow Lab",
                "url": "http://workflow.test/", "screen": [1280,720]}
        if kind == "open": item["target"] = "http://workflow.test/"
        elif kind == "type":
            item.update(element=element("textbox", "Source data"), text=case["example"],
                        expect={"values":[{"role":"textbox","name":"Source data","value":case["example"]}]})
        elif kind == "select": item.update(element=element("combobox", "Output mode"), option="Review")
        elif kind == "key": item["keys"] = ["ctrl+s"]
        else: item["element"] = element("button", "Run operation" if index == max(i for i,k in enumerate(case["operations"]) if k == "click") else "Prepare workspace")
        if index == len(case["operations"])-1:
            item["expect"] = {"appeared":[{"role":"status","name":"Result ready"}]}
        events.append(item)
    return events


class ScriptedApplication:
    def __init__(self, case, offset=0, rename=False, ambiguous=False):
        self.case, self.result, self.actions = case, None, []
        self.elements = [element("textbox", "Source data:" if rename else "Source data", offset),
                         element("combobox", "Output mode", offset), element("button", "Prepare workspace", offset),
                         element("button", "Run operation", offset)]
        if ambiguous:
            self.elements[0]["path"] = "first unfamiliar field"
            self.elements += [dict(self.elements[0], path="different field")]

    def observe(self, remember=True):
        return {"elements": self.elements, "windows":[{"title":"Workflow Lab"}], "active_title":"Workflow Lab",
                "size":[1280,720], "page_url":"http://workflow.test/"}

    def active_group(self, observation): return "workspace"

    def open_target(self, target, **kwargs):
        self.actions.append(("open", target))
        return self.observe()

    def act(self, kind, **kwargs):
        self.actions.append((kind, kwargs))
        if kind == "type":
            for field in self.elements:
                if field["path"] == kwargs.get("path"): field["value"] = kwargs["text"]
        if kind == "click" and "Run operation" in kwargs.get("path", ""):
            raw = next(field["value"] for field in self.elements if field["role"] == "textbox")
            self.result = evaluate(self.case["operation"], raw)
            self.elements.append(element("status", "Result ready"))
        return {"status":"done", "text":"ok", "observation":self.observe()}


def learn(case, root):
    from decurix_agent.computer import recording, learning, skills
    session = "demo-transfer-" + case["id"]
    if not (recording.ROOT / session / "meta.json").exists():
        recording.ensure(session, title=case["title"])
        for event in demonstration(case): recording.append(session, event)
        recording.update(session, status="complete")
    meta, events = recording.load(session)
    learned = learning.build([(meta, events)])
    for _ in range(100):
        question = learning.next_question(learned)
        if question is None: break
        answers = {"goal":"skip", "start":"no", "variable":"changes", "variables":"1 changes", "approval":"go ahead",
                   "confirm":"yes", "optional":"always", "coordinates":"keep", "unsupported":"manual", "loop":"fixed"}
        response = learning.answer(learned, question, answers[question["kind"]])
        if response.get("decision"): break
    skills.save(learned)
    return skills.load(learned["name"])


def task_id_for(case, variant):
    """Each run's own task id, in the control plane's shape ("t" + 16 hex):
    the agent attributes every outcome strictly to the task it names, so two
    runs sharing one would collide."""
    return "t" + hashlib.sha256(f"{case['id']}|{variant}".encode()).hexdigest()[:16]


def run_case(case, root, variant="holdout", learned=None):
    from decurix_agent import task_context
    from decurix_agent.computer import replay, skill_engine
    learned = learned or learn(case, root)
    task_context.start(task_id_for(case, variant), request=case["title"])
    app = ScriptedApplication(case, offset=400 if variant == "moved-layout" else 0,
                              rename=variant == "renamed-control", ambiguous=variant == "ambiguous-control")
    params = {p["name"]:case["holdout"] for p in learned["params"] if p.get("varies")}
    if variant == "missing-parameter": params = {}
    with patch.object(replay, "core", app), patch.object(replay, "FIND_SECONDS", 0), \
            patch.object(replay, "EXPECT_SECONDS", 0), patch.object(replay, "POLL_SECONDS", 0), \
            patch.object(skill_engine, "core", app), patch.object(skill_engine, "other_system", return_value=""), \
            patch.object(replay.progress, "emit", lambda *args, **kwargs: None):
        outcome = skill_engine.run(learned, params=params)
    business_ok = app.result == case["expected"]
    # A variant that must stop passes only when the replay did not report
    # success. (A "wrong-business-result" variant was removed: its scripted app
    # always answered wrongly and the row passed on the bench's own check of
    # that answer, whatever the worker did, so it measured nothing.)
    must_stop = variant in {"ambiguous-control", "missing-parameter"}
    passed = outcome["outcome"] not in replay.SUCCESS if must_stop else \
             (outcome["outcome"] in replay.SUCCESS and business_ok)
    return {"task":case["id"], "task_id":task_id_for(case, variant), "variant":variant, "passed":passed,
            "replay_outcome":outcome["outcome"],
            "business_verified":business_ok, "actions":len(app.actions), "recordings":1,
            "typed_values":[args["text"] for kind,args in app.actions if kind == "type"],
            "skill_status":learned["status"], "parameters":params, "reason":outcome.get("text", "")}


def benchmark(cases=SCENARIOS):
    with tempfile.TemporaryDirectory(prefix="workflow-transfer-") as tmp:
        root = Path(tmp)
        with patch.dict(os.environ, {"DECURIX_DIR":str(root/"runtime")}):
            from decurix_agent import task_context
            from decurix_agent.computer import recording, skills
        # The current task lives in this run's own file, never the machine-wide one.
        with patch.object(recording, "ROOT", root/"recordings"), patch.object(skills, "ROOT", root/"skills"), \
                patch.object(task_context, "CURRENT_FILE", str(root/"current-task")):
            results = []
            for case in cases:
                learned = learn(case, root)
                for variant in ("holdout", "moved-layout", "renamed-control", "ambiguous-control", "missing-parameter"):
                    results.append(run_case(case, root, variant, learned=skills.load(learned["name"])))
    return {"mode":"scripted_application_learning_transfer", "live_apps_tested":False,
            "families":len(cases), "demonstrations":len(cases), "persisted_skills":len(cases),
            "evaluations":len(results), "passed":all(r["passed"] for r in results), "results":results}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    report = benchmark()
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({k:v for k,v in report.items() if k != "results"}, indent=2))
    for result in report["results"]:
        if not result["passed"]: print(result["task"], result["variant"], result["replay_outcome"], result["reason"][:250])
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
