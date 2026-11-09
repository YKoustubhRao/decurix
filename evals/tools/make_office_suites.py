"""
make_office_suites: writes the three suites of a chartered accountant's office's monthly GST work, from the instance
tables below, onto the ca-office mock (evals/mocks/ca-office/mock_app.py):

  office-books-v1        a client emails its month's sales bills (.docx); enter each as a Sales voucher in the books
  office-gstr1-books-v1  file a client's GSTR-1 from its books: export JSON to the client's month folder, import it into
                         the return-prep software, validate receivers, save to the portal, file with EVC (OTP)
  office-gstr1-chat-v1   file a client's GSTR-1 from the invoices it sent on chat (PDFs) and its computation workbook,
                         entering tables 4A, 5, 7, 12 and 13 on the portal by hand, and file with EVC (OTP)

For each case it writes case.json, fixture.json and files/ (bills as .docx or text-layer PDFs, the computation workbook
as .xlsx), and each suite.json. Every firm, person, GSTIN, number and amount is invented; the GSTINs carry the check
character of the GST network's mod-36 rule. Everything is deterministic: the same script writes the same bytes.

    py -3 evals/tools/make_office_suites.py            # writes them under evals/suites/
    py -3 evals/tools/make_office_suites.py --check    # fails when a committed file differs from what this writes

Expected results are the case author's: the instance tables state each bill; the expected records are worked from
them here by plain arithmetic (this file's own code, written apart from the mock's), and checked against hand-written
totals (CHECKS) so a slip in either shows.
"""

import base64
import hashlib
import json
import os
import sys
from decimal import Decimal, ROUND_HALF_UP

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from make_office import _para, _zip, xlsx_bytes  # noqa: E402
from make_pdf import pdf_bytes  # noqa: E402

SUITES = os.path.join(os.path.dirname(HERE), "suites")
MOCK = "ca-office"
HOSTS = ["chat.example.test", "mail.example.test", "books.example.test", "fileprep.example.test", "taxnet.example.test"]
ROUTE = "curl $MOCK_ROUTE -fsS -o /dev/null -b \"$HOME/.oracle-cookies\" -c \"$HOME/.oracle-cookies\""
_CHARS = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
CENT = Decimal("0.01")
MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October",
          "November", "December"]
STATES = {"29": "Karnataka", "30": "Goa", "32": "Kerala", "27": "Maharashtra"}


def gstin(first14):
    """A GSTIN from its first 14 characters and the check character the mod-36 rule gives them."""
    assert len(first14) == 14, first14
    total = 0
    for i, ch in enumerate(first14):
        p = _CHARS.index(ch) * (2 if i % 2 else 1)
        total += p // 36 + p % 36
    return first14 + _CHARS[(36 - total % 36) % 36]


def q(x):
    return Decimal(x).quantize(CENT, rounding=ROUND_HALF_UP)


def s2(x):
    return str(q(x))


def inr(x):
    """1,23,456.78"""
    whole, frac = f"{q(x):.2f}".split(".")
    if len(whole) > 3:
        head, tail = whole[:-3], whole[-3:]
        groups = []
        while len(head) > 2:
            groups.insert(0, head[-2:])
            head = head[:-2]
        if head:
            groups.insert(0, head)
        whole = ",".join(groups + [tail])
    return f"{whole}.{frac}"


_ONES = ("Zero One Two Three Four Five Six Seven Eight Nine Ten Eleven Twelve Thirteen Fourteen Fifteen Sixteen "
         "Seventeen Eighteen Nineteen").split()
_TENS = "_ _ Twenty Thirty Forty Fifty Sixty Seventy Eighty Ninety".split()


def _below_100(n):
    return _ONES[n] if n < 20 else _TENS[n // 10] + (" " + _ONES[n % 10] if n % 10 else "")


def _words(n):
    parts = []
    for size, name in ((10000000, "Crore"), (100000, "Lakh"), (1000, "Thousand"), (100, "Hundred")):
        if n >= size:
            parts.append(f"{_words(n // size)} {name}")
            n %= size
    if n:
        parts.append(_below_100(n))
    return " ".join(parts) or "Zero"


def in_words(x):
    x = q(x)
    rupees, paise = int(x), int((x - int(x)) * 100)
    return f"Rupees {_words(rupees)}" + (f" and {_below_100(paise)} Paise" if paise else "") + " Only."


def dmy(iso, sep="-"):
    y, m, d = iso.split("-")
    return f"{d}{sep}{m}{sep}{y}"


def period_words(p):
    y, m = p.split("-")
    return f"{MONTHS[int(m) - 1]} {y}"


def arn_of(g, period):
    """The ARN TaxNet gives a filed return (the mock's own rule, restated here for the graders)."""
    y, m = period.split("-")
    h = hashlib.sha256(f"{g}:{period}".encode()).hexdigest()
    return f"AA{g[:2]}{int(m):02d}{int(y) % 100:02d}{int(h[:8], 16) % 1000000:06d}{chr(65 + int(h[8:10], 16) % 26)}"


def fy_folder(period):
    """The office's month folder name: <n>. <MONTH>, counted from April."""
    y, m = (int(x) for x in period.split("-"))
    n = (m - 4) % 12 + 1
    start = y if m >= 4 else y - 1
    return f"{start}-{(start + 1) % 100:02d}", f"{n}. {MONTHS[m - 1].upper()}"


def share_folder(client, period):
    fy, month = fy_folder(period)
    return f"GST Returns/{client}/{fy}/{month}"


def fy_end(period):
    """The last day of the period's financial year (April to March)."""
    y, m = (int(x) for x in period.split("-"))
    return f"{(y if m >= 4 else y - 1) + 1}-03-31"


def next_month(period):
    y, m = (int(x) for x in period.split("-"))
    return f"{y + (m == 12)}-{m % 12 + 1:02d}"


def b64(b):
    return base64.b64encode(b).decode("ascii")


# --- documents -------------------------------------------------------------------------------------------------------

def _cell(text, width):
    paras = "".join(_para(line) for line in str(text).split("\n"))
    return f'<w:tc><w:tcPr><w:tcW w:w="{width}" w:type="dxa"/></w:tcPr>{paras}</w:tc>'


def docx_blocks(blocks):
    """A Word document of paragraphs ("p", text, bold) and tables ("t", widths, rows), deterministic."""
    body = ""
    for b in blocks:
        if b[0] == "p":
            body += _para(b[1], bold=b[2])
        else:
            widths, rows = b[1], b[2]
            borders = ('<w:tblBorders>' + "".join(f'<w:{s} w:val="single" w:sz="4" w:space="0" w:color="999999"/>'
                                                   for s in ("top", "left", "bottom", "right", "insideH", "insideV"))
                       + '</w:tblBorders>')
            grid = "<w:tblGrid>" + "".join(f'<w:gridCol w:w="{w}"/>' for w in widths) + "</w:tblGrid>"
            trs = "".join("<w:tr>" + "".join(_cell(c, w) for c, w in zip(row, widths)) + "</w:tr>" for row in rows)
            body += f'<w:tbl><w:tblPr><w:tblW w:w="{sum(widths)}" w:type="dxa"/>{borders}</w:tblPr>{grid}{trs}</w:tbl>'
            body += _para("")
    document = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
                '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">'
                f'<w:body>{body}<w:sectPr><w:pgSz w:w="11906" w:h="16838"/></w:sectPr></w:body></w:document>')
    ct = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
          '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">'
          '<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>'
          '<Default Extension="xml" ContentType="application/xml"/>'
          '<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>'
          '</Types>')
    rels = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
            '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
            '<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>'
            '</Relationships>')
    return _zip([("[Content_Types].xml", ct), ("_rels/.rels", rels), ("word/document.xml", document)])


DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
XLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
PDF = "application/pdf"


# --- parties ---------------------------------------------------------------------------------------------------------
# key: (ledger name in the books, how the bill's header writes it, address, GSTIN or "", state code)

PARTIES = {
    "sagar": ("Sagar Hospitality Group", "Sagar Hospitality Group\n(Hotel Sagar Vista)", "Malpe Road, Udupi 576108",
              gstin("29AACCS5121D1Z"), "29"),
    "udyavara": ("Udyavara Apartments", "Udyavara Apartment", "Katapady Road, Udyavara, Udupi 576105", "", "29"),
    "kadri": ("Kadri Heights Builders LLP", "Kadri Heights Builders LLP", "Kadri Heights, Bejai Main Road, Mangaluru 575004",
              gstin("29AAJFK6610D1Z"), "29"),
    "pai": ("Pai Textiles", "Pai Textiles", "Car Street, Kundapura 576201", gstin("29ABEPP4410Q1Z"), "29"),
    "mithila": ("Mithila Towers Welfare Society", "Mithila Towers Welfare Society", "Ambalpady, Udupi 576103", "", "29"),
    "bhuvana": ("Bhuvana Hospital Trust", "Bhuvana Hospital Trust", "Hospital Road, Brahmavar 576213",
                gstin("29AAATB3312E1Z"), "29"),
    "lotus": ("Lotus Mall Management LLP", "Lotus Mall Management LLP", "Lotus Mall, Kankanady, Mangaluru 575002",
              gstin("29AAFFL2280M1Z"), "29"),
    "malpe": ("Malpe Fish Processors Pvt Ltd", "Malpe Fish Processors Pvt Ltd", "Harbour Area, Malpe 576108",
              gstin("29AADCM7735K1Z"), "29"),
    "sharada": ("Sharada Residency Owners Association", "Sharada Residency Owners Association",
                "Kalsanka, Udupi 576102", "", "29"),
    "vasco": ("Vasco Bay Hotels", "Vasco Bay Hotels", "Bogmalo Road, Vasco da Gama, Goa 403802",
              gstin("30AAHFV3391G1Z"), "30"),
    "hampi": ("Hampi Residency", "Hampi Residency", "12 Kunjibettu, Udupi 576102", "", "29"),
    # the return-from-books clients' customers
    "tunga": ("Tungabhadra Builders Pvt Ltd", "Tungabhadra Builders Pvt Ltd", "Station Road, Hosapete 583201",
              gstin("29AAGCT9183K1Z"), "29"),
    "malnad": ("Malnad Heights Apartment Owners Association", "Malnad Heights Apartment Owners Association",
               "Vidyanagar, Shivamogga 577203", "", "29"),
    "belur": ("Belur Textiles", "Belur Textiles", "Temple Street, Belur 573115", gstin("29ACQPB2207N1Z"), "29"),
    "coastal": ("Coastal Mall Management LLP", "Coastal Mall Management LLP", "Coastal Mall, Hampankatta, Mangaluru 575001",
                gstin("29AAQFC6610D1Z"), "29"),
    "hampi2": ("Hampi Residency", "Hampi Residency", "Kamalapura Road, Hosapete 583201", "", "29"),
    "agumbe": ("Agumbe Hotels", "Agumbe Hotels", "Main Road, Agumbe 577411", gstin("29AAKFA9902J1Z"), "29"),
    "kudremukh": ("Kudremukh Apartments", "Kudremukh Apartments", "Kalasa Road, Chikkamagaluru 577101", "", "29"),
    "rhea": ("Rhea Dsouza", "Rhea Dsouza", "Altinho, Panaji, Goa 403001", "", "30"),
    "anand": ("Anand Kulkarni", "Anand Kulkarni", "Vidyanagar, Hubballi 580021", "", "29"),
    "leena_goa": ("Leena Fernandes", "Leena Fernandes", "Miramar, Panaji, Goa 403001", "", "30"),
    "belur2": ("Belur Textiles", "Belur Textiles", "Temple Street, Belur 573115", gstin("29ACQPB2207N2Z"), "29"),
    "tunga_k": ("Tungabhadra Builders", "Tungabhadra Builders", "Station Road, Hosapete 583201",
                gstin("29AAGCT9183K1Z"), "29"),
}


def party(key):
    name, header, address, g, st = PARTIES[key]
    return {"name": name, "header": header, "address": address, "gstin": g, "state": st}


# --- a bill and its amounts ------------------------------------------------------------------------------------------

def bill(no, when, who, lines, rate=18, hsn="9987", file=None, words=None, client_state="29", cancelled=False):
    """A tax-exclusive bill: lines are (description, quantity, rate text, amount)."""
    p = party(who)
    taxable = q(sum(Decimal(str(a)) for _, _, _, a in lines))
    inter = p["state"] != client_state
    if inter:
        igst, cgst, sgst = q(taxable * rate / 100), q(0), q(0)
    else:
        igst, cgst, sgst = q(0), q(taxable * rate / 200), q(taxable * rate / 200)
    total = taxable + igst + cgst + sgst
    return {"no": no, "date": when, "who": who, "party": p, "lines": lines, "rate": rate, "hsn": hsn,
            "taxable": taxable, "igst": igst, "cgst": cgst, "sgst": sgst, "total": total, "inter": inter,
            "file": file or f"{no} {p['name']}.docx", "words": words, "cancelled": cancelled}


def bill_docx(client, b):
    """The client's own bill template (as a Word file): letterhead, customer box, items, totals, words, bank box."""
    p = b["party"]
    half = b["rate"] / 2
    half_s = f"{half:g}"
    cust = f"{p['header']}\n{p['address']}" + (f"\nGSTIN: {p['gstin']}" if p["gstin"] else "")
    items = [["SL.NO", "Description", "Quanity", "Rate", "Amount"]]
    for i, (desc, qty, rate, amt) in enumerate(b["lines"], 1):
        items.append([f"{i:02d}", desc, qty, rate, inr(amt)])
    totals = [["BASIC PRICE", inr(b["taxable"])],
              [f"CGST @ {half_s}%", inr(b["cgst"]) if b["cgst"] else ""],
              [f"SGST@{half_s}%", inr(b["sgst"]) if b["sgst"] else ""],
              [f"IGST @ {b['rate']}%", inr(b["igst"]) if b["igst"] else ""],
              ["GRAND TOTAL", inr(b["total"])]]
    blocks = [("p", client["name"].upper(), True), ("p", client["tagline"], False),
              ("p", f"{client['address']} | {client['phone']}", False), ("p", "TAX INVOICE", True)]
    if b["cancelled"]:
        blocks.append(("p", "CANCELLED", True))
    blocks += [("t", [5200, 3800], [["Customer Name and Address,", "Invoice No : " + b["no"]],
                                    [cust, "Date : " + dmy(b["date"])]]),
               ("t", [900, 3900, 1300, 1300, 1600], items),
               ("t", [5800, 3200], totals),
               ("p", "Total Invoice Amount In Words: " + (b["words"] or in_words(b["total"])), False),
               ("p", f"Customer Signature and seal                For {client['name'].upper()}", False),
               ("p", "Acknowledge                                    Authorized Signature", False),
               ("t", [3000, 6000], [["BANK ACCOUNT DETAILS", ""], ["Cheque in favour of", client["name"].upper()],
                                    ["Bank Name", "Example Co-operative Bank, Udupi"], ["Account No", client["account"]],
                                    ["IFS Code", "EXMP0000412"], ["GST TIN NO", client["gstin"]],
                                    ["PAN", client["gstin"][2:12]], ["HSN NO", b["hsn"]]]),
               ("p", f"Office Address : {client['address']}", False)]
    return docx_blocks(blocks)


def inclusive(no, when, who, total, file=None, client_state="29", cancelled=False):
    """A tax-inclusive bill of a round amount (the construction client's): the taxable value is backed out at 18%."""
    p = party(who) if who in PARTIES else None
    return {"no": no, "date": when, "who": who, "party": p, "gross": q(total), "file": file, "client_state": client_state,
            "cancelled": cancelled}


def backed_out(b, client_state="29"):
    taxable = q(b["gross"] / Decimal("1.18"))
    inter = b["party"]["state"] != client_state
    if inter:
        igst, cgst, sgst = q(taxable * Decimal("0.18")), q(0), q(0)
    else:
        igst, cgst, sgst = q(0), q(taxable * Decimal("0.09")), q(taxable * Decimal("0.09"))
    return dict(b, taxable=taxable, igst=igst, cgst=cgst, sgst=sgst, inter=inter,
                round_off=b["gross"] - (taxable + igst + cgst + sgst))


def bill_pdf(client, b):
    """The construction client's invoice template (a text-layer PDF)."""
    p = b["party"]
    lines = [f"# {client['name'].upper()}", "TAX INVOICE                                   ORIGINAL FOR RECIPIENT",
             client["address"], f"Mobile: {client['phone']}   GSTIN: {client['gstin']}   PAN Number: {client['gstin'][2:12]}",
             "", f"Invoice No.: {b['no']}    Invoice Date: {dmy(b['date'], '/')}    Due Date: {dmy(b['due'], '/')}", "",
             "BILL TO", p["header"], p["address"]]
    if p["gstin"]:
        lines.append(f"GSTIN: {p['gstin']}")
    lines += [f"Place of Supply: {STATES[p['state']]}", "",
              "SERVICES | SAC | QTY. | RATE | TAX | AMOUNT",
              f"ADVANCE FOR CONSTRUCTION OF RESIDENTIAL HOUSE | 9954 | 1 OTH | {inr(b['taxable'])} | "
              f"{inr(b['igst'] + b['cgst'] + b['sgst'])} (18%) | {inr(b['gross'])}", "",
              f"Taxable Amount: {inr(b['taxable'])}"]
    if b["inter"]:
        lines.append(f"IGST @18%: {inr(b['igst'])}")
    else:
        lines += [f"CGST @9%: {inr(b['cgst'])}", f"SGST @9%: {inr(b['sgst'])}"]
    lines += [f"Total Amount: {inr(b['gross'])}", f"Received Amount: {inr(b['gross'])}",
              f"Total Amount (in words): {in_words(b['gross'])}", "",
              "TERMS AND CONDITIONS", "1. Advances once received are adjusted against the final bill.",
              f"2. All disputes are subject to {client['town']} jurisdiction only."]
    if b["cancelled"]:
        lines += ["", "# CANCELLED - this invoice is void"]
    return pdf_bytes(lines)


def simple_pdf(title, lines):
    return pdf_bytes([f"# {title}"] + lines)


# --- graders and steps -----------------------------------------------------------------------------------------------

def N(x):
    return {"number": str(x)}


def D(iso):
    return {"date": iso}


def curl(url, **fields):
    """A form post as the oracle's shell line (curl posts only when it sends data: a bare button's form sends one field)."""
    fields = fields or {"submit": "1"}
    data = " ".join(f"--data-urlencode '{k}={v}'" for k, v in fields.items())
    for v in fields.values():
        assert "'" not in str(v) and "\n" not in str(v), v
    return f"{ROUTE} {data} {url}"


def otp_fact(code):
    return {"id": "otp", "about": "the one-time password the portal sent to the client's registered mobile for filing",
            "refers_to": ["otp", "one time password", "one-time password", "verification code", "evc code"],
            "answer": f"I called the client: the OTP is {code}.", "required": True}


def write_case(suite, case, files, fixture):
    d = os.path.join(SUITES, suite, "cases", case["id"])
    out = {f"cases/{case['id']}/case.json": (json.dumps(case, indent=2, ensure_ascii=False) + "\n").encode("utf-8"),
           f"cases/{case['id']}/fixture.json": (json.dumps(fixture, indent=1, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8")}
    for name, body in files.items():
        out[f"cases/{case['id']}/files/{name}"] = body
    return out


# How the office's apps and documents write digit dates, a fact of the mock the person knows: every app of the mock
# reads them day first (mock_app.iso_date, DD-MM-YYYY) and every document here writes them so (dmy). The expert answers
# the worker's date-order question from it (internal/eval/confirm.go; owner's ruling 2026-11-09).
DATE_ORDERS = {"app": "day", "source": "day"}


def case_doc(cid, category, kind, request, files, facts, approves, graders, oracle, secrets=(), minutes=60, tokens=8000000,
             tags=()):
    doc = {"schema": "decurix.eval-case/v1", "id": cid, "category": category, "kind": kind, "request": request,
           "start": {"files": sorted(files), "mock": MOCK, "fixture": "fixture.json"},
           "facts": facts, "approves": [{"control": a, "role": "button"} for a in approves],
           "date_orders": dict(DATE_ORDERS),
           "expected": {"outcome": "complete", "graders": graders}, "oracle": oracle,
           "budget": {"max_minutes": minutes, "max_tokens": tokens}, "tags": list(tags)}
    if secrets:
        doc["oracle_secrets"] = list(secrets)
    return doc


def suite_doc(name, description, cases, minutes, tokens):
    return {"schema": "decurix.eval-suite/v1", "name": name, "description": description,
            "mocks": {MOCK: {"hosts": HOSTS}}, "budget": {"max_tokens": tokens, "max_minutes": minutes},
            "cases": cases, "carry_state": False,
            # The person glances at a taught worker's plan and says yes, or corrects a value against their facts
            # (internal/eval/confirm.go; owner's decision 2026-11-08).
            "confirms_plans": True,
            # Its cases stand alone, and may also run in order on one taught desktop, state carried, to show the worker
            # getting cheaper with practice (internal/eval/case.go CarryAllowed; owner's ask 2026-11-09).
            "carry_allowed": True}


def base_app(today, now="17:21:07", **extra):
    a = {"today": today, "now": now, "tn_logins": [], "signatories": {}, "otps": {}, "mobiles": {},
         "hsn_master": HSN_MASTER, "fp_data": {}, "last_company": "", "export_folder": "", "import_folder": ""}
    a.update(extra)
    return a


HSN_MASTER = {"9987": "Maintenance, repair and installation (except construction) services",
              "9954": "Construction services", "9403": "Other furniture and parts thereof",
              "4418": "Builders' joinery and carpentry of wood", "8431": "Parts of lifts and escalators"}


def ids(records):
    for i, r in enumerate(records, 1):
        r["id"] = str(i)
    return records


# =====================================================================================================================
# Suite 1: office-books-v1 - sales bills from the mailbox into the books
# =====================================================================================================================

SUVARNA = {"name": "Suvarna Lift Systems", "gstin": gstin("29AAPFS6214K1Z"), "tagline": "Lift installation, modernisation and maintenance",
           "address": "Door 3-41, Temple Road, Udupi 576101", "phone": "+91 90000 41210", "account": "SLS-CA-0412",
           "email": "accounts@suvarna-lifts.example.test"}
SUVARNA_DECOY = gstin("29AAQFS3307H1Z")
BOOKS_COMPANY = "1"  # Suvarna Lift Systems 2025-26, the client's company (named for the year it began, it holds 2026-27)

BOOKS_CASES = [
    {"id": "ob-1-october", "period": "2026-10", "today": "2026-11-03", "last_company": "2", "category": "ordinary",
     "bills": [
         bill("1043", "2026-10-03", "sagar", [("Lift annual maintenance, Oct 2026 - Mar 2027, first instalment", "1", "24,000.00", 24000)],
              file="1043 Sagar Vista.docx"),
         bill("1044", "2026-10-08", "udyavara", [("Blower fan replacement", "1", "4,350.00", 4350)], file="1044 Udyavara Apt.docx"),
         bill("1045", "2026-10-14", "kadri", [("Residential lift: 8 mm rope 6x19", "540 m", "115.00", 62100),
                                              ("Safety wedge box 16 mm", "2 nos", "12,000.00", 24000)], file="1045 Kadri Heights.docx"),
         bill("1046", "2026-10-21", "pai", [("Automatic rescue device, 8 passenger", "1", "27,800.00", 27800)], file="1046 Pai Textiles.docx"),
         bill("1047", "2026-10-28", "mithila", [("Lift maintenance, Oct - Dec 2026 quarter", "1", "25,000.00", 25000)],
              file="1047 Mithila Towers.docx")],
     "new_ledgers": ["kadri"], "known": ["sagar", "udyavara", "pai", "mithila", "bhuvana", "lotus", "malpe", "sharada", "vasco"],
     "entered": [], "tags": ["stale-company", "new-ledger", "name-differs"]},
    {"id": "ob-2-november", "period": "2026-11", "today": "2026-12-02", "last_company": "3", "category": "varied",
     "bills": [
         bill("1048", "2026-11-04", "vasco", [("Lift cabin interior refit", "1", "40,000.00", 40000)], file="1048 Vasco Bay.docx"),
         bill("1049", "2026-11-09", "hampi", [("Door sensor replacement", "1", "12,000.00", 12000)], file="1049 Hampi Residency.docx"),
         bill("1050", "2026-11-11", "bhuvana", [("Lift spare parts: door roller kit", "4 sets", "4,500.00", 18000)], rate=12, hsn="8431",
              file="1050 Bhuvana Hospital.docx"),
         bill("1051", "2026-11-19", "sagar", [("Lift annual maintenance, Oct 2026 - Mar 2027, second instalment", "1", "24,000.00", 24000)],
              file="1051 Sagar Vista.docx"),
         bill("1052", "2026-11-27", "lotus", [("Escalator handrail replacement", "1", "1,10,000.00", 110000)], file="1052 Lotus Mall.docx")],
     "new_ledgers": ["hampi"], "known": ["sagar", "udyavara", "kadri", "pai", "mithila", "bhuvana", "lotus", "malpe", "sharada", "vasco"],
     "entered": [], "tags": ["inter-state", "unregistered", "other-rate", "old-company-open"]},
    {"id": "ob-3-december", "period": "2026-12", "today": "2027-01-04", "last_company": "1", "category": "duplicate",
     "bills": [
         bill("1053", "2026-12-02", "pai", [("Lift car light fittings", "1", "8,000.00", 8000)], file="1053 Pai Textiles.docx"),
         bill("1054", "2026-12-07", "sharada", [("Lift maintenance, Dec 2026", "1", "15,000.00", 15000)], file="1054 Sharada Residency.docx"),
         bill("1055", "2026-12-12", "udyavara", [("Machine room ventilation repair", "1", "20,000.00", 20000)],
              file="1055 Udyavara Apartments.docx", words="Rupees Twenty Five Thousand Only."),
         bill("1056", "2026-12-18", "malpe", [("Goods lift brake overhaul", "1", "52,000.00", 52000)], file="1056 Malpe Fish.docx"),
         bill("1057", "2026-12-29", "mithila", [("Lift maintenance, Jan - Mar 2027 quarter, advance", "1", "25,000.00", 25000)],
              file="1057 Mithila Towers.docx")],
     "duplicates": {"1056 Malpe Fish Processors copy.docx": "1056"},
     "new_ledgers": [], "known": ["sagar", "udyavara", "kadri", "pai", "mithila", "bhuvana", "lotus", "malpe", "sharada", "vasco", "hampi"],
     "entered": ["1053"], "tags": ["sent-twice", "already-entered", "words-differ"]},
    {"id": "ob-4-january", "period": "2027-01", "today": "2027-02-03", "last_company": "2", "category": "missing_input",
     "bills": [
         bill("1058", "2027-01-06", "lotus", [("Escalator step chain lubrication", "1", "30,000.00", 30000)], file="1058 Lotus Mall.docx"),
         bill("1059", "2027-01-13", "kadri", [("Residential lift: landing doors", "3 nos", "15,000.00", 45000)], file="1059 Kadri Heights.docx"),
         bill("1060", "2027-01-20", "bhuvana", [("Lift emergency phone line", "1", "10,000.00", 10000)], file="1060 Bhuvana Hospital.docx"),
         bill("1061", "2027-01-27", "sagar", [("Lift modernisation advance", "1", "24,000.00", 24000)], file="1061 Sagar Vista.docx")],
     "missing": "1060",
     "new_ledgers": [], "known": ["sagar", "udyavara", "kadri", "pai", "mithila", "bhuvana", "lotus", "malpe", "sharada", "vasco", "hampi"],
     "entered": [], "tags": ["number-gap", "short-date-year"]},
]

# The months before each case, already in the books (as the cases before it left them), so the books read as a real
# client's: September's bills, then each earlier case's own.
SEPTEMBER = [
    bill("1036", "2026-09-02", "pai", [("Lift car light fittings", "1", "6,500.00", 6500)]),
    bill("1037", "2026-09-05", "sagar", [("Lift annual maintenance, Apr - Sep 2026, second instalment", "1", "24,000.00", 24000)]),
    bill("1038", "2026-09-11", "udyavara", [("Lift maintenance, Sep 2026", "1", "6,000.00", 6000)]),
    bill("1039", "2026-09-16", "lotus", [("Escalator inspection", "1", "18,000.00", 18000)]),
    bill("1040", "2026-09-22", "mithila", [("Lift maintenance, Jul - Sep 2026 quarter", "1", "25,000.00", 25000)]),
    bill("1041", "2026-09-26", "malpe", [("Goods lift rope change", "1", "36,500.00", 36500)]),
    bill("1042", "2026-09-30", "bhuvana", [("Stretcher lift service", "1", "14,200.00", 14200)]),
]


def voucher_of(b, company, number=None):
    p = b["party"]
    sales = "Sales@12%" if b["rate"] == 12 else "Sales@18%"
    return {"company": company, "type": "Sales", "number": number or b["no"], "date": b["date"], "party": p["name"],
            "party_gstin": p["gstin"], "sales_ledger": sales, "taxable": s2(b["taxable"]), "cgst": s2(b["cgst"]),
            "sgst": s2(b["sgst"]), "igst": s2(b["igst"]), "total": s2(b["total"]), "narration": "", "cancelled": "no"}


def ledger_of(key, company):
    p = party(key)
    return {"company": company, "name": p["name"], "under": "Sundry Debtors", "mailing_name": p["header"].split("\n")[0],
            "address": p["address"], "state": f"{p['state']}-{STATES[p['state']]}", "pincode": p["address"][-6:],
            "registration": "Regular" if p["gstin"] else "Unregistered/Consumer", "gstin": p["gstin"], "rate": "", "hsn": ""}


def tax_ledgers(company, sales):
    out = []
    for name, rate, hsn in sales:
        out.append({"company": company, "name": name, "under": "Sales Accounts", "mailing_name": "", "address": "", "state": "",
                    "pincode": "", "registration": "Unknown", "gstin": "", "rate": rate, "hsn": hsn})
    for name in ("CGST@9%", "SGST@9%", "IGST@18%", "CGST@6%", "SGST@6%", "IGST@12%"):
        out.append({"company": company, "name": name, "under": "Duties & Taxes", "mailing_name": "", "address": "",
                    "state": "", "pincode": "", "registration": "Unknown", "gstin": "", "rate": "", "hsn": ""})
    return out


def books_fixture(case, earlier):
    # Both live companies are carried on into the next financial year once the month is in it (Bookwell takes a
    # voucher only inside its company's period).
    companies = ids([
        {"name": "Suvarna Lift Systems 2025-26", "number": "10041", "gstin": SUVARNA["gstin"], "state": "29-Karnataka",
         "period_from": "2025-04-01", "period_to": fy_end(case["period"])},
        {"name": "Suvarna Lifts & Escalators", "number": "10077", "gstin": SUVARNA_DECOY, "state": "29-Karnataka",
         "period_from": "2026-04-01", "period_to": fy_end(case["period"])},
        {"name": "Suvarna Lift Systems 2023-24", "number": "10012", "gstin": SUVARNA["gstin"], "state": "29-Karnataka",
         "period_from": "2023-04-01", "period_to": "2024-03-31"},
        {"name": "Sharada Traders 21-22", "number": "10006", "gstin": gstin("29ABCFS1120G1Z"), "state": "29-Karnataka",
         "period_from": "2021-04-01", "period_to": "2022-03-31"},
    ])
    ledgers = tax_ledgers("1", [("Sales@18%", "18", "9987"), ("Sales@12%", "12", "8431")])
    ledgers += [ledger_of(k, "1") for k in case["known"]]
    ledgers += tax_ledgers("2", [("Sales@18%", "18", "9987")]) + [ledger_of(k, "2") for k in ("sagar", "udyavara", "lotus")]
    ledgers += tax_ledgers("3", [("Sales@18%", "18", "9987")]) + [ledger_of(k, "3") for k in ("sagar", "udyavara", "pai")]
    ledgers += tax_ledgers("4", [("Sales@18%", "18", "9987")])
    vouchers = [voucher_of(b, "1") for b in earlier]
    vouchers += [voucher_of(b, "1") for b in case["bills"] if b["no"] in case["entered"]]
    # The other company's own sales, and - in October - two of this month's bills keyed into it by an earlier run.
    vouchers += [voucher_of(bill("SLE-0210", "2026-10-09", "lotus", [("Escalator service", "1", "9,000.00", 9000)]), "2"),
                 voucher_of(bill("SLE-0211", "2026-10-23", "sagar", [("Lift inspection", "1", "5,500.00", 5500)]), "2")]
    if case["id"] == "ob-1-october":
        vouchers += [voucher_of(b, "2") for b in case["bills"][:2]]
    return {"collections": {"companies": companies, "ledgers": ids(ledgers), "vouchers": ids(vouchers)}}


def books_mail(case, files):
    """The mailbox: the client's mail with the month's bills, an older mail from the same client (its bank statement),
    and others' mail."""
    y, m = case["period"].split("-")
    month = MONTHS[int(m) - 1]
    names = [b["file"] for b in case["bills"] if b["no"] != case.get("missing")] + list(case.get("duplicates", {}))
    keys = {n: f"b{i}" for i, n in enumerate(sorted(names), 1)}
    att = {keys[n]: {"name": n, "type": DOCX, "b64": b64(files[n])} for n in names}
    bank = simple_pdf(f"Example Co-operative Bank - statement, {month} {y}",
                      [f"Account holder: {SUVARNA['name']}", f"Account: {SUVARNA['account']}",
                       "Opening balance 3,12,450.00", "Closing balance 3,48,905.00"])
    att["bank"] = {"name": f"Bank statement {month[:3]} {y}.pdf", "type": PDF, "b64": b64(bank)}
    mails = [
        {"from": "Kalinga Hardware", "from_address": "office@kalinga-hw.example.test", "subject": "TDS certificate Q2",
         "body": "Please find the TDS certificate attached.", "at": f"{case['today']} 09:12", "label": "Rajesh", "files": ""},
        {"from": SUVARNA["name"], "from_address": SUVARNA["email"], "subject": f"Bank statement {month}",
         "body": f"Bank statement for {month} attached.", "at": f"{case['today']} 10:05", "label": "Asha", "files": "bank"},
    ]
    if case["entered"]:
        sent = next(b for b in case["bills"] if b["no"] == case["entered"][0])
        mails.insert(1, {"from": SUVARNA["name"], "from_address": SUVARNA["email"],
                         "subject": f"Bill {sent['no']}", "body": f"Bill {sent['no']} for {sent['party']['name']}, as discussed.",
                         "at": f"{case['period']}-15 16:40", "label": "Asha", "files": ""})
    mails.append({"from": SUVARNA["name"], "from_address": SUVARNA["email"], "subject": f"Sales tax bills {month}",
                  "body": f"Dear Sir/Madam,\nPlease find the sales bills for {month} {y}.\nRegards,\nAccounts, {SUVARNA['name']}",
                  "at": f"{case['today']} 11:30", "label": "Asha", "files": ",".join(keys[n] for n in sorted(names))})
    return ids(mails), att


def books_suite():
    out = {}
    earlier = list(SEPTEMBER)
    for case in BOOKS_CASES:
        files = {b["file"]: bill_docx(SUVARNA, b) for b in case["bills"] if b["no"] != case.get("missing")}
        for name, no in case.get("duplicates", {}).items():
            files[name] = bill_docx(SUVARNA, next(b for b in case["bills"] if b["no"] == no))
        fixture = books_fixture(case, earlier)
        mails, att = books_mail(case, files)
        fixture["collections"]["mails"] = mails
        fixture["files"] = att
        fixture["app"] = base_app(case["today"], last_company=case["last_company"])
        new = [b for b in case["bills"] if b["no"] not in case["entered"]]
        expected = []
        for b in new:
            v = voucher_of(b, BOOKS_COMPANY)
            rec = {"company": "1", "type": "Sales", "number": v["number"], "date": D(v["date"]), "sales_ledger": v["sales_ledger"],
                   "taxable": N(v["taxable"]), "cgst": N(v["cgst"]), "sgst": N(v["sgst"]), "igst": N(v["igst"]),
                   "total": N(v["total"])}
            if b["who"] in case["new_ledgers"] and b["party"]["gstin"]:
                rec["party_gstin"] = b["party"]["gstin"]  # a new ledger's name is the worker's to choose; its GSTIN is not
            else:
                rec["party"] = v["party"]
            expected.append(rec)
        graders = [{"id": "vouchers", "kind": "mock_records", "collection": "vouchers", "exact_count": True, "records": expected}]
        led = []
        for k in case["new_ledgers"]:
            p = party(k)
            r = {"company": "1", "under": "Sundry Debtors", "state": f"{p['state']}-{STATES[p['state']]}",
                 "registration": "Regular" if p["gstin"] else "Unregistered/Consumer", "gstin": p["gstin"]}
            if not p["gstin"]:
                r["name"] = p["name"]
            led.append(r)
        graders.append({"id": "ledgers", "kind": "mock_records", "collection": "ledgers", "exact_count": True, "records": led}
                       if led else {"id": "ledgers", "kind": "absent", "collection": "ledgers"})
        y, m = case["period"].split("-")
        month = f"{MONTHS[int(m) - 1]} {y}"
        request = (f"{SUVARNA['name']} has emailed its sales bills for {month} to our Mailbox. Enter each bill as a Sales "
                   f"voucher in the client's company in Bookwell, then check the month's sales in the books against the "
                   f"bills. The attachments are also saved in ~/work/practice/{case['id']}/. Our apps: Mailbox "
                   f"http://mail.example.test, Bookwell http://books.example.test.")
        facts = []
        if any(b["words"] for b in case["bills"]):
            facts.append({"id": "words-vs-figures", "about": "which figure to take when a bill's amount in words differs from its totals",
                          "refers_to": ["in words", "amount in words", "words", "differs", "mismatch", "does not match"],
                          "answer": "Take the figures in the bill's totals table. The client's template often keeps old text in "
                                    "the words box.", "required": False})
        if case.get("missing"):
            n = case["missing"]
            b = next(x for x in case["bills"] if x["no"] == n)
            assert not b["inter"], n  # the answer below states CGST and SGST
            facts.append({"id": f"invoice-{n}", "about": f"the bill numbered {n}, which is missing from the mail",
                          "refers_to": [n, "missing bill", "missing invoice", "number gap", "gap", "skipped number",
                                        "missing number", "not attached"],
                          "answer": (f"Bill {n} went to {b['party']['name']} on {dmy(b['date'])}: "
                                     f"{voucher_of(b, '1')['sales_ledger']}, sales amount "
                                     f"{inr(b['taxable'])}, CGST {inr(b['cgst'])} and SGST {inr(b['sgst'])} (total "
                                     f"{inr(b['total'])}). The client forgot to attach it; enter it with the others."),
                          "required": True})
            graders.append({"kind": "asked", "facts": [f"invoice-{n}"]})
        oracle = []
        for k in case["new_ledgers"]:
            p = party(k)
            oracle.append(curl("http://books.example.test/company/1/ledgers/new", name=p["name"], under="Sundry Debtors",
                               mailing_name=p["name"], address=p["address"], state=f"{p['state']}-{STATES[p['state']]}",
                               pincode=p["address"][-6:], registration="Regular" if p["gstin"] else "Unregistered/Consumer",
                               gstin=p["gstin"]))
        for b in new:
            v = voucher_of(b, "1")
            oracle.append(curl("http://books.example.test/company/1/vouchers/new", type="Sales", number=v["number"],
                               date=dmy(v["date"]), party=v["party"], sales_ledger=v["sales_ledger"], taxable=v["taxable"],
                               cgst=v["cgst"], sgst=v["sgst"], igst=v["igst"]))
        oracle.append({"answer": f"Entered {len(new)} Sales vouchers for {month} in {SUVARNA['name']} 2025-26: "
                                 + ", ".join(b["no"] for b in new) + "."})
        doc = case_doc(case["id"], case["category"], "sales_bills_to_books", request, files, facts,
                       ["Save voucher", "Save ledger"], graders, oracle, minutes=60, tokens=8000000, tags=case["tags"])
        out.update(write_case("office-books-v1", doc, files, fixture))
        earlier += [b for b in case["bills"] if b["no"] not in case["entered"]] + [b for b in case["bills"] if b["no"] in case["entered"]]
    out["suite.json"] = (json.dumps(suite_doc(
        "office-books-v1",
        "A CA office keeps a lift-maintenance client's books: each month the client emails its sales bills (Word files), "
        "and each becomes a Sales voucher in the client's company in the books app, with a customer ledger made for a new "
        "customer, then the month's sales ledger is checked against the bills. Case 1 is the one a person demonstrates; "
        "the rest are new months with the traps a real month has: the books opening on another company left open, a "
        "company of the client's for an old year, a customer whose bill writes its name otherwise than its ledger, a new "
        "customer with and without a GSTIN, an inter-state customer, a bill at another rate, a bill sent twice, a bill "
        "already entered from an earlier mail, a bill whose amount in words differs from its figures, and a bill missing "
        "from the mail (a gap in the numbers) that only the person can supply. Cases 5 to 12 are the client's next eight "
        "months (February to September 2027, its company carried into the new financial year), the same customers and "
        "some new ones, about half of them plain and the rest with one or two of those traps again, so a worker taught "
        "once can show its curve over fresh months. Graded on the books' records: exactly the "
        "expected vouchers, in the right company, and the new ledgers. Fictional data; the ca-office mock.",
        [c["id"] for c in BOOKS_CASES], 60, 8000000), indent=2) + "\n").encode("utf-8")
    return {f"office-books-v1/{k}": v for k, v in out.items()}


# =====================================================================================================================
# Suite 2: office-gstr1-books-v1 - GSTR-1 from the books, through the return-prep software, filed with EVC
# =====================================================================================================================

SAHYADRI = {"name": "Sahyadri Lift Services", "gstin": gstin("29AALFS8842N1Z"), "tagline": "Lifts: installation and annual maintenance",
            "address": "2nd Floor, Shree Complex, MG Road, Shivamogga 577201", "phone": "+91 90000 52230", "account": "SLS-CA-0877"}
KONKAN = {"name": "Konkan Coast Interiors", "gstin": gstin("29AAGFK7314L1Z"), "tagline": "Furniture, wood panelling and installation",
          "address": "Plot 14, Industrial Area, Karwar 581301", "phone": "+91 90000 63340", "account": "KCI-CA-0193"}
SAHYADRI_DECOY = gstin("29AAMFS1176P1Z")
KALINGA = gstin("29AAJFK2093B1Z")
TN_PASSWORDS = {SAHYADRI["gstin"]: "Sahyadri#Demo26", KONKAN["gstin"]: "Konkan#Demo26"}
TN_USERS = {SAHYADRI["gstin"]: "sahyadri.lift", KONKAN["gstin"]: "konkan.coast"}
SIGNATORY = {SAHYADRI["gstin"]: "Prakash Hegde", KONKAN["gstin"]: "Nandini Kamath"}

GSTR_BOOKS_CASES = [
    {"id": "og-1-october", "client": "sahyadri", "period": "2026-10", "today": "2026-11-05", "category": "ordinary",
     "otp": "604217", "export_folder": share_folder("Kalinga Hardware", "2026-10"),
     "bills": [
         bill("1101", "2026-10-02", "tunga", [("Lift annual maintenance, Oct 2026 - Mar 2027", "1", "48,600.00", 48600)]),
         bill("1102", "2026-10-06", "malnad", [("Lift maintenance, Oct 2026", "1", "21,000.00", 21000)]),
         bill("1103", "2026-10-09", "belur", [("Goods lift rope replacement", "1", "33,250.00", 33250)]),
         bill("1104", "2026-10-14", "coastal", [("Escalator step replacement", "16 nos", "7,400.00", 118400)]),
         bill("1105", "2026-10-20", "hampi2", [("Door sensor replacement", "1", "6,800.00", 6800)]),
         bill("1106", "2026-10-24", "agumbe", [("Lift annual maintenance, part payment", "1", "14,406.78", Decimal("14406.78"))]),
         bill("1107", "2026-10-30", "kudremukh", [("Lift maintenance, Oct - Dec 2026 quarter", "1", "18,000.00", 18000)])],
     "receivers_known": ["tunga", "belur", "agumbe"], "new_receivers": ["coastal"], "tags": ["wrong-export-folder", "unvalidated-receiver"]},
    {"id": "og-2-november", "client": "konkan", "period": "2026-11", "today": "2026-12-04", "category": "varied",
     "otp": "381946", "export_folder": share_folder("Konkan Coast Interiors", "2026-10"),
     "bills": [
         bill("KCI-041", "2026-11-03", "vasco", [("Hotel lobby furniture set", "1", "2,40,000.00", 240000)], hsn="9403"),
         bill("KCI-042", "2026-11-07", "rhea", [("Teak dining set with sideboard", "1", "1,35,000.00", 135000)], hsn="9403"),
         bill("KCI-043", "2026-11-12", "anand", [("Wall panelling, 140 sq ft", "140 sq ft", "200.00", 28000)], hsn="4418"),
         bill("KCI-044", "2026-11-18", "belur", [("Showroom fit-out installation", "1", "64,000.00", 64000)], hsn="9954"),
         bill("KCI-045", "2026-11-23", "leena_goa", [("Study table and bookshelf", "1", "40,000.00", 40000)], hsn="9403"),
         bill("KCI-046", "2026-11-28", "anand", [("Wardrobe installation", "1", "12,500.00", 12500)], hsn="9954")],
     "receivers_known": ["vasco", "belur"], "new_receivers": [], "tags": ["inter-state", "b2c-large", "hsn-codes", "stale-month-folder"]},
    {"id": "og-3-december", "client": "sahyadri", "period": "2026-12", "today": "2027-01-06", "category": "varied",
     "otp": "275130", "export_folder": share_folder("Sahyadri Lift Services", "2026-11"),
     "bills": [
         bill("1108", "2026-12-01", "tunga", [("Lift annual maintenance, extra visit", "1", "36,000.00", 36000)]),
         bill("1109", "2026-12-04", "malnad", [("Lift maintenance, Dec 2026", "1", "21,000.00", 21000)]),
         bill("1110", "2026-12-08", "coastal", [("Escalator handrail drive", "1", "52,500.00", 52500)]),
         bill("1111", "2026-12-11", "kudremukh", [("Lift battery replacement", "1", "18,000.00", 18000)]),
         bill("1112", "2026-12-15", "hampi2", [("Door sensor replacement", "1", "9,000.00", 9000)], cancelled=True),
         bill("1113", "2026-12-19", "belur", [("Goods lift brake service", "1", "27,750.00", 27750)]),
         bill("1114", "2026-12-23", "hampi2", [("Door sensor replacement", "1", "6,800.00", 6800)]),
         bill("1115", "2026-12-29", "agumbe", [("Lift annual maintenance, part payment", "1", "14,406.78", Decimal("14406.78"))])],
     "receivers_known": ["tunga", "belur", "agumbe", "coastal"], "new_receivers": [], "stale_import": ["1108", "1110"],
     "tags": ["cancelled-invoice", "quarter-default", "overwrite-not-append"]},
    {"id": "og-4-cancelled-gstin", "client": "konkan", "period": "2026-12", "today": "2027-01-05", "category": "invalid_input",
     "otp": "518402", "export_folder": share_folder("Konkan Coast Interiors", "2026-11"),
     "bills": [
         bill("KCI-047", "2026-12-04", "belur2", [("Showroom wall panelling", "1", "50,000.00", 50000)], hsn="4418"),
         bill("KCI-048", "2026-12-10", "tunga_k", [("Site office furniture installation", "1", "1,20,000.00", 120000)], hsn="9954"),
         bill("KCI-049", "2026-12-16", "anand", [("Shoe rack and console", "1", "22,000.00", 22000)], hsn="9403")],
     "receivers_known": ["vasco", "belur"], "new_receivers": ["belur2", "tunga_k"], "cancelled_gstin": "belur2",
     "tags": ["cancelled-receiver", "name-from-register"]},
]

CLIENT_OF = {"sahyadri": SAHYADRI, "konkan": KONKAN}
SALES_OF = {"sahyadri": [("Sales@18%", "18", "9987")],
            "konkan": [("Sales - Furniture 18%", "18", "9403"), ("Sales - Wood Panels 18%", "18", "4418"),
                       ("Installation 18%", "18", "9954")]}
FP_CLIENT_ID = {"sahyadri": "1", "konkan": "4"}
COMPANY_ID = {"sahyadri": "1", "konkan": "2"}
COMPANY_CLIENT = {"1": "sahyadri", "2": "konkan", "4": "sahyadri", "5": "konkan"}  # 3 is the look-alike firm's
OTHER_COMPANY = {"1": "2", "2": "1", "4": "5", "5": "4"}  # the other client's company of the same year


def series_no(no):
    """An invoice number's place in its series: its digits ("KCI-047" -> 47)."""
    return int("".join(ch for ch in no if ch.isdigit()))


def sales_ledger_for(client_key, b):
    return next(name for name, _, hsn in SALES_OF[client_key] if hsn == b["hsn"])


def gstr1_rows(client, bills):
    """The month's GSTR-1, worked from the bills by this file's own code: B2B by invoice, B2C Large by invoice
    (inter-state, unregistered, over 1,00,000), B2C Others by place of supply and rate, HSN by tab, code and rate, and
    the documents issued (the series, its count and the cancelled)."""
    st = client["gstin"][:2]
    b2b, b2cl, b2cs, hsn = [], [], {}, {}
    live = [b for b in bills if not b["cancelled"]]
    for b in live:
        p = b["party"]
        rt = str(b["rate"])
        tab = "B2C"
        if p["gstin"]:
            tab = "B2B"
            b2b.append({"ctin": p["gstin"], "inum": b["no"], "idt": b["date"], "val": s2(b["total"]), "pos": p["state"],
                        "rt": rt, "txval": s2(b["taxable"]), "iamt": s2(b["igst"]), "camt": s2(b["cgst"]), "samt": s2(b["sgst"])})
        elif p["state"] != st and b["total"] > 100000:
            b2cl.append({"pos": p["state"], "inum": b["no"], "idt": b["date"], "val": s2(b["total"]), "rt": rt,
                         "txval": s2(b["taxable"]), "iamt": s2(b["igst"])})
        else:
            r = b2cs.setdefault((p["state"], rt), {"pos": p["state"], "typ": "INTRA" if p["state"] == st else "INTER", "rt": rt,
                                                   "txval": q(0), "iamt": q(0), "camt": q(0), "samt": q(0)})
            for k, v in (("txval", b["taxable"]), ("iamt", b["igst"]), ("camt", b["cgst"]), ("samt", b["sgst"])):
                r[k] += v
        h = hsn.setdefault((tab, b["hsn"], rt), {"tab": tab, "hsn": b["hsn"], "uqc": "NA" if b["hsn"].startswith("99") else "OTH",
                                                 "qty": "0", "rt": rt, "txval": q(0), "iamt": q(0), "camt": q(0), "samt": q(0),
                                                 "csamt": q(0)})
        for k, v in (("txval", b["taxable"]), ("iamt", b["igst"]), ("camt", b["cgst"]), ("samt", b["sgst"])):
            h[k] += v
    nums = [b["no"] for b in bills]
    lo, hi = min(nums, key=series_no), max(nums, key=series_no)
    total = series_no(hi) - series_no(lo) + 1
    cancelled = sum(1 for b in bills if b["cancelled"])
    docs = [{"nature": "Invoices for outward supply", "from": lo, "to": hi, "total": str(total), "cancelled": str(cancelled),
             "net": str(total - cancelled)}]
    fix = lambda rows: [{k: (s2(v) if isinstance(v, Decimal) else v) for k, v in r.items()} for r in rows]  # noqa: E731
    return {"b2b": b2b, "b2cl": b2cl, "b2cs": fix(b2cs.values()), "hsn": fix(hsn.values()), "docs": docs}


TYPED = {"val", "txval", "iamt", "camt", "samt", "csamt", "qty", "total", "cancelled", "net"}


def expect_rows(rows):
    out = []
    for r in rows:
        e = {}
        for k, v in r.items():
            e[k] = N(v) if k in TYPED else D(v) if k == "idt" else v
        out.append(e)
    return out


def tn_graders(g, period, rows, ret_extra):
    graders = []
    for key, coll in (("b2b", "tn_b2b"), ("b2cl", "tn_b2cl"), ("b2cs", "tn_b2cs"), ("hsn", "tn_hsn"), ("docs", "tn_docs")):
        recs = [dict(gstin=g, period=period, **r) for r in expect_rows(rows[key])]
        if recs:
            graders.append({"id": coll.replace("_", "-"), "kind": "mock_records", "collection": coll, "exact_count": True, "records": recs})
        else:
            graders.append({"id": coll.replace("_", "-"), "kind": "absent", "collection": coll})
    graders.append({"id": "tn-returns", "kind": "mock_records", "collection": "tn_returns", "exact_count": True,
                    "records": [dict({"gstin": g, "period": period, "status": "Filed", "arn": arn_of(g, period),
                                      "method": "EVC", "signatory": SIGNATORY.get(g) or SIGNATORY_CHAT.get(g)}, **ret_extra)]})
    return graders


def taxnet_base(gstins, today, earlier_filed):
    """TaxNet's register, the credit ledgers and the returns already filed (for the calendar)."""
    reg = [{"gstin": g, "legal_name": n.upper(), "trade_name": n.upper(), "status": "Active", "registered_on": "2018-07-01"}
           for g, n in gstins]
    returns = []
    for g in [g for g, _ in gstins[:2]]:
        for p in earlier_filed:
            returns.append({"gstin": g, "period": p, "status": "Filed", "nil": "no", "arn": arn_of(g, p),
                            "filed_on": f"{p}-28", "signatory": SIGNATORY.get(g) or SIGNATORY_CHAT.get(g, ""), "method": "EVC"})
    return reg, returns


def months_before(period, n):
    y, m = (int(x) for x in period.split("-"))
    out = []
    for _ in range(n):
        y, m = (y, m - 1) if m > 1 else (y - 1, 12)
        out.insert(0, f"{y}-{m:02d}")
    return out


def gstr_books_suite():
    out = {}
    for case in GSTR_BOOKS_CASES:
        ck = case["client"]
        client = CLIENT_OF[ck]
        g = client["gstin"]
        period = case["period"]
        bills = case["bills"]
        files = {b["file"]: bill_docx(client, b) for b in bills}
        # Books: the client's company holds the month's vouchers (cancelled ones marked), the other company too.
        companies = [
            {"name": "Sahyadri Lift Services 2026-27", "number": "20031", "gstin": SAHYADRI["gstin"], "state": "29-Karnataka",
             "period_from": "2026-04-01", "period_to": "2027-03-31"},
            {"name": "Konkan Coast Interiors 2026-27", "number": "20044", "gstin": KONKAN["gstin"], "state": "29-Karnataka",
             "period_from": "2026-04-01", "period_to": "2027-03-31"},
            {"name": "Sahyadri Lifts & Escalators 2026-27", "number": "20052", "gstin": SAHYADRI_DECOY, "state": "29-Karnataka",
             "period_from": "2026-04-01", "period_to": "2027-03-31"}]
        if fy_end(period) > "2027-03-31":
            # From April 2027 each client's books are in a new company for the year (ids 4 and 5).
            companies += [dict(c, name=c["name"].replace("2026-27", "2027-28"), number=n, period_from="2027-04-01",
                               period_to="2028-03-31") for c, n in ((companies[0], "20063"), (companies[1], "20068"))]
        companies = ids(companies)
        cid = case.get("company", COMPANY_ID[ck])
        assert COMPANY_CLIENT[cid] == ck, case["id"]
        ledgers = tax_ledgers("1", SALES_OF["sahyadri"]) + tax_ledgers("2", SALES_OF["konkan"]) + tax_ledgers("3", SALES_OF["sahyadri"])
        if len(companies) > 3:
            ledgers += tax_ledgers("4", SALES_OF["sahyadri"]) + tax_ledgers("5", SALES_OF["konkan"])
        for who in sorted({b["who"] for b in bills}):
            ledgers.append(ledger_of(who, cid))
        vouchers = []
        for b in bills:
            v = voucher_of(b, cid)
            v["sales_ledger"] = sales_ledger_for(ck, b)
            if b["cancelled"]:
                v["cancelled"] = "yes"
            vouchers.append(v)
        other = OTHER_COMPANY[cid]
        ledgers.append(ledger_of("anand", other))
        vouchers.append(dict(voucher_of(bill("X-1", f"{period}-10", "anand", [("Service", "1", "5,000.00", 5000)]), other),
                             sales_ledger=SALES_OF[COMPANY_CLIENT[other]][0][0],
                             number="9001" if COMPANY_CLIENT[other] == "sahyadri" else "KCI-030"))
        folders = []
        for c in ("Sahyadri Lift Services", "Konkan Coast Interiors", "Kalinga Hardware", "Sahyadri Lifts & Escalators"):
            for p in months_before(period, 3) + [period]:
                folders.append({"path": share_folder(c, p)})
        # FilePrep: the firm's clients (similar names among them) and each client's known receivers.
        fp_clients = ids([
            {"name": "Sahyadri Lift Services", "legal_name": "SAHYADRI LIFT SERVICES", "gstin": SAHYADRI["gstin"], "state": "Karnataka",
             "registration": "Regular", "group": "Asha", "taxnet_user": TN_USERS[SAHYADRI["gstin"]]},
            {"name": "Sahyadri Lifts & Escalators", "legal_name": "SAHYADRI LIFTS AND ESCALATORS", "gstin": SAHYADRI_DECOY,
             "state": "Karnataka", "registration": "Regular", "group": "Rajesh", "taxnet_user": ""},
            {"name": "Sahyadri Traders", "legal_name": "RAGHU SAHYADRI", "gstin": gstin("29ABCPS5508R1Z"), "state": "Karnataka",
             "registration": "Composition", "group": "Handled by client", "taxnet_user": ""},
            {"name": "Konkan Coast Interiors", "legal_name": "KONKAN COAST INTERIORS", "gstin": KONKAN["gstin"], "state": "Karnataka",
             "registration": "Regular", "group": "Asha", "taxnet_user": TN_USERS[KONKAN["gstin"]]},
            {"name": "Kalinga Hardware", "legal_name": "KALINGA HARDWARE", "gstin": KALINGA, "state": "Karnataka",
             "registration": "Regular", "group": "Rajesh", "taxnet_user": ""}])
        fpid = FP_CLIENT_ID[ck]
        receivers = []
        for who in case["receivers_known"]:
            p = party(who)
            receivers.append({"client": fpid, "gstin": p["gstin"], "legal_name": p["name"].upper(), "trade_name": p["name"].upper(),
                              "status": "Valid", "tn_legal_name": p["name"].upper(), "tn_trade_name": p["name"].upper()})
        register = [(SAHYADRI["gstin"], SAHYADRI["name"]), (KONKAN["gstin"], KONKAN["name"])]
        # Every party with a GSTIN, but a second ledger of a GSTIN already listed and a GSTIN the register has cancelled
        # (that one is added, Cancelled, in its own case). The cases written first list the parties of their time.
        gone = {"tunga_k"} | {c["cancelled_gstin"] for c in GSTR_BOOKS_CASES if c.get("cancelled_gstin")}
        register += [(party(k)["gstin"], party(k)["name"]) for k in PARTIES if party(k)["gstin"] and k not in gone
                     and (k in V1_PARTIES or case["id"] not in FIRST_WRITTEN)]
        # Both clients' three months before the case's are filed; the register writes some names otherwise than the books.
        reg, returns = taxnet_base(register, case["today"], months_before(period, 3))
        for r in reg:
            if r["gstin"] == party("tunga")["gstin"]:
                r["legal_name"] = r["trade_name"] = "TUNGABHADRA BUILDERS PRIVATE LIMITED"
        if case.get("cancelled_gstin"):
            bad = party(case["cancelled_gstin"])
            reg.append({"gstin": bad["gstin"], "legal_name": bad["name"].upper(), "trade_name": bad["name"].upper(),
                        "status": "Cancelled", "registered_on": "2026-06-01"})
        app = base_app(case["today"], last_company=cid, export_folder=case["export_folder"], import_folder=case["export_folder"],
                       tn_logins=[{"gstin": x, "user": TN_USERS[x], "password": TN_PASSWORDS[x]} for x in TN_USERS],
                       signatories={x: [SIGNATORY[x]] for x in SIGNATORY}, otps={f"{g}:{period}": case["otp"]},
                       mobiles={x: "98xxxxxx" + x[10:12].replace("A", "4") for x in SIGNATORY})
        rows = gstr1_rows(client, bills)
        if case.get("stale_import"):
            stale = [r for r in rows["b2b"] if r["inum"] in case["stale_import"]]
            lo, hi = min(case["stale_import"], key=series_no), max(case["stale_import"], key=series_no)
            n = str(series_no(hi) - series_no(lo) + 1)
            app["fp_data"][f"{fpid}:{period}"] = {"b2b": stale, "b2cl": [], "b2cs": [], "hsn": [],
                                                  "docs": [{"nature": "Invoices for outward supply", "from": lo, "to": hi,
                                                            "total": n, "cancelled": "0", "net": n}]}
        fixture = {"collections": {"companies": companies, "ledgers": ids(ledgers), "vouchers": ids(vouchers),
                                   "share_folders": ids(folders), "share_files": [], "fp_clients": fp_clients,
                                   "fp_receivers": ids(receivers), "fp_uploads": [], "tn_taxpayers": ids(reg),
                                   "tn_ledger": ids([{"gstin": SAHYADRI["gstin"], "igst": "0.00", "cgst": "18420.00", "sgst": "18420.00",
                                                      "cess": "0.00"},
                                                     {"gstin": KONKAN["gstin"], "igst": "41250.00", "cgst": "9310.00", "sgst": "9310.00",
                                                      "cess": "0.00"}]),
                                   "tn_returns": ids(returns)},
                   "app": app}
        folder = share_folder(client["name"], period)
        y, m = period.split("-")
        fp = f"{m}{y}"
        export_name = f"{g}_GSTR1_{MONTHS[int(m) - 1][:3].upper()}-{y}_{case['today'][8:10]}{case['today'][5:7]}{case['today'][:4]}_172107.json"
        graders = [{"id": "export", "kind": "mock_records", "collection": "share_files", "records": [{"folder": folder, "gstin": g, "period": fp}]},
                   {"id": "no-export-in-the-folder-left-open", "kind": "absent", "collection": "share_files",
                    "records": [{"folder": case["export_folder"], "gstin": g}]}]
        oracle = [curl(f"http://books.example.test/company/{cid}/gstr1/export", month=period, folder=folder),
                  curl(f"http://fileprep.example.test/client/{fpid}/gstr1/{period}/import", folder=folder, file=export_name,
                       mode="overwrite")]
        new = [party(k)["gstin"] for k in case["new_receivers"]]
        if new:
            oracle.append(curl(f"http://fileprep.example.test/client/{fpid}/receivers", gstin=new[0], action="validate")
                          if len(new) == 1 else
                          f"{ROUTE} " + " ".join(f"--data-urlencode 'gstin={x}'" for x in new)
                          + f" --data-urlencode 'action=validate' http://fileprep.example.test/client/{fpid}/receivers")
        request = (f"File the {period_words(period)} GSTR-1 of {client['name']} (GSTIN {g}) from its books. The month's sales "
                   f"are entered in Bookwell, and the month's invoices are in ~/work/practice/{case['id']}/. Our apps: Bookwell "
                   f"http://books.example.test, FilePrep GST http://fileprep.example.test (it opens TaxNet signed in as the "
                   f"client), TaxNet http://taxnet.example.test.")
        if case.get("cancelled_gstin"):
            bad = party(case["cancelled_gstin"])
            bad_inv = next(b["no"] for b in bills if b["who"] == case["cancelled_gstin"])
            facts = [{"id": "cancelled-receiver", "about": "what to do when a customer's GSTIN is cancelled on the register",
                      "refers_to": ["cancelled", "cancelled gstin", "gstin cancelled", "receiver gstin", "customer gstin",
                                    bad["name"].split()[0].lower()],
                      "answer": "If a customer's GSTIN is cancelled, do not save or file the return. Stop, and tell me which "
                                "invoice and customer it is; I will take it up with the client.", "required": True}]
            graders += [{"id": "validated", "kind": "mock_records", "collection": "fp_receivers",
                         "records": [{"client": fpid, "gstin": bad["gstin"], "status": "Cancelled"}]},
                        {"id": "not-saved", "kind": "absent", "collection": "fp_uploads"},
                        {"id": "not-filed", "kind": "absent", "collection": "tn_returns", "records": [{"gstin": g, "period": period}]},
                        {"id": "nothing-on-the-portal", "kind": "absent", "collection": "tn_b2b"},
                        {"id": "says-which-invoice", "kind": "final_text_contains", "words": [bad_inv]},
                        {"kind": "asked", "facts": ["cancelled-receiver"]}]
            oracle.append({"answer": f"Not filed: the GSTIN {bad['gstin']} of {bad['name']} (invoice {bad_inv}) is cancelled on "
                                     f"the register, so the return was neither saved to TaxNet nor filed."})
            # (Export writes the return's JSON into the client's month folder on the office share, overwriting an
            # earlier one: the plan holds it for the person's OK, and the person gives it - found 2026-11-09. So does
            # the return's Import into FilePrep, and the receivers' validation and name update there: writes an
            # untaught worker's plan holds for the person.)
            approves = ["Export", "Import", "Validate selected", "Update names from TaxNet", "Save to portal",
                        "File with EVC", "Verify"]
        else:
            facts = [otp_fact(case["otp"])]
            graders += [{"id": "receivers", "kind": "mock_records", "collection": "fp_receivers",
                         "records": [{"client": fpid, "gstin": party(k)["gstin"], "status": "Valid",
                                      "legal_name": party(k)["name"].upper()} for k in case["new_receivers"]]
                         or [{"client": fpid, "status": "Valid"}]},
                        {"id": "saved", "kind": "mock_records", "collection": "fp_uploads",
                         "records": [{"client": fpid, "period": period, "status": "Processed"}]}]
            graders += tn_graders(g, period, rows, {"nil": "no", "filed_on": D(case["today"])})
            graders += [{"id": "reports-arn", "kind": "final_text_contains", "words": [arn_of(g, period)]},
                        {"kind": "asked", "facts": ["otp"]},
                        {"id": "not-filed-before-the-otp", "kind": "absent", "collection": "tn_returns",
                         "records": [{"gstin": g, "period": period, "status": "Filed"}], "when": "asked"}]
            for x in new:
                oracle.append(curl(f"http://fileprep.example.test/client/{fpid}/receivers", gstin=x, action="names"))
            oracle += [curl(f"http://fileprep.example.test/client/{fpid}/gstr1/{period}/save"),
                       curl("http://taxnet.example.test/login", user=TN_USERS[g], password=TN_PASSWORDS[g]),
                       curl(f"http://taxnet.example.test/gstr1/{period}/generate"),
                       curl(f"http://taxnet.example.test/gstr1/{period}/file-evc", declare="yes", signatory=SIGNATORY[g]),
                       curl(f"http://taxnet.example.test/gstr1/{period}/verify", otp=case["otp"]),
                       {"answer": f"Filed {client['name']}'s GSTR-1 for {period_words(period)} with EVC. ARN {arn_of(g, period)}."}]
            # (Export writes the return's JSON into the client's month folder on the office share, overwriting an
            # earlier one: the plan holds it for the person's OK, and the person gives it - found 2026-11-09. So does
            # the return's Import into FilePrep, and the receivers' validation and name update there: writes an
            # untaught worker's plan holds for the person.)
            approves = ["Export", "Import", "Validate selected", "Update names from TaxNet", "Save to portal",
                        "File with EVC", "Verify"]
        # The portal's demo passwords (every client's login is in the fixture); the oracle types its client's.
        secrets = tuple(TN_PASSWORDS[x] for x in TN_PASSWORDS)
        doc = case_doc(case["id"], case["category"], "gstr1_from_books", request, files, facts, approves, graders, oracle,
                       secrets=secrets, minutes=60, tokens=10000000, tags=case["tags"])
        out.update(write_case("office-gstr1-books-v1", doc, files, fixture))
    out["suite.json"] = (json.dumps(suite_doc(
        "office-gstr1-books-v1",
        "A CA office files a client's monthly GSTR-1 from its books: the month's sales return is exported from the books "
        "app as JSON into the client's own month folder on the office share, imported into the return-prep software "
        "with Overwrite, checked against the books, its receivers' GSTINs validated against the portal's register (names "
        "taken from it), saved to the portal, the period chosen there (financial year, quarter, month), the summary "
        "generated and the return filed with EVC, whose OTP only the client has. Case 1 is the one a person demonstrates; "
        "the rest are new months with the traps a real month has: the export dialog still pointing at another client's or "
        "last month's folder, a receiver never validated, inter-state supplies (IGST, B2C Large), several HSN codes, a "
        "cancelled invoice in the series, the portal opening on the current quarter, an earlier partial import that "
        "Append would double, and a receiver whose GSTIN the register shows cancelled (the return must not be filed). "
        "Cases 5 to 12 are both clients' next months (January to April 2027, April in each client's new company for "
        "2027-28), the same receivers and some new ones, about half of them plain and the rest with one of those traps "
        "again, so a worker taught once can show its curve over fresh months. "
        "Graded on the share, the software's receivers and uploads, and the portal's tables and filed return, exactly; and "
        "the ARN in the report. Fictional data; the ca-office mock.",
        [c["id"] for c in GSTR_BOOKS_CASES], 60, 10000000), indent=2) + "\n").encode("utf-8")
    return {f"office-gstr1-books-v1/{k}": v for k, v in out.items()}


# =====================================================================================================================
# Suite 3: office-gstr1-chat-v1 - GSTR-1 from invoices sent on chat, tables entered on the portal by hand
# =====================================================================================================================

SHARAVATI = {"name": "Sharavati Builders", "person": "Ravi Kamath", "gstin": gstin("29ACKPK4821M1Z"),
             "address": "Near Bus Stand, Brahmavar, Udupi 576213", "phone": "+91 90000 71150", "town": "Udupi"}
NETRAVATI = {"name": "Netravati Interiors", "person": "Sunita Rao", "gstin": gstin("29BCRPR7710C1Z"),
             "address": "Bendoorwell, Mangaluru 575002", "phone": "+91 90000 82260", "town": "Mangaluru"}
SIGNATORY_CHAT = {SHARAVATI["gstin"]: "Ravi Kamath", NETRAVATI["gstin"]: "Sunita Rao"}
CHAT_PASSWORDS = {SHARAVATI["gstin"]: "Sharavati#Demo26", NETRAVATI["gstin"]: "Netravati#Demo26"}
CHAT_USERS = {SHARAVATI["gstin"]: "sharavati.builders", NETRAVATI["gstin"]: "netravati.interiors"}

for _k, _v in {"mira": ("Mira Pinto", "Mira Pinto", "Mobile: +91 90000 10011, Kemmannu, Udupi 576115", "", "29"),
               "arun": ("Arun Shetty", "Arun Shetty", "Mobile: +91 90000 10012, Kallianpur, Udupi 576114", "", "29"),
               "agro": ("Coastal Agro Foods LLP", "Coastal Agro Foods LLP", "Baikampady Industrial Area, Mangaluru 575011",
                        gstin("29AAMFC4471R1Z"), "29"),
               "prakash": ("Prakash Bhat", "Prakash Bhat", "Mobile: +91 90000 10013, Hiriyadka, Udupi 576113", "", "29"),
               "leena": ("Leena Fernandes", "Leena Fernandes", "Mobile: +91 90000 10014, Shirva, Udupi 574116", "", "29"),
               "anil": ("Anil Varghese", "Anil Varghese", "Nileshwar, Kasaragod, Kerala 671314", "", "32"),
               "sneha": ("Sneha Nair", "Sneha Nair", "Payyannur, Kannur, Kerala 670307", "", "32"),
               "rohan": ("Rohan Kini", "Rohan Kini", "Mobile: +91 90000 10015, Santhekatte, Udupi 576105", "", "29")}.items():
    PARTIES[_k] = _v

CHAT_CASES = [
    {"id": "oc-1-october", "client": SHARAVATI, "period": "2026-10", "today": "2026-11-04", "otp": "730418", "category": "ordinary",
     "bills": [inclusive("201", "2026-10-06", "mira", 150000), inclusive("202", "2026-10-15", "arun", 240000),
               inclusive("203", "2026-10-27", "mira", 50000)],
     "chat": ["Please send the invoices for October."], "client_says": ["These are all the bills for October."],
     "tags": ["b2c-others", "tax-inclusive"]},
    {"id": "oc-2-november", "client": SHARAVATI, "period": "2026-11", "today": "2026-12-03", "otp": "462091", "category": "varied",
     "bills": [inclusive("204", "2026-11-05", "agro", 354000), inclusive("205", "2026-11-12", "prakash", 118000, cancelled=True),
               inclusive("206", "2026-11-24", "leena", 118000)],
     "chat": ["Please send the invoices for November."],
     "client_says": ["Invoice 205 is cancelled, the buyer backed out. I have sent it anyway, marked cancelled."],
     "tags": ["registered-buyer", "cancelled-invoice"]},
    {"id": "oc-3-december", "client": SHARAVATI, "period": "2026-12", "today": "2027-01-07", "otp": "915364", "category": "varied",
     "bills": [inclusive("207", "2026-12-04", "anil", 236000), inclusive("208", "2026-12-11", "sneha", 59000),
               inclusive("209", "2026-12-22", "rohan", 120000)],
     "chat": ["Please send the invoices for December."], "client_says": ["Two of these are for the Kerala sites."],
     "tags": ["b2c-large", "inter-state", "quarter-default"]},
    {"id": "oc-4-nil-month", "client": NETRAVATI, "period": "2026-10", "today": "2026-11-06", "otp": "208857", "category": "varied",
     "bills": [], "chat": ["Please send the invoices for October."],
     "client_says": ["There are no bills this month, nothing was billed in October."], "tags": ["nil-return"]},
]

# Each client's earlier months, in its workbook and filed on the portal.
CHAT_HISTORY = {
    SHARAVATI["gstin"]: [("2026-08", [inclusive("194", "2026-08-04", "arun", 100000), inclusive("195", "2026-08-19", "mira", 160000),
                                      inclusive("196", "2026-08-28", "rohan", 75000)]),
                         ("2026-09", [inclusive("197", "2026-09-09", "leena", 125000), inclusive("198", "2026-09-17", "arun", 90000),
                                      inclusive("199", "2026-09-25", "mira", 110000), inclusive("200", "2026-09-30", "rohan", 60000)])],
    NETRAVATI["gstin"]: [("2026-08", []), ("2026-09", [])],
}
CREDIT = {SHARAVATI["gstin"]: ("0.00", "16964.00", "16964.00"), NETRAVATI["gstin"]: ("0.00", "4210.00", "4210.00")}


def chat_bill(client, b):
    b = backed_out(b, client["gstin"][:2])
    y, m, d = (int(x) for x in b["date"].split("-"))
    from datetime import date, timedelta
    b["due"] = (date(y, m, d) + timedelta(days=7)).isoformat()
    b["file"] = f"{b['no']}_sales_invoice_{b['party']['name'].replace(' ', '_')}.pdf"
    b["rate"], b["hsn"], b["total"] = 18, "9954", b["gross"]
    return b


def workbook(client, months, current):
    """The client's computation workbook: SALES (a block per month, the current month's made ready and empty) and COMP
    (one line per month)."""
    sales = [[client["name"].upper()], [f"SALES REGISTER {fy_folder(current)[0]}"], []]
    head = ["Date", "Party", "Invoice no.", "HSN Code", "GSTIN", "Qty.", "Taxable Value", "CGST @9%", "SGST @9%", "IGST @18%",
            "Round off", "Total Sales"]
    comp = [[client["name"].upper()], [f"GST COMPUTATION {fy_folder(current)[0]}"], [],
            ["Month", "Sales taxable", "CGST", "SGST", "IGST", "Invoices", "GSTR-1 ARN"]]
    for period, bills in months:
        word = period_words(period).upper()
        sales += [[word.split()[0], f"{word} RECEIPTS"], head]
        t = [q(0)] * 5
        for raw in bills:
            b = backed_out(raw, client["gstin"][:2])
            sales.append([dmy(b["date"], "/"), b["party"]["name"].upper(), int(b["no"]), 9954, b["party"]["gstin"], "",
                          float(b["taxable"]), float(b["cgst"]), float(b["sgst"]), float(b["igst"]), float(b["round_off"]) or "",
                          float(b["gross"])])
            for i, k in enumerate(("taxable", "cgst", "sgst", "igst", "gross")):
                t[i] += b[k]
        sales += [["TOTAL", "", "", "", "", "", float(t[0]), float(t[1]), float(t[2]), float(t[3]), "", float(t[4])], []]
        comp.append([period_words(period), float(t[0]), float(t[1]), float(t[2]), float(t[3]), len(bills),
                     arn_of(client["gstin"], period)])
    word = period_words(current).upper()
    sales += [[word.split()[0], f"{word} RECEIPTS"], head]
    comp.append([period_words(current), "", "", "", "", "", ""])
    return xlsx_bytes({"SALES": sales, "COMP": comp})


def workbook_months(client, period):
    """The months of the period's financial year before it, as the client's workbook holds them once the earlier cases
    are in it: each month's bills (a cancelled one is no sale), a month with none empty."""
    known = dict(CHAT_HISTORY[client["gstin"]])
    for c in CHAT_CASES:
        if c["client"]["gstin"] == client["gstin"] and c["period"] < period:
            known[c["period"]] = c["bills"]
    fy = fy_folder(period)[0]
    seen = [p for p in known if fy_folder(p)[0] == fy and p < period]
    out, p = [], min(seen) if seen else period
    while p < period:
        out.append((p, [b for b in known.get(p, []) if not b["cancelled"]]))
        p = next_month(p)
    return out


def chat_suite():
    out = {}
    for case in CHAT_CASES:
        client = case["client"]
        g = client["gstin"]
        period = case["period"]
        bills = [chat_bill(client, b) for b in case["bills"]]
        files = {b["file"]: bill_pdf(client, b) for b in bills}
        resent = {name: next(b["file"] for b in bills if b["no"] == no) for name, no in case.get("duplicates", {}).items()}
        for name, original in resent.items():
            files[name] = files[original]
        book = f"COMPUTATION-{client['name'].split()[0].upper()}-{fy_folder(period)[0]}.xlsx"
        files[book] = workbook(client, CHAT_HISTORY[g] if case["id"] in FIRST_WRITTEN else workbook_months(client, period),
                               period)
        # ClientChat: the client's thread (an older standing arrangement, the office's ask, the PDFs, a last word) and
        # another client's thread.
        att = {f"p{i}": {"name": b["file"], "type": PDF, "b64": b64(files[b["file"]])} for i, b in enumerate(bills, 1)}
        for i, name in enumerate(resent, len(bills) + 1):
            att[f"p{i}"] = {"name": name, "type": PDF, "b64": b64(files[name])}
        thread_title = f"{client['person']} ({client['name']})"
        msgs = [{"thread": "1", "sender": "office", "text": "We have to pay 18% GST on the rent you pay every month, under reverse "
                                                           "charge, in cash.", "at": f"{months_before(period, 2)[0]}-12 15:57", "file": ""},
                {"thread": "1", "sender": "client", "text": "Okay, we will pay.", "at": f"{months_before(period, 2)[0]}-12 15:58", "file": ""},
                {"thread": "1", "sender": "office", "text": case["chat"][0], "at": f"{case['today']} 10:05", "file": ""}]
        for i, b in enumerate(bills, 1):
            msgs.append({"thread": "1", "sender": "client", "text": "", "at": f"{case['today']} 18:29", "file": f"p{i}"})
        for i, _ in enumerate(resent, len(bills) + 1):
            msgs.append({"thread": "1", "sender": "client", "text": "", "at": f"{case['today']} 18:30", "file": f"p{i}"})
        msgs += [{"thread": "1", "sender": "client", "text": t, "at": f"{case['today']} 18:31", "file": ""} for t in case["client_says"]]
        msgs += [{"thread": "2", "sender": "client", "text": "Sir, when is the ITR due?", "at": f"{case['today']} 09:40", "file": ""}]
        threads = ids([{"client": client["name"], "title": thread_title},
                       {"client": "Kalinga Hardware", "title": "Manjunath (Kalinga Hardware)"}])
        fp_clients = ids([
            {"name": SHARAVATI["name"], "legal_name": SHARAVATI["person"].upper(), "gstin": SHARAVATI["gstin"], "state": "Karnataka",
             "registration": "Regular", "group": "Asha", "taxnet_user": CHAT_USERS[SHARAVATI["gstin"]]},
            {"name": "Sharavati Agencies", "legal_name": "SHARAVATI AGENCIES", "gstin": gstin("29AAKFS3311C1Z"), "state": "Karnataka",
             "registration": "Regular", "group": "Rajesh", "taxnet_user": ""},
            {"name": NETRAVATI["name"], "legal_name": NETRAVATI["person"].upper(), "gstin": NETRAVATI["gstin"], "state": "Karnataka",
             "registration": "Regular", "group": "Asha", "taxnet_user": CHAT_USERS[NETRAVATI["gstin"]]},
            {"name": "Kalinga Hardware", "legal_name": "KALINGA HARDWARE", "gstin": KALINGA, "state": "Karnataka",
             "registration": "Regular", "group": "Rajesh", "taxnet_user": ""}])
        reg = [{"gstin": x["gstin"], "legal_name": x["legal_name"], "trade_name": x["name"].upper(), "status": "Active",
                "registered_on": "2021-04-27"} for x in fp_clients]
        reg.append({"gstin": party("agro")["gstin"], "legal_name": "COASTAL AGRO FOODS LLP", "trade_name": "COASTAL AGRO FOODS LLP",
                    "status": "Active", "registered_on": "2019-02-11"})
        returns = []
        for x in (SHARAVATI["gstin"], NETRAVATI["gstin"]):
            for p in months_before(period, 3):
                returns.append({"gstin": x, "period": p, "status": "Filed", "nil": "yes" if x == NETRAVATI["gstin"] else "no",
                                "arn": arn_of(x, p), "filed_on": f"{p}-27", "signatory": SIGNATORY_CHAT[x], "method": "EVC"})
        app = base_app(case["today"], tn_logins=[{"gstin": x, "user": CHAT_USERS[x], "password": CHAT_PASSWORDS[x]} for x in CHAT_USERS],
                       signatories={x: [SIGNATORY_CHAT[x]] for x in SIGNATORY_CHAT}, otps={f"{g}:{period}": case["otp"]},
                       mobiles={SHARAVATI["gstin"]: "98xxxxxx50", NETRAVATI["gstin"]: "98xxxxxx60"})
        cr = [{"gstin": x, "igst": v[0], "cgst": v[1], "sgst": v[2], "cess": "0.00"} for x, v in CREDIT.items()]
        fixture = {"collections": {"chat_threads": threads, "chat_messages": ids(msgs), "fp_clients": fp_clients,
                                   "tn_taxpayers": ids(reg), "tn_ledger": ids(cr), "tn_returns": ids(returns)},
                   "files": att, "app": app}
        rows = gstr1_rows(client, bills) if bills else {"b2b": [], "b2cl": [], "b2cs": [], "hsn": [], "docs": []}
        for r in rows["b2cs"] + rows["hsn"]:
            # The portal works out Table 7's tax on the consolidated value; the bills' own taxes must add up to it, or
            # the case would hold two right answers (the instances are chosen so they do).
            want = q(Decimal(r["txval"]) * Decimal("0.09"))
            assert r["iamt"] != "0.00" or Decimal(r["camt"]) == want, (case["id"], r, want)
        graders = tn_graders(g, period, rows, {"nil": "yes" if not bills else "no", "filed_on": D(case["today"])})
        graders += [{"id": "reports-arn", "kind": "final_text_contains", "words": [arn_of(g, period)]},
                    {"kind": "asked", "facts": ["otp"]},
                    {"id": "not-filed-before-the-otp", "kind": "absent", "collection": "tn_returns",
                     "records": [{"gstin": g, "period": period, "status": "Filed"}], "when": "asked"}]
        oracle = [curl("http://taxnet.example.test/login", user=CHAT_USERS[g], password=CHAT_PASSWORDS[g])]
        if not bills:
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/nil", nil="yes"))
        for r in rows["b2b"]:
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/b2b", ctin=r["ctin"], inum=r["inum"], idt=r["idt"],
                               val=r["val"], pos=r["pos"], rt=r["rt"], txval=r["txval"]))
        for r in rows["b2cl"]:
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/b2cl", pos=r["pos"], inum=r["inum"], idt=r["idt"],
                               val=r["val"], rt=r["rt"], txval=r["txval"]))
        for r in rows["b2cs"]:
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/b2cs", pos=r["pos"], rt=r["rt"], txval=r["txval"]))
        for r in rows["hsn"]:
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/hsn", tab=r["tab"], hsn=r["hsn"], uqc=r["uqc"], qty="0",
                               txval=r["txval"], rt=r["rt"], iamt=r["iamt"], camt=r["camt"], samt=r["samt"], csamt="0"))
        if bills:
            d = rows["docs"][0]
            oracle.append(curl(f"http://taxnet.example.test/gstr1/{period}/docs", **{"from": d["from"], "to": d["to"],
                                                                                    "total": d["total"], "cancelled": d["cancelled"]}))
        oracle += [curl(f"http://taxnet.example.test/gstr1/{period}/generate"),
                   curl(f"http://taxnet.example.test/gstr1/{period}/file-evc", declare="yes", signatory=SIGNATORY_CHAT[g]),
                   curl(f"http://taxnet.example.test/gstr1/{period}/verify", otp=case["otp"]),
                   {"answer": f"Filed {client['name']}'s GSTR-1 for {period_words(period)} with EVC. ARN {arn_of(g, period)}."}]
        request = (f"Prepare and file the {period_words(period)} GSTR-1 of {client['name']} (GSTIN {g}). The client sends the "
                   f"month's sales invoices on ClientChat; whatever it sent, and the client's computation workbook, are also "
                   f"in ~/work/practice/{case['id']}/. Our apps: ClientChat http://chat.example.test, FilePrep GST "
                   f"http://fileprep.example.test (it opens TaxNet signed in as the client), TaxNet http://taxnet.example.test.")
        doc = case_doc(case["id"], case["category"], "gstr1_from_invoices", request, files, [otp_fact(case["otp"])],
                       ["File with EVC", "Verify"], graders, oracle, secrets=tuple(CHAT_PASSWORDS.values()), minutes=60, tokens=10000000,
                       tags=case["tags"])
        out.update(write_case("office-gstr1-chat-v1", doc, files, fixture))
    out["suite.json"] = (json.dumps(suite_doc(
        "office-gstr1-chat-v1",
        "A CA office files a small client's monthly GSTR-1 from the sales invoices the client sends as PDFs on chat: each "
        "invoice is read (tax-inclusive round amounts, the taxable value backed out at 18%), the client's computation "
        "workbook shows the series and the earlier months, and the portal's tables are entered by hand: 4A (B2B, a buyer "
        "with a GSTIN), 5 (B2C Large: inter-state to an unregistered buyer over 1,00,000), 7 (B2C Others, one row per place "
        "of supply and rate), 12 (HSN, in its B2B and B2C tabs) and 13 (documents issued, cancelled ones counted), or the "
        "return filed nil; then the summary is generated and the return filed with EVC, whose OTP only the client has. Case "
        "1 is the one a person demonstrates; the rest are new months: a registered buyer and a cancelled invoice, "
        "inter-state buyers with the portal opening on the current quarter, and a month with no bills. Cases 5 to 12 are "
        "the next months (January to July 2027, the workbook holding the months before), the same buyers and some new "
        "ones, about half of them plain and the rest with a cancelled invoice, a nil month, Kerala buyers again or an "
        "invoice sent twice on the chat, so a worker taught once can show its curve over fresh months. Graded on the "
        "portal's tables and filed return, exactly, and the ARN in the report. Fictional data; the ca-office mock.",
        [c["id"] for c in CHAT_CASES], 60, 10000000), indent=2) + "\n").encode("utf-8")
    return {f"office-gstr1-chat-v1/{k}": v for k, v in out.items()}


# =====================================================================================================================
# Later months: cases 5 to 12 of each suite
# =====================================================================================================================
# A worker taught case 1 once runs these in order, its state carried, and should get more right, faster and cheaper:
# fresh inputs of the same work - the clients' following months, the same customers and some new ones, about half of
# them plain and the rest with one or two of the traps above. The cases written first keep their bytes (a worker was
# taught on case 1 and runs on 2-4 are recorded), so what they list is kept as it was.

FIRST_WRITTEN = frozenset(c["id"] for c in BOOKS_CASES + GSTR_BOOKS_CASES + CHAT_CASES)
V1_PARTIES = frozenset(PARTIES)

PARTIES.update({
    # the lift-maintenance client's new customers
    "brahmagiri": ("Brahmagiri Diagnostics LLP", "Brahmagiri Diagnostics LLP", "Court Road, Udupi 576101",
                   gstin("29AAKFB4408C1Z"), "29"),
    "chandragiri": ("Chandragiri Hospital Trust", "Chandragiri Hospital Trust", "Vidyanagar, Kasaragod, Kerala 671123",
                    gstin("32AABTC5506H1Z"), "32"),
    "shambhavi": ("Shambhavi Residency", "Shambhavi Residency", "Santhekatte, Udupi 576105", "", "29"),
    # the return-from-books clients' new customers
    "bhadra": ("Bhadra Valley Hospital Trust", "Bhadra Valley Hospital Trust", "Lakkavalli Road, Bhadravathi 577301",
               gstin("29AABTB6627L1Z"), "29"),
    "gajanur": ("Gajanur Heights Apartments", "Gajanur Heights Apartments", "Gajanur Road, Shivamogga 577204", "", "29"),
    "tirthahalli": ("Tirthahalli Commercial Complex LLP", "Tirthahalli Commercial Complex LLP",
                    "Bus Stand Road, Tirthahalli 577432", gstin("29AAKFT2290N1Z"), "29"),
    "sindhudurg": ("Sindhudurg Coast Hotels LLP", "Sindhudurg Coast Hotels LLP", "Bandar Road, Malvan, Maharashtra 416606",
                   gstin("27AAMFS5862K1Z"), "27"),
    "savio": ("Savio Menezes", "Savio Menezes", "Porvorim, Goa 403521", "", "30"),
    "majali": ("Majali Bay Resorts Pvt Ltd", "Majali Bay Resorts Pvt Ltd", "Majali, Karwar 581345",
               gstin("29AAJCM8841E1Z"), "29"),
    # the builder's new buyers
    "kavya": ("Kavya Hegde", "Kavya Hegde", "Mobile: +91 90000 10016, Parkala, Udupi 576107", "", "29"),
    "sunil": ("Sunil Acharya", "Sunil Acharya", "Mobile: +91 90000 10017, Kota, Udupi 576221", "", "29"),
    "joseph": ("Joseph Mathew", "Joseph Mathew", "Bekal, Kasaragod, Kerala 671318", "", "32"),
})

LATER_BOOKS_CASES = [
    {"id": "ob-5-february", "period": "2027-02", "today": "2027-03-03", "last_company": "1", "category": "ordinary",
     "bills": [
         bill("1062", "2027-02-03", "udyavara", [("Lift maintenance, Feb 2027", "1", "6,000.00", 6000)], file="1062 Udyavara Apt.docx"),
         bill("1063", "2027-02-09", "pai", [("Lift door operator service", "1", "9,500.00", 9500)], file="1063 Pai Textiles.docx"),
         bill("1064", "2027-02-16", "sagar", [("Lift rope inspection and lubrication", "1", "8,000.00", 8000)],
              file="1064 Sagar Vista.docx"),
         bill("1065", "2027-02-24", "sharada", [("Lift maintenance, Feb 2027", "1", "15,000.00", 15000)],
              file="1065 Sharada Residency.docx")],
     "new_ledgers": [], "entered": [], "tags": ["name-differs"]},
    {"id": "ob-6-march", "period": "2027-03", "today": "2027-04-05", "last_company": "1", "category": "duplicate",
     "bills": [
         bill("1066", "2027-03-02", "mithila", [("Lift maintenance, Apr - Jun 2027 quarter, advance", "1", "25,000.00", 25000)],
              file="1066 Mithila Towers.docx"),
         bill("1067", "2027-03-05", "lotus", [("Escalator comb plate replacement", "1", "46,000.00", 46000)], file="1067 Lotus Mall.docx"),
         bill("1068", "2027-03-11", "kadri", [("Residential lift: car operating panel", "2 nos", "18,500.00", 37000)],
              file="1068 Kadri Heights.docx"),
         bill("1069", "2027-03-17", "malpe", [("Goods lift annual inspection", "1", "12,500.00", 12500)], file="1069 Malpe Fish.docx"),
         bill("1070", "2027-03-23", "bhuvana", [("Bed lift door sensor", "1", "16,800.00", 16800)], file="1070 Bhuvana Hospital.docx"),
         bill("1071", "2027-03-30", "udyavara", [("Lift battery replacement", "1", "7,200.00", 7200)],
              file="1071 Udyavara Apartments.docx")],
     "duplicates": {"Copy of 1067 Lotus Mall.docx": "1067"},
     "new_ledgers": [], "entered": [], "tags": ["sent-twice"]},
    {"id": "ob-7-april", "period": "2027-04", "today": "2027-05-04", "last_company": "1", "category": "ordinary",
     "bills": [
         bill("1072", "2027-04-02", "sagar", [("Lift annual maintenance, Apr - Sep 2027, first instalment", "1", "24,000.00", 24000)],
              file="1072 Sagar Vista.docx"),
         bill("1073", "2027-04-08", "brahmagiri", [("Machine-room-less lift: guide rails and brackets", "1 set", "85,000.00", 85000)],
              file="1073 Brahmagiri Diagnostics.docx"),
         bill("1074", "2027-04-15", "pai", [("Lift car light fittings", "1", "7,400.00", 7400)], file="1074 Pai Textiles.docx"),
         bill("1075", "2027-04-22", "sharada", [("Lift maintenance, Apr 2027", "1", "15,000.00", 15000)],
              file="1075 Sharada Residency.docx"),
         bill("1076", "2027-04-28", "hampi", [("Door sensor replacement", "1", "6,500.00", 6500)], file="1076 Hampi Residency.docx")],
     "new_ledgers": ["brahmagiri"], "entered": [], "tags": ["new-ledger"]},
    {"id": "ob-8-may", "period": "2027-05", "today": "2027-06-03", "last_company": "3", "category": "missing_input",
     "bills": [
         bill("1077", "2027-05-05", "lotus", [("Escalator annual inspection", "1", "18,000.00", 18000)], file="1077 Lotus Mall.docx"),
         bill("1078", "2027-05-12", "udyavara", [("Lift maintenance, May 2027", "1", "6,000.00", 6000)], file="1078 Udyavara Apt.docx"),
         bill("1079", "2027-05-19", "malpe", [("Goods lift door interlock repair", "1", "14,600.00", 14600)], file="1079 Malpe Fish.docx"),
         bill("1080", "2027-05-26", "kadri", [("Residential lift: rope tensioning", "1", "8,800.00", 8800)],
              file="1080 Kadri Heights.docx")],
     "missing": "1079", "new_ledgers": [], "entered": [], "tags": ["number-gap", "old-company-open"]},
    {"id": "ob-9-june", "period": "2027-06", "today": "2027-07-05", "last_company": "1", "category": "ordinary",
     "bills": [
         bill("1081", "2027-06-04", "sharada", [("Lift maintenance, Jun 2027", "1", "15,000.00", 15000)],
              file="1081 Sharada Residency.docx"),
         bill("1082", "2027-06-15", "bhuvana", [("Stretcher lift service", "1", "14,200.00", 14200)], file="1082 Bhuvana Hospital.docx"),
         bill("1083", "2027-06-25", "mithila", [("Lift maintenance, Jul - Sep 2027 quarter, advance", "1", "25,000.00", 25000)],
              file="1083 Mithila Towers.docx")],
     "new_ledgers": [], "entered": [], "tags": []},
    {"id": "ob-10-july", "period": "2027-07", "today": "2027-08-03", "last_company": "1", "category": "varied",
     "bills": [
         bill("1084", "2027-07-02", "chandragiri", [("Bed lift installation, first stage", "1", "1,40,000.00", 140000)],
              file="1084 Chandragiri Hospital.docx"),
         bill("1085", "2027-07-07", "sagar", [("Lift car interior panels", "1", "32,000.00", 32000)], file="1085 Sagar Vista.docx"),
         bill("1086", "2027-07-13", "kadri", [("Lift spare parts: landing door rollers", "6 sets", "2,250.00", 13500)], rate=12,
              hsn="8431", file="1086 Kadri Heights.docx"),
         bill("1087", "2027-07-20", "udyavara", [("Lift maintenance, Jul 2027", "1", "6,000.00", 6000)], file="1087 Udyavara Apt.docx"),
         bill("1088", "2027-07-26", "pai", [("Automatic rescue device battery", "1", "4,800.00", 4800)], file="1088 Pai Textiles.docx"),
         bill("1089", "2027-07-30", "lotus", [("Escalator handrail cleaning and polish", "1", "9,000.00", 9000)],
              file="1089 Lotus Mall.docx")],
     "new_ledgers": ["chandragiri"], "entered": [], "tags": ["inter-state", "new-ledger", "other-rate"]},
    {"id": "ob-11-august", "period": "2027-08", "today": "2027-09-02", "last_company": "1", "category": "ordinary",
     "bills": [
         bill("1090", "2027-08-03", "pai", [("Lift door operator belt", "1", "3,900.00", 3900)], file="1090 Pai Textiles.docx"),
         bill("1091", "2027-08-09", "shambhavi", [("Lift maintenance, Aug 2027", "1", "4,500.00", 4500)],
              file="1091 Shambhavi Residency.docx"),
         bill("1092", "2027-08-16", "malpe", [("Goods lift gear oil change", "1", "11,000.00", 11000)], file="1092 Malpe Fish.docx"),
         bill("1093", "2027-08-23", "sharada", [("Lift maintenance, Aug 2027", "1", "15,000.00", 15000)],
              file="1093 Sharada Residency.docx"),
         bill("1094", "2027-08-30", "kadri", [("Residential lift: annual maintenance contract", "1", "36,000.00", 36000)],
              file="1094 Kadri Heights.docx")],
     "new_ledgers": ["shambhavi"], "entered": [], "tags": ["new-ledger", "unregistered"]},
    {"id": "ob-12-september", "period": "2027-09", "today": "2027-10-04", "last_company": "1", "category": "duplicate",
     "bills": [
         bill("1095", "2027-09-02", "sagar", [("Lift annual maintenance, Apr - Sep 2027, second instalment", "1", "24,000.00", 24000)],
              file="1095 Sagar Vista.docx"),
         bill("1096", "2027-09-06", "udyavara", [("Lift maintenance, Sep 2027", "1", "6,000.00", 6000)], file="1096 Udyavara Apt.docx"),
         bill("1097", "2027-09-10", "brahmagiri", [("Machine-room-less lift: car and doors", "1 set", "92,000.00", 92000)],
              file="1097 Brahmagiri Diagnostics.docx"),
         bill("1098", "2027-09-14", "lotus", [("Escalator step chain lubrication", "1", "30,000.00", 30000)],
              file="1098 Lotus Mall.docx", words="Rupees Thirty Thousand Only."),
         bill("1099", "2027-09-20", "shambhavi", [("Lift maintenance, Sep 2027", "1", "4,500.00", 4500)],
              file="1099 Shambhavi Residency.docx"),
         bill("1100", "2027-09-24", "mithila", [("Lift maintenance, Oct - Dec 2027 quarter, advance", "1", "25,000.00", 25000)],
              file="1100 Mithila Towers.docx"),
         bill("1101", "2027-09-29", "bhuvana", [("Bed lift annual load test", "1", "12,000.00", 12000)],
              file="1101 Bhuvana Hospital.docx")],
     "new_ledgers": [], "entered": ["1096"], "tags": ["already-entered", "words-differ"]},
]
# Each month's ledgers are the ones the months before it left: January's, then every new customer's since.
_known = list(BOOKS_CASES[-1]["known"])
for _c in LATER_BOOKS_CASES:
    _c["known"] = list(_known)
    _known += _c["new_ledgers"]
BOOKS_CASES += LATER_BOOKS_CASES

GSTR_BOOKS_CASES += [
    {"id": "og-5-january", "client": "sahyadri", "period": "2027-01", "today": "2027-02-04", "category": "ordinary",
     "otp": "739204", "export_folder": share_folder("Konkan Coast Interiors", "2026-12"),
     "bills": [
         bill("1116", "2027-01-05", "malnad", [("Lift maintenance, Jan 2027", "1", "21,000.00", 21000)]),
         bill("1117", "2027-01-09", "coastal", [("Escalator annual inspection", "1", "24,500.00", 24500)]),
         bill("1118", "2027-01-14", "kudremukh", [("Lift rope lubrication", "1", "7,500.00", 7500)]),
         bill("1119", "2027-01-21", "belur", [("Goods lift door interlock", "1", "16,400.00", 16400)]),
         bill("1120", "2027-01-28", "agumbe", [("Lift annual maintenance, part payment", "1", "14,406.78", Decimal("14406.78"))])],
     "receivers_known": ["tunga", "belur", "agumbe", "coastal"], "new_receivers": [], "tags": ["wrong-export-folder"]},
    {"id": "og-6-january", "client": "konkan", "period": "2027-01", "today": "2027-02-05", "category": "ordinary",
     "otp": "146583", "export_folder": share_folder("Sahyadri Lift Services", "2027-01"),
     "bills": [
         bill("KCI-050", "2027-01-06", "belur", [("Display shelving units", "6 nos", "12,000.00", 72000)], hsn="9403"),
         bill("KCI-051", "2027-01-13", "anand", [("Ceiling panelling, 90 sq ft", "90 sq ft", "220.00", 19800)], hsn="4418"),
         bill("KCI-052", "2027-01-22", "tunga_k", [("Site office partition installation", "1", "58,000.00", 58000)], hsn="9954")],
     "receivers_known": ["vasco", "belur", "tunga_k"], "new_receivers": [], "tags": ["wrong-export-folder", "hsn-codes"]},
    {"id": "og-7-february", "client": "sahyadri", "period": "2027-02", "today": "2027-03-04", "category": "varied",
     "otp": "862317", "export_folder": share_folder("Konkan Coast Interiors", "2027-01"),
     "bills": [
         bill("1121", "2027-02-02", "tunga", [("Lift door operator overhaul", "1", "42,000.00", 42000)]),
         bill("1122", "2027-02-06", "malnad", [("Lift maintenance, Feb 2027", "1", "21,000.00", 21000)]),
         bill("1123", "2027-02-10", "hampi2", [("Door sensor replacement", "1", "6,800.00", 6800)], cancelled=True),
         bill("1124", "2027-02-15", "bhadra", [("Bed lift annual maintenance, Feb 2027 - Jan 2028", "1", "96,000.00", 96000)]),
         bill("1125", "2027-02-22", "kudremukh", [("Lift battery replacement", "1", "18,000.00", 18000)]),
         bill("1126", "2027-02-26", "coastal", [("Escalator step chain replacement", "1", "64,750.00", 64750)])],
     "receivers_known": ["tunga", "belur", "agumbe", "coastal"], "new_receivers": ["bhadra"],
     "tags": ["cancelled-invoice", "unvalidated-receiver"]},
    {"id": "og-8-february", "client": "konkan", "period": "2027-02", "today": "2027-03-05", "category": "varied",
     "otp": "305941", "export_folder": share_folder("Sahyadri Lift Services", "2027-02"),
     "bills": [
         bill("KCI-053", "2027-02-03", "vasco", [("Banquet hall chairs", "80 nos", "2,150.00", 172000)], hsn="9403"),
         bill("KCI-054", "2027-02-08", "savio", [("Teak bed with wardrobe", "1", "1,18,000.00", 118000)], hsn="9403"),
         bill("KCI-055", "2027-02-12", "anand", [("Kitchen cabinet installation", "1", "22,500.00", 22500)], hsn="9954"),
         bill("KCI-056", "2027-02-19", "sindhudurg", [("Lobby wall panelling, 300 sq ft", "300 sq ft", "240.00", 72000)], hsn="4418"),
         bill("KCI-057", "2027-02-25", "leena_goa", [("Study table", "1", "18,000.00", 18000)], hsn="9403")],
     "receivers_known": ["vasco", "belur", "tunga_k"], "new_receivers": ["sindhudurg"],
     "tags": ["inter-state", "b2c-large", "unvalidated-receiver"]},
    {"id": "og-9-march", "client": "sahyadri", "period": "2027-03", "today": "2027-04-06", "category": "ordinary",
     "otp": "927650", "export_folder": share_folder("Konkan Coast Interiors", "2027-02"),
     "bills": [
         bill("1127", "2027-03-03", "malnad", [("Lift maintenance, Mar 2027", "1", "21,000.00", 21000)]),
         bill("1128", "2027-03-09", "gajanur", [("Lift maintenance, Mar 2027", "1", "8,500.00", 8500)]),
         bill("1129", "2027-03-17", "belur", [("Goods lift annual maintenance", "1", "30,000.00", 30000)]),
         bill("1130", "2027-03-26", "tunga", [("Lift annual maintenance, Apr - Sep 2027, advance", "1", "48,600.00", 48600)])],
     "receivers_known": ["tunga", "belur", "agumbe", "coastal", "bhadra"], "new_receivers": [], "tags": ["quarter-default"]},
    {"id": "og-10-march", "client": "konkan", "period": "2027-03", "today": "2027-04-07", "category": "varied",
     "otp": "418372", "export_folder": share_folder("Sahyadri Lift Services", "2027-03"),
     "bills": [
         bill("KCI-058", "2027-03-02", "belur", [("Showroom ceiling panels", "1", "46,000.00", 46000)], hsn="4418"),
         bill("KCI-059", "2027-03-09", "anand", [("Dining table set", "1", "38,500.00", 38500)], hsn="9403"),
         bill("KCI-060", "2027-03-15", "vasco", [("Lobby furniture installation", "1", "35,000.00", 35000)], hsn="9954"),
         bill("KCI-061", "2027-03-22", "tunga_k", [("Site office furniture refit", "1", "24,000.00", 24000)], hsn="9954"),
         bill("KCI-062", "2027-03-29", "rhea", [("Rosewood sofa set", "1", "82,000.00", 82000)], hsn="9403")],
     "receivers_known": ["vasco", "belur", "tunga_k", "sindhudurg"], "new_receivers": [], "stale_import": ["KCI-058", "KCI-060"],
     "tags": ["overwrite-not-append", "quarter-default"]},
    {"id": "og-11-april-cancelled-gstin", "client": "sahyadri", "company": "4", "period": "2027-04", "today": "2027-05-05",
     "category": "invalid_input", "otp": "550816", "export_folder": share_folder("Konkan Coast Interiors", "2027-03"),
     "bills": [
         bill("1131", "2027-04-02", "coastal", [("Escalator maintenance, Apr - Jun 2027", "1", "45,000.00", 45000)]),
         bill("1132", "2027-04-07", "malnad", [("Lift maintenance, Apr 2027", "1", "21,000.00", 21000)]),
         bill("1133", "2027-04-12", "tirthahalli", [("Lift installation, first stage", "1", "1,10,000.00", 110000)]),
         bill("1134", "2027-04-20", "gajanur", [("Lift maintenance, Apr 2027", "1", "8,500.00", 8500)]),
         bill("1135", "2027-04-27", "bhadra", [("Bed lift door sensor", "1", "12,600.00", 12600)])],
     "receivers_known": ["tunga", "belur", "agumbe", "coastal", "bhadra"], "new_receivers": ["tirthahalli"],
     "cancelled_gstin": "tirthahalli", "tags": ["cancelled-receiver"]},
    {"id": "og-12-april", "client": "konkan", "company": "5", "period": "2027-04", "today": "2027-05-06", "category": "ordinary",
     "otp": "671029", "export_folder": share_folder("Sahyadri Lift Services", "2027-04"),
     "bills": [
         bill("KCI-063", "2027-04-03", "majali", [("Resort cottage furniture sets", "6 sets", "38,000.00", 228000)], hsn="9403"),
         bill("KCI-064", "2027-04-08", "anand", [("Wall panelling, 120 sq ft", "120 sq ft", "210.00", 25200)], hsn="4418"),
         bill("KCI-065", "2027-04-14", "tunga_k", [("Site office furniture installation", "1", "44,000.00", 44000)], hsn="9954"),
         bill("KCI-066", "2027-04-19", "belur", [("Showroom counter installation", "1", "31,000.00", 31000)], hsn="9954"),
         bill("KCI-067", "2027-04-24", "anand", [("Teak study table", "1", "24,000.00", 24000)], hsn="9403"),
         bill("KCI-068", "2027-04-29", "majali", [("Cottage furniture installation", "1", "36,000.00", 36000)], hsn="9954")],
     "receivers_known": ["vasco", "belur", "tunga_k", "sindhudurg"], "new_receivers": ["majali"],
     "tags": ["unvalidated-receiver", "hsn-codes"]},
]

CHAT_CASES += [
    {"id": "oc-5-january", "client": SHARAVATI, "period": "2027-01", "today": "2027-02-03", "otp": "284716", "category": "ordinary",
     "bills": [inclusive("210", "2027-01-08", "rohan", 118000), inclusive("211", "2027-01-19", "leena", 236000),
               inclusive("212", "2027-01-28", "arun", 59000)],
     "chat": ["Please send the invoices for January."], "client_says": ["These are all the bills for January."],
     "tags": ["b2c-others", "tax-inclusive"]},
    {"id": "oc-6-february", "client": SHARAVATI, "period": "2027-02", "today": "2027-03-04", "otp": "593028", "category": "varied",
     "bills": [inclusive("213", "2027-02-04", "mira", 177000), inclusive("214", "2027-02-11", "prakash", 295000),
               inclusive("215", "2027-02-17", "rohan", 59000, cancelled=True), inclusive("216", "2027-02-25", "kavya", 147500)],
     "chat": ["Please send the invoices for February."],
     "client_says": ["Invoice 215 is cancelled, Rohan has put off the work. I have sent it anyway, marked cancelled."],
     "tags": ["cancelled-invoice"]},
    {"id": "oc-7-february-nil", "client": NETRAVATI, "period": "2027-02", "today": "2027-03-05", "otp": "817364",
     "category": "varied", "bills": [], "chat": ["Please send the invoices for February."],
     "client_says": ["Nothing was billed in February either, there are no bills this month."], "tags": ["nil-return"]},
    {"id": "oc-8-march", "client": SHARAVATI, "period": "2027-03", "today": "2027-04-05", "otp": "360592", "category": "ordinary",
     "bills": [inclusive("217", "2027-03-05", "mira", 100000), inclusive("218", "2027-03-16", "sunil", 236000),
               inclusive("219", "2027-03-27", "prakash", 177000)],
     "chat": ["Please send the invoices for March."], "client_says": ["These are all the bills for March."],
     "tags": ["tax-inclusive", "quarter-default"]},
    {"id": "oc-9-april", "client": SHARAVATI, "period": "2027-04", "today": "2027-05-04", "otp": "749183", "category": "ordinary",
     "bills": [inclusive("220", "2027-04-03", "kavya", 59000), inclusive("221", "2027-04-09", "leena", 118000),
               inclusive("222", "2027-04-15", "rohan", 150000), inclusive("223", "2027-04-22", "arun", 295000),
               inclusive("224", "2027-04-29", "sunil", 59000)],
     "chat": ["Please send the invoices for April."], "client_says": ["These are all the bills for April."],
     "tags": ["b2c-others", "tax-inclusive"]},
    {"id": "oc-10-may", "client": SHARAVATI, "period": "2027-05", "today": "2027-06-03", "otp": "128467", "category": "ordinary",
     "bills": [inclusive("225", "2027-05-06", "agro", 472000), inclusive("226", "2027-05-13", "mira", 118000),
               inclusive("227", "2027-05-20", "rohan", 59000), inclusive("228", "2027-05-27", "prakash", 236000)],
     "chat": ["Please send the invoices for May."],
     "client_says": ["These are all the bills for May. Coastal Agro has started its second godown with us."],
     "tags": ["registered-buyer"]},
    {"id": "oc-11-june", "client": SHARAVATI, "period": "2027-06", "today": "2027-07-05", "otp": "905314", "category": "varied",
     "bills": [inclusive("229", "2027-06-03", "anil", 354000), inclusive("230", "2027-06-08", "joseph", 59000),
               inclusive("231", "2027-06-12", "mira", 177000), inclusive("232", "2027-06-18", "leena", 59000),
               inclusive("233", "2027-06-23", "arun", 118000), inclusive("234", "2027-06-29", "kavya", 236000)],
     "chat": ["Please send the invoices for June."], "client_says": ["Invoices 229 and 230 are for the Kasaragod sites."],
     "tags": ["b2c-large", "inter-state", "quarter-default"]},
    {"id": "oc-12-july", "client": SHARAVATI, "period": "2027-07", "today": "2027-08-04", "otp": "671845", "category": "duplicate",
     "bills": [inclusive("235", "2027-07-02", "rohan", 118000), inclusive("236", "2027-07-09", "sunil", 177000),
               inclusive("237", "2027-07-15", "prakash", 59000), inclusive("238", "2027-07-21", "mira", 236000),
               inclusive("239", "2027-07-28", "kavya", 147500)],
     "duplicates": {"238_sales_invoice_Mira_Pinto-1.pdf": "238"},
     "chat": ["Please send the invoices for July."], "client_says": ["These are all the bills for July."],
     "tags": ["sent-twice", "b2c-others"]},
]


# Hand-written totals the worked records must equal (a slip in the arithmetic above shows here).
CHECKS = {
    ("og-1-october", "b2b", "txval"): "214656.78",      # 48,600 + 33,250 + 1,18,400 + 14,406.78
    ("og-1-october", "b2cs", "txval"): "45800.00",      # 21,000 + 6,800 + 18,000
    ("og-2-november", "b2cl", "iamt"): "24300.00",      # 18% of 1,35,000
    ("og-3-december", "docs", "net"): "7",              # 1108-1115, 1112 cancelled
    ("oc-1-october", "b2cs", "txval"): "372881.35",     # 1,27,118.64 + 2,03,389.83 + 42,372.88
    ("oc-3-december", "hsn", "iamt"): "45000.00",       # 36,000 + 9,000
    # the later months: the vouchers to enter (not one already in the books, the missing one included) ...
    ("ob-5-february", "vouchers", "total"): "45430.00",    # 38,500 at 18%
    ("ob-6-march", "vouchers", "total"): "170510.00",      # 1,44,500 at 18%; the copy of 1067 is not a bill
    ("ob-7-april", "vouchers", "total"): "162722.00",      # 1,37,900 at 18%
    ("ob-8-may", "vouchers", "total"): "55932.00",         # 47,400 at 18%, 1079 (14,600) included
    ("ob-9-june", "vouchers", "total"): "63956.00",        # 54,200 at 18%
    ("ob-10-july", "vouchers", "total"): "241444.00",      # 1,65,200 (IGST) + 15,120 (12%) + 61,124
    ("ob-10-july", "vouchers", "igst"): "25200.00",        # 18% of 1,40,000
    ("ob-11-august", "vouchers", "total"): "83072.00",     # 70,400 at 18%
    ("ob-12-september", "vouchers", "total"): "221250.00",  # 1,87,500 at 18%; 1096 is already in the books
    # ... the returns from the books ...
    ("og-5-january", "b2b", "txval"): "55306.78",       # 24,500 + 16,400 + 14,406.78
    ("og-5-january", "b2cs", "txval"): "28500.00",      # 21,000 + 7,500
    ("og-6-january", "b2b", "txval"): "130000.00",      # 72,000 + 58,000
    ("og-6-january", "hsn", "txval"): "149800.00",      # 72,000 + 19,800 + 58,000
    ("og-7-february", "b2b", "txval"): "202750.00",     # 42,000 + 96,000 + 64,750
    ("og-7-february", "docs", "net"): "5",              # 1121-1126, 1123 cancelled
    ("og-8-february", "b2cl", "iamt"): "21240.00",      # 18% of 1,18,000
    ("og-8-february", "b2b", "iamt"): "43920.00",       # 18% of 1,72,000 + 72,000
    ("og-8-february", "b2cs", "iamt"): "3240.00",       # 18% of 18,000
    ("og-9-march", "b2b", "txval"): "78600.00",         # 30,000 + 48,600
    ("og-9-march", "b2cs", "txval"): "29500.00",        # 21,000 + 8,500
    ("og-10-march", "b2cs", "txval"): "120500.00",      # 38,500 + 82,000
    ("og-10-march", "b2cs", "iamt"): "14760.00",        # 18% of 82,000
    ("og-10-march", "b2b", "txval"): "105000.00",       # 46,000 + 35,000 + 24,000
    ("og-11-april-cancelled-gstin", "b2b", "txval"): "167600.00",  # 45,000 + 1,10,000 + 12,600
    ("og-12-april", "b2b", "txval"): "339000.00",       # 2,28,000 + 44,000 + 31,000 + 36,000
    ("og-12-april", "hsn", "txval"): "388200.00",       # 3,39,000 + 25,200 + 24,000
    # ... and the returns from the chat's invoices
    ("oc-5-january", "b2cs", "txval"): "350000.00",     # 1,00,000 + 2,00,000 + 50,000
    ("oc-6-february", "b2cs", "txval"): "525000.00",    # 1,50,000 + 2,50,000 + 1,25,000; 215 cancelled
    ("oc-6-february", "docs", "cancelled"): "1",
    ("oc-8-march", "b2cs", "camt"): "39127.12",         # 7,627.12 + 18,000 + 13,500
    ("oc-9-april", "b2cs", "txval"): "577118.64",       # 50,000 + 1,00,000 + 1,27,118.64 + 2,50,000 + 50,000
    ("oc-10-may", "b2b", "txval"): "400000.00",         # 4,72,000 / 1.18
    ("oc-10-may", "b2cs", "txval"): "350000.00",        # 1,00,000 + 50,000 + 2,00,000
    ("oc-11-june", "b2cl", "iamt"): "54000.00",         # 18% of 3,00,000
    ("oc-11-june", "b2cs", "txval"): "550000.00",       # 5,00,000 in the state + 50,000 to Kerala
    ("oc-12-july", "b2cs", "txval"): "625000.00",       # 1,00,000 + 1,50,000 + 50,000 + 2,00,000 + 1,25,000; 238 once
    ("oc-12-july", "docs", "total"): "5",
}


def check_totals():
    cases = {c["id"]: (CLIENT_OF[c["client"]], c["bills"]) for c in GSTR_BOOKS_CASES}
    cases.update({c["id"]: (c["client"], [chat_bill(c["client"], b) for b in c["bills"]]) for c in CHAT_CASES if c["bills"]})
    books = {c["id"]: c for c in BOOKS_CASES}
    for (cid, key, field), want in CHECKS.items():
        if key == "vouchers":
            c = books[cid]
            got = sum(b[field] for b in c["bills"] if b["no"] not in c["entered"])
            assert got == Decimal(want), (cid, key, field, got, want)
            continue
        client, bills = cases[cid]
        rows = gstr1_rows(client, bills)[key]
        got = sum(Decimal(r[field]) for r in rows)
        assert got == Decimal(want), (cid, key, field, got, want)


def everything():
    check_totals()
    out = {}
    out.update(books_suite())
    out.update(gstr_books_suite())
    out.update(chat_suite())
    return out


def main(argv):
    check = "--check" in argv
    bad = []
    for rel, body in sorted(everything().items()):
        path = os.path.join(SUITES, *rel.split("/"))
        if check:
            try:
                with open(path, "rb") as f:
                    if f.read() != body:
                        bad.append(rel)
            except OSError:
                bad.append(rel)
            continue
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(body)
    if bad:
        print("differs from what make_office_suites.py writes:", ", ".join(bad), file=sys.stderr)
        return 1
    if not check:
        print("wrote the three office suites under", SUITES)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
