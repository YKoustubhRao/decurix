import csv
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest

from decurix_agent.tools import doc_tool, erp_tool, excel_tool, mail_tool
from decurix_agent.computer import ingest, replay, recording


class TestEnterpriseWorkflows(unittest.TestCase):
    """
    Comprehensive test suite verifying that Decurix can execute all 10 enterprise workflows:
    1. PO to sales-order draft
    2. Supplier invoice to purchase-invoice draft
    3. Employee onboarding
    4. Product-price update
    5. Recurring MIS report
    6. Customer-onboarding KYC verification & portal draft
    7. Clean and ship ERP reports
    8. Customer statement preparation
    9. Accounts-receivable follow-up
    10. Daily commodity price trends update
    + Multi-app recording ingestion and parameterized replay.
    """

    def setUp(self):
        self.test_dir = tempfile.mkdtemp(prefix="decurix_enterprise_test_")
        self.orig_rec_dir = os.environ.get("DECURIX_RECORDINGS_DIR")
        rec_dir = os.path.join(self.test_dir, "recordings")
        os.environ["DECURIX_RECORDINGS_DIR"] = rec_dir
        recording.ROOT = Path(rec_dir)

    def tearDown(self):
        if self.orig_rec_dir:
            os.environ["DECURIX_RECORDINGS_DIR"] = self.orig_rec_dir
            recording.ROOT = Path(self.orig_rec_dir)
        else:
            os.environ.pop("DECURIX_RECORDINGS_DIR", None)
            recording.ROOT = Path.home() / ".decurix" / "recordings"
        shutil.rmtree(self.test_dir, ignore_errors=True)

    # ------------------------------------------------------------
    # Workflow 1: PO to Sales-Order Draft
    # ------------------------------------------------------------
    def test_workflow_01_po_to_sales_order_draft(self):
        po_content = """
        PURCHASE ORDER
        PO Number: PO-2026-9081
        Date: 2026-09-22
        Customer: Global Dynamics Ltd
        Billing Address: 500 Enterprise Way, Suite 400

        Line Items:
        SKU-1001 Industrial Sensor A1 50 120.00 6000.00
        SKU-1002 Digital Controller X 20 250.00 5000.00
        SKU-1003 High-Speed Cable 100 15.50 1550.00

        Tax: 2259.00
        Total: 14809.00
        """
        po_file = os.path.join(self.test_dir, "incoming_po.txt")
        with open(po_file, "w") as f:
            f.write(po_content)

        parsed = doc_tool.parse_purchase_order(po_file)
        self.assertEqual(parsed["po_number"], "PO-2026-9081")
        self.assertEqual(parsed["customer"], "Global Dynamics Ltd")
        self.assertEqual(len(parsed["items"]), 3)
        self.assertEqual(parsed["items"][0]["item_code"], "SKU-1001")
        self.assertEqual(parsed["items"][0]["qty"], 50.0)
        self.assertEqual(parsed["items"][0]["rate"], 120.00)
        self.assertEqual(parsed["items"][0]["amount"], 6000.00)
        self.assertEqual(parsed["subtotal"], 12550.00)

        # Check ERPNext draft payload
        draft = parsed["sales_order_draft"]
        self.assertEqual(draft["doctype"], "Sales Order")
        self.assertEqual(draft["customer"], "Global Dynamics Ltd")
        self.assertEqual(draft["po_no"], "PO-2026-9081")
        self.assertEqual(draft["docstatus"], 0)  # Must be draft
        self.assertEqual(len(draft["items"]), 3)

    # ------------------------------------------------------------
    # Workflow 2: Supplier Invoice to Purchase-Invoice Draft
    # ------------------------------------------------------------
    def test_workflow_02_supplier_invoice_to_purchase_invoice_draft(self):
        inv_content = """
        TAX INVOICE
        Invoice #: INV-SUP-7731
        Date: 2026-09-21
        Supplier: Acme Raw Materials Inc
        PO Ref: PO-2026-9081

        Items:
        RAW-COPPER Pure Copper Wire 1000 8.50 8500.00
        RAW-TIN Tin Alloy Ingot 500 12.00 6000.00

        Total Amount: 14500.00
        """
        inv_file = os.path.join(self.test_dir, "supplier_invoice.txt")
        with open(inv_file, "w") as f:
            f.write(inv_content)

        existing_pos = {
            "PO-2026-9081": {
                "po_number": "PO-2026-9081",
                "vendor": "Acme Raw Materials Inc",
                "approved_budget": 15000.00
            }
        }

        parsed = doc_tool.parse_supplier_invoice(inv_file, existing_pos=existing_pos)
        self.assertEqual(parsed["invoice_number"], "INV-SUP-7731")
        self.assertEqual(parsed["supplier"], "Acme Raw Materials Inc")
        self.assertEqual(parsed["po_reference"], "PO-2026-9081")
        self.assertIsNotNone(parsed["matched_po"])
        self.assertEqual(len(parsed["items"]), 2)
        self.assertEqual(parsed["total_amount"], 14500.00)

        # Check ERPNext Purchase Invoice Draft
        draft = parsed["purchase_invoice_draft"]
        self.assertEqual(draft["doctype"], "Purchase Invoice")
        self.assertEqual(draft["bill_no"], "INV-SUP-7731")
        self.assertEqual(draft["purchase_order"], "PO-2026-9081")
        self.assertEqual(draft["docstatus"], 0)

    # ------------------------------------------------------------
    # Workflow 3: Employee Onboarding
    # ------------------------------------------------------------
    def test_workflow_03_employee_onboarding(self):
        emp_sheet = """
        First Name: Sarah
        Last Name: Connor
        Employee ID: EMP-2026-042
        Email: sarah.connor@cyberdyne.com
        Designation: Senior Automation Architect
        Department: Systems Engineering
        Joining Date: 2026-10-01
        """
        emp_file = os.path.join(self.test_dir, "employee_info.txt")
        with open(emp_file, "w") as f:
            f.write(emp_sheet)

        # Prepare dummy onboarding documents
        docs_dir = os.path.join(self.test_dir, "emp_documents")
        os.makedirs(docs_dir, exist_ok=True)
        id_doc = os.path.join(docs_dir, "Passport_Photo.pdf")
        with open(id_doc, "w") as f: f.write("Dummy Passport")
        resume_doc = os.path.join(docs_dir, "Resume.pdf")
        with open(resume_doc, "w") as f: f.write("Dummy Resume")

        parsed = doc_tool.parse_employee_info(emp_file, documents_dir=docs_dir)
        self.assertEqual(parsed["first_name"], "Sarah")
        self.assertEqual(parsed["last_name"], "Connor")
        self.assertEqual(parsed["employee_id"], "EMP-2026-042")
        self.assertEqual(parsed["designation"], "Senior Automation Architect")
        self.assertEqual(len(parsed["documents"]), 2)

        hrms = parsed["hrms_payload"]
        self.assertEqual(hrms["first_name"], "Sarah")
        self.assertEqual(len(hrms["upload_files"]), 2)
        self.assertTrue(any("Resume.pdf" in p for p in hrms["upload_files"]))

    # ------------------------------------------------------------
    # Workflow 4: Product-Price Update
    # ------------------------------------------------------------
    def test_workflow_04_product_price_update(self):
        # Current catalog
        catalog_path = os.path.join(self.test_dir, "current_catalog.csv")
        excel_tool.write_sheet(catalog_path, ["Product Code", "Product Name", "Price"], [
            ["PROD-01", "Hydraulic Valve", "100.00"],
            ["PROD-02", "Pneumatic Cylinder", "150.00"],
            ["PROD-03", "Pressure Gauge", "50.00"],
        ])

        # Revised supplier price sheet received via mail
        revised_path = os.path.join(self.test_dir, "revised_prices.csv")
        excel_tool.write_sheet(revised_path, ["Product Code", "Product Name", "Price"], [
            ["PROD-01", "Hydraulic Valve", "110.00"],  # +10% increase
            ["PROD-02", "Pneumatic Cylinder", "135.00"],  # -10% decrease
            ["PROD-03", "Pressure Gauge", "50.00"],   # unchanged
        ])

        review_path = os.path.join(self.test_dir, "price_changes_for_review.csv")
        result = excel_tool.generate_price_comparison(catalog_path, revised_path, review_path)

        self.assertEqual(result["total_products_checked"], 3)
        self.assertEqual(result["affected_products"], 2)  # PROD-01 and PROD-02
        self.assertTrue(os.path.exists(result["output_file"]))

        # Verify draft review email creation
        draft_eml = os.path.join(self.test_dir, "Price_Update_Review.eml")
        email_res = mail_tool.create_email_draft(
            to="pricing.committee@company.com",
            subject="Product Price Revision Review Required",
            body_text=f"Please review the attached price revision. Total affected products: {result['affected_products']}.",
            attachments=[result["output_file"]],
            output_path=draft_eml,
        )
        self.assertTrue(os.path.exists(draft_eml))
        self.assertIn("PRICE_CHANGES_FOR_REVIEW", result["output_file"].upper())
        self.assertIn("price_changes_for_review.csv", email_res["attachments"])

    # ------------------------------------------------------------
    # Workflow 5: Recurring MIS Report (Reconciliation & Total Checks)
    # ------------------------------------------------------------
    def test_workflow_05_recurring_mis_report(self):
        # Monthly exports gathered
        monthly_sales = {
            "headers": ["Region", "Sales_Rep", "Revenue", "Units"],
            "rows": [
                ["North", "Alice", "50000", "200"],
                ["South", "Bob", "45000", "180"],
                ["East", "Charlie", "60000", "240"],
                ["West", "Dana", "55000", "220"],
            ]
        }
        monthly_expenses = {
            "headers": ["Category", "Amount"],
            "rows": [
                ["Salaries", "80000"],
                ["Operations", "40000"],
                ["Marketing", "25000"],
            ]
        }

        # Established model reconciliation
        model_path = os.path.join(self.test_dir, "Master_MIS_Model.xlsx")
        control_totals = {
            "Revenue": 210000.00,   # 50k + 45k + 60k + 55k = 210,000
            "Amount": 145000.00,    # 80k + 40k + 25k = 145,000
        }

        res = excel_tool.reconcile_mis_model(
            model_path=model_path,
            new_sheets_data={"Sales": monthly_sales, "Expenses": monthly_expenses},
            control_totals=control_totals
        )

        self.assertTrue(res["ok"])
        self.assertEqual(len(res["checks"]), 2)
        for check in res["checks"]:
            self.assertTrue(check["passed"])
            self.assertEqual(check["diff"], 0.0)

        # Create distribution email
        mis_eml = os.path.join(self.test_dir, "Monthly_MIS_Distribution.eml")
        email_draft = mail_tool.create_email_draft(
            to="executive.board@company.com",
            subject="Monthly MIS Report - Controls Verified",
            body_text="All control totals reconciled. Operating Margin: $65,000.",
            output_path=mis_eml
        )
        self.assertTrue(os.path.exists(mis_eml))

    # ------------------------------------------------------------
    # Workflow 6: Customer-Onboarding File Check & KYC Verification
    # ------------------------------------------------------------
    def test_workflow_06_customer_onboarding_kyc_success(self):
        kyc_folder = os.path.join(self.test_dir, "kyc_docs")
        os.makedirs(kyc_folder, exist_ok=True)

        # 1. PAN document (Valid PAN: ABCDE1234F)
        with open(os.path.join(kyc_folder, "pan_card.txt"), "w") as f:
            f.write("INCOME TAX DEPARTMENT\nPermanent Account Number: ABCDE1234F\nName: BHARAT TECH CORP")

        # 2. GST Certificate (Valid GSTIN: 27ABCDE1234FZ5) -> contains ABCDE1234F
        with open(os.path.join(kyc_folder, "gst_certificate.txt"), "w") as f:
            f.write("GOVERNMENT OF INDIA\nGSTIN: 27ABCDE1234F1Z5\nLegal Name: BHARAT TECH CORP\nTrade Name: BHARAT TECH")

        # 3. Bank Proof (Valid Account & IFSC: HDFC0001234)
        with open(os.path.join(kyc_folder, "cancelled_cheque.txt"), "w") as f:
            f.write("HDFC BANK\nA/C No: 50100234567890\nIFSC: HDFC0001234\nAccount Name: BHARAT TECH CORP")

        # 4. Application Form
        with open(os.path.join(kyc_folder, "kyc_application_form.txt"), "w") as f:
            f.write("CUSTOMER ONBOARDING FORM\nLegal Name: BHARAT TECH CORP\nPAN: ABCDE1234F\nGSTIN: 27ABCDE1234F1Z5\nAccount: 50100234567890\nIFSC: HDFC0001234")

        std_folder = os.path.join(self.test_dir, "standardized_kyc")
        result = doc_tool.verify_customer_onboarding(kyc_folder, output_dir=std_folder)

        self.assertEqual(result["status"], "APPROVED_FOR_PORTAL")
        self.assertEqual(len(result["missing_documents"]), 0)
        self.assertEqual(len(result["mismatches"]), 0)
        self.assertEqual(result["extracted_data"]["pan_number"], "ABCDE1234F")
        self.assertEqual(result["extracted_data"]["gstin"], "27ABCDE1234F1Z5")

        # Standardized renamed files
        std_files = result["standardized_files"]
        self.assertIn("PAN", std_files)
        self.assertTrue(std_files["PAN"].endswith("ABCDE1234F_PAN.txt"))
        self.assertTrue(std_files["GST"].endswith("ABCDE1234F_GST_Certificate.txt"))

        # Portal application draft
        portal = result["portal_draft"]
        self.assertEqual(portal["verification_status"], "VERIFIED")
        self.assertEqual(portal["customer_name"], "BHARAT TECH CORP")

    def test_workflow_06_customer_onboarding_kyc_mismatch_detection(self):
        kyc_folder = os.path.join(self.test_dir, "kyc_docs_bad")
        os.makedirs(kyc_folder, exist_ok=True)

        # PAN is ABCDE1234F
        with open(os.path.join(kyc_folder, "pan.txt"), "w") as f:
            f.write("PAN: ABCDE1234F")

        # GSTIN belongs to different PAN: 27ZZZZZ9999Z1Z5
        with open(os.path.join(kyc_folder, "gst.txt"), "w") as f:
            f.write("GSTIN: 27ZZZZZ9999Z1Z5")

        with open(os.path.join(kyc_folder, "bank.txt"), "w") as f:
            f.write("A/C: 123456789012 IFSC: SBIN0001234")

        with open(os.path.join(kyc_folder, "app_form.txt"), "w") as f:
            f.write("PAN: ABCDE1234F GSTIN: 27ZZZZZ9999Z1Z5")

        result = doc_tool.verify_customer_onboarding(kyc_folder)
        self.assertEqual(result["status"], "REQUIRES_REVIEW")
        self.assertTrue(any("PAN-GSTIN Consistency" in m["field"] for m in result["mismatches"]))

    # ------------------------------------------------------------
    # Workflow 7: Clean and Ship ERP Reports
    # ------------------------------------------------------------
    def test_workflow_07_clean_and_ship_erp_report(self):
        # Raw noisy export from Tally / Busy ERP
        raw_rows = [
            ["TALLY ERP 9 - GENERATED REPORT", "", "", ""],
            ["Report Date: 2026-09-22", "", "", ""],
            ["Confidential Internal Document", "", "", ""],
            ["Voucher No", "Party Name", "Debit", "Credit"],
            ["V-101", "Delta Corp", "12000.00", "0.00"],
            ["V-102", "Echo Systems", "0.00", "5500.00"],
            ["V-103", "Foxtrot Logistics", "8400.00", "0.00"],
        ]
        raw_path = os.path.join(self.test_dir, "raw_tally_report.csv")
        excel_tool.write_sheet(raw_path, [], raw_rows)

        clean_path = os.path.join(self.test_dir, "Cleaned_Tally_Report.csv")
        excel_tool.clean_erp_report(
            report_path=raw_path,
            output_path=clean_path,
            skip_header_rows=3,  # skip first 3 noisy header rows
            keep_columns=["Voucher No", "Party Name", "Debit"]
        )

        cleaned = excel_tool.read_sheet(clean_path)
        self.assertEqual(cleaned["headers"], ["Voucher No", "Party Name", "Debit"])
        self.assertEqual(len(cleaned["rows"]), 3)
        self.assertEqual(cleaned["rows"][0][0], "V-101")

        # Mail draft
        draft_path = os.path.join(self.test_dir, "Tally_Report_Mail.eml")
        mail_tool.create_email_draft(
            to="finance@company.com",
            subject="Cleaned Daily Voucher Report",
            body_text="Please find attached the cleaned daily voucher report.",
            attachments=[clean_path],
            output_path=draft_path
        )
        self.assertTrue(os.path.exists(draft_path))

    # ------------------------------------------------------------
    # Workflow 8: Customer Statement Preparation
    # ------------------------------------------------------------
    def test_workflow_08_customer_statements(self):
        dummy_stmt = os.path.join(self.test_dir, "statement_alpha.pdf")
        with open(dummy_stmt, "w") as f: f.write("Dummy PDF statement")

        customers_data = {
            "Alpha Corp": {
                "email": "ap@alphacorp.com",
                "invoices": [
                    {"invoice_no": "INV-101", "amount": 1250.00, "due_date": "2026-09-15"},
                    {"invoice_no": "INV-109", "amount": 3400.00, "due_date": "2026-09-30"},
                ],
                "statement_pdf": dummy_stmt
            },
            "Beta Industries": {
                "email": "billing@betaind.com",
                "invoices": [
                    {"invoice_no": "INV-105", "amount": 780.00, "due_date": "2026-09-10"},
                ]
            }
        }

        output_dir = os.path.join(self.test_dir, "customer_statements")
        emails = mail_tool.prepare_customer_statements(customers_data, output_dir)

        self.assertEqual(len(emails), 2)
        self.assertEqual(emails[0]["to"], "ap@alphacorp.com")
        self.assertIn("Statement of Account - Alpha Corp", emails[0]["subject"])
        self.assertEqual(len(emails[0]["attachments"]), 1)
        self.assertTrue(os.path.exists(emails[0]["eml_path"]))

    # ------------------------------------------------------------
    # Workflow 9: Accounts-Receivable Follow-Up (Excluding Disputed)
    # ------------------------------------------------------------
    def test_workflow_09_accounts_receivable_followup(self):
        inv1_pdf = os.path.join(self.test_dir, "inv_501.pdf")
        with open(inv1_pdf, "w") as f: f.write("PDF 501")
        inv2_pdf = os.path.join(self.test_dir, "inv_502.pdf")
        with open(inv2_pdf, "w") as f: f.write("PDF 502")

        invoices = [
            {"invoice_no": "INV-501", "customer": "Stark Industries", "email": "tony@stark.com", "due_days": 45, "amount": 15000.0, "disputed": False, "pdf_path": inv1_pdf},
            {"invoice_no": "INV-502", "customer": "Stark Industries", "email": "tony@stark.com", "due_days": 75, "amount": 25000.0, "disputed": False, "pdf_path": inv2_pdf},
            {"invoice_no": "INV-503", "customer": "Wayne Enterprises", "email": "bruce@wayne.com", "due_days": 90, "amount": 50000.0, "disputed": True, "pdf_path": None},  # Disputed!
        ]

        output_dir = os.path.join(self.test_dir, "ar_followups")
        res = mail_tool.prepare_ar_followup_emails(invoices, output_dir)

        self.assertEqual(res["disputed_excluded_count"], 1)
        self.assertEqual(res["disputed_invoices"], ["INV-503"])
        self.assertEqual(res["reminders_prepared"], 1)  # Only Stark Industries

        stark_email = res["emails"][0]
        self.assertEqual(stark_email["to"], "tony@stark.com")
        self.assertIn("URGENT PAYMENT REMINDER", stark_email["subject"])  # max days = 75 (>60)
        self.assertEqual(len(stark_email["attachments"]), 2)
        self.assertTrue(os.path.exists(stark_email["eml_path"]))

    # ------------------------------------------------------------
    # Workflow 10: Daily Commodity Price Update
    # ------------------------------------------------------------
    def test_workflow_10_daily_commodity_update(self):
        trend_file = os.path.join(self.test_dir, "commodity_trends.csv")

        # Initial history
        excel_tool.write_sheet(trend_file, ["Date", "Gold", "Silver", "Platinum"], [
            ["2026-09-20", "2640.00", "30.80", "980.00"],
            ["2026-09-21", "2645.50", "31.10", "985.00"],
        ])

        # Today's market rates
        today_rates = {
            "Gold": 2655.00,
            "Silver": 31.45,
            "Platinum": 992.00,
        }

        output_dir = os.path.join(self.test_dir, "commodity_mail")
        result = erp_tool.process_daily_commodity_update(
            commodity_data=today_rates,
            history_file=trend_file,
            email_to="treasury@company.com",
            output_dir=output_dir
        )

        # Check sheet rows appended
        updated = excel_tool.read_sheet(trend_file)
        self.assertEqual(len(updated["rows"]), 3)
        self.assertEqual(updated["rows"][-1][1], "2655.00")

        # Check email draft
        draft = result["email_draft"]
        self.assertEqual(draft["to"], "treasury@company.com")
        self.assertTrue(os.path.exists(draft["eml_path"]))
        self.assertIn("commodity_trends.csv", draft["attachments"])

    # ------------------------------------------------------------
    # Recording Ingestion & Replay Integration
    # ------------------------------------------------------------
    def test_workflow_recording_ingestion_and_replay_compilation(self):
        demo_data = {
            "title": "Automate PO Upload to ERP",
            "workflow": "po_upload",
            "screen": [1920, 1080],
            "steps": [
                {"kind": "open", "target": "http://localhost:8000/desk#Form/Sales%20Order/new"},
                {"kind": "type", "target": {"role": "textbox", "name": "Customer"}, "text": "Acme Industries"},
                {"kind": "upload", "target": {"role": "button", "name": "Attach PO Document"}, "files": ["/data/PO-1024.pdf"]},
                {"kind": "click", "target": {"role": "button", "name": "Save Draft"}},
            ]
        }

        # Ingestion test
        fmt = ingest.detect(demo_data)
        self.assertEqual(fmt, ingest.WORKFLOW_DEMO)

        imported = ingest.import_recording(demo_data)
        self.assertEqual(imported["format"], ingest.WORKFLOW_DEMO)
        self.assertEqual(imported["imported"], 4)
        session_id = imported["session"]

        # Verify events recorded properly
        meta, events = recording.load(session_id)
        self.assertEqual(len(events), 4)
        upload_event = events[2]
        self.assertEqual(upload_event["kind"], "upload")
        self.assertEqual(upload_event["files"], ["/data/PO-1024.pdf"])

        # Compile program
        prog = replay.compile_program(meta, events)
        self.assertEqual(len(prog["steps"]), 4)

        # Verify upload step compiled with target and files
        upload_step = prog["steps"][2]
        self.assertEqual(upload_step["kind"], "upload")
        self.assertEqual(upload_step["files"], ["/data/PO-1024.pdf"])


if __name__ == "__main__":
    unittest.main()
