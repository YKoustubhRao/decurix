"""
Run every code check a planned stage goes through on a synthetic stage (independent review, 2026-10-21): the same
long_task.stage_code_findings the live run and the replay gate call, over a throwaway planned run in a temp folder.
For tests that build correct (or wrong) work in code and assert what blocks - e.g. test_correct_work_never_refused.

    from stage_check_harness import run_checks
    found = run_checks({"title": "Welcome drafts", "does": "Draft a welcome email to each joiner."},
                       {"joiners.csv": "Name,Email\\nPriya Nair,priya@example.com\\n"},
                       request="Draft a welcome email to each joiner in joiners.csv.",
                       drafts=[{"to": ["priya@example.com"], "subject": "Welcome", "body": "..."}])
    blocking = [f for check, f, blocks in found if blocks]

Not a test module (no test_ prefix): unittest discovery never imports it on its own.
"""

import tempfile
from pathlib import Path
from unittest import mock

import test_long_requests as base  # noqa: F401 - stubs config first

from decurix_agent import config, long_task, stage_checks, workflow_task
from decurix_agent.workflow_runner import WorkflowRunner


def run_checks(stage, files, request="", reply="Done.", drafts=(), amendments=(), decisions=(), tool_record=None,
               inputs=None, typed=(), page_notes=()):
    """
    [(check, finding, blocking)] of one synthetic stage.

    stage       {"title", "does", optional "enters", "drafts", "decision", "outputs"}: the planner's stage. "outputs"
                defaults to the names in `files` that are not `inputs`.
    files       {relative path: text or bytes}: the work folder - the request's inputs and what the stage made.
    inputs      the paths in `files` that are inputs (default: those the request names).
    request     the person's request; reply the stage's report; drafts its email drafts (office drafts' dicts).
    amendments  ledger amendments ({"text", "removed", "restored"}); decisions ledger decisions.
    tool_record (actions, read[, stage_read]) for system entry - None: no tool record (nothing decided on it).
    typed       values the tools typed into forms during the stage.
    page_notes  what the tools read off the system's pages.
    """

    with tempfile.TemporaryDirectory(ignore_cleanup_errors=True) as tmp:
        root = Path(tmp)
        work = root / "work"
        work.mkdir()
        for rel, content in (files or {}).items():
            path = work / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            if isinstance(content, bytes):
                path.write_bytes(content)
            else:
                path.write_text(content, encoding="utf-8")
        named = inputs if inputs is not None else [rel for rel in files or {} if rel in (request or "")]
        outputs = stage.get("outputs") or [rel for rel in files or {} if rel not in named]
        plan = {"long": True, "title": "synthetic", "stages": [
            {"id": "a", "title": stage.get("title", "Stage"), "does": stage.get("does", stage.get("title", "Stage")),
             "drafts": stage.get("drafts", 1 if drafts else 0), "decision": stage.get("decision", False),
             "outputs": [{"file": o} for o in outputs] or [{"file": "log.csv"}],
             **({"enters": stage["enters"]} if "enters" in stage else {})},
            {"id": "b", "title": "Next", "does": "The next stage.", "outputs": [{"file": "next.csv"}],
             "depends_on": ["a"]}]}
        req = (request or "").replace("{work}", str(work))
        spec = long_task.spec_from_plan(plan, (), request=req)
        if spec is None:
            raise ValueError("the synthetic stage makes no planned run")
        runner = WorkflowRunner(root / "workflows")
        patches = [mock.patch.object(workflow_task, "runner", return_value=runner),
                   mock.patch.object(config, "WORK_DIR", work),
                   mock.patch.object(stage_checks, "_page_notes", lambda *a, **k: list(page_notes)),
                   mock.patch.object(stage_checks, "_entered", lambda: list(typed)),
                   mock.patch.object(stage_checks, "_stage_tool_record", lambda *a, **k: tool_record)]
        for patch in patches:
            patch.start()
        try:
            run_id = runner.create(spec, work_dir=str(work))["run_id"]
            long_task._update(run_id, lambda d: d.update(request=req, amendments=list(amendments),
                                                         decisions=list(decisions)))
            run_state, step = runner.claim(run_id)
            made = [str(work / o) for o in outputs if (work / o).is_file()]
            found = long_task.stage_code_findings(run_id, run_state, step, made, list(drafts), req, reply,
                                                  {"entered": 0, "t": 0})
        finally:
            for patch in reversed(patches):
                patch.stop()
    return [(check, text, long_task.is_blocking(check)) for check, text in found]
