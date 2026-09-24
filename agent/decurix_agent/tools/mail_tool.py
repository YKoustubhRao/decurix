import email
from email import policy
from email.message import EmailMessage
import mimetypes
import os
from pathlib import Path


# ============================================================
# Email Extraction and Draft Generation Tool
# ============================================================

def parse_email_message(eml_path, extract_attachments_dir=None):
    """
    Parse an .eml file.
    Extracts headers, body text, and any attached files.
    """
    path = Path(eml_path).expanduser()
    if not path.exists():
        raise FileNotFoundError(f"Email file not found: {eml_path}")

    with open(path, "rb") as f:
        msg = email.message_from_binary_file(f, policy=policy.default)

    subject = str(msg.get("subject", ""))
    sender = str(msg.get("from", ""))
    recipients = str(msg.get("to", ""))
    date_str = str(msg.get("date", ""))

    body_text = ""
    body_html = ""

    body_part = msg.get_body(preferencelist=("plain", "html"))
    if body_part:
        if body_part.get_content_type() == "text/plain":
            body_text = body_part.get_content()
        elif body_part.get_content_type() == "text/html":
            body_html = body_part.get_content()

    attachments = []
    attach_dir = Path(extract_attachments_dir).expanduser() if extract_attachments_dir else None
    if attach_dir:
        attach_dir.mkdir(parents=True, exist_ok=True)

    for part in msg.iter_attachments():
        filename = part.get_filename() or "attachment"
        content_type = part.get_content_type()
        payload = part.get_payload(decode=True)

        saved_path = None
        if attach_dir and payload:
            saved_file = attach_dir / filename
            saved_file.write_bytes(payload)
            saved_path = str(saved_file)

        attachments.append({
            "filename": filename,
            "content_type": content_type,
            "size": len(payload) if payload else 0,
            "saved_path": saved_path,
        })

    return {
        "subject": subject,
        "from": sender,
        "to": recipients,
        "date": date_str,
        "body_text": body_text,
        "body_html": body_html,
        "attachments": attachments,
    }


def create_email_draft(to, subject, body_text, attachments=None, from_addr="noreply@company.com", output_path=None):
    """
    Create a MIME email message draft with attachments and optionally write to an .eml file.
    """
    msg = EmailMessage()
    msg["From"] = from_addr
    msg["To"] = to
    msg["Subject"] = subject
    msg.set_content(body_text)

    attached_files = []
    if attachments:
        for att in attachments:
            att_path = Path(att).expanduser()
            if not att_path.exists():
                continue
            ctype, encoding = mimetypes.guess_type(str(att_path))
            if ctype is None or encoding is not None:
                ctype = "application/octet-stream"
            maintype, subtype = ctype.split("/", 1)
            file_data = att_path.read_bytes()
            msg.add_attachment(
                file_data,
                maintype=maintype,
                subtype=subtype,
                filename=att_path.name
            )
            attached_files.append(att_path.name)

    saved_file = None
    if output_path:
        out = Path(output_path).expanduser()
        out.parent.mkdir(parents=True, exist_ok=True)
        with open(out, "wb") as f:
            f.write(msg.as_bytes())
        saved_file = str(out)

    return {
        "to": to,
        "subject": subject,
        "body_preview": body_text[:200] + "..." if len(body_text) > 200 else body_text,
        "attachments": attached_files,
        "eml_path": saved_file,
    }


def prepare_customer_statements(customers_invoices, output_dir):
    """
    Workflow 8: Customer statement preparation.
    Retrieves open invoices for each customer, generates statements and drafts emails.
    `customers_invoices`: dict of customer -> {
        "email": str,
        "invoices": [{"invoice_no": ..., "date": ..., "amount": ..., "due_date": ...}],
        "statement_pdf": str (path)
    }
    """
    out_dir = Path(output_dir).expanduser()
    out_dir.mkdir(parents=True, exist_ok=True)

    prepared_emails = []
    for cust_name, info in customers_invoices.items():
        email_addr = info.get("email", f"{cust_name.lower().replace(' ', '.')}@example.com")
        invoices = info.get("invoices", [])
        total_open = sum(inv["amount"] for inv in invoices)

        subject = f"Statement of Account - {cust_name}"
        lines = [
            f"Dear {cust_name},",
            "",
            "Please find attached your latest Statement of Account.",
            f"Total Outstanding Balance: ${total_open:,.2f}",
            "",
            "Open Invoices:",
        ]
        for inv in invoices:
            lines.append(f" - Inv #{inv.get('invoice_no')}: ${inv.get('amount'):,.2f} (Due: {inv.get('due_date')})")
        lines.extend([
            "",
            "Please review the attached statement and let us know if you have any questions.",
            "",
            "Best regards,",
            "Accounts Team"
        ])
        body = "\n".join(lines)

        attachments = []
        if info.get("statement_pdf") and os.path.exists(info["statement_pdf"]):
            attachments.append(info["statement_pdf"])

        draft_file = out_dir / f"Statement_{cust_name.replace(' ', '_')}.eml"
        draft = create_email_draft(
            to=email_addr,
            subject=subject,
            body_text=body,
            attachments=attachments,
            output_path=str(draft_file)
        )
        prepared_emails.append(draft)

    return prepared_emails


def prepare_ar_followup_emails(invoices, output_dir):
    """
    Workflow 9: Accounts-receivable follow-up.
    - Exports overdue invoices
    - Excludes disputed accounts
    - Prepares personalized reminder emails with correct invoices attached.
    `invoices`: list of dicts:
      {"invoice_no": ..., "customer": ..., "email": ..., "due_days": ..., "amount": ..., "disputed": bool, "pdf_path": ...}
    """
    out_dir = Path(output_dir).expanduser()
    out_dir.mkdir(parents=True, exist_ok=True)

    # Filter out disputed invoices
    valid_invoices = [inv for inv in invoices if not inv.get("disputed", False)]
    disputed_invoices = [inv for inv in invoices if inv.get("disputed", False)]

    # Group by customer
    grouped = {}
    for inv in valid_invoices:
        cust = inv["customer"]
        grouped.setdefault(cust, []).append(inv)

    prepared_emails = []
    for cust, cust_invoices in grouped.items():
        email_addr = cust_invoices[0].get("email", f"{cust.lower().replace(' ', '.')}@example.com")
        total_overdue = sum(inv["amount"] for inv in cust_invoices)
        max_days = max(inv.get("due_days", 0) for inv in cust_invoices)

        urgency = "URGENT PAYMENT REMINDER" if max_days > 60 else "Payment Reminder"
        subject = f"{urgency}: Overdue Invoices for {cust}"

        body_lines = [
            f"Dear {cust},",
            "",
            f"This is a reminder that the following invoices are overdue (up to {max_days} days):",
            ""
        ]
        attachments = []
        for inv in cust_invoices:
            body_lines.append(f" - Invoice #{inv['invoice_no']}: ${inv['amount']:,.2f} ({inv.get('due_days', 0)} days overdue)")
            if inv.get("pdf_path") and os.path.exists(inv["pdf_path"]):
                attachments.append(inv["pdf_path"])

        body_lines.extend([
            "",
            f"Total Overdue: ${total_overdue:,.2f}",
            "",
            "Please arrange for immediate settlement. If payment has already been sent, please disregard this note.",
            "",
            "Sincerely,",
            "Credit & Collections"
        ])

        draft_file = out_dir / f"AR_Reminder_{cust.replace(' ', '_')}.eml"
        draft = create_email_draft(
            to=email_addr,
            subject=subject,
            body_text="\n".join(body_lines),
            attachments=attachments,
            output_path=str(draft_file)
        )
        prepared_emails.append(draft)

    return {
        "disputed_excluded_count": len(disputed_invoices),
        "disputed_invoices": [inv["invoice_no"] for inv in disputed_invoices],
        "reminders_prepared": len(prepared_emails),
        "emails": prepared_emails,
    }

