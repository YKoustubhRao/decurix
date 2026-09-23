import csv
import io
import json
import math
import os
from pathlib import Path
import xml.etree.ElementTree as ET
import zipfile

# Try importing openpyxl if available
try:
    import openpyxl
    HAS_OPENPYXL = True
except ImportError:
    HAS_OPENPYXL = False


# ============================================================
# Excel & Spreadsheet Automation Tool
# ============================================================

def read_sheet(filepath, sheet_name=None):
    """
    Read tabular data from CSV, TSV, or XLSX file.
    Returns: {"headers": [str], "rows": [[val, ...], ...], "sheet": str}
    """
    path = Path(filepath).expanduser()
    if not path.exists():
        raise FileNotFoundError(f"Spreadsheet file does not exist: {filepath}")

    ext = path.suffix.lower()
    if ext in (".csv", ".tsv"):
        delimiter = "\t" if ext == ".tsv" else ","
        with open(path, "r", encoding="utf-8-sig", errors="replace") as f:
            reader = csv.reader(f, delimiter=delimiter)
            all_rows = [row for row in reader if any(cell.strip() for cell in row)]
        if not all_rows:
            return {"headers": [], "rows": [], "sheet": path.stem}
        headers = [h.strip() for h in all_rows[0]]
        rows = all_rows[1:]
        return {"headers": headers, "rows": rows, "sheet": path.stem}

    if ext in (".xlsx", ".xlsm"):
        if HAS_OPENPYXL:
            wb = openpyxl.load_workbook(path, data_only=True)
            active_sheet = wb[sheet_name] if sheet_name and sheet_name in wb.sheetnames else wb.active
            data = []
            for row in active_sheet.iter_rows(values_only=True):
                if any(c is not None and str(c).strip() != "" for c in row):
                    data.append([c if c is not None else "" for c in row])
            if not data:
                return {"headers": [], "rows": [], "sheet": active_sheet.title}
            headers = [str(h).strip() for h in data[0]]
            rows = data[1:]
            return {"headers": headers, "rows": rows, "sheet": active_sheet.title}
        else:
            return _read_xlsx_xml(path, sheet_name)

    raise ValueError(f"Unsupported spreadsheet format: {ext}")


def _read_xlsx_xml(path, target_sheet=None):
    """Fallback pure-Python standard library reader for XLSX using zipfile + XML."""
    with zipfile.ZipFile(path, "r") as z:
        # Load shared strings
        shared_strings = []
        if "xl/sharedStrings.xml" in z.namelist():
            tree = ET.fromstring(z.read("xl/sharedStrings.xml"))
            for si in tree.findall("{http://schemas.openxmlformats.org/spreadsheetml/2006/main}si"):
                t = si.find("{http://schemas.openxmlformats.org/spreadsheetml/2006/main}t")
                shared_strings.append(t.text if t is not None and t.text else "")

        # Find sheets
        sheet_path = "xl/worksheets/sheet1.xml"
        if "xl/workbook.xml" in z.namelist():
            wb_tree = ET.fromstring(z.read("xl/workbook.xml"))
            sheets = wb_tree.findall(".//{http://schemas.openxmlformats.org/spreadsheetml/2006/main}sheet")
            if target_sheet:
                for idx, s in enumerate(sheets, start=1):
                    if s.attrib.get("name") == target_sheet:
                        sheet_path = f"xl/worksheets/sheet{idx}.xml"
                        break

        if sheet_path not in z.namelist():
            sheet_path = [n for n in z.namelist() if n.startswith("xl/worksheets/sheet")][0]

        tree = ET.fromstring(z.read(sheet_path))
        data_rows = []
        ns = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
        for row_elem in tree.findall(f".//{ns}row"):
            row = []
            for c_elem in row_elem.findall(f"{ns}c"):
                t_attr = c_elem.attrib.get("t")
                v_elem = c_elem.find(f"{ns}v")
                val = v_elem.text if v_elem is not None else ""
                if t_attr == "s" and val.isdigit() and int(val) < len(shared_strings):
                    val = shared_strings[int(val)]
                row.append(val)
            if any(cell.strip() for cell in row if isinstance(cell, str)):
                data_rows.append(row)

        if not data_rows:
            return {"headers": [], "rows": [], "sheet": "Sheet1"}
        return {"headers": [str(h).strip() for h in data_rows[0]], "rows": data_rows[1:], "sheet": "Sheet1"}


def write_sheet(filepath, headers, rows, sheet_name="Sheet1"):
    """Write headers and rows to CSV or XLSX."""
    path = Path(filepath).expanduser()
    path.parent.mkdir(parents=True, exist_ok=True)
    ext = path.suffix.lower()

    if ext in (".csv", ".tsv"):
        delimiter = "\t" if ext == ".tsv" else ","
        with open(path, "w", newline="", encoding="utf-8") as f:
            writer = csv.writer(f, delimiter=delimiter)
            if headers:
                writer.writerow(headers)
            writer.writerows(rows)
        return str(path)

    if ext in (".xlsx", ".xlsm") and HAS_OPENPYXL:
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = sheet_name
        if headers:
            ws.append(headers)
        for r in rows:
            ws.append(r)
        wb.save(path)
        return str(path)

    # Fallback to CSV if openpyxl not installed
    csv_fallback = path.with_suffix(".csv")
    with open(csv_fallback, "w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        if headers:
            writer.writerow(headers)
        writer.writerows(rows)
    return str(csv_fallback)


def append_rows(filepath, rows, sheet_name=None):
    """
    Append rows to an existing spreadsheet.
    (Used in Workflow 10 for daily commodity updates).
    """
    path = Path(filepath).expanduser()
    if not path.exists():
        raise FileNotFoundError(f"Spreadsheet does not exist: {filepath}")

    ext = path.suffix.lower()
    if ext in (".csv", ".tsv"):
        delimiter = "\t" if ext == ".tsv" else ","
        with open(path, "a", newline="", encoding="utf-8") as f:
            writer = csv.writer(f, delimiter=delimiter)
            writer.writerows(rows)
        return len(rows)

    if ext in (".xlsx", ".xlsm") and HAS_OPENPYXL:
        wb = openpyxl.load_workbook(path)
        ws = wb[sheet_name] if sheet_name and sheet_name in wb.sheetnames else wb.active
        for r in rows:
            ws.append(r)
        wb.save(path)
        return len(rows)

    # Fallback for CSV
    with open(path, "a", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerows(rows)
    return len(rows)


def generate_price_comparison(current_catalog_path, revised_supplier_path, output_path,
                              code_col="Product Code", name_col="Product Name", price_col="Price"):
    """
    Workflow 4: Product-Price Update.
    Compares existing catalog prices against revised supplier prices.
    Identifies affected products and generates a review spreadsheet.
    """
    current_data = read_sheet(current_catalog_path)
    revised_data = read_sheet(revised_supplier_path)

    c_headers = [h.strip().lower() for h in current_data["headers"]]
    r_headers = [h.strip().lower() for h in revised_data["headers"]]

    def find_idx(headers, name):
        name_lower = name.lower()
        for idx, h in enumerate(headers):
            if name_lower in h:
                return idx
        return 0

    c_code_idx = find_idx(c_headers, code_col)
    c_price_idx = find_idx(c_headers, price_col)
    c_name_idx = find_idx(c_headers, name_col) if any(name_col.lower() in h for h in c_headers) else -1

    r_code_idx = find_idx(r_headers, code_col)
    r_price_idx = find_idx(r_headers, price_col)
    r_name_idx = find_idx(r_headers, name_col) if any(name_col.lower() in h for h in r_headers) else -1

    catalog = {}
    for row in current_data["rows"]:
        if len(row) > max(c_code_idx, c_price_idx):
            code = str(row[c_code_idx]).strip()
            name = str(row[c_name_idx]).strip() if c_name_idx >= 0 and len(row) > c_name_idx else code
            try:
                price = float(str(row[c_price_idx]).replace("$", "").replace(",", "").strip())
            except ValueError:
                price = 0.0
            catalog[code] = {"name": name, "price": price}

    review_headers = [
        "Product Code", "Product Name", "Current Price", "Revised Price",
        "Change Amount", "% Change", "Status"
    ]
    review_rows = []
    affected_count = 0
    total_change_pct = 0.0

    for row in revised_data["rows"]:
        if len(row) > max(r_code_idx, r_price_idx):
            code = str(row[r_code_idx]).strip()
            if not code or code not in catalog:
                continue
            try:
                new_price = float(str(row[r_price_idx]).replace("$", "").replace(",", "").strip())
            except ValueError:
                continue

            current_entry = catalog[code]
            old_price = current_entry["price"]
            diff = round(new_price - old_price, 2)
            pct = round((diff / old_price) * 100, 2) if old_price > 0 else 0.0

            if abs(diff) > 0.001:
                status = "INCREASE" if diff > 0 else "DECREASE"
                affected_count += 1
                total_change_pct += pct
            else:
                status = "UNCHANGED"

            review_rows.append([
                code, current_entry["name"], f"{old_price:.2f}", f"{new_price:.2f}",
                f"{diff:+.2f}", f"{pct:+.2f}%", status
            ])

    # Sort affected items to top
    review_rows.sort(key=lambda r: (r[6] == "UNCHANGED", r[0]))

    saved_path = write_sheet(output_path, review_headers, review_rows, sheet_name="Price_Changes")
    avg_pct = round(total_change_pct / affected_count, 2) if affected_count > 0 else 0.0

    return {
        "output_file": saved_path,
        "total_products_checked": len(review_rows),
        "affected_products": affected_count,
        "average_change_pct": avg_pct,
        "rows": review_rows,
    }


def reconcile_mis_model(model_path, new_sheets_data, control_totals=None):
    """
    Workflow 5: Recurring MIS Report (30+ min workflow).
    Updates multi-sheet MIS models, calculates totals, checks against control totals,
    and reports reconciliation status.
    `new_sheets_data`: dict of {"SheetName": {"headers": [...], "rows": [[...], ...]}}
    `control_totals`: dict of {"MetricName": expected_float_total}
    """
    summary = {
        "model": str(model_path),
        "sheets_updated": list(new_sheets_data.keys()),
        "checks": [],
        "ok": True,
        "kpis": {},
    }

    # Calculate actual totals from updated data
    computed_totals = {}
    for sheet_name, data in new_sheets_data.items():
        headers = [h.strip().lower() for h in data.get("headers", [])]
        rows = data.get("rows", [])
        # Find numeric columns
        for col_idx, col_name in enumerate(headers):
            col_sum = 0.0
            is_numeric = False
            for r in rows:
                if len(r) > col_idx:
                    try:
                        clean_val = str(r[col_idx]).replace("$", "").replace(",", "").strip()
                        col_sum += float(clean_val)
                        is_numeric = True
                    except (ValueError, TypeError):
                        pass
            if is_numeric:
                computed_totals[f"{sheet_name}.{col_name}"] = round(col_sum, 2)

    summary["kpis"] = computed_totals

    if control_totals:
        for metric, expected in control_totals.items():
            matched_key = next((k for k in computed_totals if metric.lower() in k.lower()), None)
            actual = computed_totals.get(matched_key, 0.0) if matched_key else 0.0
            diff = round(abs(actual - expected), 2)
            passed = diff < 0.01
            if not passed:
                summary["ok"] = False
            summary["checks"].append({
                "metric": metric,
                "expected": expected,
                "actual": actual,
                "diff": diff,
                "passed": passed,
            })

    return summary


def clean_erp_report(report_path, output_path, skip_header_rows=0, keep_columns=None):
    """
    Workflow 7: Ship out auto-prepared reports from Tally/Busy/ERP.
    Cleans raw exports (removing extraneous headers, filtering columns, formatting data).
    """
    raw = read_sheet(report_path)
    all_data = [raw["headers"]] + raw["rows"] if raw["headers"] else raw["rows"]

    if skip_header_rows > 0 and len(all_data) > skip_header_rows:
        all_data = all_data[skip_header_rows:]

    if not all_data:
        return write_sheet(output_path, [], [])

    headers = [str(h).strip() for h in all_data[0]]
    rows = all_data[1:]

    if keep_columns:
        keep_indices = [idx for idx, h in enumerate(headers) if any(k.lower() in h.lower() for k in keep_columns)]
        headers = [headers[i] for i in keep_indices]
        rows = [[r[i] if i < len(r) else "" for i in keep_indices] for r in rows]

    # Clean rows: strip whitespace
    cleaned_rows = [[str(cell).strip() for cell in r] for r in rows if any(str(c).strip() for c in r)]

    return write_sheet(output_path, headers, cleaned_rows, sheet_name="Report")

