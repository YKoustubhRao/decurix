"""
Ten back-office workflows, end to end, each verified from what it produced.

Every workflow gets realistic inputs, is asked for in plain words through the
chat API (the same door the chat page uses), and is then checked by this
script - the files it wrote, the Outbox drafts it left, and for the ERP and the
HRMS the records read back through those systems' own APIs from the desktop's
signed-in browser. The agent's own report is printed, never trusted.

The live systems are public demos: Odoo (https://demo.odoo.com, a fresh
database per visitor) and OrangeHRM (https://opensource-demo.orangehrmlive.com,
Admin/admin123 as its login page says). Nothing about them is hardcoded: the
customer, products and purchase order used are read from the instance first,
and the PO / invoice PDFs are generated from them.

    python3 bench/backoffice/e2e.py <desktop id> [--only wf1,wf4,...]

Needs the control plane on 127.0.0.1:8080 with AZURE_* set, a running desktop
on the "azure" harness, and LibreOffice + poppler on this machine (fixtures).
"""

import datetime as dt
import json
import os
import random
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request

BASE = "http://127.0.0.1:8080"
DESK = sys.argv[1] if len(sys.argv) > 1 and not sys.argv[1].startswith("-") else "office1"
ONLY = set(sys.argv[sys.argv.index("--only") + 1].split(",")) if "--only" in sys.argv else None
CONTAINER = f"desktop_{DESK}"
HOME = "/home/headless"
TODAY = dt.date.today()

results = []


# ------------------------------------------------------------------ plumbing

def check(name, ok, detail=""):
    results.append((CURRENT, name, bool(ok)))
    print(f"  {'ok ' if ok else 'BAD'} {name}" + (f"  ({str(detail)[:400]})" if detail and not ok else ""), flush=True)
    return ok


def sh(cmd, timeout=180, user="headless"):
    return subprocess.run(["docker", "exec", "-u", user, "-e", "DISPLAY=:1", "-e", "PYTHONPATH=/opt", CONTAINER,
                           "bash", "-lc", cmd], capture_output=True, text=True, timeout=timeout)


def py(script, timeout=180):
    """Run Python in the desktop; its stdout, parsed as JSON when it is."""

    r = sh("python3 - <<'PYEOF'\n" + script + "\nPYEOF", timeout=timeout)
    out = r.stdout.strip()
    try:
        return json.loads(out.splitlines()[-1]) if out else None
    except (ValueError, IndexError):
        raise SystemExit(f"desktop script failed:\n{out[-800:]}\n{r.stderr[-800:]}")


def put(local, remote_dir):
    sh(f"mkdir -p {remote_dir}")
    subprocess.run(["docker", "cp", local, f"{CONTAINER}:{remote_dir}/"], check=True)
    subprocess.run(["docker", "exec", "-u", "root", CONTAINER, "chown", "-R", "1001:1001", remote_dir], check=True)
    return f"{remote_dir}/{os.path.basename(local)}"


def get(remote, local_dir):
    subprocess.run(["docker", "cp", f"{CONTAINER}:{remote}", local_dir], check=True)
    return os.path.join(local_dir, os.path.basename(remote))


def api(path, data=None, timeout=120):
    body = urllib.parse.urlencode(data).encode() if data is not None else None
    for _ in range(10):
        try:
            with urllib.request.urlopen(urllib.request.Request(BASE + path, data=body), timeout=timeout) as r:
                return json.loads(r.read() or b"{}")
        except OSError:
            time.sleep(3)
    raise SystemExit(f"the control plane did not answer {path}")


def chat():
    return api(f"/api/desktops/chat?id={DESK}")


def ask(text, timeout=1800):
    """Send a request and wait for the turn to end. Returns (reply entry, seconds)."""

    # By id, not position: a desktop's chat keeps its last 200 entries, so once
    # it is full a new reply does not lengthen the list.
    seen = {e.get("id") for e in chat().get("entries") or []}
    t0 = time.time()
    api("/api/desktops/send", {"id": DESK, "content": text})
    while time.time() - t0 < timeout:
        th = chat()
        replies = [e for e in th.get("entries") or [] if e.get("id") not in seen and e.get("role") != "user"]
        if replies and not th.get("running"):
            return replies[-1], time.time() - t0
        time.sleep(4)
    raise SystemExit("no reply in time")


def outbox_since(t0):
    return [d for d in api("/api/outbox/list").get("drafts", [])
            if dt.datetime.fromisoformat(d["created_at"].replace("Z", "+00:00")).timestamp() >= t0 - 5]


def html_to_pdf(html, name, workdir):
    src = os.path.join(workdir, name + ".html")
    with open(src, "w") as f:
        f.write("<html><body style='font-family:Arial'>" + html + "</body></html>")
    subprocess.run(["soffice", "--headless", "--convert-to", "pdf", "--outdir", workdir, src],
                   capture_output=True, timeout=180, check=True)
    return os.path.join(workdir, name + ".pdf")


def scan_of(pdf, name, workdir):
    """A page as a scanned image: a PNG with no text layer, so only reading the picture finds its words."""

    base = os.path.join(workdir, name)
    subprocess.run(["pdftoppm", "-r", "100", "-png", "-singlefile", pdf, base], check=True, timeout=120)
    return base + ".png"


def newest(pattern_dir, ext, exclude=()):
    """Files with `ext` in a desktop folder, newest first, minus inputs."""

    out = sh(f"ls -t {pattern_dir}/*{ext} 2>/dev/null").stdout.split("\n")
    return [p for p in out if p and os.path.basename(p) not in exclude and ".orig." not in p]


def made_since(t0, ext, exclude=()):
    """Files with `ext` written anywhere under ~/work since t0, newest first: where an output is saved is the
    agent's choice when the request names no folder, so it is looked for, not assumed."""

    out = sh(f"find {HOME}/work -name '*{ext}' -newermt @{int(t0) - 2} -printf '%T@ %p\\n' | sort -rn | cut -d' ' -f2-").stdout
    return [p for p in out.split("\n") if p and os.path.basename(p) not in exclude and ".orig." not in p]


def xlsx_rows(path):
    return py(f"""
import json, openpyxl
wb = openpyxl.load_workbook({path!r}, data_only=False)
out = {{}}
for ws in wb:
    out[ws.title] = [[c for c in row] for row in ws.iter_rows(values_only=True)]
print(json.dumps(out, default=str))
""")


def browser_rpc(url_part, js, timeout=90):
    """Evaluate `js` (an async expression) in the desktop's browser tab whose URL contains url_part."""

    return py(f"""
import json, urllib.request
from decurix_agent.computer import cdp
tabs = [t for t in json.loads(urllib.request.urlopen("http://127.0.0.1:9222/json/list").read())
        if t.get("type") == "page" and {url_part!r} in t.get("url", "")]
if not tabs:
    print(json.dumps({{"error": "no tab on {url_part}"}})); raise SystemExit
page = cdp.Page(tabs[0])
try:
    r = page.call("Runtime.evaluate", {{"expression": {js!r}, "awaitPromise": True, "returnByValue": True}}, timeout={timeout})
    print(json.dumps(r.get("result", {{}}).get("value")))
finally:
    page.close()
""", timeout=timeout + 30)


# ------------------------------------------------------------------ Odoo

ODOO_CALL = """(async () => {
  const r = await fetch('/web/dataset/call_kw', {method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({jsonrpc: '2.0', method: 'call', params: {model: %s, method: %s, args: %s, kwargs: %s}})});
  const j = await r.json(); return j.error ? {error: j.error.data ? j.error.data.message : j.error.message} : j.result;
})()"""


def odoo(model, method, args, kwargs=None):
    js = ODOO_CALL % (json.dumps(model), json.dumps(method), json.dumps(args), json.dumps(kwargs or {}))
    out = browser_rpc("odoo.com", js)
    if isinstance(out, dict) and out.get("error"):
        raise SystemExit(f"odoo {model}.{method}: {out['error']}")
    return out


def open_odoo():
    """A fresh Odoo demo database in the desktop's browser, signed in; returns its origin."""

    # A previous workflow's demo database stays open in its own tab; reading
    # that tab instead of the fresh one made the next workflow fail to start.
    py("""
import json, urllib.request
for t in json.loads(urllib.request.urlopen("http://127.0.0.1:9222/json/list").read()):
    if t.get("type") == "page" and "odoo.com" in t.get("url", ""):
        urllib.request.urlopen("http://127.0.0.1:9222/json/close/" + t["id"]).read()
""")
    sh("gui open https://demo.odoo.com", timeout=240)
    state = None
    for _ in range(60):
        state = browser_rpc("odoo.com", "location.origin + ' ' + location.pathname + ' ' + document.body.innerText.length")
        if state and "/odoo" in state and int(state.split()[-1]) > 100:
            return state.split()[0]
        time.sleep(5)
    raise SystemExit(f"the Odoo demo did not open: {state}")


# ------------------------------------------------------------------ workflows

def wf1_po_to_sales_order(work):
    open_odoo()
    customer = odoo("res.partner", "search_read", [[["customer_rank", ">", 0], ["is_company", "=", True]]],
                    {"fields": ["name"], "limit": 1})[0]
    # A real PO names the exact item: a product with variants ("Stool" in Gray, Green, Navy) is written with its
    # variant, as Odoo's own display name has it - "Stool" alone is a question the agent rightly asks.
    candidates = odoo("product.product", "search_read", [[["sale_ok", "=", True], ["list_price", ">", 10]]],
                      {"fields": ["display_name", "name", "list_price"], "limit": 40})
    products = list({p["name"]: p for p in candidates}.values())[:3]
    for p in products:
        p["name"] = p["display_name"].split("] ", 1)[-1]
    po = f"PO-{random.randint(10000, 99999)}"
    lines = [(p["name"], random.randint(1, 9), round(p["list_price"], 2)) for p in products]
    rows = "".join(f"<tr><td>{n}</td><td>{q}</td><td>{p:,.2f}</td><td>{q * p:,.2f}</td></tr>" for n, q, p in lines)
    pdf = html_to_pdf(f"<h2>{customer['name'].upper()}</h2><h3>PURCHASE ORDER</h3><p>PO Number: <b>{po}</b> &nbsp; "
                      f"Date: {TODAY:%d/%m/%Y}</p><p>Supplier: YourCompany</p><table border=1 cellpadding=5>"
                      f"<tr><th>Description</th><th>Qty</th><th>Unit Price</th><th>Amount</th></tr>{rows}</table>",
                      po, work)
    remote = put(pdf, f"{HOME}/work/wf1")
    print(f"  PO {po} from {customer['name']}: {lines}")

    reply, took = ask(f"A customer emailed us a purchase order: {remote}. Create the corresponding sales order in our ERP as a "
                      "DRAFT quotation - it is Odoo, already open and signed in in the browser. Use the PO's quantities and "
                      "prices, put the customer's PO number in the customer reference, and do not confirm or send it.")
    print(f"  ({took:.0f}s) {reply.get('content', '')[:700]}")

    orders = odoo("sale.order", "search_read", [[["client_order_ref", "=", po]]],
                  {"fields": ["name", "state", "partner_id", "order_line"]})
    check("one quotation carries the PO number", len(orders) == 1, orders)
    if orders:
        o = orders[0]
        check("it is a draft (not confirmed or sent)", o["state"] == "draft", o["state"])
        check("for the PO's customer", o["partner_id"][1].startswith(customer["name"]), o["partner_id"])
        got = odoo("sale.order.line", "search_read", [[["order_id", "=", o["id"]], ["display_type", "=", False]]],
                   {"fields": ["product_id", "product_uom_qty", "price_unit"]})
        want = sorted((n, float(q), p) for n, q, p in lines)
        # A line left with no product (an empty row the agent added) is a
        # line the PO does not have - reported, not a crash.
        have = sorted(((l["product_id"][1].split("] ")[-1] if l["product_id"] else "(no product)"),
                       l["product_uom_qty"], round(l["price_unit"], 2)) for l in got)
        check("with the PO's exact products, quantities and prices", len(have) == len(want) and
              all(any(h[0] == w[0] and h[1] == w[1] and abs(h[2] - w[2]) < 0.01 for h in have) for w in want),
              f"want {want} have {have}")


def wf2_invoice_to_bill(work):
    open_odoo()
    pos = odoo("purchase.order", "search_read", [[["state", "in", ["purchase", "done"]], ["invoice_status", "=", "to invoice"]]],
               {"fields": ["name", "partner_id", "order_line"], "limit": 1})
    if not pos:
        pos = odoo("purchase.order", "search_read", [[["state", "in", ["purchase", "done"]]]],
                   {"fields": ["name", "partner_id", "order_line"], "limit": 1})
    po = pos[0]
    lines = odoo("purchase.order.line", "search_read", [[["order_id", "=", po["id"]], ["display_type", "=", False]]],
                 {"fields": ["product_id", "product_qty", "price_unit"]})
    inv = f"INV-{random.randint(100000, 999999)}"
    # One price differs from the PO: a real invoice's commonest problem, which must be reported, not smoothed over.
    billed = [(l["product_id"][1], l["product_qty"], round(l["price_unit"] * (1.05 if i == 0 else 1), 2)) for i, l in enumerate(lines)]
    rows = "".join(f"<tr><td>{n}</td><td>{q:g}</td><td>{p:,.2f}</td><td>{q * p:,.2f}</td></tr>" for n, q, p in billed)
    subtotal = sum(q * p for _, q, p in billed)
    pdf = html_to_pdf(f"<h2>{po['partner_id'][1].upper()}</h2><h3>TAX INVOICE</h3><p>Invoice No: <b>{inv}</b> &nbsp; Date: "
                      f"{TODAY:%d/%m/%Y}</p><p>Bill to: YourCompany &nbsp; Your order: {po['name']}</p><table border=1 cellpadding=5>"
                      f"<tr><th>Item</th><th>Qty</th><th>Rate</th><th>Amount</th></tr>{rows}<tr><td colspan=3>Subtotal (before tax)</td>"
                      f"<td>{subtotal:,.2f}</td></tr></table>", inv, work)
    scan = scan_of(pdf, inv + "_scan", work)
    remote = put(scan, f"{HOME}/work/wf2")
    print(f"  invoice {inv} for {po['name']} ({po['partner_id'][1]}): {billed}")

    reply, took = ask(f"Supplier invoice received (a scan): {remote}. Extract the invoice, find the matching purchase order in our "
                      "ERP (Odoo, already open and signed in in the browser) and prepare a DRAFT vendor bill for it from that PO - "
                      "do not post or pay it. Report every difference between the invoice and the PO.")
    content = reply.get("content", "")
    print(f"  ({took:.0f}s) {content[:700]}")

    bills = odoo("account.move", "search_read", [[["move_type", "=", "in_invoice"], ["ref", "ilike", inv]]],
                 {"fields": ["name", "state", "partner_id", "invoice_origin", "invoice_line_ids"]})
    check("one vendor bill carries the invoice number", len(bills) == 1, bills)
    if bills:
        b = bills[0]
        check("it is a draft (not posted)", b["state"] == "draft", b["state"])
        check("for the PO's vendor", b["partner_id"] and b["partner_id"][0] == po["partner_id"][0], b["partner_id"])
        linked = odoo("account.move.line", "search_read", [[["move_id", "=", b["id"]], ["purchase_line_id", "!=", False]]],
                      {"fields": ["purchase_line_id"]})
        check("its lines are linked to the PO (not typed in loose)", bool(linked) or po["name"] in (b.get("invoice_origin") or ""),
              b.get("invoice_origin"))
    first = billed[0][0].split("] ")[-1]
    check("the price difference was reported", re.search(r"price|rate|unit cost", content, re.I) and first.split()[0].lower() in content.lower()
          and f"{billed[0][2]:.2f}" in content.replace(",", ""), content[:300])


def wf3_employee_onboarding(work):
    first, last = random.choice(["Ananya", "Meera", "Kavya", "Ishita"]), random.choice(["Kulkarni", "Deshpande", "Iyer", "Menon"])
    emp_id = f"OC-{random.randint(1000, 9999)}"
    remote_dir = f"{HOME}/work/wf3-{emp_id}"
    sheet = py(f"""
import json, openpyxl
from PIL import Image, ImageDraw
import os
os.makedirs({remote_dir!r}, exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'New Joiner'
for k, v in [('First Name', {first!r}), ('Middle Name', 'R'), ('Last Name', {last!r}), ('Employee ID', {emp_id!r}),
             ('Date of Birth', '1998-03-14'), ('Gender', 'Female'), ('Marital Status', 'Single'), ('Nationality', 'Indian'),
             ('Joining Date', '2026-10-01'), ('Job Title', 'Accounts Executive'), ('Department', 'Finance')]:
    ws.append([k, v])
wb.save({remote_dir!r} + '/new_joiner.xlsx')
im = Image.new('RGB', (200, 240), (200, 215, 235)); d = ImageDraw.Draw(im)
d.ellipse((60, 40, 140, 120), fill=(120, 90, 70)); d.rectangle((40, 130, 160, 240), fill=(60, 80, 120)); im.save({remote_dir!r} + '/photo.png')
print(json.dumps("ok"))
""")
    offer = html_to_pdf(f"<h2>OurCo Engineering Pvt Ltd</h2><h3>Offer of Employment</h3><p>Dear {first} {last},</p><p>We are "
                        f"pleased to offer you the position of Accounts Executive, joining on 1 October 2026. Employee ID: "
                        f"<b>{emp_id}</b></p>", f"Offer_{first}", work)
    put(offer, remote_dir)
    # The trap: an ID scan that belongs to someone else. It must not be filed on her record.
    other = html_to_pdf("<p>INCOME TAX DEPARTMENT - GOVT. OF INDIA</p><p>Permanent Account Number Card</p>"
                        "<h2>BXPPK4471M</h2><p>Name<br><b>RAJESH KUMAR SHARMA</b></p><p>Date of Birth<br>02/11/1985</p>",
                        "id_card", work)
    put(scan_of(other, "ID_scan", work), remote_dir)
    print(f"  new joiner {first} {last} ({emp_id}) in {remote_dir}")

    reply, took = ask(f"Employee onboarding. HR sent the new joiner's information sheet {remote_dir}/new_joiner.xlsx with her "
                      f"documents in {remote_dir}. Create the employee in our HRMS (OrangeHRM at "
                      "https://opensource-demo.orangehrmlive.com, username Admin, password admin123): add her with her name, "
                      "employee ID and photo, fill in her personal details from the sheet, and upload her documents as "
                      "attachments on her record. Report what you entered and anything that does not look right.", timeout=2700)
    content = reply.get("content", "")
    print(f"  ({took:.0f}s) {content[:900]}")

    api_base = "https://opensource-demo.orangehrmlive.com/web/index.php/api/v2/pim"
    found = browser_rpc("orangehrmlive.com", f"""(async () => (await (await fetch('{api_base}/employees?nameOrId={emp_id}&limit=5')).json()))()""")
    emps = (found or {}).get("data") or []
    check("the employee exists with her employee ID", len(emps) == 1 and emps[0].get("firstName") == first
          and emps[0].get("lastName") == last, found)
    if emps:
        n = emps[0]["empNumber"]
        pd = browser_rpc("orangehrmlive.com", f"""(async () => (await (await fetch('{api_base}/employees/{n}/personal-details')).json()).data)()""")
        check("her personal details match the sheet", pd and pd.get("birthday") == "1998-03-14" and pd.get("gender") in ("F", 2, "2")
              and (pd.get("maritalStatus") or "").lower() == "single" and ((pd.get("nationality") or {}).get("name") or "") == "Indian", pd)
        att = browser_rpc("orangehrmlive.com", f"""(async () => (await (await fetch('{api_base}/employees/{n}/screen/personal/attachments?limit=20')).json()).data)()""")
        names = [a.get("filename") for a in att or []]
        check("her offer letter is attached", any(nm and nm.startswith("Offer_") for nm in names), names)
        check("someone else's ID was not filed on her record", not any(nm and nm.startswith("ID_scan") for nm in names), names)
    check("the report says the ID scan is someone else's", re.search(r"(rajesh|someone else|not (hers|her)|does not (match|belong))",
                                                                     content, re.I), content[:300])


def wf4_price_update(work):
    wb = py(f"""
import json, openpyxl, os
os.makedirs('{HOME}/work/wf4', exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'Catalogue'
ws.append(['Item Code', 'Description', 'Supplier', 'UOM', 'Current Price (INR)'])
for r in [('BRG-6204','Ball bearing 6204 2RS','Mehta Bearings','Nos',142.0),('BRG-6205','Ball bearing 6205 2RS','Mehta Bearings','Nos',168.5),
          ('BRG-6306','Ball bearing 6306 ZZ','Mehta Bearings','Nos',310.0),('SL-40x62','Oil seal 40x62x8','Mehta Bearings','Nos',38.0),
          ('VB-A42','V-belt A42','Kaveri Belts','Nos',255.0),('GRS-EP2-1','EP2 grease 1kg','Mehta Bearings','Kg',420.0),
          ('BRG-6008','Ball bearing 6008','Mehta Bearings','Nos',205.0)]:
    ws.append(list(r))
wb.save('{HOME}/work/wf4/current_catalogue.xlsx'); print(json.dumps('ok'))
""")
    rows = [("6204-2RS", "Ball Bearing 6204 2RS", "Nos", "142.00", "151.00"), ("6205-2RS", "Ball Bearing 6205 2RS", "Nos", "168.50", "179.00"),
            ("6306-ZZ", "Ball Bearing 6306 ZZ", "Nos", "310.00", "348.00"), ("OS-40628", "Oil Seal 40x62x8", "Nos", "38.00", "38.00"),
            ("EP2-1KG", "EP2 Lithium Grease 1 kg", "Kg", "420.00", "398.00"), ("6010-ZZ", "Ball Bearing 6010 ZZ (new)", "Nos", "-", "265.00")]
    table = "".join("<tr>" + "".join(f"<td>{c}</td>" for c in r) + "</tr>" for r in rows)
    pdf = html_to_pdf("<h2>MEHTA BEARINGS &amp; SEALS PVT. LTD.</h2><h3>REVISED PRICE LIST - effective 01-Oct-2026</h3>"
                      "<p>Prices exclusive of GST @18%.</p><table border=1 cellpadding=5><tr><th>Our Part No.</th><th>Description"
                      f"</th><th>UOM</th><th>Old Rate</th><th>New Rate</th></tr>{table}</table><p>Items not listed remain at "
                      "existing prices.</p>", "Mehta_revised_price_list", work)
    remote = put(pdf, f"{HOME}/work/wf4")
    t0 = time.time()
    reply, took = ask(f"Our supplier Mehta Bearings sent a revised price list: {remote}. Our current catalogue is "
                      f"{HOME}/work/wf4/current_catalogue.xlsx. Find which of our catalogue items are affected and prepare the "
                      "price changes for review in a new Excel file (old price, new price, change and % change, effective "
                      "date), flagging anything that needs a decision. Then draft an email to purchase@ourco.example with the "
                      "file attached, summarising the changes.")
    print(f"  ({took:.0f}s) {reply.get('content', '')[:500]}")

    files = made_since(t0, ".xlsx", exclude=("current_catalogue.xlsx",))
    check("a review workbook was written", files, files)
    if files:
        text = json.dumps(xlsx_rows(files[0]))
        for code, price in (("BRG-6204", "151"), ("BRG-6205", "179"), ("BRG-6306", "348"), ("GRS-EP2-1", "398")):
            check(f"{code} at its new price {price}", code in text and price in text)
        check("the other supplier's item is left out", "VB-A42" not in text)
        check("the new item and the unlisted one are flagged", "6010" in text and "BRG-6008" in text)
    drafts = [d for d in outbox_since(t0) if "purchase@ourco.example" in d["to"]]
    check("a draft email to purchasing, with the workbook attached", drafts and any(f["name"].endswith(".xlsx") for f in drafts[0]["files"]), drafts)


def wf5_mis(work):
    py(f"""
import json, os, openpyxl
from openpyxl.chart import LineChart, BarChart, Reference
os.makedirs('{HOME}/work/wf5', exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'Summary'
ws.append(['Month', 'Sales', 'Purchases', 'Gross Margin', 'Margin %'])
for i, (m, s, p) in enumerate([('Apr-2026',1820000,1210000),('May-2026',1945000,1302000),('Jun-2026',2010500,1350200),('Jul-2026',1987000,1333400)], 2):
    ws.append([m, s, p, f'=B{{i}}-C{{i}}', f'=D{{i}}/B{{i}}'])
ws['G1'] = 'YTD Sales'; ws['H1'] = '=SUM(B2:B5)'; ws['G2'] = 'YTD Purchases'; ws['H2'] = '=SUM(C2:C5)'
c = LineChart(); c.title = 'Sales vs Purchases'
c.add_data(Reference(ws, min_col=2, max_col=3, min_row=1, max_row=5), titles_from_data=True)
c.set_categories(Reference(ws, min_col=1, min_row=2, max_row=5)); ws.add_chart(c, 'J2')
wb.save('{HOME}/work/wf5/MIS_FY26-27.xlsx')
for name, rows in [('sales_register_aug-2026.xlsx', [('SI-0812',412000),('SI-0813',298500),('SI-0819',655000),('SI-0825',377250),('SI-0831',318900)]),
                   ('purchase_register_aug-2026.xlsx', [('PB-2231',520400),('PB-2240',244300),('PB-2246',598100)])]:
    w = openpyxl.Workbook(); s = w.active; s.append(['Voucher', 'Taxable Value', 'GST'])
    for v, t in rows: s.append([v, t, round(t * 0.18)])
    w.save('{HOME}/work/wf5/' + name)
print(json.dumps('ok'))
""")
    t0 = time.time()
    reply, took = ask(f"Monthly MIS for August 2026. This month's exports are {HOME}/work/wf5/sales_register_aug-2026.xlsx and "
                      f"{HOME}/work/wf5/purchase_register_aug-2026.xlsx. Update our established MIS model {HOME}/work/wf5/MIS_FY26-27.xlsx "
                      "with August (taxable values, like the earlier months), keep its formulas and make sure the chart includes "
                      "August. Run total checks, then draft the distribution email to management@ourco.example and "
                      "cfo@ourco.example with the updated model attached and a short commentary on August vs July.")
    print(f"  ({took:.0f}s) {reply.get('content', '')[:500]}")

    files = [f for f in made_since(t0, ".xlsx") if "MIS" in os.path.basename(f)]
    rows = xlsx_rows(files[0]).get("Summary", []) if files else []
    aug = next((r for r in rows if r and str(r[0]).startswith("Aug")), None)
    check("August is in the model with the registers' taxable totals", aug and float(aug[1]) == 2061650 and float(aug[2]) == 1362800, aug)
    if files:
        local = get(files[0], work)
        chart = subprocess.run(["unzip", "-p", local, "xl/charts/chart1.xml"], capture_output=True, text=True).stdout
        check("the chart reads through August's row", "$6" in chart, re.findall(r"<(?:\w+:)?f>([^<]+)<", chart))
        subprocess.run(["soffice", "--headless", "--convert-to", "csv", "--outdir", work, local], capture_output=True, timeout=180)
        csv = open(os.path.splitext(local)[0] + ".csv").read()
        check("it recalculates to the right year-to-date sales", "9824150" in csv, csv[:300])
    drafts = [d for d in outbox_since(t0) if {"management@ourco.example", "cfo@ourco.example"} <= set((d.get("to") or []) + (d.get("cc") or []))]
    check("a distribution draft with the model attached", drafts and any(f["name"].endswith(".xlsx") for f in drafts[0]["files"]), drafts)


def wf6_onboarding_file(work):
    remote_dir = f"{HOME}/work/wf6"
    for name, html, as_scan in [
        ("Account_opening_form", "<h2>Customer Account Opening Form</h2><p>Company name: Rao Precision Tools LLP</p><p>PAN: AAKFR7412Q "
         "&nbsp; GSTIN: 29AAKFR7412Q1ZX</p><p>Billing address: No. 48, 2nd Stage, Peenya Industrial Area, Bengaluru 560058</p>"
         "<p>Bank: Canara Bank, A/c 124520100034 87, IFSC CNRB0001245</p>", False),
        ("GST_certificate", "<h3>Form GST REG-06 - Registration Certificate</h3><p>Registration Number: <b>29AAKFR7412Q1ZX</b></p>"
         "<p>Legal Name: RAO PRECISION TOOLS LLP</p><p>Address: No. 48, 2nd Stage, Peenya Industrial Area, Bengaluru, Karnataka, 560058</p>", False),
        ("scan_0012", "<p>INCOME TAX DEPARTMENT - GOVT. OF INDIA</p><p>Permanent Account Number Card</p><h2>AAKFR7412Q</h2>"
         "<p>Name<br><b>RAO PRECISION TOOLS LLP</b></p>", True),
        ("IMG_cheque", "<p><b>CANARA BANK</b> Peenya Industrial Estate Branch, IFSC: CNRB0001245</p><h2 style='color:#b00'>CANCELLED</h2>"
         "<p>A/c No. <b>1245 2010 0034 87</b> &nbsp; A/c Name: <b>RAO PRECISION TOOL LLP</b></p>", True)]:
        pdf = html_to_pdf(html, name, work)
        put(scan_of(pdf, name, work) if as_scan else pdf, remote_dir)
    subprocess.run(["docker", "exec", "-u", "headless", CONTAINER, "bash", "-lc", f"echo 'meeting notes' > {remote_dir}/notes.txt"])
    t0 = time.time()
    reply, took = ask(f"Prepare the customer-onboarding file for the new customer whose documents are in {remote_dir}. Check the "
                      "folder has the PAN card, GST certificate, a cancelled cheque and the account opening form, and that the "
                      "details agree with each other (name, PAN inside the GSTIN, address, bank account). Then make one PDF, in "
                      "this order: account opening form, PAN, GST certificate, cancelled cheque, named <customer>_onboarding.pdf, "
                      "and report anything that does not match.")
    content = reply.get("content", "")
    print(f"  ({took:.0f}s) {content[:500]}")
    files = [f for f in made_since(t0, ".pdf") if "onboarding" in f.lower()]
    check("a combined onboarding PDF was made", files, files)
    if files:
        local = get(files[0], work)
        pages = int(re.search(r"Pages:\s+(\d+)", subprocess.run(["pdfinfo", local], capture_output=True, text=True).stdout).group(1))
        check("with the four documents", pages == 4, pages)
        text = [subprocess.run(["pdftotext", "-f", str(i), "-l", str(i), local, "-"], capture_output=True, text=True).stdout
                for i in range(1, pages + 1)]
        check("in the order asked (form first, GST certificate third)", "Account Opening" in text[0] and "GST" in (text[2] if pages > 2 else ""),
              [t[:40] for t in text])
    check("the cheque's account name mismatch is reported", re.search(r"TOOL LLP|singular|account name", content, re.I), content[:300])


def wf7_tally_report(work):
    py(f"""
import json, os, openpyxl
os.makedirs('{HOME}/work/wf7', exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'Sales Register'
ws.append(['Rao Traders Pvt Ltd']); ws.append(['12, Industrial Estate, Hubli']); ws.append(['Sales Register'])
ws.append(['1-Aug-2026 to 31-Aug-2026']); ws.append([])
ws.append(['Date', 'Particulars', 'Vch Type', 'Vch No.', 'Debit', 'Credit'])
rows = [('02-08-2026','Sai Motors','Sales','S/101',None,118000),('05-08-2026','Kiran Pumps','Sales','S/102',None,236000),
        ('09-08-2026','Sai Motors','Credit Note','CN/7',11800,None),('14-08-2026','Deccan Agro','Sales','S/103',None,59000),
        ('21-08-2026','Rao Engineering','Sales','S/104',None,177000),('28-08-2026','Kiran Pumps','Credit Note','CN/8',23600,None)]
for r in rows: ws.append(list(r))
ws.append([None, 'Grand Total', None, None, 35400, 590000])
wb.save('{HOME}/work/wf7/Tally_SalesRegister_Aug2026.xlsx'); print(json.dumps('ok'))
""")
    t0 = time.time()
    reply, took = ask(f"Ship out the August sales register exported from Tally: {HOME}/work/wf7/Tally_SalesRegister_Aug2026.xlsx. "
                      "Clean it up for the accounts team: drop Tally's header lines, keep only the sales vouchers (no credit "
                      "notes), with a total that you check against Tally's own figures, save it as a new Excel file, and draft "
                      "an email to accounts@ourco.example with it attached.")
    print(f"  ({took:.0f}s) {reply.get('content', '')[:500]}")
    files = made_since(t0, ".xlsx", exclude=("Tally_SalesRegister_Aug2026.xlsx",))
    check("a cleaned workbook was written", files, files)
    if files:
        sheets = xlsx_rows(files[0])
        rows = next(iter(sheets.values()))
        text = json.dumps(rows)
        check("Tally's header lines are gone (the table starts at the top)", rows and "Date" in [str(c) for c in rows[0] if c], rows[:2])
        check("only the four sales vouchers remain", all(v in text for v in ("S/101", "S/102", "S/103", "S/104"))
              and "CN/7" not in text and "CN/8" not in text)
        # The total may be a formula (=SUM(...)): what counts is what it comes to when the file is opened.
        local = get(files[0], work)
        subprocess.run(["soffice", "--headless", "--convert-to", "csv", "--outdir", work, local], capture_output=True, timeout=180)
        csv = open(os.path.splitext(local)[0] + ".csv").read()
        check("the total comes to the sales vouchers' 590,000", re.search(r"\b590000(\.0+)?\b|590,000", csv), csv[-300:])
    drafts = [d for d in outbox_since(t0) if "accounts@ourco.example" in d["to"]]
    check("a draft to accounts with the cleaned file", drafts and any(f["name"].endswith(".xlsx") for f in drafts[0]["files"]), drafts)


def wf8_9_receivables(work):
    py(f"""
import json, os, openpyxl
os.makedirs('{HOME}/work/wf8', exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'Accounts Receivable'
ws.append(['Invoice', 'Posting Date', 'Due Date', 'Customer', 'Customer Email', 'Invoice Amount', 'Outstanding', 'Disputed'])
for r in [('SI-0701','2026-07-03','2026-08-02','Rao Engineering','accounts@raoeng.example',250000,250000,'No'),
          ('SI-0744','2026-07-18','2026-08-17','Rao Engineering','accounts@raoeng.example',118000,43000,'No'),
          ('SI-0761','2026-07-25','2026-08-24','Sai Motors','finance@saimotors.example',96500,96500,'Yes'),
          ('SI-0812','2026-08-02','2026-09-01','Rao Engineering','accounts@raoeng.example',486160,486160,'No'),
          ('SI-0813','2026-08-06','2026-09-05','Sai Motors','finance@saimotors.example',352230,352230,'No'),
          ('SI-0819','2026-08-14','2026-10-13','Kiran Pumps','ap@kiranpumps.example',772900,772900,'No'),
          ('SI-0831','2026-08-29','{TODAY.isoformat()}','Deccan Agro','pay@deccanagro.example',376302,376302,'No')]:
    ws.append(list(r))
wb.save('{HOME}/work/wf8/open_invoices_export.xlsx'); print(json.dumps('ok'))
""")
    t0 = time.time()
    reply, took = ask(f"Accounts-receivable follow-up. The ERP export of open invoices is {HOME}/work/wf8/open_invoices_export.xlsx. "
                      f"Today is {TODAY:%d %B %Y}. For every customer with overdue invoices (due before today), excluding disputed "
                      "invoices, prepare a customer statement (Excel, one per customer, with their open invoices, outstanding "
                      "amounts and days overdue) and draft a personalised reminder email to that customer with their statement "
                      "attached. Summarise who was chased for how much, and what was left out and why.")
    print(f"  ({took:.0f}s) {reply.get('content', '')[:500]}")
    drafts = outbox_since(t0)
    to = {a for d in drafts for a in d["to"]}
    check("reminders drafted for Rao Engineering and Sai Motors", {"accounts@raoeng.example", "finance@saimotors.example"} <= to, to)
    check("none for the customers who are not overdue", not ({"ap@kiranpumps.example", "pay@deccanagro.example"} & to), to)
    for addr, total, inv in (("accounts@raoeng.example", "779,160", "SI-0744"), ("finance@saimotors.example", "352,230", "SI-0813")):
        d = next((d for d in drafts if addr in d["to"]), None)
        check(f"{addr}'s email has its statement attached", d and any(f["name"].endswith(".xlsx") for f in d["files"]), d)
        check(f"and states the overdue {total}", d and (total in d["body"] or total.replace(",", "") in d["body"]), (d or {}).get("body", "")[:200])
    sai = next((d for d in drafts if "finance@saimotors.example" in d["to"]), None)
    check("the disputed invoice is not chased", sai and "SI-0761" not in sai["body"], (sai or {}).get("body", "")[:300])


def wf10_commodities(work):
    py(f"""
import json, os, openpyxl
os.makedirs('{HOME}/work/wf10', exist_ok=True)
wb = openpyxl.Workbook(); ws = wb.active; ws.title = 'Trend'
ws.append(['Date', 'Gold 24K (INR/10g)', 'Silver (INR/kg)', 'Source'])
wb.save('{HOME}/work/wf10/commodity_trends.xlsx'); print(json.dumps('ok'))
""")
    t0 = time.time()
    reply, took = ask(f"Daily commodity price update. Using the browser on this desktop, find today's gold (24K, per 10 g) and "
                      f"silver (per kg) prices in India from a reliable public web page, and note the source. Add today's row "
                      f"({TODAY.isoformat()}) to {HOME}/work/wf10/commodity_trends.xlsx, then draft an email to "
                      "management@ourco.example with the prices, the source and the workbook attached.")
    content = reply.get("content", "")
    print(f"  ({took:.0f}s) {content[:500]}")
    steps = " ".join(s.get("text") or "" for s in reply.get("steps") or [])
    check("the browser was used", re.search(r"Opening https?://|Looking at the screen|gui (?:ask|open|observe)\b", steps), steps[:200])
    rows = xlsx_rows(f"{HOME}/work/wf10/commodity_trends.xlsx").get("Trend", [])
    row = next((r for r in rows if r and str(r[0]).startswith(TODAY.isoformat())), None)
    check("today's row was added", row, rows)
    if row:
        gold, silver = float(str(row[1]).replace(",", "")), float(str(row[2]).replace(",", ""))
        # Per 10 g is roughly ten times per gram: a per-gram figure (~15,000) in this column is the unit error to catch.
        check("gold is a per-10 g figure (not per gram)", 50_000 < gold < 500_000, gold)
        check("silver is a per-kg figure", 20_000 < silver < 1_000_000, silver)
    drafts = [d for d in outbox_since(t0) if "management@ourco.example" in d["to"]]
    check("a draft to management with the workbook", drafts and any(f["name"].endswith(".xlsx") for f in drafts[0]["files"]), drafts)


WORKFLOWS = [
    ("wf1", "Purchase order to a draft sales order (Odoo, in the browser)", wf1_po_to_sales_order),
    ("wf2", "Scanned supplier invoice to a draft bill against its PO (Odoo)", wf2_invoice_to_bill),
    ("wf3", "Employee onboarding in the HRMS (OrangeHRM)", wf3_employee_onboarding),
    ("wf4", "Supplier price sheet to price changes for review", wf4_price_update),
    ("wf5", "Monthly MIS model update and distribution", wf5_mis),
    ("wf6", "Customer onboarding file from scans", wf6_onboarding_file),
    ("wf7", "Tally report cleaned up and shipped out", wf7_tally_report),
    ("wf8", "Customer statements and overdue reminders", wf8_9_receivables),
    ("wf10", "Daily commodity prices from the web", wf10_commodities),
]

CURRENT = ""


def settle():
    """End a task left waiting on a question: the next request would otherwise be taken as its answer."""

    for _ in range(3):
        entries = [e for e in chat().get("entries") or [] if e.get("role") != "user"]
        if not entries or entries[-1].get("kind") not in ("query", "approval"):
            return
        print("  (ending a task left waiting on a question)")
        ask("stop", timeout=300)


def main():
    global CURRENT
    for key, title, run in WORKFLOWS:
        if ONLY and key not in ONLY:
            continue
        settle()
        CURRENT = key
        print(f"\n== {key}: {title}", flush=True)
        work = tempfile.mkdtemp(prefix=f"backoffice-{key}-")
        try:
            run(work)
        except SystemExit as exc:
            check("the workflow ran to a verdict", False, exc)
        finally:
            shutil.rmtree(work, ignore_errors=True)

    print("\n== summary")
    for key, _, _ in WORKFLOWS:
        mine = [r for r in results if r[0] == key]
        if mine:
            print(f"  {key}: {sum(r[2] for r in mine)}/{len(mine)}" + ("" if all(r[2] for r in mine) else
                  "  - " + "; ".join(r[1] for r in mine if not r[2])))
    failed = [r for r in results if not r[2]]
    print("\nBACK OFFICE:", "OK" if not failed else f"{len(failed)} check(s) failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
