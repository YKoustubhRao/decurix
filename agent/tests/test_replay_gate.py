"""
The replay gate (docs/long-requests.md): every code-decided check a planned
stage goes through, run again on saved live runs that were right or nearly
right. A check that finds something in correct work fails here, before it
ships - found live (2026-10-20): checks that flagged a customer's own figures
and a correct allocation sent right stages back.

Each fixtures/replay/<run>/ holds what the checks saw: request.txt (the
person's request), work/ (the run's work folder: the inputs and what the
stages saved), drafts.json (the Outbox when the run ended), stages.json (the
stages as the run's report gives them: title, files, drafts, what each said;
the changes the person made) and expected.json ({"allowed": [...]}: for a
nearly right run, words of the findings its known-wrong items may raise; empty
for a right run). Any finding outside "allowed" fails the gate.

Two corpora. replay_corpus/ (in the repo, always run): synthetic, suite-free right runs with invented names and
figures, built to carry what the checks read - a removed record, values typed into a form (typed.json:
{stage number: [values]}), a change to a given file (sources.json: {stage number: [source_guard.changed_sources
facts]}). The private corpus (fixtures/replay, live runs, outside the repo) runs where it is; DECURIX_REQUIRE_FIXTURES=1
makes its absence a failure (review 2026-11-03, E-16: the gate skipped on every machine but one, and patched three
blocking checks out).

Every new code check must pass this gate, and every blocking check must be evaluated by it (the coverage test below).
The script that built the private corpus (decurix-tests/wide/process/build_replay.py) was lost with that folder: the
corpus must be rebuilt from kept eval oracle runs (their R9 bundles, docs/trajectory.md), each laid out as above; a
run belongs there when its grader scored it right or nearly right.

    PYTHONPATH="agent;agent/tests" python -m unittest test_replay_gate
"""

import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

# Imported first: it stubs decurix_agent.config on a machine with no desktop.
import test_recordings  # noqa: F401
import fixture_paths  # noqa: E402 - live-run fixtures live outside the repo

from decurix_agent import config, long_task, workflow_runner, workflow_task
from decurix_agent.tools import office, table_quality
from decurix_agent.workflow_runner import WorkflowRunner

FIXTURES = fixture_paths.fixture("replay")
# The synthetic corpus in the repository: always there.
CORPUS = Path(__file__).resolve().parent / "replay_corpus"
# Where the runs' worker had its work folder; replaced with the replay's own.
LIVE_WORK = "/home/headless/work"


def _stored_pdf_text(fixture, work, read):
    """
    office.read_document with each fixture PDF read from its stored `pdftotext -layout` text (pdf_text/, made at build
    time with poppler, as the desktops have it). Always, not only where poppler is missing: a Windows host may have
    xpdf's pdftotext, whose layout differs (a wrapped row's cells land on another line), and a check would then be
    judged on text no desktop ever reads.
    """

    def read_document(path, *args, **kwargs):
        p = Path(path)
        try:
            stored = fixture / "pdf_text" / (p.resolve().relative_to(work.resolve()).as_posix() + ".txt")
        except ValueError:
            stored = None
        if p.suffix.lower() == ".pdf" and stored is not None and stored.is_file():
            return {"text": f"{p.name}: PDF.\n\n" + stored.read_text(encoding="utf-8"), "images": []}
        return read(path, *args, **kwargs)
    return read_document


def _stored_pdf_check_text(fixture, work, extract):
    """
    workflow_runner._pdf_text with each fixture PDF's stored text, for the same reason as above - and because a Windows
    host often has no pdftotext at all, where every PDF stage would fail to hand in for a tool, not for its work.
    """

    def pdf_text(path):
        p = Path(path)
        try:
            stored = fixture / "pdf_text" / (p.resolve().relative_to(work.resolve()).as_posix() + ".txt")
        except ValueError:
            stored = None
        if stored is not None and stored.is_file():
            return stored.read_text(encoding="utf-8")
        return extract(path)
    return pdf_text


def replay(fixture, root, crosscheck=(), seen=None):
    """
    Every code finding the saved run's stages meet now: [(stage title, finding)].
    The run is rebuilt as a planned run over a copy of its work folder, and each
    stage is checked as stage_finished checks it, in order; then the figures
    the stages' outputs share are compared (the cross-check's code part).
    """

    work = root / "work"
    work.mkdir()
    here = work.as_posix()
    request = (fixture / "request.txt").read_text(encoding="utf-8").replace(LIVE_WORK, here)
    saved = json.loads((fixture / "stages.json").read_text(encoding="utf-8"))
    drafts = json.loads((fixture / "drafts.json").read_text(encoding="utf-8"))
    for draft in drafts:
        draft["attachments"] = [str(a).replace(LIVE_WORK, here) for a in draft.get("attachments") or []]
    plan = {"long": True, "title": fixture.name, "stages": [
        {"id": f"s{n}", "title": s["title"], "does": s["title"], "drafts": s["drafts"],
         "outputs": [{"file": o} for o in s["outputs"]], "depends_on": [f"s{n - 1}"] if n > 1 else []}
        for n, s in enumerate(saved["stages"], 1)]}
    spec = long_task.spec_from_plan(plan, (), request=request)
    if spec is None:
        raise AssertionError(f"{fixture.name}: the saved stages make no planned run")
    runner = WorkflowRunner(root / "workflows")
    found = []
    seen = seen if seen is not None else {}
    with mock.patch.object(workflow_task, "runner", return_value=runner), \
            mock.patch.object(office, "read_document", _stored_pdf_text(fixture, work, office.read_document)), \
            mock.patch.object(workflow_runner, "_pdf_text", _stored_pdf_check_text(fixture, work, workflow_runner._pdf_text)), \
            mock.patch.object(table_quality, "_ranges_file", lambda: root / "table-ranges.json"):
        run_id = runner.create(spec, work_dir=str(work))["run_id"]
        # The inputs are there from the start; each stage's files arrive once it runs, as it made them (the runner
        # counts a file already there, unchanged, as no stage's work).
        outputs = {o for s in saved["stages"] for o in s["outputs"]}
        shutil.copytree(fixture / "work", work, dirs_exist_ok=True,
                        ignore=lambda folder, names: [n for n in names if (Path(folder) / n).relative_to(
                            fixture / "work").as_posix() in outputs])
        # An amendment is its text, or {"text", "removed", "restored"} as classify reads it.
        long_task._update(run_id, lambda d: d.update(request=request, amendments=[
            dict({"n": n, "at": 0, "affects": []}, **(a if isinstance(a, dict) else {"text": a}))
            for n, a in enumerate(saved["amendments"], 1)]))
        # What the worker typed into forms, and the given files that changed, per stage - when the run kept them.
        typed = json.loads((fixture / "typed.json").read_text(encoding="utf-8")) if (fixture / "typed.json").is_file() else {}
        changes = (json.loads((fixture / "sources.json").read_text(encoding="utf-8"))
                   if (fixture / "sources.json").is_file() else {})
        # (No office read record of the inputs: the repeated-rows check reads the request's input tables itself - found
        # live, series 11: the worker read them with python and office read never saw them.)
        handed = set()
        for said in saved["stages"]:
            # A file an earlier stage already handed in stays (a workbook several stages build): taking it away would
            # make the runner reopen that stage (found: series 10's P2, where two stages hand in order_working.xlsx).
            for rel in said["outputs"]:
                if rel not in handed:
                    (work / rel).unlink(missing_ok=True)
            handed.update(said["outputs"])
            run_state, step = runner.claim(run_id)
            for rel in said["outputs"]:
                if (fixture / "work" / rel).is_file():
                    (work / rel).parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(fixture / "work" / rel, work / rel)
            if step is None:
                raise AssertionError(f"{fixture.name}: no stage to run for {said['title']}")
            made = long_task._stage_files(run_state, step)
            mine = drafts if step.get("drafts") else []
            reply = said["reply"].replace(LIVE_WORK, here)
            # What the tools did and read during the stage (tools/<n>.json: {"actions": [...], "read": "..."}), when
            # the run kept it; otherwise nothing is decided on it (a saved run rarely has its tool record).
            tools = fixture / "tools" / f"{saved['stages'].index(said) + 1}.json"
            record = None
            if tools.is_file():
                kept = json.loads(tools.read_text(encoding="utf-8"))
                record = (kept.get("actions") or [], kept.get("read") or "", kept.get("stage_read", kept.get("read") or ""))
            number = str(saved["stages"].index(said) + 1)
            with mock.patch.object(long_task, "_stage_tool_record", lambda *a, **k: record), \
                    mock.patch.object(long_task, "_entered", lambda values=typed.get(number) or []: list(values)), \
                    mock.patch("decurix_agent.source_guard.changed_sources",
                               lambda task_id, request, facts=changes.get(number) or []: [dict(f) for f in facts]):
                for problem in long_task.stage_code_problems(run_id, run_state, step, made, mine, request, reply,
                                                             {"entered": 0}):
                    found.append((said["title"], problem))
            after = runner.complete(run_id, step["id"], result={"drafts": mine}, report=reply)
            if after["steps"][step["id"]]["status"] != "completed":
                found.append((said["title"], "not handed in: " + str(after["steps"][step["id"]].get("error"))))
        outputs = long_task._outputs(runner.status(run_id))
        for item in long_task.code_disagreements(outputs)[0]:
            found.append(("cross-check", f"Figures that do not agree - {item['figure']}: {item['values']}"))
        # The live cross-check's model pointed at these places (expected.json "crosscheck"); code decides on them.
        points = list(crosscheck)
        if points:
            for item in long_task.model_disagreements(outputs, ask=lambda system, shown: {"figures": points},
                                                      request=request)[0]:
                found.append(("cross-check", f"Figures that do not agree - {item['figure']}: {item['values']}"))
    return found


class ReplayGate(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        patches = [
            mock.patch.object(config, "DECURIX_DIR", root),
            mock.patch.object(config, "WORK_DIR", root / "work"),
            mock.patch.dict("os.environ", {"DECURIX_DIR": str(root)}),
            mock.patch.object(long_task.progress, "emit", lambda *a, **k: None),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def check_runs(self, folder):
        runs = sorted(p for p in folder.iterdir() if (p / "stages.json").is_file())
        self.assertTrue(runs, f"no saved runs to replay in {folder}")
        for fixture in runs:
            with self.subTest(run=fixture.name):
                expected = json.loads((fixture / "expected.json").read_text(encoding="utf-8"))
                root = Path(self.tmp.name) / fixture.name
                root.mkdir()
                with mock.patch.object(config, "WORK_DIR", root / "work"):
                    seen = {}
                    found = replay(fixture, root, expected.get("crosscheck") or (), seen)
                allowed = [a.lower() for a in expected.get("allowed") or []]
                unexpected = [f"[{stage}] {text}" for stage, text in found
                              if not any(a in text.lower() for a in allowed)]
                self.assertEqual(unexpected, [], f"{fixture.name}: a code check flags correct work")
                # A run kept for what the checks must catch (expected.json "must_find": words of each finding).
                for words in expected.get("must_find") or []:
                    self.assertTrue(any(words.lower() in text.lower() for _, text in found),
                                    f"{fixture.name}: a check no longer catches {words!r}")

    def test_the_synthetic_right_runs_meet_no_finding(self):
        self.check_runs(CORPUS)

    def test_saved_right_runs_meet_no_finding_on_their_right_parts(self):
        if not FIXTURES.is_dir():
            raise fixture_paths.missing(f"fixture not here: {FIXTURES} (set DECURIX_TEST_FIXTURES)")
        self.check_runs(FIXTURES)

    def test_every_blocking_check_is_evaluated_by_the_replay(self):
        # A check made BLOCKING must have met correct work here (CLAUDE.md, docs/long-requests.md): one the replay
        # never ran would be blocking unproven.
        ran = set()
        real = long_task._checked

        def recorded(check, unmade, fn, *args, **kwargs):
            ran.add(check)
            return real(check, unmade, fn, *args, **kwargs)
        for fixture in sorted(p for p in CORPUS.iterdir() if (p / "stages.json").is_file()):
            root = Path(self.tmp.name) / ("coverage-" + fixture.name)
            root.mkdir()
            with mock.patch.object(config, "WORK_DIR", root / "work"), mock.patch.object(long_task, "_checked", recorded):
                replay(fixture, root)
        blocking = {c for c in long_task.CHECK_MATURITY if long_task.is_blocking(c) or long_task.asks_person(c)}
        self.assertEqual(blocking - ran, set(), "blocking checks the replay never evaluated")

    def test_the_gate_sees_a_check_that_flags_correct_work(self):
        # A check added to the stage checks that finds something in correct work shows up in the replay.
        fixture = min((p for p in CORPUS.iterdir() if (p / "stages.json").is_file()),
                      key=lambda p: sum(f.stat().st_size for f in p.rglob("*") if f.is_file()))
        root = Path(self.tmp.name) / "planted"
        root.mkdir()
        with mock.patch.object(config, "WORK_DIR", root / "work"), \
                mock.patch.object(long_task, "_summary_totals", lambda made: ["planted finding"] if made else []):
            found = replay(fixture, root)
        self.assertIn("planted finding", [text for _, text in found])


if __name__ == "__main__":
    unittest.main()
