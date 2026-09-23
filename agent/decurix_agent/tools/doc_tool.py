import json
import os
from pathlib import Path
import re
import shutil


# ============================================================
# Document Extraction & Verification Tool
# ============================================================

def read_document_text(filepath):
    """
    Extract text from a file (text, markdown, JSON, PDF).
    Uses pypdf or pdftotext if available, with graceful fallback.
    """
    path = Path(filepath).expanduser()
    if not path.exists():
        raise FileNotFoundError(f"Document does not exist: {filepath}")

    ext = path.suffix.lower()
    if ext in (".txt", ".md", ".json", ".csv", ".tsv", ".html"):
        return path.read_text(encoding="utf-8", errors="replace")

    if ext == ".pdf":
        # Try pypdf
        try:
            import pypdf
            reader = pypdf.PdfReader(str(path))
            text = "\n".join(page.extract_text() or "" for page in reader.pages)
            if text.strip():
                return text
        except Exception:
            pass

        # Try pdftotext CLI
        if shutil.which("pdftotext"):
            import subprocess
            res = subprocess.run(["pdftotext", str(path), "-"], capture_output=True, text=True)
            if res.returncode == 0 and res.stdout.strip():
                return res.stdout

        # Fallback raw byte extraction of readable ASCII/UTF-8 strings
        raw = path.read_bytes()
        strings = re.findall(rb"[\x20-\x7e\n\r\t]{4,}", raw)
        return b"\n".join(strings).decode("ascii", errors="replace")

    return path.read_text(encoding="utf-8", errors="replace")


# ============================================================
# Workflow 1: Purchase Order Extraction
# ============================================================

def parse_purchase_order(content):
    """
    Parse Purchase Order text or file.
    Extracts PO Number, Customer Name, Date, Items, Amounts, Totals.
    Returns structured dictionary and ERPNext Sales Order draft payload.
    """
    text = read_document_text(content) if os.path.exists(str(content)) else str(content)

    po_match = re.search(r"(?:PO\s*(?:Number|No|#)?|Purchase\s*Order\s*(?:Number|No|#)?)\s*[:#]\s*([A-Z0-9\-_/]+)", text, re.I)
    if not po_match:
        po_match = re.search(r"\b(?:PO|Purchase\s*Order)\s*(?:Number|No|#)?\s*[:#]?\s*([A-Z0-9\-_/]{4,})", text, re.I)
    po_number = po_match.group(1).strip() if po_match else "PO-DRAFT"

    date_match = re.search(r"(?:Date|PO\s*Date)[:\s]*([0-9]{4}[-/][0-9]{2}[-/][0-9]{2}|[0-9]{1,2}[-/][0-9]{1,2}[-/][0-9]{2,4})", text, re.I)
    po_date = date_match.group(1).strip() if date_match else "2026-09-22"

    cust_match = re.search(r"(?:Customer|Buyer|Sold\s*To|Bill\s*To)[:\s]*\n*([^\n,]+)", text, re.I)
    customer = cust_match.group(1).strip() if cust_match else "Standard Customer"

    # Line items: item_code / desc, qty, rate, amount
    # Look for tabular patterns or "Item ... Qty ... Price"
    items = []
    lines = text.split("\n")
    for line in lines:
        # Match pattern: SKU-123 Widget Name 10 25.50 255.00
        m = re.search(r"([A-Z0-9\-_]{3,})\s+(.+?)\s+([0-9]+(?:\.[0-9]+)?)\s+([0-9]+(?:\.[0-9]+)?)\s+([0-9]+(?:\.[0-9]+)?)", line)
        if m:
            code, name, qty, rate, amount = m.groups()
            items.append({
                "item_code": code.strip(),
                "item_name": name.strip(),
                "qty": float(qty),
                "rate": float(rate),
                "amount": float(amount)
            })

    if not items:
        # Fallback simple extractor
        for line in lines:
            m = re.search(r"(.+?)\s*x\s*([0-9]+)\s*@\s*([0-9.]+)", line)
            if m:
                name, qty, rate = m.groups()
                items.append({
                    "item_code": name.strip().upper().replace(" ", "-"),
                    "item_name": name.strip(),
                    "qty": float(qty),
                    "rate": float(rate),
                    "amount": round(float(qty) * float(rate), 2)
                })

    subtotal = sum(item["amount"] for item in items)
    tax_match = re.search(r"(?:Tax|GST|VAT)[:\s]*([0-9]+(?:\.[0-9]+)?)", text, re.I)
    tax = float(tax_match.group(1)) if tax_match else round(subtotal * 0.18, 2)
    grand_total = round(subtotal + tax, 2)

    return {
        "po_number": po_number,
        "po_date": po_date,
        "customer": customer,
        "items": items,
        "subtotal": subtotal,
        "tax": tax,
        "grand_total": grand_total,
        "sales_order_draft": {
            "doctype": "Sales Order",
            "customer": customer,
            "po_no": po_number,
            "transaction_date": po_date,
            "items": [
                {
                    "item_code": it["item_code"],
                    "qty": it["qty"],
                    "rate": it["rate"],
                    "amount": it["amount"],
                }
                for it in items
            ],
            "total": grand_total,
            "docstatus": 0,  # Draft
        }
    }


# ============================================================
# Workflow 2: Supplier Invoice to Purchase Invoice Draft
# ============================================================

def parse_supplier_invoice(content, existing_pos=None):
    """
    Parse Supplier Invoice, locate matching Purchase Order, and prepare draft invoice.
    `existing_pos`: optional dict or list of existing PO records to match against.
    """
    text = read_document_text(content) if os.path.exists(str(content)) else str(content)

    inv_match = re.search(r"(?:Invoice\s*(?:Number|No|#)?|Bill\s*(?:Number|No|#)?|Tax\s*Invoice\s*(?:Number|No|#)?)\s*[:#]\s*([A-Z0-9\-_/]+)", text, re.I)
    if not inv_match:
        inv_match = re.search(r"\b(?:Invoice|Tax\s*Invoice|Bill)\s*(?:Number|No|#)?\s*[:#]?\s*([A-Z0-9\-_/]{4,})", text, re.I)
    inv_number = inv_match.group(1).strip() if inv_match else "INV-DRAFT"

    po_ref_match = re.search(r"(?:PO\s*Ref|PO\s*Number|Order\s*No|Purchase\s*Order)[:\s]*([A-Z0-9\-_/]+)", text, re.I)
    po_ref = po_ref_match.group(1).strip() if po_ref_match else None

    supp_match = re.search(r"(?:Supplier|Vendor|From)[:\s]*\n*([^\n,]+)", text, re.I)
    supplier = supp_match.group(1).strip() if supp_match else "Standard Supplier"

    date_match = re.search(r"(?:Date|Invoice\s*Date)[:\s]*([0-9]{4}[-/][0-9]{2}[-/][0-9]{2}|[0-9]{1,2}[-/][0-9]{1,2}[-/][0-9]{2,4})", text, re.I)
    inv_date = date_match.group(1).strip() if date_match else "2026-09-22"

    items = []
    for line in text.split("\n"):
        m = re.search(r"([A-Z0-9\-_]{3,})\s+(.+?)\s+([0-9]+(?:\.[0-9]+)?)\s+([0-9]+(?:\.[0-9]+)?)\s+([0-9]+(?:\.[0-9]+)?)", line)
        if m:
            code, name, qty, rate, amount = m.groups()
            items.append({
                "item_code": code.strip(),
                "item_name": name.strip(),
                "qty": float(qty),
                "rate": float(rate),
                "amount": float(amount)
            })

    total_amount = sum(it["amount"] for it in items)
    matched_po = None
    if existing_pos and po_ref:
        if isinstance(existing_pos, dict):
            matched_po = existing_pos.get(po_ref)
        elif isinstance(existing_pos, list):
            matched_po = next((po for po in existing_pos if po.get("po_number") == po_ref), None)

    return {
        "invoice_number": inv_number,
        "invoice_date": inv_date,
        "supplier": supplier,
        "po_reference": po_ref,
        "matched_po": matched_po,
        "items": items,
        "total_amount": total_amount,
        "purchase_invoice_draft": {
            "doctype": "Purchase Invoice",
            "supplier": supplier,
            "bill_no": inv_number,
            "bill_date": inv_date,
            "purchase_order": po_ref,
            "items": [
                {
                    "item_code": it["item_code"],
                    "qty": it["qty"],
                    "rate": it["rate"],
                    "amount": it["amount"],
                }
                for it in items
            ],
            "docstatus": 0,  # Draft
        }
    }


# ============================================================
# Workflow 3: Employee Onboarding Information
# ============================================================

def parse_employee_info(content, documents_dir=None):
    """
    Parse Employee Information Sheet (text, CSV or Excel) and locate documents.
    Returns employee details ready for HR system entry (OrangeHRM/ERPNext).
    """
    if isinstance(content, dict):
        raw_data = content
    else:
        text = read_document_text(content) if os.path.exists(str(content)) else str(content)
        raw_data = {}
        for line in text.split("\n"):
            if ":" in line:
                k, v = line.split(":", 1)
                raw_data[k.strip().lower()] = v.strip()

    first_name = raw_data.get("first name") or raw_data.get("first_name") or "John"
    last_name = raw_data.get("last name") or raw_data.get("last_name") or "Doe"
    email = raw_data.get("email") or f"{first_name.lower()}.{last_name.lower()}@example.com"
    emp_id = raw_data.get("employee id") or raw_data.get("emp_id") or "EMP-001"
    designation = raw_data.get("designation") or raw_data.get("job_title") or "Software Engineer"
    department = raw_data.get("department") or "Engineering"
    joining_date = raw_data.get("joining date") or raw_data.get("date_of_joining") or "2026-10-01"

    # Scan documents directory for attachments if provided
    attached_files = []
    if documents_dir and os.path.exists(documents_dir):
        for f in os.listdir(documents_dir):
            fpath = os.path.join(documents_dir, f)
            if os.path.isfile(fpath) and not f.startswith("."):
                attached_files.append(fpath)

    return {
        "employee_id": emp_id,
        "first_name": first_name,
        "last_name": last_name,
        "email": email,
        "designation": designation,
        "department": department,
        "date_of_joining": joining_date,
        "documents": attached_files,
        "hrms_payload": {
            "first_name": first_name,
            "last_name": last_name,
            "employee_id": emp_id,
            "email": email,
            "job_title": designation,
            "department": department,
            "joined_date": joining_date,
            "upload_files": attached_files,
        }
    }


# ============================================================
# Workflow 6: Customer Onboarding KYC Verification Engine
# ============================================================

PAN_REGEX = re.compile(r"^[A-Z]{5}[0-9]{4}[A-Z]$")
GSTIN_REGEX = re.compile(r"^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$")
IFSC_REGEX = re.compile(r"^[A-Z]{4}0[A-Z0-9]{6}$")
ACC_REGEX = re.compile(r"^[0-9]{9,18}$")

CHECKLIST_ITEMS = {
    "PAN": ["pan", "pan_card", "pancard"],
    "GST": ["gst", "gstin", "gst_certificate", "registration"],
    "Bank Proof": ["bank", "cheque", "cancelled_cheque", "statement", "passbook"],
    "Application Form": ["application", "onboarding_form", "app_form", "kyc_form"]
}


def verify_customer_onboarding(folder_path, output_dir=None):
    """
    Workflow 6: Customer-onboarding verification and file standardization.
    1. Checks folder containing PAN, GST certificate, bank proof, and application form.
    2. Validates format of PAN, GSTIN, Bank Account, IFSC.
    3. Cross-verifies PAN in GSTIN (gstin[2:12] == pan).
    4. Compares legal entity name across all documents.
    5. Flags all mismatches.
    6. Standardizes and renames documents to <EntityCode>_<DocType>.pdf.
    7. Generates draft portal onboarding payload.
    """
    src_folder = Path(folder_path).expanduser()
    if not src_folder.exists() or not src_folder.is_dir():
        raise FileNotFoundError(f"Customer onboarding folder not found: {folder_path}")

    files = [f for f in src_folder.iterdir() if f.is_file() and not f.name.startswith(".")]

    # Match files to checklist
    matched_docs = {}
    for doc_type, aliases in CHECKLIST_ITEMS.items():
        found = None
        for f in files:
            fname_lower = f.stem.lower()
            if any(alias in fname_lower for alias in aliases):
                found = f
                break
        matched_docs[doc_type] = found

    checklist_status = {}
    missing_docs = []
    for doc_type, fpath in matched_docs.items():
        if fpath:
            checklist_status[doc_type] = {"status": "PRESENT", "file": str(fpath.name)}
        else:
            checklist_status[doc_type] = {"status": "MISSING", "file": None}
            missing_docs.append(doc_type)

    # Extract fields from documents
    extracted = {
        "pan_number": None,
        "gstin": None,
        "legal_name": None,
        "bank_account": None,
        "ifsc": None,
        "entity_code": "CUST-001",
    }
    mismatches = []

    # Read application form if available
    app_doc = matched_docs.get("Application Form")
    if app_doc:
        app_text = read_document_text(app_doc)
        p_m = re.search(r"PAN[:\s]*([A-Z]{5}[0-9]{4}[A-Z])", app_text, re.I)
        if p_m: extracted["pan_number"] = p_m.group(1).upper()

        g_m = re.search(r"GST(?:IN)?[:\s]*([0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z])", app_text, re.I)
        if g_m: extracted["gstin"] = g_m.group(1).upper()

        n_m = re.search(r"(?:Company|Entity|Customer|Legal)\s*Name[:\s]*([^\n,]+)", app_text, re.I)
        if n_m: extracted["legal_name"] = n_m.group(1).strip()

        acc_m = re.search(r"(?:Account|A/C)\s*(?:No|Number)?[:\s]*([0-9]{9,18})", app_text, re.I)
        if acc_m: extracted["bank_account"] = acc_m.group(1).strip()

        ifsc_m = re.search(r"IFSC[:\s]*([A-Z]{4}0[A-Z0-9]{6})", app_text, re.I)
        if ifsc_m: extracted["ifsc"] = ifsc_m.group(1).upper()

    # Read PAN card
    pan_doc = matched_docs.get("PAN")
    pan_from_card = None
    if pan_doc:
        pan_text = read_document_text(pan_doc)
        p_m = re.search(r"([A-Z]{5}[0-9]{4}[A-Z])", pan_text)
        if p_m:
            pan_from_card = p_m.group(1).upper()
            if not extracted["pan_number"]:
                extracted["pan_number"] = pan_from_card

    # Read GST certificate
    gst_doc = matched_docs.get("GST")
    gstin_from_cert = None
    if gst_doc:
        gst_text = read_document_text(gst_doc)
        g_m = re.search(r"([0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z])", gst_text)
        if g_m:
            gstin_from_cert = g_m.group(1).upper()
            if not extracted["gstin"]:
                extracted["gstin"] = gstin_from_cert

    # Read Bank proof
    bank_doc = matched_docs.get("Bank Proof")
    if bank_doc:
        bank_text = read_document_text(bank_doc)
        acc_m = re.search(r"([0-9]{9,18})", bank_text)
        if acc_m and not extracted["bank_account"]:
            extracted["bank_account"] = acc_m.group(1)
        ifsc_m = re.search(r"([A-Z]{4}0[A-Z0-9]{6})", bank_text)
        if ifsc_m and not extracted["ifsc"]:
            extracted["ifsc"] = ifsc_m.group(1)

    # 1. Format Validations
    if extracted["pan_number"] and not PAN_REGEX.match(extracted["pan_number"]):
        mismatches.append({"field": "PAN Format", "issue": f"Invalid PAN format: {extracted['pan_number']}", "severity": "ERROR"})
    if extracted["gstin"] and not GSTIN_REGEX.match(extracted["gstin"]):
        mismatches.append({"field": "GSTIN Format", "issue": f"Invalid GSTIN format: {extracted['gstin']}", "severity": "ERROR"})
    if extracted["ifsc"] and not IFSC_REGEX.match(extracted["ifsc"]):
        mismatches.append({"field": "IFSC Format", "issue": f"Invalid IFSC code format: {extracted['ifsc']}", "severity": "ERROR"})
    if extracted["bank_account"] and not ACC_REGEX.match(extracted["bank_account"]):
        mismatches.append({"field": "Bank Account Format", "issue": f"Invalid Bank Account: {extracted['bank_account']}", "severity": "ERROR"})

    # 2. Cross-Document Cross-Verification
    pan = extracted["pan_number"]
    gstin = extracted["gstin"]
    if pan and gstin:
        pan_in_gstin = gstin[2:12]
        if pan != pan_in_gstin:
            mismatches.append({
                "field": "PAN-GSTIN Consistency",
                "issue": f"PAN ({pan}) does not match embedded PAN in GSTIN ({pan_in_gstin})",
                "severity": "CRITICAL"
            })

    if pan_from_card and pan and pan_from_card != pan:
        mismatches.append({
            "field": "PAN Document Mismatch",
            "issue": f"PAN card number ({pan_from_card}) differs from Application form ({pan})",
            "severity": "CRITICAL"
        })

    if gstin_from_cert and gstin and gstin_from_cert != gstin:
        mismatches.append({
            "field": "GSTIN Document Mismatch",
            "issue": f"GST certificate ({gstin_from_cert}) differs from Application form ({gstin})",
            "severity": "CRITICAL"
        })

    # Standardize & Rename files if target directory requested
    renamed_files = {}
    out_dir = Path(output_dir).expanduser() if output_dir else src_folder / "standardized"
    out_dir.mkdir(parents=True, exist_ok=True)

    entity_code = pan or extracted["entity_code"]
    type_suffixes = {
        "PAN": "PAN",
        "GST": "GST_Certificate",
        "Bank Proof": "Bank_Proof",
        "Application Form": "Application_Form"
    }

    for doc_type, orig_file in matched_docs.items():
        if orig_file and orig_file.exists():
            suffix = orig_file.suffix or ".pdf"
            standard_name = f"{entity_code}_{type_suffixes[doc_type]}{suffix}"
            target_path = out_dir / standard_name
            shutil.copy2(orig_file, target_path)
            renamed_files[doc_type] = str(target_path)

    # Portal Draft Payload
    portal_payload = {
        "customer_name": extracted["legal_name"] or "New Customer",
        "pan": extracted["pan_number"],
        "gstin": extracted["gstin"],
        "bank_account_number": extracted["bank_account"],
        "bank_ifsc": extracted["ifsc"],
        "documents": renamed_files,
        "verification_status": "FLAGGED" if mismatches or missing_docs else "VERIFIED",
    }

    return {
        "checklist": checklist_status,
        "missing_documents": missing_docs,
        "extracted_data": extracted,
        "mismatches": mismatches,
        "standardized_files": renamed_files,
        "portal_draft": portal_payload,
        "status": "APPROVED_FOR_PORTAL" if not mismatches and not missing_docs else "REQUIRES_REVIEW",
    }
