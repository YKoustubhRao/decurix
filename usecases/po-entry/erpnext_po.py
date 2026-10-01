#!/usr/bin/env python3
"""
The stage and the referee for the PO-entry use case, on ERPNext (run on the host).

    python3 erpnext_po.py setup          finish ERPNext's setup wizard as a US distributor,
                                         add its customers and items, write the POs as PDFs
    python3 erpnext_po.py check 4502     is the customer's PO in ERPNext as a sales order
                                         (draft or submitted), line for line? (exit 0 when it is)
    python3 erpnext_po.py reset          delete every draft sales order (a clean start)
    python3 erpnext_po.py list           every sales order, briefly

"Field Supply Co." is a fastener distributor shaped like the ones that re-key
emailed purchase orders into their ERP by hand (Field Fastener's CSRs did, until
they paid for Conexiom - see README.md). A customer's PO is a one-page PDF, as
it arrives by email; the clerk's job is a Sales Order with the customer's PO
number on it, one line per PO line, at the price the PO says - which is not
always the list price (PO 4502 has a contract price).

Nothing here touches a screen. ERPNext is reached over its REST API at
ERPNEXT_URL (default http://127.0.0.1:8010, see erpnext/compose.yml) as
Administrator / ERPNEXT_PASSWORD (default admin). Only the standard library.
"""

import http.cookiejar
import json
import os
import pathlib
import sys
import urllib.error
import urllib.parse
import urllib.request
from decimal import Decimal

URL = os.environ.get("ERPNEXT_URL", "http://127.0.0.1:8010").rstrip("/")
PASSWORD = os.environ.get("ERPNEXT_PASSWORD", "admin")
OUT = pathlib.Path(os.environ.get("PO_OUT", pathlib.Path(__file__).resolve().parent / "out"))

COMPANY = "Field Supply Co."
ABBR = "FSC"

CUSTOMERS = [
    ("Northwind Machining", "410 Industrial Pkwy", "Rockford, IL 61109"),
    ("Rockford Tool & Die", "2200 Harrison Ave", "Rockford, IL 61104"),
    ("Prairie Ag Equipment", "88 County Rd 9", "Belvidere, IL 61008"),
]

# Our catalogue: item code, description, list price (a box).
ITEMS = [
    ("FSC-HHCS-1213X2", "Hex cap screw 1/2-13 x 2 Gr5 ZP, box of 50", "18.40"),
    ("FSC-FW-12USS", "Flat washer 1/2 USS ZP, box of 100", "9.75"),
    ("FSC-SHCS-M8X30", "Socket head cap screw M8x1.25x30 12.9, box of 100", "32.10"),
    ("FSC-HN-M8", "Hex nut M8 Class 10 ZP, box of 100", "7.85"),
    ("FSC-LW-M8", "Split lock washer M8 ZP, box of 100", "5.20"),
    ("FSC-CB-3816X3", "Carriage bolt 3/8-16 x 3 ZP, box of 50", "14.60"),
]

# The customers' POs: who, PO number, PO date, ship-by date, lines (our item code, qty, their price).
POS = {
    "4501": {"customer": "Northwind Machining", "date": "2026-09-29", "ship_by": "2026-10-09",
             "lines": [("FSC-HHCS-1213X2", "12", "18.40"), ("FSC-FW-12USS", "6", "9.75")]},
    "4502": {"customer": "Rockford Tool & Die", "date": "2026-09-30", "ship_by": "2026-10-12",
             "lines": [("FSC-SHCS-M8X30", "4", "30.90"), ("FSC-HN-M8", "4", "7.85"), ("FSC-LW-M8", "2", "5.20")]},
    "4503": {"customer": "Prairie Ag Equipment", "date": "2026-10-01", "ship_by": "2026-10-15",
             "lines": [("FSC-CB-3816X3", "10", "14.60")]},
}


def us_date(iso):
    year, month, day = iso.split("-")
    return f"{month}/{day}/{year}"


def po_total(po):
    return sum(Decimal(q) * Decimal(p) for _, q, p in po["lines"])


# ------------------------------------------------------------------ the API


class ERPNext:
    def __init__(self, url=URL, password=PASSWORD):
        self.url = url
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self.call("POST", "/api/method/login", {"usr": "Administrator", "pwd": password})

    def call(self, method, path, data=None, form=False):
        body, headers = None, {"Accept": "application/json"}
        if data is not None:
            if form:
                body = urllib.parse.urlencode(data).encode()
                headers["Content-Type"] = "application/x-www-form-urlencoded"
            else:
                body = json.dumps(data).encode()
                headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=headers)
        try:
            with self.opener.open(request, timeout=600) as response:
                return json.loads(response.read() or b"{}")
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")
            try:
                parsed = json.loads(detail)
                detail = parsed.get("exception") or parsed.get("_server_messages") or detail
            except ValueError:
                pass
            raise SystemExit(f"{method} {path}: HTTP {exc.code}: {str(detail)[:600]}")

    def get_list(self, doctype, filters=None, fields=("name",)):
        query = urllib.parse.urlencode({"filters": json.dumps(filters or []), "fields": json.dumps(list(fields)),
                                        "limit_page_length": 0})
        return self.call("GET", f"/api/resource/{urllib.parse.quote(doctype)}?{query}")["data"]

    def get_doc(self, doctype, name):
        return self.call("GET", f"/api/resource/{urllib.parse.quote(doctype)}/{urllib.parse.quote(name)}")["data"]

    def exists(self, doctype, name):
        return bool(self.get_list(doctype, [["name", "=", name]]))

    def insert(self, doctype, doc):
        return self.call("POST", f"/api/resource/{urllib.parse.quote(doctype)}", doc)["data"]


# ------------------------------------------------------------------ setup


def finish_wizard(api):
    done = api.call("GET", "/api/method/frappe.client.get_single_value?doctype=System%20Settings&field=setup_complete")
    if done.get("message"):
        return "already set up"
    args = {
        "language": "English", "country": "United States", "timezone": "America/Chicago", "currency": "USD",
        "company_name": COMPANY, "company_abbr": ABBR, "chart_of_accounts": "Standard",
        "fy_start_date": "2026-01-01", "fy_end_date": "2026-12-31", "setup_demo": 0,
    }
    result = api.call("POST", "/api/method/frappe.desk.page.setup_wizard.setup_wizard.setup_complete",
                      {"args": json.dumps(args)}, form=True)
    return result.get("message")


def add_masters(api):
    added = []
    for name, street, city in CUSTOMERS:
        if not api.exists("Customer", name):
            api.insert("Customer", {"customer_name": name, "customer_type": "Company",
                                    "customer_group": "Commercial", "territory": "All Territories"})
            added.append(name)
    for code, description, price in ITEMS:
        if not api.exists("Item", code):
            api.insert("Item", {"item_code": code, "item_name": description, "description": description,
                                "item_group": "Products", "stock_uom": "Box", "is_stock_item": 1,
                                "standard_rate": float(price)})
            added.append(code)
    return added


def write_pdfs():
    OUT.mkdir(parents=True, exist_ok=True)
    written = []
    for number, po in POS.items():
        path = OUT / f"PO-{number}.pdf"
        path.write_bytes(po_pdf(number, po))
        written.append(str(path))
    return written


def setup():
    api = ERPNext()
    print(json.dumps({"wizard": finish_wizard(api), "added": add_masters(api), "pdfs": write_pdfs()}, indent=1))


# ------------------------------------------------------------------ the referee


def sales_orders(api, po_no=None):
    filters = [["po_no", "=", po_no]] if po_no else []
    found = []
    for row in api.get_list("Sales Order", filters, ("name", "po_no", "customer", "docstatus", "grand_total")):
        doc = api.get_doc("Sales Order", row["name"])
        found.append({
            "name": doc["name"], "customer": doc["customer"], "po_no": doc.get("po_no") or "",
            "docstatus": doc["docstatus"], "delivery_date": doc.get("delivery_date"),
            "total": str(doc.get("total")),
            "lines": [(i["item_code"], str(i["qty"]), str(i["rate"])) for i in sorted(doc["items"], key=lambda i: i["idx"])],
        })
    return found


def _same(a, b):
    return Decimal(str(a)).normalize() == Decimal(str(b)).normalize()


def check(number):
    po = POS[number]
    mine = sales_orders(ERPNext(), number)
    if not mine:
        return [f"no sales order has the customer's PO {number}"]
    if len(mine) > 1:
        return [f"{len(mine)} sales orders have PO {number}: {', '.join(m['name'] for m in mine)} - entered twice"]
    order, problems = mine[0], []
    if order["customer"] != po["customer"]:
        problems.append(f"{order['name']} is for {order['customer']!r}, the PO is from {po['customer']!r}")
    # Decurix keys the order and saves it as a draft; a person reviews it and
    # submits. Either state is the PO entered - a cancelled order is not.
    if order["docstatus"] == 2:
        problems.append(f"{order['name']} is cancelled")
    if order["delivery_date"] != po["ship_by"]:
        problems.append(f"delivery date {order['delivery_date']}, the PO says ship by {po['ship_by']}")
    if len(order["lines"]) != len(po["lines"]):
        problems.append(f"{len(order['lines'])} lines, the PO has {len(po['lines'])}")
    for n, ((want_item, want_qty, want_rate), (item, qty, rate)) in enumerate(zip(po["lines"], order["lines"]), 1):
        if item != want_item:
            problems.append(f"line {n}: item {item}, the PO says {want_item}")
        if not _same(qty, want_qty):
            problems.append(f"line {n}: qty {qty}, the PO says {want_qty}")
        if not _same(rate, want_rate):
            problems.append(f"line {n}: rate {rate}, the PO says {want_rate}")
    if not _same(order["total"], po_total(po)):
        problems.append(f"total {order['total']}, the PO says {po_total(po)}")
    return problems


# ------------------------------------------------------------------ the PO, as a PDF


def _pdf_escape(text):
    return text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")


def make_pdf(runs):
    """A one-page PDF of (x, y, size, bold, text) runs in Helvetica - no library needed."""

    content = "\n".join(
        f"BT /{'F2' if bold else 'F1'} {size} Tf {x} {y} Td ({_pdf_escape(text)}) Tj ET" for x, y, size, bold, text in runs
    ).encode("latin-1")
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "
        b"/Resources << /Font << /F1 5 0 R /F2 6 0 R >> >> >>",
        b"<< /Length " + str(len(content)).encode() + b" >>\nstream\n" + content + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>",
    ]
    out, offsets = bytearray(b"%PDF-1.4\n"), []
    for n, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += f"{n} 0 obj\n".encode() + body + b"\nendobj\n"
    xref = len(out)
    out += f"xref\n0 {len(objects) + 1}\n0000000000 65535 f \n".encode()
    out += b"".join(f"{offset:010d} 00000 n \n".encode() for offset in offsets)
    out += f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()
    return bytes(out)


def po_pdf(number, po):
    _, street, city = next(c for c in CUSTOMERS if c[0] == po["customer"])
    describe = {code: description for code, description, _ in ITEMS}
    runs = [
        (50, 740, 20, True, po["customer"]), (50, 722, 10, False, f"{street}, {city}"),
        (400, 740, 18, True, "PURCHASE ORDER"), (400, 718, 11, False, f"PO Number: {number}"),
        (400, 703, 11, False, f"PO Date: {us_date(po['date'])}"), (400, 688, 11, True, f"Ship by: {us_date(po['ship_by'])}"),
        (50, 670, 11, True, "Vendor:"), (50, 655, 11, False, COMPANY), (50, 641, 11, False, "1500 Commerce Dr, Rockford, IL 61109"),
        (40, 600, 10, True, "Line"), (70, 600, 10, True, "Your Item #"), (170, 600, 10, True, "Description"),
        (420, 600, 10, True, "Qty"), (455, 600, 10, True, "Unit Price"), (525, 600, 10, True, "Amount"),
    ]
    y = 582
    for n, (code, qty, price) in enumerate(po["lines"], start=1):
        runs += [(40, y, 10, False, str(n)), (70, y, 10, False, code), (170, y, 9, False, describe[code][:48]),
                 (420, y, 10, False, qty), (455, y, 10, False, f"${Decimal(price):,.2f}"),
                 (525, y, 10, False, f"${Decimal(qty) * Decimal(price):,.2f}")]
        y -= 18
    runs += [(455, y - 12, 11, True, "Total:"), (525, y - 12, 11, True, f"${po_total(po):,.2f}"),
             (50, y - 60, 10, False, "Prices as agreed. Quote our PO number on your acknowledgement and invoice."),
             (50, y - 75, 10, False, "Terms: Net 30. Ship best way.")]
    return make_pdf(runs)


def main(argv):
    command = argv[1] if len(argv) > 1 else ""
    if command == "setup":
        setup()
    elif command == "list":
        print(json.dumps(sales_orders(ERPNext()), indent=1))
    elif command == "reset":
        api = ERPNext()
        drafts = [o["name"] for o in api.get_list("Sales Order", [["docstatus", "=", 0]])]
        for name in drafts:
            api.call("DELETE", f"/api/resource/Sales%20Order/{urllib.parse.quote(name)}")
        print(json.dumps({"deleted_drafts": drafts}))
    elif command == "check" and len(argv) == 3 and argv[2] in POS:
        problems = check(argv[2])
        orders = sales_orders(ERPNext(), argv[2])
        state = {0: "draft", 1: "submitted", 2: "cancelled"}.get(orders[0]["docstatus"]) if len(orders) == 1 else None
        print(json.dumps({"po": argv[2], "ok": not problems, "order": orders[0]["name"] if len(orders) == 1 else None,
                          "state": state, "problems": problems}))
        return 0 if not problems else 1
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
