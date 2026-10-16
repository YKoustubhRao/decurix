"""Independent PM acceptance tests: orchestration only, never live app claims."""
import copy
import json
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch

from decurix_agent.workflow_runner import WorkflowRunner, validate


def step(name, **extra):
    return {"id": name, "app": "unseen-custom-app", "instruction": "Produce checked business output",
            "checks": [{"path": name + ".txt", "format": "text", "contains": ["verified"]}], **extra}


def spec(*steps):
    return {"id": "sixth-unseen-workflow", "steps": list(steps)}


class WorkflowProductAcceptance(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.calls = []

        def executor(stage, context):
            self.calls.append((stage["id"], copy.deepcopy(context)))
            (Path(context["work_dir"]) / (stage["id"] + ".txt")).write_text("verified", encoding="utf-8")
            return {"status": "I claim success"}

        self.executor = executor
        self.runner = WorkflowRunner(self.root, executor)

    def create(self, *stages, run_id="run1"):
        return self.runner.create(spec(*(stages or (step("one"),))), {"case_id": "customer-001"}, run_id=run_id)

    def test_unseen_app_and_workflow_run_without_catalog_dispatch(self):
        self.create(step("one"), step("two", depends_on=["one"]))
        state = self.runner.advance("run1")
        self.assertEqual(state["status"], "completed")
        self.assertEqual(self.calls[1][1]["inputs"]["case_id"], "customer-001")
        self.assertEqual(self.calls[1][1]["outputs"]["one"][0]["path"], "one.txt")
        self.assertEqual(len(self.calls[1][1]["outputs"]["one"][0]["sha256"]), 64)

    def test_restart_reuses_checked_completed_work(self):
        self.create(step("one"), step("two", depends_on=["one"]))
        self.runner.advance("run1", max_steps=1)
        restarted = WorkflowRunner(self.root, self.executor)
        self.assertEqual(restarted.advance("run1")["status"], "completed")
        self.assertEqual([c[0] for c in self.calls], ["one", "two"])

    def test_model_claim_without_artifact_fails_and_blocks_downstream(self):
        self.create(step("one"), step("two", depends_on=["one"]))
        self.runner.executor = lambda *args: {"success": True, "evidence": ["everything looks great"]}
        state = self.runner.advance("run1")
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["two"]["attempts"], 0)

    def test_wrong_artifact_content_fails(self):
        self.create()
        def wrong(stage, context):
            (Path(context["work_dir"]) / "one.txt").write_text("unrelated customer", encoding="utf-8")
        self.runner.executor = wrong
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_no_checks_is_rejected_before_any_execution(self):
        with self.assertRaises(ValueError):
            self.create(step("one", checks=[]))
        self.assertEqual(self.calls, [])

    def test_missing_or_future_dependency_is_rejected(self):
        for dependencies in (["missing"], ["two"], ["one"]):
            with self.subTest(dependencies=dependencies), self.assertRaises(ValueError):
                validate(spec(step("one", depends_on=dependencies), step("two")))

    def test_duplicate_ids_are_rejected(self):
        with self.assertRaises(ValueError):
            validate(spec(step("one"), step("one")))

    def test_artifact_paths_cannot_escape_run(self):
        for path in ("../other.txt", "a/../../other.txt", "C:/other.txt", "\\\\host\\share\\other.txt"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                validate(spec(step("one", checks=[{"path": path}])))

    def test_approval_is_scoped_to_run_and_step(self):
        self.create(step("one", approval=True))
        for wrong in ([], ["yes"], ["run2:one"], ["run1:two"]):
            self.assertEqual(self.runner.advance("run1", approvals=wrong)["status"], "awaiting_approval")
        self.assertEqual(self.calls, [])
        self.assertEqual(self.runner.advance("run1", approvals=["run1:one"])["status"], "completed")

    def test_cancellation_survives_restart_and_stops_side_effects(self):
        self.create(step("one"), step("two"))
        self.runner.advance("run1", max_steps=1)
        self.runner.cancel("run1")
        state = WorkflowRunner(self.root, self.executor).advance("run1")
        self.assertEqual(state["status"], "cancelled")
        self.assertEqual([c[0] for c in self.calls], ["one"])

    def test_failed_non_idempotent_action_is_never_retried(self):
        self.create(step("one", retry_safe=False, max_attempts=5))
        attempts = []
        def external_action(*args):
            attempts.append("might already have submitted")
            raise TimeoutError("response lost")
        self.runner.executor = external_action
        self.assertEqual(self.runner.advance("run1")["status"], "failed")
        self.runner.advance("run1")
        self.assertEqual(len(attempts), 1)

    def test_retry_safe_failure_has_finite_attempt_budget(self):
        self.create(step("one", retry_safe=True, max_attempts=3))
        def failing(*args):
            raise OSError("temporary outage")
        self.runner.executor = failing
        state = self.runner.advance("run1")
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["steps"]["one"]["attempts"], 3)

    def test_interrupted_external_action_requires_reconciliation(self):
        self.create(step("one", retry_safe=False))
        self.runner.claim("run1")
        # A second runner is only an observer until the actual owning process dies.
        with patch("decurix_agent.workflow_runner._alive", return_value=False):
            state = WorkflowRunner(self.root, self.executor).advance("run1")
        self.assertEqual(state["status"], "uncertain")
        self.assertEqual(self.calls, [])

    def test_resume_detects_mutation_of_previously_verified_artifact(self):
        self.create(step("one"), step("two"))
        self.runner.advance("run1", max_steps=1)
        (self.root / "run1" / "artifacts" / "one.txt").write_text("tampered", encoding="utf-8")
        self.assertEqual(self.runner.advance("run1")["status"], "failed")
        self.assertEqual([c[0] for c in self.calls], ["one"])

    def test_existing_run_id_cannot_replace_business_case(self):
        self.create()
        with self.assertRaises(ValueError):
            self.runner.create(spec(step("other")), {}, run_id="run1")

    def test_missing_required_business_inputs_fail_before_execution(self):
        workflow = {**spec(step("one")), "required_inputs": ["source_files", "business_rules"]}
        for inputs in ({}, {"source_files": []}, {"source_files": ["invoice.csv"], "business_rules": ""}):
            with self.subTest(inputs=inputs), self.assertRaises(ValueError):
                self.runner.create(workflow, inputs)
        self.assertEqual(self.calls, [])

    def test_run_artifacts_and_inputs_are_isolated(self):
        self.create(run_id="customer1")
        self.create(run_id="customer2")
        self.runner.advance("customer1")
        self.assertFalse((self.root / "customer2" / "artifacts" / "one.txt").exists())
        self.assertEqual(self.runner.status("customer2")["steps"]["one"]["attempts"], 0)

    def test_independent_business_verifier_can_reject_valid_file(self):
        self.create()
        self.runner.verifier = lambda *args: False
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_long_workflow_resumes_across_many_app_transitions(self):
        stages = [step("stage" + str(i), app=["browser", "excel", "powerpoint", "word", "mail"][i % 5],
                       depends_on=["stage" + str(i-1)] if i else []) for i in range(100)]
        self.create(*stages)
        for _ in range(10):
            WorkflowRunner(self.root, self.executor).advance("run1", max_steps=10)
        self.assertEqual(WorkflowRunner(self.root, self.executor).advance("run1")["status"], "completed")
        self.assertEqual(len(self.calls), 100)

    def test_corrupted_checkpoint_does_not_produce_success(self):
        self.create()
        (self.root / "run1" / "checkpoint.json").write_text("truncated", encoding="utf-8")
        with self.assertRaises((ValueError, json.JSONDecodeError)):
            self.runner.advance("run1")

    def package_executor(self, members):
        def produce(stage, context):
            path = Path(context["work_dir"]) / stage["checks"][0]["path"]
            with zipfile.ZipFile(path, "w") as archive:
                for name, content in members.items():
                    archive.writestr(name, content)
        return produce

    def test_malformed_powerpoint_xml_cannot_count_as_verified_deck(self):
        self.create(step("one", checks=[{"path": "deck.pptx", "format": "pptx", "contains": ["verified"]}]))
        self.runner.executor = self.package_executor({"[Content_Types].xml": "not XML", "ppt/presentation.xml": "verified"})
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_metadata_marker_cannot_substitute_for_slide_content(self):
        self.create(step("one", checks=[{"path": "deck.pptx", "format": "pptx", "contains": ["verified"]}]))
        self.runner.executor = self.package_executor({
            "[Content_Types].xml": '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
            "ppt/presentation.xml": '<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>',
            "docProps/custom.xml": '<metadata>verified</metadata>'})
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_empty_presentation_cannot_count_as_valid_deck(self):
        self.create(step("one", checks=[{"path": "deck.pptx", "format": "pptx"}]))
        self.runner.executor = self.package_executor({
            "[Content_Types].xml": '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
            "ppt/presentation.xml": '<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>'})
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_comment_marker_cannot_substitute_for_word_document_content(self):
        self.create(step("one", checks=[{"path": "report.docx", "format": "docx", "contains": ["verified"]}]))
        self.runner.executor = self.package_executor({
            "[Content_Types].xml": '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
            "word/document.xml": '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>wrong customer</w:t></w:r></w:p><!-- verified --></w:body></w:document>'})
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_unreferenced_spreadsheet_string_cannot_substitute_for_business_cells(self):
        self.create(step("one", checks=[{"path": "analysis.xlsx", "format": "xlsx", "contains": ["verified"]}]))
        self.runner.executor = self.package_executor({
            "[Content_Types].xml": '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>',
            "xl/workbook.xml": '<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets><sheet name="Analysis" sheetId="1"/></sheets></workbook>',
            "xl/worksheets/sheet1.xml": '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>wrong customer</t></is></c></row></sheetData></worksheet>',
            "xl/sharedStrings.xml": '<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>verified</t></si></sst>'})
        self.assertEqual(self.runner.advance("run1")["status"], "failed")

    def test_pdf_header_without_a_document_is_not_verified(self):
        self.create(step("one", checks=[{"path": "report.pdf", "format": "pdf"}]))
        def fake_pdf(stage, context):
            (Path(context["work_dir"]) / "report.pdf").write_bytes(b"%PDF-1.7\nThis is not a PDF document.")
        self.runner.executor = fake_pdf
        self.assertEqual(self.runner.advance("run1")["status"], "failed")


if __name__ == "__main__":
    unittest.main()
