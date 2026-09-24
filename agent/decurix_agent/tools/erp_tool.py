from datetime import datetime
import os
from pathlib import Path
from decurix_agent.tools import excel_tool, mail_tool


# ============================================================
# ERP & Enterprise System Integration Tool
# ============================================================

def prepare_sales_order_draft(customer, po_number, po_date, items, tax_rate=0.18):
    """
    Workflow 1: Purchase Order to Sales-Order Draft.
    Prepares an ERPNext Sales Order draft document structure.
    """
    processed_items = []
    net_total = 0.0

    for it in items:
        qty = float(it.get("qty", 1))
        rate = float(it.get("rate", 0.0))
        amount = round(qty * rate, 2)
        net_total += amount
        processed_items.append({
            "item_code": it.get("item_code", "ITEM-GENERIC"),
            "item_name": it.get("item_name", it.get("item_code", "Item")),
            "qty": qty,
            "rate": rate,
            "amount": amount,
        })

    taxes = round(net_total * tax_rate, 2)
    grand_total = round(net_total + taxes, 2)

    return {
        "doctype": "Sales Order",
        "customer": customer,
        "po_no": po_number,
        "po_date": po_date,
        "items": processed_items,
        "net_total": net_total,
        "taxes": taxes,
        "grand_total": grand_total,
        "docstatus": 0,  # 0 = Draft in ERPNext
        "status": "Draft",
    }


def prepare_purchase_invoice_draft(supplier, invoice_number, invoice_date, po_reference, items, tax_rate=0.18):
    """
    Workflow 2: Supplier Invoice to Purchase Invoice Draft.
    Prepares an ERPNext Purchase Invoice draft matched against a PO.
    """
    processed_items = []
    net_total = 0.0

    for it in items:
        qty = float(it.get("qty", 1))
        rate = float(it.get("rate", 0.0))
        amount = round(qty * rate, 2)
        net_total += amount
        processed_items.append({
            "item_code": it.get("item_code", "ITEM-GENERIC"),
            "item_name": it.get("item_name", it.get("item_code", "Item")),
            "qty": qty,
            "rate": rate,
            "amount": amount,
            "purchase_order": po_reference,
        })

    taxes = round(net_total * tax_rate, 2)
    grand_total = round(net_total + taxes, 2)

    return {
        "doctype": "Purchase Invoice",
        "supplier": supplier,
        "bill_no": invoice_number,
        "bill_date": invoice_date,
        "purchase_order": po_reference,
        "items": processed_items,
        "net_total": net_total,
        "taxes": taxes,
        "grand_total": grand_total,
        "docstatus": 0,  # 0 = Draft
        "status": "Draft",
    }


def prepare_employee_onboarding(first_name, last_name, employee_id, email, job_title, department, joined_date, documents=None):
    """
    Workflow 3: Employee Onboarding payload for OrangeHRM / ERPNext.
    """
    return {
        "system": "OrangeHRM",
        "employee_id": employee_id,
        "first_name": first_name,
        "last_name": last_name,
        "email": email,
        "job_title": job_title,
        "department": department,
        "joined_date": joined_date,
        "documents": [str(d) for d in (documents or [])],
        "status": "Draft",
    }


def process_daily_commodity_update(commodity_data, history_file, email_to="management@company.com", output_dir=None):
    """
    Workflow 10: Daily price update.
    1. Checks the price of key commodities (gold, silver, etc.).
    2. Updates the trends Excel sheet with today's date and rates.
    3. Prepares the email draft with summary and trends sheet attached.
    `commodity_data`: dict of {"Gold": 2650.50, "Silver": 31.25, ...}
    """
    today_str = datetime.now().strftime("%Y-%m-%d")
    out_dir = Path(output_dir).expanduser() if output_dir else Path(history_file).parent
    out_dir.mkdir(parents=True, exist_ok=True)

    # Format new row
    # Expected columns: Date, Gold, Silver, ...
    headers = ["Date"] + list(commodity_data.keys())
    new_row = [today_str] + [f"{commodity_data[k]:.2f}" for k in commodity_data]

    hist_path = Path(history_file).expanduser()
    if hist_path.exists():
        excel_tool.append_rows(str(hist_path), [new_row])
    else:
        excel_tool.write_sheet(str(hist_path), headers, [new_row], sheet_name="Commodity_Trends")

    # Prepare summary email
    lines = [
        "Daily Commodity Prices & Trends Update",
        f"Date: {today_str}",
        "",
        "Current Commodity Spot Rates:",
    ]
    for commodity, price in commodity_data.items():
        lines.append(f" - {commodity}: ${price:,.2f}")

    lines.extend([
        "",
        "The commodity trends tracking spreadsheet has been updated.",
        "Please find the latest file attached.",
        "",
        "Best regards,",
        "Automated Market Intel Bot"
    ])

    draft_path = out_dir / f"Commodity_Update_{today_str}.eml"
    draft = mail_tool.create_email_draft(
        to=email_to,
        subject=f"Daily Commodity Update - {today_str}",
        body_text="\n".join(lines),
        attachments=[str(hist_path)],
        output_path=str(draft_path)
    )

    return {
        "date": today_str,
        "updated_commodities": commodity_data,
        "history_file": str(hist_path),
        "email_draft": draft,
    }

