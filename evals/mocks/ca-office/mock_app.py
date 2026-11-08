"""
CA office - five small, plain, server-rendered apps a chartered accountant's office uses for a client's monthly GST
work, in one mock for eval suites (docs/evals.md). Every name in it is generic; the data is the case's fixture.

It speaks the practice sandbox's mock contract (docs/simulation.md):

    python3 mock_app.py --port 8000 --fixture fixture.json --state state.json [--host 0.0.0.0]

and writes the whole state, {"collections": {name: [records]}, "app": {...}}, to --state at start and after every
change (atomically: a temporary file, then a rename). The graders read "collections"; "app" is the apps' own memory
(the company opened last, the folder an export used last, the return-prep software's imported data, messages sent).
Standard library only; no external assets. The app is chosen by the first label of the request's Host header:

  chat.*      ClientChat     one thread per client, messages and file attachments to download, a message box
  mail.*      Mailbox        an inbox, a mail with its attachments
  books.*     Bookwell       companies (opens on the last one used), ledgers, Sales vouchers, ledger vouchers by month,
                             a day book, a GSTR-1 view of the books and its JSON export into the office's file share
  fileprep.*  FilePrep GST   clients, a period's GSTR-1: import JSON from the share (Overwrite or Append), summary,
                             HSN summary, documents issued, receivers validated against TaxNet's register, a pre-upload
                             check and Save to portal, and a link that opens TaxNet signed in as the client
  taxnet.*    TaxNet         sign-in, returns calendar and credit ledger, File returns (financial year, quarter, month),
                             GSTR-1 tables 4A (B2B), 5 (B2C Large), 7 (B2C Others), 12 (HSN, B2B and B2C tabs) and 13
                             (documents), File Nil, Generate summary, the summary, declaration and signatory, and File
                             with EVC, which asks for the OTP sent to the taxpayer's phone and gives an ARN

Any other host gets a page listing the five. The committing controls are "Save voucher", "Save ledger", "Save to
portal" and "File with EVC" (and "Verify", which completes a filing once the OTP is typed).

Fixture: {"collections": {...}, "app": {...}, "files": {key: {"name", "type", "b64"}}}. Collections (each a list of
flat records with an "id"; values are strings):

  mails             from, from_address, subject, body, at, label, files ("key,key")
  chat_threads      client, title
  chat_messages     thread, sender ("client" | "office"), text, at, file ("" or a key)
  companies         name, number, gstin, state, period_from, period_to
  ledgers           company, name, under, mailing_name, address, state, pincode, registration, gstin, rate, hsn
  vouchers          company, type, number, date, party, party_gstin, sales_ledger, taxable, cgst, sgst, igst, total,
                    narration, cancelled ("yes" | "no")
  share_folders     path
  share_files       folder, name, gstin, period (MMYYYY), content (the file's text)
  fp_clients        name, legal_name, gstin, state, registration, group, taxnet_user
  fp_receivers      client (an fp_clients id), gstin, legal_name, trade_name, status, tn_legal_name, tn_trade_name
  fp_uploads        client, period (YYYY-MM), reference, at, status
  tn_taxpayers      gstin, legal_name, trade_name, status ("Active" | "Cancelled"), registered_on   (the register)
  tn_ledger         gstin, igst, cgst, sgst, cess   (the electronic credit ledger)
  tn_returns        gstin, period (YYYY-MM), status ("Not Filed" | "Ready to File" | "Filed"), nil, arn, filed_on,
                    signatory, method
  tn_b2b            gstin, period, ctin, inum, idt, val, pos, rt, txval, iamt, camt, samt
  tn_b2cl           gstin, period, pos, inum, idt, val, rt, txval, iamt
  tn_b2cs           gstin, period, pos, typ ("INTRA" | "INTER"), rt, txval, iamt, camt, samt
  tn_hsn            gstin, period, tab ("B2B" | "B2C"), hsn, description, uqc, qty, rt, txval, iamt, camt, samt, csamt
  tn_docs           gstin, period, nature, from, to, total, cancelled, net

"app" in the fixture: today (YYYY-MM-DD) and now (HH:MM:SS), the apps' clock; last_company (a companies id);
export_folder (the folder Bookwell's export dialog last used); import_folder (FilePrep's); tn_logins
[{gstin, user, password}] (TaxNet's accounts; a case lists the password under oracle_secrets); signatories
{gstin: [names]}; otps {"gstin:YYYY-MM": code} (the code TaxNet "sends to the taxpayer's phone"); hsn_master
{code: description}; mobiles {gstin: masked number}; fp_data {} (FilePrep's imported returns, kept here).

Amounts are strings with two decimals ("1234.50"); dates ISO ("2026-10-03"). A form refused is drawn again with each
problem in a role=alert line and status 400; nothing is stored. Every field has a <label for>, so the accessibility
tree names it.
"""

import argparse
import base64
import hashlib
import html
import json
import os
import re
import secrets
import threading
import traceback
import urllib.parse
from datetime import date, timedelta
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from http.cookies import SimpleCookie
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.RLock()
STATE_PATH = ""
STATE = {"collections": {}, "app": {}}
FILES = {}
SESSIONS = {}  # TaxNet session id -> GSTIN (memory only: a restart signs everyone out, as a real portal's would)
MAX_BODY = 1 << 20

COLLECTIONS = [
    "mails", "chat_threads", "chat_messages",
    "companies", "ledgers", "vouchers", "share_folders", "share_files",
    "fp_clients", "fp_receivers", "fp_uploads",
    "tn_taxpayers", "tn_ledger", "tn_returns", "tn_b2b", "tn_b2cl", "tn_b2cs", "tn_hsn", "tn_docs",
]
TN_TABLES = ["tn_b2b", "tn_b2cl", "tn_b2cs", "tn_hsn", "tn_docs"]

STATES = {"07": "Delhi", "24": "Gujarat", "27": "Maharashtra", "29": "Karnataka", "30": "Goa", "32": "Kerala",
          "33": "Tamil Nadu", "36": "Telangana"}
STATE_OPTIONS = [f"{c}-{n}" for c, n in STATES.items()]
RATES = ["0", "0.1", "0.25", "1", "1.5", "3", "5", "6", "7.5", "12", "18", "28", "40"]
UQCS = ["NA", "NOS", "OTH", "KGS", "MTR", "SQM"]
GROUPS = ["Capital Account", "Current Assets", "Sundry Debtors", "Sundry Creditors", "Sales Accounts",
          "Purchase Accounts", "Duties & Taxes", "Bank Accounts", "Indirect Expenses"]
REGISTRATIONS = ["Unknown", "Composition", "Regular", "Unregistered/Consumer"]
VOUCHER_TYPES = ["Payment", "Receipt", "Sales", "Purchase", "Journal"]
MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October",
          "November", "December"]
QUARTERS = {"Q1": ("Quarter 1 (Apr - Jun)", [4, 5, 6]), "Q2": ("Quarter 2 (Jul - Sep)", [7, 8, 9]),
            "Q3": ("Quarter 3 (Oct - Dec)", [10, 11, 12]), "Q4": ("Quarter 4 (Jan - Mar)", [1, 2, 3])}
B2CL_LIMIT = Decimal("100000")
CENT = Decimal("0.01")
GSTIN_CHARS = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
GSTIN_SHAPE = re.compile(r"^\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]$")


class Refused(Exception):
    """A request the app refuses: its sentences are shown to the person."""

    def __init__(self, *problems):
        super().__init__("; ".join(problems))
        self.problems = list(problems)


# --- state -----------------------------------------------------------------------------------------------------------

def save_state():
    tmp = STATE_PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(STATE, f, indent=1, ensure_ascii=False)
    os.replace(tmp, STATE_PATH)


def load_fixture(path):
    fixture = {}
    if path and os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            text = f.read().strip()
        fixture = json.loads(text) if text else {}
    if not isinstance(fixture, dict):
        raise ValueError("the fixture must be a JSON object")
    given = fixture.get("collections") or {}
    STATE["collections"] = {}
    for name in COLLECTIONS:
        recs = []
        for i, rec in enumerate(given.get(name) or [], 1):
            rec = {k: ("" if v is None else v if isinstance(v, (list, dict)) else str(v)) for k, v in dict(rec).items()}
            rec.setdefault("id", str(i))
            recs.append(rec)
        STATE["collections"][name] = recs
    app = dict(fixture.get("app") or {})
    app.setdefault("today", "2026-11-05")
    app.setdefault("now", "17:21:07")
    for key, empty in (("tn_logins", []), ("signatories", {}), ("otps", {}), ("hsn_master", {}), ("mobiles", {}),
                       ("fp_data", {}), ("sent", []), ("last_company", ""), ("export_folder", ""),
                       ("import_folder", "")):
        app.setdefault(key, empty)
    STATE["app"] = app
    FILES.clear()
    for key, f in (fixture.get("files") or {}).items():
        FILES[str(key)] = {"name": str(f.get("name") or key), "type": str(f.get("type") or "application/octet-stream"),
                           "body": base64.b64decode(f.get("b64") or "")}


def recs(name):
    return STATE["collections"].setdefault(name, [])


def find(name, rid):
    for rec in recs(name):
        if str(rec.get("id")) == str(rid):
            return rec
    return None


def where(collection, **match):
    return [r for r in recs(collection) if all(str(r.get(k, "")) == str(v) for k, v in match.items())]


def add(name, rec):
    ids = [int(r["id"]) for r in recs(name) if str(r.get("id", "")).isdigit()]
    rec = {k: ("" if v is None else str(v)) for k, v in rec.items()}
    rec["id"] = str(max(ids, default=0) + 1)
    recs(name).append(rec)
    return rec


def app():
    return STATE["app"]


def today():
    try:
        return date.fromisoformat(app()["today"])
    except (KeyError, ValueError):
        return date(2026, 11, 5)


# --- values ----------------------------------------------------------------------------------------------------------

def esc(value):
    return html.escape("" if value is None else str(value), quote=True)


def amount(text, label, required=True, negative=False):
    """A typed amount as a two-decimal string; commas, spaces and a rupee sign are allowed. Blank is "" when optional."""
    raw = str(text or "").strip().replace(",", "").replace(" ", "").replace("₹", "").replace("Rs.", "")
    if raw == "":
        if required:
            raise Refused(f"{label} is required.")
        return ""
    if not re.fullmatch(r"-?\d{1,12}(\.\d{1,2})?", raw):
        raise Refused(f"{label} must be an amount with at most two decimals.")
    value = Decimal(raw)
    if value < 0 and not negative:
        raise Refused(f"{label} cannot be negative.")
    return str(value.quantize(CENT))


def dec(text):
    try:
        return Decimal(str(text or "0").replace(",", "")).quantize(CENT)
    except InvalidOperation:
        return Decimal("0.00")


def money(value):
    """An amount as the Indian apps show it: 1,23,456.78."""
    d = dec(value)
    sign = "-" if d < 0 else ""
    whole, frac = f"{abs(d):.2f}".split(".")
    if len(whole) > 3:
        head, tail = whole[:-3], whole[-3:]
        groups = []
        while len(head) > 2:
            groups.insert(0, head[-2:])
            head = head[:-2]
        if head:
            groups.insert(0, head)
        whole = ",".join(groups + [tail])
    return f"{sign}{whole}.{frac}"


def tax_of(taxable, rate, halves):
    t = dec(taxable) * Decimal(rate) / Decimal(100)
    if halves:
        t = t / 2
    return str(t.quantize(CENT, rounding=ROUND_HALF_UP))


def gstin_valid(g):
    g = str(g or "").strip().upper()
    if not GSTIN_SHAPE.match(g):
        return False
    total = 0
    for i, ch in enumerate(g[:14]):
        p = GSTIN_CHARS.index(ch) * (2 if i % 2 else 1)
        total += p // 36 + p % 36
    return g[14] == GSTIN_CHARS[(36 - total % 36) % 36]


def state_code(option):
    """"29-Karnataka" or "29" -> "29"; "" when it is not one of the states offered."""
    code = str(option or "").split("-", 1)[0].strip()
    return code if code in STATES else ""


def state_option(code):
    return f"{code}-{STATES[code]}" if code in STATES else ""


def iso_date(text, label):
    """A date written YYYY-MM-DD, DD-MM-YYYY, DD/MM/YYYY or DD.MM.YYYY, as ISO."""
    s = str(text or "").strip()
    m = re.fullmatch(r"(\d{4})-(\d{1,2})-(\d{1,2})", s)
    if m:
        y, mo, d = int(m[1]), int(m[2]), int(m[3])
    else:
        m = re.fullmatch(r"(\d{1,2})[-/.](\d{1,2})[-/.](\d{4})", s)
        if not m:
            raise Refused(f"{label} must be a date (DD-MM-YYYY).")
        d, mo, y = int(m[1]), int(m[2]), int(m[3])
    try:
        return date(y, mo, d).isoformat()
    except ValueError:
        raise Refused(f"{label} is not a real date.")


def show_date(iso):
    try:
        d = date.fromisoformat(iso)
    except (TypeError, ValueError):
        return iso or ""
    return f"{d.day}-{MONTHS[d.month - 1][:3]}-{d.year % 100:02d}"


def period_of(text):
    """"2026-10" -> (2026, 10); None when it is not a month."""
    m = re.fullmatch(r"(\d{4})-(\d{2})", str(text or ""))
    if not m or not 1 <= int(m[2]) <= 12:
        return None
    return int(m[1]), int(m[2])


def period_words(p):
    y, m = period_of(p)
    return f"{MONTHS[m - 1]} {y}"


def fy_of(y, m):
    start = y if m >= 4 else y - 1
    return f"{start}-{(start + 1) % 100:02d}"


def quarter_of(m):
    for q, (_, months) in QUARTERS.items():
        if m in months:
            return q
    return "Q1"


def due_date(p):
    y, m = period_of(p)
    y, m = (y + 1, 1) if m == 12 else (y, m + 1)
    return date(y, m, 11).isoformat()


def arn_of(gstin, period):
    """The acknowledgement reference number of a filed return: AA, the state code, MMYY, six digits and a letter."""
    y, m = period_of(period)
    h = hashlib.sha256(f"{gstin}:{period}".encode()).hexdigest()
    return f"AA{gstin[:2]}{m:02d}{y % 100:02d}{int(h[:8], 16) % 1000000:06d}{chr(65 + int(h[8:10], 16) % 26)}"


# --- pages -----------------------------------------------------------------------------------------------------------

STYLE = """
body{font-family:sans-serif;margin:0;color:#111;background:#fff}
header{background:#e9edf2;padding:10px 20px;border-bottom:1px solid #c5ccd6}
header strong{font-size:18px;margin-right:24px} header a{margin-right:14px}
main{padding:16px 20px;max-width:1100px}
table{border-collapse:collapse;margin:10px 0} td,th{border:1px solid #bbb;padding:5px 9px;text-align:left;vertical-align:top}
th{background:#f3f5f8} td.num,th.num{text-align:right}
.field{margin:8px 0} .field label{display:inline-block;min-width:210px}
input,select,textarea{font-size:15px;padding:3px} button{font-size:15px;padding:5px 14px;margin:6px 6px 6px 0}
.note{color:#060} .error{color:#a00} .muted{color:#555} .box{border:1px solid #c5ccd6;padding:10px 14px;margin:10px 0}
.tiles{display:flex;flex-wrap:wrap;gap:10px} .tile{border:1px solid #9aa7b6;padding:10px;width:230px}
"""


def page(appname, title, body, nav=(), notes=(), errors=()):
    links = "".join(f'<a href="{esc(h)}">{esc(t)}</a>' for t, h in nav)
    alerts = "".join(f'<p class="error" role="alert">{esc(e)}</p>' for e in errors)
    shown = "".join(f'<p class="note" role="status">{esc(n)}</p>' for n in notes if n)
    return (f'<!doctype html><html lang="en"><head><meta charset="utf-8"><title>{esc(title)} - {esc(appname)}</title>'
            f'<style>{STYLE}</style></head><body><header><strong>{esc(appname)}</strong>'
            f'<nav aria-label="{esc(appname)}">{links}</nav></header><main><h1>{esc(title)}</h1>{alerts}{shown}{body}'
            f'</main></body></html>')


def table(label, head, rows, nums=()):
    if not rows:
        return f'<p class="muted">There are no records to be displayed.</p>'
    th = "".join(f'<th class="{"num" if i in nums else ""}">{esc(h)}</th>' for i, h in enumerate(head))
    body = ""
    for row in rows:
        body += "<tr>" + "".join(
            f'<td class="{"num" if i in nums else ""}">{c if isinstance(c, Raw) else esc(c)}</td>'
            for i, c in enumerate(row)) + "</tr>"
    return f'<table aria-label="{esc(label)}"><thead><tr>{th}</tr></thead><tbody>{body}</tbody></table>'


class Raw(str):
    """Markup that a table cell holds as it is (a link, a button)."""


def field(name, label, value="", kind="text", required=False, extra=""):
    fid = "f-" + name
    req = " required" if required else ""
    if kind == "textarea":
        control = f'<textarea id="{fid}" name="{name}" rows="3" cols="44"{req}>{esc(value)}</textarea>'
    else:
        control = f'<input id="{fid}" name="{name}" type="{kind}" value="{esc(value)}"{req}{extra}>'
    return f'<div class="field"><label for="{fid}">{esc(label)}</label> {control}</div>'


def select(name, label, options, value="", blank="-- Select --"):
    fid = "f-" + name
    opts = (f'<option value="">{esc(blank)}</option>' if blank is not None else "") + "".join(
        f'<option value="{esc(v)}"{" selected" if str(v) == str(value) else ""}>{esc(t)}</option>'
        for v, t in ((o, o) if not isinstance(o, tuple) else o for o in options))
    return f'<div class="field"><label for="{fid}">{esc(label)}</label> <select id="{fid}" name="{name}">{opts}</select></div>'


def form(action, label, inner, button, method="post"):
    return (f'<form method="{method}" action="{esc(action)}" aria-label="{esc(label)}">{inner}'
            f'<button type="submit">{esc(button)}</button></form>')


def link(text, href):
    return Raw(f'<a href="{esc(href)}">{esc(text)}</a>')


# --- responses -------------------------------------------------------------------------------------------------------

class Resp:
    def __init__(self, status=200, body="", ctype="text/html; charset=utf-8", headers=None):
        self.status, self.body, self.ctype, self.headers = status, body, ctype, dict(headers or {})


def ok(text, status=200):
    return Resp(status, text)


def see(location, cookie=None):
    headers = {"Location": location}
    if cookie is not None:
        headers["Set-Cookie"] = cookie
    return Resp(303, "", headers=headers)


def not_found(appname="CA office"):
    return Resp(404, page(appname, "Not found", "<p>There is no such page.</p>"))


def download(key):
    f = FILES.get(key)
    if f is None:
        return not_found()
    name = f["name"].replace('"', "")
    quoted = urllib.parse.quote(f["name"])
    return Resp(200, f["body"], f["type"], {
        "Content-Disposition": f"attachment; filename=\"{name}\"; filename*=UTF-8''{quoted}"})


class Req:
    def __init__(self, method, host, path, query, form_data, cookies):
        self.method, self.host, self.path = method, host, path
        self.parts = [urllib.parse.unquote(p) for p in path.strip("/").split("/") if p]
        self.query, self.form, self.cookies = query, form_data, cookies

    def q(self, name, default=""):
        return (self.query.get(name) or [default])[-1]

    def f(self, name, default=""):
        return str((self.form.get(name) or [default])[-1]).strip()

    def flist(self, name):
        return [str(v).strip() for v in self.form.get(name) or []]


# === ClientChat ======================================================================================================

def chat(req):
    A = "ClientChat"
    nav = [("Chats", "/")]
    p = req.parts
    if req.method == "GET" and not p:
        rows = []
        for t in recs("chat_threads"):
            msgs = where("chat_messages", thread=t["id"])
            last = msgs[-1] if msgs else {}
            rows.append([link(t.get("title") or t.get("client"), f"/thread/{t['id']}"),
                         (last.get("text") or (FILES.get(last.get("file", ""), {}).get("name", "")))[:60],
                         last.get("at", "")])
        return ok(page(A, "Chats", table("Chats", ["Chat", "Last message", "When"], rows), nav))
    if len(p) == 2 and p[0] == "files" and req.method == "GET":
        return download(p[1])
    if len(p) >= 2 and p[0] == "thread":
        t = find("chat_threads", p[1])
        if t is None:
            return not_found(A)
        errors = []
        if req.method == "POST" and p[2:] == ["send"]:
            text = req.f("message")
            if not text:
                errors.append("Type a message first.")
            elif len(text) > 4000:
                errors.append("A message is at most 4000 characters.")
            else:
                app()["sent"].append({"app": "chat", "thread": t["id"], "text": text, "at": app()["today"]})
                save_state()
                return see(f"/thread/{t['id']}?sent=1")
        if req.method == "GET" and p[2:] or req.method == "POST" and p[2:] != ["send"]:
            return not_found(A)
        items = []
        for m in where("chat_messages", thread=t["id"]):
            who = t.get("client") if m.get("sender") == "client" else "You (office)"
            body = esc(m.get("text", ""))
            if m.get("file"):
                f = FILES.get(m["file"])
                if f:
                    body += (f'<br>Document: {esc(f["name"])} '
                             f'<a href="/files/{esc(m["file"])}">Download {esc(f["name"])}</a>')
            items.append(f'<li><strong>{esc(who)}</strong> <span class="muted">{esc(m.get("at", ""))}</span><br>{body}</li>')
        for s in app()["sent"]:
            if s.get("app") == "chat" and s.get("thread") == t["id"]:
                items.append(f'<li><strong>You (office)</strong> <span class="muted">{esc(s["at"])}</span><br>'
                             f'{esc(s["text"])}</li>')
        body = (f'<ul aria-label="Messages">{"".join(items)}</ul>'
                + form(f"/thread/{t['id']}/send", "Message", field("message", "Message", kind="textarea"), "Send"))
        return ok(page(A, t.get("title") or t.get("client"), body, nav,
                       notes=["Sent." if req.q("sent") else ""], errors=errors), 400 if errors else 200)
    return not_found(A)


# === Mailbox =========================================================================================================

def mail(req):
    A = "Mailbox"
    nav = [("Inbox", "/")]
    p = req.parts
    if req.method != "GET":
        return not_found(A)
    if not p:
        rows = [[m.get("from", ""), m.get("label", ""), link(m.get("subject") or "(no subject)", f"/mail/{m['id']}"),
                 (m.get("body") or "")[:50], m.get("at", "")] for m in reversed(recs("mails"))]
        return ok(page(A, "Inbox", table("Inbox", ["From", "Label", "Subject", "Snippet", "Date"], rows), nav))
    if len(p) == 2 and p[0] == "mail":
        m = find("mails", p[1])
        if m is None:
            return not_found(A)
        keys = [k for k in str(m.get("files") or "").split(",") if k.strip()]
        att = "".join(f'<li>{esc(FILES[k]["name"])} <a href="/files/{esc(k)}">Download {esc(FILES[k]["name"])}</a></li>'
                      for k in keys if k in FILES)
        body = (f'<p>From: {esc(m.get("from"))} &lt;{esc(m.get("from_address"))}&gt;<br>Date: {esc(m.get("at"))}'
                f'<br>Label: {esc(m.get("label"))}</p><div class="box">{esc(m.get("body") or "").replace(chr(10), "<br>")}</div>'
                + (f'<h2>{len(keys)} attachments</h2><ul aria-label="Attachments">{att}</ul>' if keys else ""))
        return ok(page(A, m.get("subject") or "(no subject)", body, nav))
    if len(p) == 2 and p[0] == "files":
        return download(p[1])
    return not_found(A)


# === Bookwell ========================================================================================================

def company_period(c):
    return date.fromisoformat(c["period_from"]), date.fromisoformat(c["period_to"])


def book_date(text, c):
    """A voucher date. A full date is taken as written; a short one (2/10, 2-10) is the first day of that day and
    month in the company's period, as the books app has always done (and as can put it in the wrong year)."""
    s = str(text or "").strip()
    start, end = company_period(c)
    m = re.fullmatch(r"(\d{1,2})[-/.](\d{1,2})", s)
    if m:
        d, mo = int(m[1]), int(m[2])
        for y in range(start.year, end.year + 1):
            try:
                cand = date(y, mo, d)
            except ValueError:
                continue
            if start <= cand <= end:
                return cand.isoformat()
        raise Refused("Date is not a day in this company's period.")
    iso = iso_date(s, "Date")
    if not start <= date.fromisoformat(iso) <= end:
        raise Refused(f"Date {show_date(iso)} is outside this company's period "
                      f"({show_date(c['period_from'])} to {show_date(c['period_to'])}).")
    return iso


def ledgers_of(cid, under=None):
    return [l for l in where("ledgers", company=cid) if under is None or l.get("under") == under]


def books(req):
    A = "Bookwell"
    p = req.parts
    if req.method == "GET" and not p:
        c = find("companies", app().get("last_company"))
        return see(f"/company/{c['id']}" if c else "/companies")
    if p == ["companies"] and req.method == "GET":
        q = req.q("q").lower()
        rows = [[link(c["name"], f"/company/{c['id']}"), c.get("number", ""),
                 f"{show_date(c['period_from'])} to {show_date(c['period_to'])}"]
                for c in recs("companies") if q in c["name"].lower()]
        body = (form("/companies", "Filter companies", field("q", "Filter companies", req.q("q")), "Filter", "get")
                + table("List of companies", ["Name", "Number", "Period"], rows))
        return ok(page(A, "Select company", body, [("Companies", "/companies")]))
    if p == ["share"] and req.method == "GET":
        back = req.q("back")
        rows = []
        for f in sorted(recs("share_folders"), key=lambda r: r["path"]):
            n = len(where("share_files", folder=f["path"]))
            use = link(f"Use {f['path']}", f"{back}?folder={urllib.parse.quote(f['path'])}") if back.startswith("/company/") else ""
            rows.append([f["path"], str(n), use])
        return ok(page(A, "Office file share", table("Folders", ["Folder", "Files", ""], rows), [("Companies", "/companies")]))
    if len(p) >= 2 and p[0] == "company":
        c = find("companies", p[1])
        if c is None:
            return not_found(A)
        try:
            return company_page(req, c, p[2:])
        except Refused as r:
            return ok(page(A, "Not done", "", [("Back", f"/company/{c['id']}")], errors=r.problems), 400)
    return not_found(A)


def company_nav(c):
    b = f"/company/{c['id']}"
    return [("Gateway", b), ("Change company", "/companies"), ("Create voucher", b + "/vouchers/new"),
            ("Ledgers", b + "/ledgers"), ("Create ledger", b + "/ledgers/new"), ("Day book", b + "/daybook"),
            ("GSTR-1", b + "/gstr1")]


def months_of(c):
    start, end = company_period(c)
    out, y, m = [], start.year, start.month
    while (y, m) <= (end.year, end.month):
        out.append(f"{y}-{m:02d}")
        y, m = (y + 1, 1) if m == 12 else (y, m + 1)
    return out


def month_bounds(month):
    y, m = period_of(month)
    first = date(y, m, 1)
    last = (date(y + (m == 12), m % 12 + 1, 1) - timedelta(days=1))
    return first.isoformat(), last.isoformat()


def company_page(req, c, rest):
    A = f"Bookwell - {c['name']}"
    cid = c["id"]
    base = f"/company/{cid}"
    nav = company_nav(c)
    if not rest and req.method == "GET":
        if app().get("last_company") != cid:
            app()["last_company"] = cid
            save_state()
        vs = [v for v in where("vouchers", company=cid)]
        last = max((v["date"] for v in vs), default="")
        body = (f'<dl><dt>Name of company</dt><dd>{esc(c["name"])}</dd><dt>GSTIN</dt><dd>{esc(c.get("gstin"))}</dd>'
                f'<dt>Current period</dt><dd>{esc(show_date(c["period_from"]))} to {esc(show_date(c["period_to"]))}</dd>'
                f'<dt>Date of last entry</dt><dd>{esc(show_date(last)) or "none"}</dd></dl>')
        return ok(page(A, "Gateway", body, nav))
    if rest == ["ledgers"] and req.method == "GET":
        rows = [[l["name"], l.get("under", ""), l.get("gstin", ""), l.get("state", ""),
                 link(f"Ledger vouchers: {l['name']}", f"{base}/ledgers/{l['id']}/vouchers"),
                 link(f"Alter {l['name']}", f"{base}/ledgers/{l['id']}/edit")] for l in ledgers_of(cid)]
        return ok(page(A, "Ledgers", table("Ledgers", ["Name", "Under", "GSTIN", "State", "", ""], rows), nav))
    if rest[:1] == ["ledgers"] and (rest[1:] == ["new"] or len(rest) == 3 and rest[2] == "edit"):
        led = None if rest[1] == "new" else find("ledgers", rest[1])
        if rest[1] != "new" and (led is None or led.get("company") != cid):
            return not_found(A)
        action = f"{base}/ledgers/new" if led is None else f"{base}/ledgers/{led['id']}/edit"
        values = dict(led or {})
        errors = []
        if req.method == "POST":
            values = {k: req.f(k) for k in ("name", "under", "mailing_name", "address", "state", "pincode",
                                             "registration", "gstin")}
            values["address"] = str((req.form.get("address") or [""])[-1]).strip()
            try:
                saved = save_ledger(c, led, values)
                return see(f"{base}/ledgers?saved={urllib.parse.quote(saved['name'])}")
            except Refused as r:
                errors = r.problems
        elif req.method != "GET":
            return not_found(A)
        inner = (field("name", "Name", values.get("name", ""), required=True)
                 + select("under", "Under", GROUPS, values.get("under") or "Capital Account", blank=None)
                 + field("mailing_name", "Mailing name", values.get("mailing_name", ""))
                 + field("address", "Address", values.get("address", ""), kind="textarea")
                 + select("state", "State", STATE_OPTIONS, values.get("state", ""))
                 + field("pincode", "Pincode", values.get("pincode", ""))
                 + select("registration", "Registration type", REGISTRATIONS, values.get("registration") or "Unknown",
                          blank=None)
                 + field("gstin", "GSTIN/UIN", values.get("gstin", "")))
        title = "Ledger creation" if led is None else f"Ledger alteration: {led['name']}"
        return ok(page(A, title, form(action, title, inner, "Save ledger"), nav, errors=errors), 400 if errors else 200)
    if len(rest) == 3 and rest[0] == "ledgers" and rest[2] == "vouchers" and req.method == "GET":
        led = find("ledgers", rest[1])
        if led is None or led.get("company") != cid:
            return not_found(A)
        return ok(page(A, f"Ledger vouchers: {led['name']}", ledger_vouchers(c, led, req.q("month")), nav,
                       notes=["Saved." if req.q("saved") else ""]))
    if rest == ["daybook"] and req.method == "GET":
        month = req.q("month") or ""
        vs = sorted(where("vouchers", company=cid), key=lambda v: (v["date"], v.get("number", "")))
        if period_of(month):
            lo, hi = month_bounds(month)
            vs = [v for v in vs if lo <= v["date"] <= hi]
        rows = [[show_date(v["date"]), v.get("party", ""), v.get("type", ""),
                 link(v.get("number", ""), f"{base}/vouchers/{v['id']}"), money(v.get("total")),
                 "Cancelled" if v.get("cancelled") == "yes" else ""] for v in vs]
        pick = form(f"{base}/daybook", "Day book month",
                    select("month", "Month", [(m, period_words(m)) for m in months_of(c)], month, blank="All"), "Show", "get")
        return ok(page(A, "Day book", pick + table("Day book", ["Date", "Particulars", "Vch type", "Vch no.",
                                                                "Amount", ""], rows, nums=(4,)), nav))
    if rest[:1] == ["vouchers"]:
        return voucher_page(req, c, rest[1:], nav)
    if rest[:1] == ["gstr1"]:
        return books_gstr1(req, c, rest[1:], nav)
    return not_found(A)


def save_ledger(c, led, v):
    problems = []
    name = " ".join(v.get("name", "").split())
    if not name:
        problems.append("Name is required.")
    elif len(name) > 120:
        problems.append("Name is at most 120 characters.")
    elif any(l["name"].lower() == name.lower() and (led is None or l["id"] != led["id"]) for l in ledgers_of(c["id"])):
        problems.append(f"A ledger named {name} already exists in this company.")
    if v.get("under") not in GROUPS:
        problems.append("Under: choose a group from the list.")
    if v.get("registration") not in REGISTRATIONS:
        problems.append("Registration type: choose one from the list.")
    if v.get("state") and not state_code(v["state"]):
        problems.append("State: choose one from the list.")
    g = v.get("gstin", "").strip().upper()
    if g and not gstin_valid(g):
        problems.append(f"GSTIN/UIN {g} is not a valid GSTIN.")
    if g and v.get("registration") in ("Unregistered/Consumer", "Unknown"):
        problems.append("A ledger with a GSTIN is registered: choose Regular or Composition.")
    if v.get("pincode") and not re.fullmatch(r"\d{6}", v["pincode"]):
        problems.append("Pincode is six digits.")
    if problems:
        raise Refused(*problems)
    values = {"company": c["id"], "name": name, "under": v["under"], "mailing_name": v.get("mailing_name", ""),
              "address": v.get("address", ""), "state": v.get("state", ""), "pincode": v.get("pincode", ""),
              "registration": v["registration"], "gstin": g}
    if led is None:
        values.update(rate="", hsn="")
        rec = add("ledgers", values)
    else:
        led.update(values)
        rec = led
    save_state()
    return rec


def ledger_vouchers(c, led, month):
    cid = c["id"]
    months = months_of(c)
    if not period_of(month):
        month = f"{today().year}-{today().month:02d}" if f"{today().year}-{today().month:02d}" in months else months[-1]
    lo, hi = month_bounds(month)
    rows, dr, cr = [], Decimal(0), Decimal(0)
    for v in sorted(where("vouchers", company=cid), key=lambda v: (v["date"], v.get("number", ""))):
        if not lo <= v["date"] <= hi or v.get("cancelled") == "yes":
            continue
        debit = credit = ""
        if v.get("party") == led["name"]:
            debit = v.get("total")
        elif v.get("sales_ledger") == led["name"]:
            credit = v.get("taxable")
        else:
            for part in ("cgst", "sgst", "igst"):
                if dec(v.get(part)) and tax_ledger(v, part, c) == led["name"]:
                    credit = v.get(part)
        if not debit and not credit:
            continue
        dr += dec(debit)
        cr += dec(credit)
        other = v.get("sales_ledger") if debit else v.get("party")
        rows.append([show_date(v["date"]), other, v.get("type", ""), link(v.get("number", ""), f"/company/{cid}/vouchers/{v['id']}"),
                     money(debit) if debit else "", money(credit) if credit else ""])
    pick = form(f"/company/{cid}/ledgers/{led['id']}/vouchers", "Ledger vouchers month",
                select("month", "Month", [(m, period_words(m)) for m in months], month, blank=None), "Show", "get")
    totals = f'<p>Current total: debit {money(dr)}, credit {money(cr)} ({len(rows)} vouchers)</p>'
    return (pick + f'<p>Period: {esc(show_date(lo))} to {esc(show_date(hi))}</p>'
            + table(f"Ledger vouchers of {led['name']}", ["Date", "Particulars", "Vch type", "Vch no.", "Debit", "Credit"],
                    rows, nums=(4, 5)) + totals)


def tax_ledger(v, part, c):
    """The tax ledger a voucher's CGST/SGST/IGST amount went to: by its rate, read from the sales ledger."""
    sales = next((l for l in ledgers_of(c["id"], "Sales Accounts") if l["name"] == v.get("sales_ledger")), None)
    rate = Decimal(sales.get("rate") or "18") if sales else Decimal(18)
    half = rate / 2
    r = f"{(rate if part == 'igst' else half).normalize():f}"
    return f"{part.upper()}@{r}%"


def voucher_page(req, c, rest, nav):
    A = f"Bookwell - {c['name']}"
    cid = c["id"]
    base = f"/company/{cid}"
    if len(rest) == 1 and rest[0] != "new" and req.method == "GET":
        v = find("vouchers", rest[0])
        if v is None or v.get("company") != cid:
            return not_found(A)
        lines = [[v.get("party", ""), money(v.get("total")), ""], [v.get("sales_ledger", ""), "", money(v.get("taxable"))]]
        for part in ("cgst", "sgst", "igst"):
            if dec(v.get(part)):
                lines.append([tax_ledger(v, part, c), "", money(v.get(part))])
        body = (f'<p>Voucher type: {esc(v.get("type"))}. No.: {esc(v.get("number"))}. Date: {esc(show_date(v["date"]))}.'
                + (" Cancelled." if v.get("cancelled") == "yes" else "") + "</p>"
                + table("Voucher", ["Particulars", "Debit", "Credit"], lines, nums=(1, 2))
                + f'<p>Narration: {esc(v.get("narration"))}</p><p>{link("Alter voucher " + v.get("number", ""), base + "/vouchers/" + v["id"] + "/edit")}</p>')
        return ok(page(A, f"{v.get('type')} voucher {v.get('number')}", body, nav,
                       notes=["Voucher saved." if req.q("saved") else ""]))
    editing = len(rest) == 2 and rest[1] == "edit"
    if not (rest == ["new"] or editing):
        return not_found(A)
    v = find("vouchers", rest[0]) if editing else None
    if editing and (v is None or v.get("company") != cid):
        return not_found(A)
    values = dict(v or {"type": req.q("type") if req.q("type") in VOUCHER_TYPES else "Payment"})
    errors = []
    if req.method == "POST":
        values = {k: req.f(k) for k in ("type", "number", "date", "party", "sales_ledger", "taxable", "cgst", "sgst",
                                         "igst", "narration")}
        try:
            saved = save_voucher(c, v, values)
            return see(f"{base}/vouchers/{saved['id']}?saved=1")
        except Refused as r:
            errors = r.problems
    elif req.method != "GET":
        return not_found(A)
    shown_date = show_date(values.get("date")) if editing and req.method == "GET" else values.get("date", "")
    parties = [l["name"] for l in ledgers_of(cid, "Sundry Debtors")]
    sales = [l["name"] for l in ledgers_of(cid, "Sales Accounts")]
    inner = (select("type", "Voucher type", VOUCHER_TYPES, values.get("type"), blank=None)
             + f'<p class="muted">Mode: Accounting Invoice. Party A/c is debited the total; the sales ledger and the tax '
               f'ledgers are credited.</p>'
             + field("number", "Voucher No.", values.get("number", ""), required=True)
             + field("date", "Date (DD-MM-YYYY)", shown_date, required=True)
             + select("party", "Party A/c name", parties, values.get("party", ""))
             + f'<p><a href="{base}/ledgers/new">Create a ledger</a></p>'
             + select("sales_ledger", "Sales ledger", sales, values.get("sales_ledger", ""))
             + field("taxable", "Sales amount (taxable value)", values.get("taxable", ""))
             + field("cgst", "CGST amount", values.get("cgst", ""))
             + field("sgst", "SGST amount", values.get("sgst", ""))
             + field("igst", "IGST amount", values.get("igst", ""))
             + field("narration", "Narration", values.get("narration", "")))
    action = f"{base}/vouchers/new" if v is None else f"{base}/vouchers/{v['id']}/edit"
    title = "Accounting voucher creation" if v is None else f"Voucher alteration: {v.get('number')}"
    return ok(page(A, title, form(action, title, inner, "Save voucher"), nav, errors=errors), 400 if errors else 200)


def save_voucher(c, v, values):
    problems = []
    if values.get("type") not in VOUCHER_TYPES:
        problems.append("Voucher type: choose one from the list.")
    elif values["type"] != "Sales":
        problems.append(f"A {values['type']} voucher needs a cash or bank ledger; this form enters Sales vouchers. "
                        "Choose the voucher type Sales.")
    number = " ".join(values.get("number", "").split())
    if not number:
        problems.append("Voucher No. is required.")
    elif len(number) > 40:
        problems.append("Voucher No. is at most 40 characters.")
    when = ""
    try:
        when = book_date(values.get("date"), c)
    except Refused as r:
        problems += r.problems
    party = next((l for l in ledgers_of(c["id"], "Sundry Debtors") if l["name"] == values.get("party")), None)
    if party is None:
        problems.append("Party A/c name: choose a ledger under Sundry Debtors.")
    sales = next((l for l in ledgers_of(c["id"], "Sales Accounts") if l["name"] == values.get("sales_ledger")), None)
    if sales is None:
        problems.append("Sales ledger: choose a ledger under Sales Accounts.")
    amounts = {}
    for key, label, req_ in (("taxable", "Sales amount", True), ("cgst", "CGST amount", False),
                             ("sgst", "SGST amount", False), ("igst", "IGST amount", False)):
        try:
            amounts[key] = amount(values.get(key), label, req_) or "0.00"
        except Refused as r:
            problems += r.problems
    if not problems:
        if dec(amounts["taxable"]) <= 0:
            problems.append("Sales amount must be more than zero.")
        if dec(amounts["igst"]) and (dec(amounts["cgst"]) or dec(amounts["sgst"])):
            problems.append("A voucher has CGST and SGST, or IGST, not both.")
    if problems:
        raise Refused(*problems)
    total = dec(amounts["taxable"]) + dec(amounts["cgst"]) + dec(amounts["sgst"]) + dec(amounts["igst"])
    rec = {"company": c["id"], "type": "Sales", "number": number, "date": when, "party": party["name"],
           "party_gstin": party.get("gstin", ""), "sales_ledger": sales["name"], "taxable": amounts["taxable"],
           "cgst": amounts["cgst"], "sgst": amounts["sgst"], "igst": amounts["igst"], "total": str(total.quantize(CENT)),
           "narration": values.get("narration", ""), "cancelled": "no" if v is None else v.get("cancelled", "no")}
    if v is None:
        v = add("vouchers", rec)
    else:
        v.update(rec)
    save_state()
    return v


# --- Bookwell's GSTR-1 view and its JSON export ----------------------------------------------------------------------

def books_return(c, month):
    """The month's GSTR-1 from the company's Sales vouchers, in the offline JSON's shape, and its counts."""
    lo, hi = month_bounds(month)
    supplier = c["gstin"][:2]
    vs = sorted((v for v in where("vouchers", company=c["id"], type="Sales") if lo <= v["date"] <= hi),
                key=lambda v: (v["date"], v["number"]))
    led = {l["name"]: l for l in ledgers_of(c["id"])}
    b2b, b2cl, b2cs, hsn = {}, [], {}, {}
    live = [v for v in vs if v.get("cancelled") != "yes"]
    for v in live:
        party = led.get(v.get("party"), {})
        sales = led.get(v.get("sales_ledger"), {})
        rt = sales.get("rate") or "18"
        pos = state_code(party.get("state")) or (party.get("gstin") or "")[:2] or supplier
        y, m, d = v["date"].split("-")
        item = {"rt": rt, "txval": v["taxable"], "iamt": v["igst"], "camt": v["cgst"], "samt": v["sgst"], "csamt": "0.00"}
        tab = "B2C"
        if v.get("party_gstin"):
            tab = "B2B"
            b2b.setdefault(v["party_gstin"], []).append({"inum": v["number"], "idt": f"{d}-{m}-{y}", "val": v["total"],
                                                          "pos": pos, "itms": [item]})
        elif pos != supplier and dec(v["total"]) > B2CL_LIMIT:
            b2cl.append({"pos": pos, "inum": v["number"], "idt": f"{d}-{m}-{y}", "val": v["total"], "itms": [item]})
        else:
            key = (pos, rt)
            row = b2cs.setdefault(key, {"sply_ty": "INTRA" if pos == supplier else "INTER", "pos": pos, "rt": rt,
                                        "txval": Decimal(0), "iamt": Decimal(0), "camt": Decimal(0), "samt": Decimal(0)})
            for k in ("txval", "iamt", "camt", "samt"):
                row[k] += dec(item[k])
        code = sales.get("hsn") or ""
        h = hsn.setdefault((tab, code, rt), {"tab": tab, "hsn_sc": code, "uqc": "NA" if code.startswith("99") else "OTH",
                                             "qty": "0", "rt": rt, "txval": Decimal(0), "iamt": Decimal(0),
                                             "camt": Decimal(0), "samt": Decimal(0), "csamt": Decimal(0)})
        for k in ("txval", "iamt", "camt", "samt"):
            h[k] += dec(item[k])
    docs = []
    if vs:
        nums = [v["number"] for v in vs]
        def key(n):
            m = re.search(r"(\d+)\D*$", n)
            return (re.sub(r"\d+\D*$", "", n), int(m.group(1)) if m else 0)
        lo_n, hi_n = min(nums, key=key), max(nums, key=key)
        total = key(hi_n)[1] - key(lo_n)[1] + 1 if key(lo_n)[0] == key(hi_n)[0] else len(nums)
        cancelled = sum(1 for v in vs if v.get("cancelled") == "yes")
        docs.append({"num": 1, "from": lo_n, "to": hi_n, "totnum": total, "cancel": cancelled, "net_issue": total - cancelled})

    def s(x):
        return str(x.quantize(CENT)) if isinstance(x, Decimal) else x
    doc = {"gstin": c["gstin"], "fp": f"{month[5:7]}{month[:4]}", "version": "GST3.2",
           "b2b": [{"ctin": k, "inv": inv} for k, inv in b2b.items()],
           "b2cl": [{"pos": x["pos"], "inv": [x]} for x in b2cl],
           "b2cs": [{k: s(v) for k, v in row.items()} for row in b2cs.values()],
           "hsn": {"data": [{k: s(v) for k, v in row.items()} for row in hsn.values()]},
           "doc_issue": {"doc_det": [{"doc_num": 1, "docs": docs}] if docs else []}}
    counts = {"vouchers": len(vs), "cancelled": len(vs) - len(live), "b2b": sum(len(x) for x in b2b.values()),
              "b2cl": len(b2cl), "b2cs": len(b2cs), "hsn": len(hsn)}
    return doc, counts


def books_gstr1(req, c, rest, nav):
    A = f"Bookwell - {c['name']}"
    base = f"/company/{c['id']}/gstr1"
    month = req.q("month") or req.f("month")
    months = months_of(c)
    if not period_of(month) or month not in months:
        t = today()
        y, m = (t.year, t.month - 1) if t.month > 1 else (t.year - 1, 12)
        month = f"{y}-{m:02d}" if f"{y}-{m:02d}" in months else months[-1]
    doc, counts = books_return(c, month)
    if not rest and req.method == "GET":
        def tot(rows, k):
            return money(sum((dec(r.get(k)) for r in rows), Decimal(0)))
        b2b_inv = [i for x in doc["b2b"] for i in x["inv"]]
        b2b_items = [i["itms"][0] for i in b2b_inv]
        b2cl_items = [i["itms"][0] for x in doc["b2cl"] for i in x["inv"]]
        rows = [["B2B Invoices - 4A, 4B, 6B, 6C", str(len(b2b_inv)), tot(b2b_items, "txval"), tot(b2b_items, "iamt"),
                 tot(b2b_items, "camt"), tot(b2b_items, "samt")],
                ["B2C (Large) Invoices - 5", str(len(b2cl_items)), tot(b2cl_items, "txval"), tot(b2cl_items, "iamt"), "0.00", "0.00"],
                ["B2C (Small) Invoices - 7", str(len(doc["b2cs"])), tot(doc["b2cs"], "txval"), tot(doc["b2cs"], "iamt"),
                 tot(doc["b2cs"], "camt"), tot(doc["b2cs"], "samt")],
                ["HSN Summary - 12", str(len(doc["hsn"]["data"])), tot(doc["hsn"]["data"], "txval"),
                 tot(doc["hsn"]["data"], "iamt"), tot(doc["hsn"]["data"], "camt"), tot(doc["hsn"]["data"], "samt")]]
        docs = doc["doc_issue"]["doc_det"][0]["docs"] if doc["doc_issue"]["doc_det"] else []
        dl = "".join(f'<li>Invoices for outward supply: {esc(d["from"])} to {esc(d["to"])}, total {d["totnum"]}, '
                     f'cancelled {d["cancel"]}, net {d["net_issue"]}</li>' for d in docs)
        pick = form(base, "GSTR-1 period", select("month", "Period", [(m, period_words(m)) for m in months], month,
                                                  blank=None), "Show", "get")
        body = (pick + f'<p>GST registration: {esc(c["gstin"])}. Period: {esc(period_words(month))}.</p>'
                + f'<p>Total vouchers: {counts["vouchers"]}. Cancelled: {counts["cancelled"]}. Included in return: '
                  f'{counts["vouchers"] - counts["cancelled"]}. Uncertain transactions: 0.</p>'
                + table("Return view", ["Particulars", "Count", "Taxable amount", "IGST", "CGST", "SGST"], rows, nums=(1, 2, 3, 4, 5))
                + f'<h2>Document summary - 13</h2><ul>{dl or "<li>No documents</li>"}</ul>'
                + f'<p><a href="{base}/export?month={month}">Export GSTR-1 for {esc(period_words(month))}</a></p>')
        return ok(page(A, "GSTR-1", body, nav))
    if rest == ["export"]:
        folder = req.q("folder") or app().get("export_folder", "")
        errors, notes = [], []
        if req.method == "POST":
            folder = " ".join(req.f("folder").replace("\\", "/").strip("/ ").split())
            if not any(f["path"] == folder for f in recs("share_folders")):
                errors.append(f"There is no folder {folder or '(blank)'} on the office share. Browse the share to pick one.")
            elif counts["vouchers"] == 0:
                errors.append(f"There are no Sales vouchers in {period_words(month)} to export.")
            else:
                stamp = f"{today():%d%m%Y}_{str(app().get('now', '17:21:07')).replace(':', '')}"
                y, m = period_of(month)
                name = f"{c['gstin']}_GSTR1_{MONTHS[m - 1][:3].upper()}-{y}_{stamp}.json"
                content = json.dumps(doc, indent=1)
                old = next((f for f in where("share_files", folder=folder, name=name)), None)
                rec = {"folder": folder, "name": name, "gstin": c["gstin"], "period": doc["fp"], "content": content}
                if old:
                    old.update(rec)
                else:
                    add("share_files", rec)
                app()["export_folder"] = folder
                save_state()
                return see(f"{base}/export/done?month={month}&name={urllib.parse.quote(name)}"
                           f"&folder={urllib.parse.quote(folder)}")
        elif req.method != "GET":
            return not_found(A)
        inner = (f'<input type="hidden" name="month" value="{esc(month)}">'
                 f'<p>Exporting GSTR-1 for {esc(period_words(month))}: {counts["vouchers"]} documents.</p>'
                 '<p>File format: JSON (Data Interchange). Export separate files for each section: No.</p>'
                 + field("folder", "Folder path", folder))
        browse = f'<p><a href="/share?back={urllib.parse.quote(base + "/export")}%3Fmonth%3D{month}">Browse the office share</a></p>'
        return ok(page(A, "Export GSTR-1", form(f"{base}/export", "Export GSTR-1", inner, "Export") + browse, nav,
                       errors=errors), 400 if errors else 200)
    if rest == ["export", "done"] and req.method == "GET":
        f = next((x for x in where("share_files", folder=req.q("folder"), name=req.q("name"))), None)
        if f is None:
            return not_found(A)
        body = (f'<p class="note" role="status">Exported {counts["vouchers"]} of {counts["vouchers"]} documents.</p>'
                f'<p>File: {esc(f["folder"])}/{esc(f["name"])}</p>'
                f'<p><a href="/share/file/{esc(f["id"])}">Download {esc(f["name"])}</a></p>')
        return ok(page(A, "Exported", body, nav))
    return not_found(A)


# === FilePrep GST ====================================================================================================

def fp_key(client, period):
    return f"{client['id']}:{period}"


def fp_return(client, period):
    return app()["fp_data"].get(fp_key(client, period)) or {"b2b": [], "b2cl": [], "b2cs": [], "hsn": [], "docs": []}


def fileprep(req):
    A = "FilePrep GST"
    nav = [("Clients", "/")]
    p = req.parts
    if not p and req.method == "GET":
        q = req.q("q").lower()
        rows = [[c.get("id"), link(c["name"], f"/client/{c['id']}"), c.get("legal_name", ""), c.get("gstin", ""),
                 c.get("state", ""), c.get("registration", ""), c.get("group", "")]
                for c in recs("fp_clients") if q in (c["name"] + " " + c.get("legal_name", "") + " " + c.get("gstin", "")).lower()]
        body = (form("/", "Search clients", field("q", "Search", req.q("q")), "Search", "get")
                + table("Clients", ["File no", "Dealer name", "Legal name", "GSTIN", "State", "Registration type",
                                    "Handled by"], rows))
        return ok(page(A, "Clients", body, nav))
    if len(p) >= 2 and p[0] == "client":
        client = find("fp_clients", p[1])
        if client is None:
            return not_found(A)
        try:
            return fp_client(req, client, p[2:], nav + [(client["name"], f"/client/{client['id']}")])
        except Refused as r:
            return ok(page(A, "Not done", "", nav, errors=r.problems), 400)
    return not_found(A)


def fp_client(req, client, rest, nav):
    A = "FilePrep GST"
    base = f"/client/{client['id']}"
    if not rest and req.method == "GET":
        t = today()
        fys = sorted({fy_of(t.year, t.month), fy_of(t.year - 1, t.month)})
        inner = (select("fy", "Financial year", fys, fy_of(t.year, t.month), blank=None)
                 + select("month", "Return period", [(f"{i:02d}", MONTHS[i - 1]) for i in (4, 5, 6, 7, 8, 9, 10, 11, 12, 1, 2, 3)],
                          "", blank="-- Select --"))
        login = next((l for l in app()["tn_logins"] if l.get("gstin") == client.get("gstin")), None)
        # TaxNet is reached under this host's own domain (fileprep.<domain> -> taxnet.<domain>).
        tn_host = "taxnet." + (req.host.split(".", 1)[1] if "." in req.host else req.host)
        open_tn = (f'<p><a href="http://{esc(tn_host)}/login?user={urllib.parse.quote(login["user"])}&amp;via=fileprep">'
                   f'Open TaxNet as {esc(client["name"])}</a></p>') if login else ""
        body = (f'<p>{esc(client.get("legal_name"))}, GSTIN {esc(client.get("gstin"))}, {esc(client.get("state"))}, '
                f'{esc(client.get("registration"))}.</p>'
                + form(f"{base}/gstr1", "Select return period", inner, "OK", "get") + open_tn
                + f'<p><a href="{base}/receivers">Validate receiver GSTINs</a></p>')
        return ok(page(A, f"GST returns dashboard: {client['name']}", body, nav))
    if rest == ["gstr1"] and req.method == "GET":
        fy, month = req.q("fy"), req.q("month")
        m = re.fullmatch(r"(\d{4})-(\d{2})", fy or "")
        if not m or not re.fullmatch(r"\d{2}", month or "") or not 1 <= int(month) <= 12:
            raise Refused("Choose a financial year and a return period.")
        y = int(m[1]) if int(month) >= 4 else int(m[1]) + 1
        return see(f"{base}/gstr1/{y}-{month}")
    if rest == ["receivers"]:
        return fp_receivers(req, client, nav)
    if len(rest) >= 2 and rest[0] == "gstr1" and period_of(rest[1]):
        return fp_gstr1(req, client, rest[1], rest[2:], nav)
    return not_found(A)


def fp_receivers(req, client, nav):
    A = "FilePrep GST"
    base = f"/client/{client['id']}/receivers"
    notes, errors = [], []
    if req.method == "POST":
        chosen = set(req.flist("gstin"))
        action = req.f("action")
        mine = [r for r in where("fp_receivers", client=client["id"]) if r["gstin"] in chosen]
        if not mine:
            errors.append("Tick at least one receiver first.")
        elif action == "validate":
            for r in mine:
                reg = next((t for t in recs("tn_taxpayers") if t["gstin"] == r["gstin"]), None)
                if not gstin_valid(r["gstin"]) or reg is None:
                    r.update(status="Invalid", tn_legal_name="", tn_trade_name="")
                else:
                    r.update(status="Valid" if reg.get("status") == "Active" else "Cancelled",
                             tn_legal_name=reg.get("legal_name", ""), tn_trade_name=reg.get("trade_name", ""))
            save_state()
            return see(f"{base}?done=validated")
        elif action == "names":
            for r in mine:
                if r.get("tn_legal_name"):
                    r.update(legal_name=r["tn_legal_name"], trade_name=r.get("tn_trade_name") or r["tn_legal_name"])
            save_state()
            return see(f"{base}?done=names")
        else:
            errors.append("Choose Validate selected or Update names from TaxNet.")
    elif req.method != "GET":
        return not_found(A)
    if req.q("done") == "validated":
        notes.append("Validated against TaxNet.")
    if req.q("done") == "names":
        notes.append("Receiver names updated as per TaxNet.")
    status = req.q("status")
    rows = []
    for r in where("fp_receivers", client=client["id"]):
        if status and r.get("status") != status:
            continue
        box = Raw(f'<input type="checkbox" id="r-{esc(r["id"])}" name="gstin" value="{esc(r["gstin"])}">'
                  f'<label for="r-{esc(r["id"])}">Select {esc(r["gstin"])}</label>')
        rows.append([box, r.get("trade_name", ""), r.get("legal_name", ""), r["gstin"], r.get("tn_trade_name", ""),
                     r.get("tn_legal_name", ""), r.get("status", "")])
    filt = form(base, "Filter receivers", select("status", "Show", ["Valid", "Invalid", "Cancelled", "Not Validated"],
                                                  status, blank="All"), "Show", "get")
    grid = table("Receivers", ["Select", "Trade name as per software", "Legal name as per software", "GSTIN",
                               "Trade name as per TaxNet", "Legal name as per TaxNet", "Status"], rows)
    buttons = ('<button type="submit" name="action" value="validate">Validate selected</button>'
               '<button type="submit" name="action" value="names">Update names from TaxNet</button>')
    body = filt + f'<form method="post" action="{esc(base)}" aria-label="Receivers">{grid}{buttons}</form>'
    return ok(page(A, f"Validate receiver GSTINs: {client['name']}", body, nav, notes=notes, errors=errors),
              400 if errors else 200)


def import_rows(doc):
    """FilePrep's rows from a GSTR-1 JSON (TaxNet's row shapes, without the GSTIN and period)."""
    def amt(x):
        return amount(x, "An amount in the file", required=False) or "0.00"

    def idt(x):
        return iso_date(x, "An invoice date in the file")
    out = {"b2b": [], "b2cl": [], "b2cs": [], "hsn": [], "docs": []}
    for party in doc.get("b2b") or []:
        for inv in party.get("inv") or []:
            it = (inv.get("itms") or [{}])[0]
            out["b2b"].append({"ctin": str(party.get("ctin", "")).upper(), "inum": str(inv.get("inum", "")),
                               "idt": idt(inv.get("idt")), "val": amt(inv.get("val")), "pos": str(inv.get("pos", "")),
                               "rt": str(it.get("rt", "")), "txval": amt(it.get("txval")), "iamt": amt(it.get("iamt")),
                               "camt": amt(it.get("camt")), "samt": amt(it.get("samt"))})
    for group in doc.get("b2cl") or []:
        for inv in group.get("inv") or []:
            it = (inv.get("itms") or [{}])[0]
            out["b2cl"].append({"pos": str(group.get("pos", "")), "inum": str(inv.get("inum", "")), "idt": idt(inv.get("idt")),
                                "val": amt(inv.get("val")), "rt": str(it.get("rt", "")), "txval": amt(it.get("txval")),
                                "iamt": amt(it.get("iamt"))})
    for row in doc.get("b2cs") or []:
        out["b2cs"].append({"pos": str(row.get("pos", "")), "typ": str(row.get("sply_ty", "")), "rt": str(row.get("rt", "")),
                            "txval": amt(row.get("txval")), "iamt": amt(row.get("iamt")), "camt": amt(row.get("camt")),
                            "samt": amt(row.get("samt"))})
    for row in (doc.get("hsn") or {}).get("data") or []:
        code = str(row.get("hsn_sc", ""))
        out["hsn"].append({"tab": str(row.get("tab", "B2C")), "hsn": code,
                           "description": app()["hsn_master"].get(code, ""), "uqc": str(row.get("uqc", "NA")),
                           "qty": str(row.get("qty", "0")), "rt": str(row.get("rt", "")), "txval": amt(row.get("txval")),
                           "iamt": amt(row.get("iamt")), "camt": amt(row.get("camt")), "samt": amt(row.get("samt")),
                           "csamt": amt(row.get("csamt"))})
    for det in (doc.get("doc_issue") or {}).get("doc_det") or []:
        for d in det.get("docs") or []:
            out["docs"].append({"nature": "Invoices for outward supply", "from": str(d.get("from", "")),
                                "to": str(d.get("to", "")), "total": str(d.get("totnum", "")),
                                "cancelled": str(d.get("cancel", "")), "net": str(d.get("net_issue", ""))})
    return out


def fp_gstr1(req, client, period, rest, nav):
    A = "FilePrep GST"
    base = f"/client/{client['id']}/gstr1/{period}"
    nav = nav + [(f"GSTR-1 {period_words(period)}", base)]
    data = fp_return(client, period)
    notes, errors = [], []
    if not rest and req.method == "GET":
        def tot(rows, k):
            return money(sum((dec(r.get(k)) for r in rows), Decimal(0)))
        rows = []
        for label, key in (("B2B Invoices - 4A, 4B, 6B, 6C", "b2b"), ("B2C (Large) Invoices - 5A, 5B", "b2cl"),
                           ("B2C (Small) - 7", "b2cs"), ("HSN-wise Summary of Outward Supplies - 12", "hsn")):
            rs = data[key]
            rows.append([label, str(len(rs)), tot(rs, "val") if key in ("b2b", "b2cl") else "", tot(rs, "txval"),
                         tot(rs, "iamt"), tot(rs, "camt"), tot(rs, "samt")])
        rows.append(["Documents Issued - 13", str(len(data["docs"])), "", "", "", "", ""])
        uploads = [u for u in where("fp_uploads", client=client["id"], period=period)]
        status = (f"GSTR-1 last saved at TaxNet on {uploads[-1]['at']}" if uploads else "GSTR-1 not saved at TaxNet.")
        b2b = table("B2B invoices", ["Receiver GSTIN", "Invoice", "Date", "Value", "POS", "Rate", "Taxable", "IGST", "CGST", "SGST"],
                    [[r["ctin"], r["inum"], show_date(r["idt"]), money(r["val"]), r["pos"], r["rt"], money(r["txval"]),
                      money(r["iamt"]), money(r["camt"]), money(r["samt"])] for r in data["b2b"]], nums=(3, 6, 7, 8, 9))
        save = form(f"{base}/save", "Save to portal", "", "Save to portal")
        body = (f'<p>{esc(client["name"])}, GSTIN {esc(client["gstin"])}. Status: {esc(status)}</p>'
                + table("GSTR-1 summary", ["Particulars", "No. of records", "Invoice value", "Taxable value", "Integrated tax",
                                           "Central tax", "State/UT tax"], rows, nums=(1, 2, 3, 4, 5, 6))
                + f'<p><a href="{base}/import">Import from JSON</a> | <a href="{base}/hsn">HSN summary</a> | '
                  f'<a href="{base}/docs">Documents issued</a> | <a href="/client/{client["id"]}/receivers">Validate GSTIN</a> | '
                  f'<a href="{base}/history">Upload history</a></p>'
                + "<h2>B2B invoices</h2>" + b2b + save)
        return ok(page(A, f"Form GSTR-1: {period_words(period)}", body, nav,
                       notes=[{"imported": "Imported.", "saved": "GSTR-1 saved successfully at TaxNet."}.get(req.q("done"), "")]))
    if rest == ["import"]:
        folder = req.q("folder") or app().get("import_folder", "")
        if req.method == "POST":
            folder = " ".join(req.f("folder").replace("\\", "/").strip("/ ").split())
            name, mode = req.f("file"), req.f("mode") or "overwrite"
            f = next((x for x in where("share_files", folder=folder, name=name)), None)
            if f is None:
                errors.append("Choose a JSON file from the folder.")
            elif mode not in ("overwrite", "append"):
                errors.append("Choose Overwrite or Append.")
            else:
                try:
                    doc = json.loads(f.get("content") or "")
                    if not isinstance(doc, dict):
                        raise ValueError
                except ValueError:
                    doc = None
                    errors.append(f"{name} is not a GSTR-1 JSON file.")
                if doc is not None:
                    y, m = period_of(period)
                    if str(doc.get("gstin", "")).upper() != client["gstin"]:
                        errors.append(f"{name} is for GSTIN {doc.get('gstin')}, not {client['name']}'s ({client['gstin']}).")
                    elif str(doc.get("fp", "")) != f"{m:02d}{y}":
                        errors.append(f"{name} is for the period {doc.get('fp')}, not {m:02d}{y}.")
                    else:
                        try:
                            rows = import_rows(doc)
                        except Refused as r:
                            errors += r.problems
                        else:
                            if mode == "append":
                                rows = {k: data[k] + rows[k] for k in rows}
                            app()["fp_data"][fp_key(client, period)] = rows
                            known = {r["gstin"] for r in where("fp_receivers", client=client["id"])}
                            for ctin in dict.fromkeys(r["ctin"] for r in rows["b2b"]):
                                if ctin not in known:
                                    add("fp_receivers", {"client": client["id"], "gstin": ctin, "legal_name": "",
                                                         "trade_name": "", "status": "Not Validated", "tn_legal_name": "",
                                                         "tn_trade_name": ""})
                            app()["import_folder"] = folder
                            save_state()
                            return see(f"{base}?done=imported")
        elif req.method != "GET":
            return not_found(A)
        files = where("share_files", folder=folder)
        radios = "".join(f'<div><input type="radio" id="file-{esc(x["id"])}" name="file" value="{esc(x["name"])}">'
                         f'<label for="file-{esc(x["id"])}">{esc(x["name"])}</label></div>' for x in files)
        look = form(f"{base}/import", "Look in folder", field("folder", "Folder", folder), "Show files", "get")
        inner = (f'<input type="hidden" name="folder" value="{esc(folder)}">'
                 + (f'<fieldset><legend>Files in {esc(folder)}</legend>{radios}</fieldset>' if files
                    else f'<p class="muted">No JSON files in {esc(folder or "(no folder chosen)")}.</p>')
                 + '<fieldset><legend>Import mode</legend>'
                   '<div><input type="radio" id="mode-overwrite" name="mode" value="overwrite" checked><label for="mode-overwrite">Overwrite</label></div>'
                   '<div><input type="radio" id="mode-append" name="mode" value="append"><label for="mode-append">Append</label></div></fieldset>')
        return ok(page(A, "Import GSTR-1 from JSON", look + form(f"{base}/import", "Import", inner, "Import"), nav,
                       errors=errors), 400 if errors else 200)
    if rest == ["hsn"] and req.method == "GET":
        rows = [[r["tab"], r["hsn"], r.get("description", ""), r["uqc"], r["qty"], money(r["txval"]), r["rt"],
                 money(r["iamt"]), money(r["camt"]), money(r["samt"])] for r in data["hsn"]]
        return ok(page(A, "HSN summary of outward supplies", table("HSN summary", ["HSN type", "HSN/SAC", "Description",
                       "UQC", "Total quantity", "Total taxable value", "Rate (%)", "IGST", "CGST", "SGST"], rows,
                       nums=(5, 7, 8, 9)), nav))
    if rest == ["docs"] and req.method == "GET":
        rows = [[d["nature"], d["from"], d["to"], d["total"], d["cancelled"], d["net"]] for d in data["docs"]]
        return ok(page(A, "Documents issued - 13", table("Documents issued", ["Document type", "From serial no", "To serial no",
                       "Total no", "Cancelled", "Net issued"], rows), nav))
    if rest == ["history"] and req.method == "GET":
        rows = [[u["at"], "TaxNet", "SAVE", u["reference"], u["status"]] for u in where("fp_uploads", client=client["id"], period=period)]
        return ok(page(A, "Upload history", table("Upload history", ["Date", "Source", "Type", "Reference id", "Status"], rows), nav))
    if rest == ["save"] and req.method == "POST":
        problems = precheck(client, period, data)
        if problems:
            body = table("Validate and generate JSON", ["Section", "GSTIN", "Invoice", "Error description"], problems)
            return ok(page(A, "Pre-upload check: errors", body + f'<p><a href="{base}">Back to the return</a></p>', nav,
                           errors=[f"{len(problems)} error(s): nothing was saved at TaxNet."]), 400)
        ret = tn_return(client["gstin"], period, make=True)
        if ret.get("status") == "Filed":
            raise Refused("This return is already filed at TaxNet.")
        for table_name, key in (("tn_b2b", "b2b"), ("tn_b2cl", "b2cl"), ("tn_b2cs", "b2cs"), ("tn_hsn", "hsn"), ("tn_docs", "docs")):
            STATE["collections"][table_name] = [r for r in recs(table_name)
                                                if not (r.get("gstin") == client["gstin"] and r.get("period") == period)]
            for row in data[key]:
                add(table_name, dict(row, gstin=client["gstin"], period=period))
        ret.update(status="Not Filed", nil="no")
        n = len(where("fp_uploads", client=client["id"], period=period)) + 1
        ref = hashlib.sha256(f"{client['gstin']}:{period}:{n}".encode()).hexdigest()[:20].upper()
        add("fp_uploads", {"client": client["id"], "period": period, "reference": ref,
                           "at": f"{app()['today']} {app().get('now', '')}".strip(), "status": "Processed"})
        save_state()
        return see(f"{base}?done=saved")
    return not_found(A)


def precheck(client, period, data):
    """The pre-upload check: what TaxNet would refuse, as rows (section, GSTIN, invoice, error)."""
    out = []
    if not any(data[k] for k in data):
        return [["-", client["gstin"], "-", "Nothing to save: import the period's data first."]]
    status = {r["gstin"]: r.get("status", "Not Validated") for r in where("fp_receivers", client=client["id"])}
    seen = {}
    for r in data["b2b"]:
        seen[r["inum"]] = seen.get(r["inum"], 0) + 1
        st = status.get(r["ctin"], "Not Validated")
        if st != "Valid":
            words = {"Not Validated": "Receiver GSTIN is not validated.", "Cancelled": "Receiver GSTIN is cancelled.",
                     "Invalid": "Receiver GSTIN is invalid."}.get(st, "Receiver GSTIN is not validated.")
            out.append(["B2B", r["ctin"], r["inum"], words])
    for r in data["b2cl"]:
        seen[r["inum"]] = seen.get(r["inum"], 0) + 1
    for inum, n in seen.items():
        if n > 1:
            out.append(["B2B/B2CL", "", inum, f"Invoice {inum} appears {n} times in the return."])
    keys = {}
    for r in data["b2cs"]:
        k = (r["pos"], r["rt"])
        keys[k] = keys.get(k, 0) + 1
    for (pos, rt), n in keys.items():
        if n > 1:
            out.append(["B2CS", "", "", f"Place of supply {pos} at {rt}% appears {n} times."])
    return out


# === TaxNet ==========================================================================================================

def tn_return(gstin, period, make=False):
    r = next((x for x in where("tn_returns", gstin=gstin, period=period)), None)
    if r is None and make:
        r = add("tn_returns", {"gstin": gstin, "period": period, "status": "Not Filed", "nil": "no", "arn": "",
                               "filed_on": "", "signatory": "", "method": ""})
    return r


def taxpayer(gstin):
    return next((t for t in recs("tn_taxpayers") if t["gstin"] == gstin), {"gstin": gstin, "legal_name": gstin})


def tn_user(req):
    m = req.cookies.get("taxnet_session")
    return SESSIONS.get(m.value) if m is not None else None


def taxnet(req):
    A = "TaxNet"
    p = req.parts
    if p == ["login"]:
        errors = []
        user = req.q("user")
        pw = ""
        if req.method == "POST":
            user, pw = req.f("user"), req.f("password")
            login = next((l for l in app()["tn_logins"] if l.get("user") == user and l.get("password") == pw), None)
            if login is not None:
                sid = secrets.token_hex(16)
                SESSIONS[sid] = login["gstin"]
                return see("/", cookie=f"taxnet_session={sid}; Path=/; HttpOnly")
            errors.append("Invalid user name or password.")
        elif req.method != "GET":
            return not_found(A)
        elif req.q("via") == "fileprep":
            # The return-prep software holds the client's portal login and fills it in, as such software does.
            login = next((l for l in app()["tn_logins"] if l.get("user") == user), None)
            pw = login["password"] if login else ""
        inner = field("user", "User name", user) + field("password", "Password", pw, kind="password")
        return ok(page(A, "Sign in", form("/login", "Sign in", inner, "Sign in"), errors=errors), 400 if errors else 200)
    gstin = tn_user(req)
    if gstin is None:
        return see("/login")
    if p == ["logout"]:
        sid = req.cookies.get("taxnet_session")
        SESSIONS.pop(sid.value if sid else "", None)
        return see("/login", cookie="taxnet_session=; Path=/; Max-Age=0")
    nav = [("Dashboard", "/"), ("File returns", "/returns"), ("Sign out", "/logout")]
    try:
        return taxnet_signed_in(req, gstin, p, nav)
    except Refused as r:
        return ok(page(A, "Not done", "", nav, errors=r.problems), 400)


def taxnet_signed_in(req, gstin, p, nav):
    A = "TaxNet"
    tp = taxpayer(gstin)
    if not p and req.method == "GET":
        t = today()
        cols, y, m = [], t.year, t.month
        for _ in range(5):
            y, m = (y, m - 1) if m > 1 else (y - 1, 12)
            cols.insert(0, f"{y}-{m:02d}")
        cells = []
        for c in cols:
            r = tn_return(gstin, c)
            cells.append("Filed" if r and r.get("status") == "Filed" else "To be filed")
        cal = table("Returns calendar", ["Return"] + [period_words(c) for c in cols], [["GSTR-1"] + cells])
        led = next((x for x in where("tn_ledger", gstin=gstin)), {})
        ledger = table("Ledger balance", ["Ledger", "IGST", "CGST", "SGST", "CESS"],
                       [["Electronic credit ledger", money(led.get("igst")), money(led.get("cgst")), money(led.get("sgst")),
                         money(led.get("cess"))]], nums=(1, 2, 3, 4))
        body = (f'<p>Welcome {esc(tp.get("legal_name"))} (GSTIN {esc(gstin)}). Today is {esc(show_date(t.isoformat()))}.</p>'
                f'<h2>Returns calendar (last 5 return periods)</h2>{cal}<h2>Ledger balance</h2>{ledger}'
                f'<p><a href="/returns">File returns</a></p>')
        return ok(page(A, "Dashboard", body, nav))
    if p == ["returns"] and req.method == "GET":
        t = today()
        fy_now = fy_of(t.year, t.month)
        fy, q, month = req.q("fy") or fy_now, req.q("quarter") or quarter_of(t.month), req.q("period")
        start = int(fy[:4]) if re.fullmatch(r"\d{4}-\d{2}", fy) else t.year
        fys = [fy_of(t.year - 1, t.month), fy_now]
        months = QUARTERS.get(q, QUARTERS["Q1"])[1]
        inner = (select("fy", "Financial year", fys, fy, blank=None)
                 + select("quarter", "Quarter", [(k, v[0]) for k, v in QUARTERS.items()], q, blank=None)
                 + select("period", "Period", [(f"{mm:02d}", MONTHS[mm - 1]) for mm in months], month, blank="Select"))
        body = form("/returns", "File returns", inner, "Search", "get")
        errors = []
        if req.q("period"):
            if q not in QUARTERS or not re.fullmatch(r"\d{2}", month) or int(month) not in months:
                errors.append("The period is not a month of the quarter chosen.")
            else:
                mm = int(month)
                y = start if mm >= 4 else start + 1
                per = f"{y}-{mm:02d}"
                if (y, mm) >= (t.year, t.month):
                    errors.append(f"The return period {period_words(per)} has not ended yet.")
                else:
                    r = tn_return(gstin, per)
                    body += ('<p>You have selected to file the return on monthly frequency.</p><div class="tiles"><div class="tile">'
                             f'<strong>GSTR-1</strong><br>Details of outward supplies of goods or services<br>'
                             f'Period: {esc(period_words(per))}<br>Due date: {esc(show_date(due_date(per)))}<br>'
                             f'Status: {esc(r["status"] if r else "Not Filed")}<br>'
                             f'<a href="/gstr1/{per}">Prepare online: GSTR-1 {esc(period_words(per))}</a></div></div>')
        return ok(page(A, "File returns", body, nav, errors=errors), 400 if errors else 200)
    if len(p) >= 2 and p[0] == "gstr1" and period_of(p[1]):
        y, m = period_of(p[1])
        t = today()
        if (y, m) >= (t.year, t.month):
            raise Refused(f"The return period {period_words(p[1])} has not ended yet.")
        return gstr1(req, gstin, p[1], p[2:], nav)
    return not_found(A)


SECTIONS = {
    "b2b": ("4A, 4B, 6B, 6C - B2B, SEZ, DE Invoices", "tn_b2b"),
    "b2cl": ("5 - B2C (Large) Invoices", "tn_b2cl"),
    "b2cs": ("7 - B2C (Others)", "tn_b2cs"),
    "hsn": ("12 - HSN-wise summary of outward supplies", "tn_hsn"),
    "docs": ("13 - Documents Issued", "tn_docs"),
}
EMPTY_TILES = ["6A - Exports Invoices", "8A, 8B, 8C, 8D - Nil Rated Supplies", "9B - Credit / Debit Notes (Registered)",
               "9B - Credit / Debit Notes (Unregistered)", "11A(1), 11A(2) - Tax Liability (Advances Received)",
               "11B(1), 11B(2) - Adjustment of Advances", "14 - Supplies made through ECO"]


def rows_of(table_name, gstin, period):
    return where(table_name, gstin=gstin, period=period)


def editable(gstin, period):
    r = tn_return(gstin, period)
    if r and r.get("status") == "Filed":
        raise Refused("This return is filed; it cannot be changed.")


def changed(gstin, period):
    r = tn_return(gstin, period, make=True)
    if r.get("status") != "Filed":
        r["status"] = "Not Filed"
    return r


def gstr1(req, gstin, period, rest, nav):
    A = "TaxNet"
    base = f"/gstr1/{period}"
    tp = taxpayer(gstin)
    r = tn_return(gstin, period)
    status = r["status"] if r else "Not Filed"
    head = (f'<p>GSTIN {esc(gstin)}. Legal name {esc(tp.get("legal_name"))}. Trade name {esc(tp.get("trade_name"))}. '
            f'FY {esc(fy_of(*period_of(period)))}. Tax period {esc(period_words(period))}. Status: {esc(status)}. '
            f'Due date {esc(show_date(due_date(period)))}.</p>')
    nav = nav + [(f"GSTR-1 {period_words(period)}", base)]
    if not rest and req.method == "GET":
        tiles = ""
        for key, (title, tname) in SECTIONS.items():
            tiles += (f'<div class="tile"><a href="{base}/{key}">{esc(title)}</a><br>Records: '
                      f'{len(rows_of(tname, gstin, period))}</div>')
        for title in EMPTY_TILES:
            tiles += f'<div class="tile">{esc(title)}<br>Records: 0</div>'
        nil = r is not None and r.get("nil") == "yes"
        nilform = form(f"{base}/nil", "File Nil GSTR-1",
                       f'<input type="checkbox" id="f-nil" name="nil" value="yes"{" checked" if nil else ""}>'
                       '<label for="f-nil">File Nil GSTR-1</label> ', "Save nil choice")
        done = ""
        if r and r.get("status") == "Filed":
            done = (f'<p class="note" role="status">GSTR-1 for {esc(period_words(period))} has been filed successfully. '
                    f'The acknowledgement reference number (ARN) is {esc(r["arn"])}.</p>')
        body = (head + done + nilform + f'<h2>Add record details</h2><div class="tiles">{tiles}</div>'
                + form(f"{base}/generate", "Generate summary", "", "Generate summary")
                + f'<p><a href="{base}/summary">Proceed to file / summary</a></p>')
        return ok(page(A, "GSTR-1 - Details of outward supplies of goods or services", body, nav,
                       notes=[{"generated": "Your Generate Summary request has been processed. Status: Ready to File."}
                              .get(req.q("done"), "")]))
    if rest == ["nil"] and req.method == "POST":
        editable(gstin, period)
        want = "yes" if req.f("nil") == "yes" else "no"
        if want == "yes" and any(rows_of(t, gstin, period) for _, t in SECTIONS.values()):
            raise Refused("A nil return cannot have records: remove them first.")
        ret = changed(gstin, period)
        ret["nil"] = want
        save_state()
        return see(base)
    if rest == ["generate"] and req.method == "POST":
        editable(gstin, period)
        has_rows = any(rows_of(t, gstin, period) for _, t in SECTIONS.values())
        if not has_rows and not (r and r.get("nil") == "yes"):
            raise Refused("Nothing to summarise: add records, or tick File Nil GSTR-1.")
        ret = changed(gstin, period)
        ret["status"] = "Ready to File"
        save_state()
        return see(f"{base}?done=generated")
    if rest == ["summary"] and req.method == "GET":
        return ok(page(A, "GSTR-1 summary", head + summary_html(gstin, period)
                       + f'<p><a href="{base}/file">File statement</a></p>', nav))
    if rest[:1] == ["file"] or rest in (["file-evc"], ["file-dsc"], ["verify"], ["otp"]):
        return file_return(req, gstin, period, rest, nav, head, r)
    if rest and rest[0] in SECTIONS:
        return section(req, gstin, period, rest, nav, head)
    return not_found(A)


def summary_html(gstin, period):
    def tot(rows, k):
        return money(sum((dec(x.get(k)) for x in rows), Decimal(0)))
    lines = []
    for key, (title, tname) in SECTIONS.items():
        rs = rows_of(tname, gstin, period)
        if key == "docs":
            lines.append([title, str(len(rs)), "Net issued documents", str(sum(int(x.get("net") or 0) for x in rs)), "", "", "", ""])
            continue
        if key == "hsn":
            for tab in ("B2B", "B2C"):
                sub = [x for x in rs if x.get("tab") == tab]
                lines.append([f"{title} - {tab} total", str(len(sub)), "", tot(sub, "txval"), tot(sub, "iamt"),
                              tot(sub, "camt"), tot(sub, "samt"), tot(sub, "csamt")])
            continue
        lines.append([title, str(len(rs)), "Invoice" if key in ("b2b", "b2cl") else "Net value", tot(rs, "txval"),
                      tot(rs, "iamt"), tot(rs, "camt"), tot(rs, "samt"), "0.00"])
    liable = [x for t in ("tn_b2b", "tn_b2cl", "tn_b2cs") for x in rows_of(t, gstin, period)]
    lines.append(["Total liability (outward supplies other than reverse charge)", "", "", tot(liable, "txval"),
                  tot(liable, "iamt"), tot(liable, "camt"), tot(liable, "samt"), "0.00"])
    return table("Consolidated summary", ["Description", "No. of records", "Document type", "Value", "Integrated tax",
                                          "Central tax", "State/UT tax", "Cess"], lines, nums=(3, 4, 5, 6, 7))


def section(req, gstin, period, rest, nav, head):
    A = "TaxNet"
    key = rest[0]
    title, tname = SECTIONS[key]
    base = f"/gstr1/{period}/{key}"
    errors, values = [], {}
    tab = (req.q("tab") or req.f("tab") or "B2B").upper() if key == "hsn" else ""
    if tab and tab not in ("B2B", "B2C"):
        tab = "B2B"
    if len(rest) == 3 and rest[2] == "remove" and req.method == "POST":
        editable(gstin, period)
        row = find(tname, rest[1])
        if row is None or row.get("gstin") != gstin or row.get("period") != period:
            return not_found(A)
        STATE["collections"][tname] = [x for x in recs(tname) if x is not row]
        changed(gstin, period)
        save_state()
        return see(base + (f"?tab={tab}" if tab else ""))
    if len(rest) != 1:
        return not_found(A)
    if req.method == "POST":
        fields = ("ctin", "inum", "idt", "val", "pos", "rt", "txval", "iamt", "camt", "samt", "csamt", "hsn", "uqc",
                  "qty", "from", "to", "total", "cancelled")
        values = {k: req.f(k) for k in fields}
        try:
            editable(gstin, period)
            ret = tn_return(gstin, period)
            if ret and ret.get("nil") == "yes":
                raise Refused("This return is marked nil: untick File Nil GSTR-1 first.")
            row = build_row(key, gstin, period, values, tab)
            add(tname, dict(row, gstin=gstin, period=period))
            changed(gstin, period)
            save_state()
            return see(base + (f"?tab={tab}&saved=1" if tab else "?saved=1"))
        except Refused as r:
            errors = r.problems
    elif req.method != "GET":
        return not_found(A)
    rows = [x for x in rows_of(tname, gstin, period) if not tab or x.get("tab") == tab]
    remove = lambda x, i: Raw(form(f"{base}/{x['id']}/remove" + (f"?tab={tab}" if tab else ""), f"Remove row {i}", "",
                                   f"Remove row {i}"))
    pos_opts = [(c, f"{c}-{n}") for c, n in STATES.items()]
    if key == "b2b":
        grid = table("B2B invoices", ["Recipient GSTIN", "Invoice no.", "Date", "Value", "POS", "Rate", "Taxable", "IGST", "CGST",
                                      "SGST", ""],
                     [[x["ctin"], x["inum"], show_date(x["idt"]), money(x["val"]), x["pos"], x["rt"], money(x["txval"]),
                       money(x["iamt"]), money(x["camt"]), money(x["samt"]), remove(x, i)] for i, x in enumerate(rows, 1)],
                     nums=(3, 6, 7, 8, 9))
        inner = (field("ctin", "Recipient GSTIN", values.get("ctin", "")) + field("inum", "Invoice number", values.get("inum", ""))
                 + field("idt", "Invoice date", values.get("idt", ""), kind="date") + field("val", "Total invoice value", values.get("val", ""))
                 + select("pos", "Place of supply", pos_opts, values.get("pos", "")) + select("rt", "Rate (%)", RATES, values.get("rt", ""))
                 + field("txval", "Taxable value", values.get("txval", ""))
                 + '<p class="muted">Leave the taxes blank to have them worked out from the taxable value and rate.</p>'
                 + field("iamt", "Integrated tax", values.get("iamt", "")) + field("camt", "Central tax", values.get("camt", ""))
                 + field("samt", "State/UT tax", values.get("samt", "")))
    elif key == "b2cl":
        grid = table("B2C Large invoices", ["POS", "Invoice no.", "Date", "Value", "Rate", "Taxable", "IGST", ""],
                     [[x["pos"], x["inum"], show_date(x["idt"]), money(x["val"]), x["rt"], money(x["txval"]), money(x["iamt"]),
                       remove(x, i)] for i, x in enumerate(rows, 1)], nums=(3, 5, 6))
        inner = (select("pos", "Place of supply", pos_opts, values.get("pos", "")) + field("inum", "Invoice number", values.get("inum", ""))
                 + field("idt", "Invoice date", values.get("idt", ""), kind="date") + field("val", "Total invoice value", values.get("val", ""))
                 + select("rt", "Rate (%)", RATES, values.get("rt", "")) + field("txval", "Taxable value", values.get("txval", ""))
                 + field("iamt", "Integrated tax", values.get("iamt", "")))
    elif key == "b2cs":
        grid = table("B2C Others", ["POS", "Supply type", "Rate", "Taxable", "IGST", "CGST", "SGST", ""],
                     [[x["pos"], x["typ"], x["rt"], money(x["txval"]), money(x["iamt"]), money(x["camt"]), money(x["samt"]),
                       remove(x, i)] for i, x in enumerate(rows, 1)], nums=(3, 4, 5, 6))
        inner = (select("pos", "Place of supply", pos_opts, values.get("pos", "")) + select("rt", "Rate (%)", RATES, values.get("rt", ""))
                 + field("txval", "Taxable value", values.get("txval", ""))
                 + '<p class="muted">The supply type and the taxes are worked out from the place of supply, when left blank.</p>'
                 + field("iamt", "Integrated tax", values.get("iamt", "")) + field("camt", "Central tax", values.get("camt", ""))
                 + field("samt", "State/UT tax", values.get("samt", "")))
    elif key == "hsn":
        grid = table(f"HSN summary {tab}", ["HSN", "Description", "UQC", "Quantity", "Taxable", "Rate", "IGST", "CGST", "SGST", "Cess", ""],
                     [[x["hsn"], x.get("description", ""), x["uqc"], x["qty"], money(x["txval"]), x["rt"], money(x["iamt"]),
                       money(x["camt"]), money(x["samt"]), money(x["csamt"]), remove(x, i)] for i, x in enumerate(rows, 1)],
                     nums=(4, 6, 7, 8, 9))
        tabs = f'<p>Tabs: <a href="{base}?tab=B2B">B2B supplies</a> | <a href="{base}?tab=B2C">B2C supplies</a>. Showing: {tab} supplies.</p>'
        grid = tabs + grid
        inner = (f'<input type="hidden" name="tab" value="{tab}">' + field("hsn", "HSN or SAC code", values.get("hsn", ""))
                 + select("uqc", "UQC", UQCS, values.get("uqc", "")) + field("qty", "Total quantity", values.get("qty", "0"))
                 + field("txval", "Total taxable value", values.get("txval", "")) + select("rt", "Rate (%)", RATES, values.get("rt", ""))
                 + field("iamt", "Integrated tax", values.get("iamt", "")) + field("camt", "Central tax", values.get("camt", ""))
                 + field("samt", "State/UT tax", values.get("samt", "")) + field("csamt", "Cess", values.get("csamt", "0")))
    else:
        grid = table("Documents issued", ["Nature of document", "From", "To", "Total number", "Cancelled", "Net issued", ""],
                     [[x["nature"], x["from"], x["to"], x["total"], x["cancelled"], x["net"], remove(x, i)]
                      for i, x in enumerate(rows, 1)])
        inner = ('<p>1. Invoices for outward supply</p>' + field("from", "Sr. no. from", values.get("from", ""))
                 + field("to", "Sr. no. to", values.get("to", "")) + field("total", "Total number", values.get("total", ""))
                 + field("cancelled", "Cancelled", values.get("cancelled", "0")))
    action = base + (f"?tab={tab}" if tab else "")
    body = head + grid + "<h2>Add details</h2>" + form(action, f"Add {title}", inner, "Save" if key != "hsn" else "Add")
    return ok(page(A, title, body, nav, notes=["Saved." if req.q("saved") else ""], errors=errors), 400 if errors else 200)


def build_row(key, gstin, period, v, tab):
    """A table row from a form, checked as the portal checks it."""
    supplier = gstin[:2]
    problems = []

    def money_of(name, label, required=True):
        try:
            return amount(v.get(name), label, required)
        except Refused as r:
            problems.extend(r.problems)
            return ""
    if key in ("b2b", "b2cl", "b2cs"):
        pos = state_code(v.get("pos"))
        if not pos:
            problems.append("Place of supply: choose a state.")
        rt = v.get("rt")
        if rt not in RATES:
            problems.append("Rate: choose one from the list.")
        txval = money_of("txval", "Taxable value")
        iamt, camt, samt = (money_of(k, label, False) for k, label in
                            (("iamt", "Integrated tax"), ("camt", "Central tax"), ("samt", "State/UT tax")))
        if problems:
            raise Refused(*problems)
        inter = pos != supplier
        if inter:
            iamt = iamt or tax_of(txval, rt, False)
            if dec(camt) or dec(samt):
                problems.append("An inter-state supply has integrated tax only, no central or state tax.")
            camt = samt = "0.00"
            want = Decimal(tax_of(txval, rt, False))
            if abs(dec(iamt) - want) > 1:
                problems.append(f"Integrated tax should be {money(want)} at {rt}% of {money(txval)}.")
        else:
            camt = camt or tax_of(txval, rt, True)
            samt = samt or tax_of(txval, rt, True)
            if dec(iamt):
                problems.append("An intra-state supply has central and state tax, no integrated tax.")
            iamt = "0.00"
            want = Decimal(tax_of(txval, rt, True))
            if abs(dec(camt) - want) > 1 or abs(dec(samt) - want) > 1:
                problems.append(f"Central and state tax should each be {money(want)} at {rt}% of {money(txval)}.")
        row = {"pos": pos, "rt": rt, "txval": txval, "iamt": iamt, "camt": camt, "samt": samt}
        if key == "b2cs":
            if any(x["pos"] == pos and x["rt"] == rt for x in rows_of("tn_b2cs", gstin, period)):
                problems.append(f"A row for place of supply {pos} at {rt}% exists: remove it and add the total again.")
            row = {"pos": pos, "typ": "INTER" if inter else "INTRA", "rt": rt, "txval": txval, "iamt": iamt,
                   "camt": camt, "samt": samt}
        else:
            inum = " ".join(v.get("inum", "").split())
            if not inum or len(inum) > 16:
                problems.append("Invoice number is required (at most 16 characters).")
            idt = ""
            try:
                idt = iso_date(v.get("idt"), "Invoice date")
                y, m = period_of(period)
                if date.fromisoformat(idt) > date(y + (m == 12), m % 12 + 1, 1) - timedelta(days=1):
                    problems.append("Invoice date cannot be after the end of the tax period.")
            except Refused as r:
                problems += r.problems
            val = money_of("val", "Total invoice value")
            if any(x["inum"] == inum for t in ("tn_b2b", "tn_b2cl") for x in rows_of(t, gstin, period)):
                problems.append(f"Invoice {inum} is already in this return.")
            if key == "b2b":
                ctin = v.get("ctin", "").strip().upper()
                if not gstin_valid(ctin):
                    problems.append("Recipient GSTIN is not a valid GSTIN.")
                elif ctin == gstin:
                    problems.append("The recipient cannot be the supplier.")
                row = dict(ctin=ctin, inum=inum, idt=idt, val=val, **row)
            else:
                if not inter:
                    problems.append("B2C (Large) is for inter-state supplies only: an intra-state one goes in Table 7.")
                if val and dec(val) <= B2CL_LIMIT:
                    problems.append("B2C (Large) is for invoices of more than 1,00,000: a smaller one goes in Table 7.")
                row = {"pos": pos, "inum": inum, "idt": idt, "val": val, "rt": rt, "txval": txval, "iamt": iamt}
    elif key == "hsn":
        code = v.get("hsn", "").strip()
        desc = app()["hsn_master"].get(code)
        if desc is None:
            problems.append("HSN or SAC code: choose a code from the HSN list.")
        uqc = v.get("uqc")
        if uqc not in UQCS:
            problems.append("UQC: choose one from the list.")
        qty = v.get("qty") or "0"
        if not re.fullmatch(r"\d{1,9}(\.\d{1,3})?", qty):
            problems.append("Total quantity must be a number.")
        rt = v.get("rt")
        if rt not in RATES:
            problems.append("Rate: choose one from the list.")
        vals = {k: money_of(k, label, k == "txval") or "0.00" for k, label in
                (("txval", "Total taxable value"), ("iamt", "Integrated tax"), ("camt", "Central tax"),
                 ("samt", "State/UT tax"), ("csamt", "Cess"))}
        if not problems and any(x["hsn"] == code and x["rt"] == rt and x.get("tab") == tab for x in rows_of("tn_hsn", gstin, period)):
            problems.append(f"HSN {code} at {rt}% is already in the {tab} tab.")
        row = dict(tab=tab, hsn=code, description=desc or "", uqc=uqc, qty=qty, rt=rt, **vals)
    else:
        nums = {}
        for k, label in (("total", "Total number"), ("cancelled", "Cancelled")):
            s = v.get(k, "").strip() or ("0" if k == "cancelled" else "")
            if not re.fullmatch(r"\d{1,6}", s):
                problems.append(f"{label} must be a whole number.")
            else:
                nums[k] = int(s)
        frm, to = v.get("from", "").strip(), v.get("to", "").strip()
        if not frm or not to or len(frm) > 16 or len(to) > 16:
            problems.append("Sr. no. from and Sr. no. to are required (at most 16 characters).")
        if len(nums) == 2 and nums["cancelled"] > nums["total"]:
            problems.append("Cancelled cannot be more than the total number.")
        row = {"nature": "Invoices for outward supply", "from": frm, "to": to, "total": str(nums.get("total", "")),
               "cancelled": str(nums.get("cancelled", "")), "net": str(nums.get("total", 0) - nums.get("cancelled", 0))}
    if problems:
        raise Refused(*problems)
    return row


def file_return(req, gstin, period, rest, nav, head, r):
    A = "TaxNet"
    base = f"/gstr1/{period}"
    names = app()["signatories"].get(gstin) or []
    errors = []
    if rest == ["file"] and req.method == "GET":
        if not r or r.get("status") != "Ready to File":
            raise Refused("Generate the summary first: the return is not ready to file.")
        inner = ('<input type="checkbox" id="f-declare" name="declare" value="yes">'
                 '<label for="f-declare">I/We hereby solemnly affirm and declare that the information given herein above '
                 'is true and correct to the best of my/our knowledge and belief and nothing has been concealed therefrom.</label>'
                 + select("signatory", "Authorised signatory", names, ""))
        body = (head + f'<form method="post" action="{base}/file-evc" aria-label="Returns filing for GSTR-1">{inner}'
                '<button type="submit" formaction="' + base + '/file-dsc">File with DSC</button>'
                '<button type="submit">File with EVC</button></form>')
        return ok(page(A, "Returns filing for GSTR-1", body, nav))
    if rest == ["file-dsc"] and req.method == "POST":
        raise Refused("No digital signature certificate is registered for this taxpayer: file with EVC.")
    if rest == ["file-evc"] and req.method == "POST":
        if not r or r.get("status") != "Ready to File":
            raise Refused("Generate the summary first: the return is not ready to file.")
        if req.f("declare") != "yes":
            errors.append("Tick the declaration first.")
        if req.f("signatory") not in names:
            errors.append("Choose the authorised signatory.")
        if errors:
            raise Refused(*errors)
        r["signatory"] = req.f("signatory")
        save_state()
        return see(f"{base}/otp")
    if rest == ["otp"] and req.method == "GET":
        if not r or r.get("status") != "Ready to File" or not r.get("signatory"):
            raise Refused("Start the filing from File statement.")
        return ok(page(A, "Validate one-time password", head + otp_form(gstin, period), nav,
                       errors=["The OTP is not valid. Ask the taxpayer for the OTP sent to their phone."] if req.q("wrong") else []))
    if rest == ["verify"] and req.method == "POST":
        if not r or r.get("status") != "Ready to File" or not r.get("signatory"):
            raise Refused("Start the filing from File statement.")
        want = str(app()["otps"].get(f"{gstin}:{period}") or "")
        if not want or req.f("otp") != want:
            return see(f"{base}/otp?wrong=1")
        r.update(status="Filed", arn=arn_of(gstin, period), filed_on=app()["today"], method="EVC")
        save_state()
        return see(base)
    return not_found(A)


def otp_form(gstin, period):
    mobile = app()["mobiles"].get(gstin) or "98xxxxxx10"
    inner = (f'<p>A one-time password (OTP) has been sent to the registered mobile number {esc(mobile)} of the authorised '
             'signatory. It is valid for 10 minutes.</p>'
             + field("otp", "Enter the one-time password (OTP)", "", extra=' inputmode="numeric" autocomplete="off"'))
    return form(f"/gstr1/{period}/verify", "Validate OTP", inner, "Verify")


# === server ==========================================================================================================

def index(req):
    links = "".join(f"<li>{esc(n)}: <code>{esc(h)}</code></li>" for n, h in (
        ("ClientChat", "chat.*"), ("Mailbox", "mail.*"), ("Bookwell", "books.*"), ("FilePrep GST", "fileprep.*"),
        ("TaxNet", "taxnet.*")))
    return ok(page("CA office", "Apps", f"<p>Reach an app by its host name:</p><ul>{links}</ul>"))


def share_file(req):
    """A file of the office share, downloaded (Bookwell links it after an export)."""
    p = req.parts
    if len(p) == 3 and p[:2] == ["share", "file"]:
        f = find("share_files", p[2])
        if f is not None:
            name = f["name"].replace('"', "")
            return Resp(200, f.get("content", "").encode("utf-8"), "application/json",
                        {"Content-Disposition": f'attachment; filename="{name}"'})
    return None


APPS = {"chat": chat, "mail": mail, "books": books, "fileprep": fileprep, "taxnet": taxnet}


def dispatch(req):
    label = req.host.split(".", 1)[0] if "." in req.host else ""
    handler = APPS.get(label)
    if handler is None:
        return index(req) if not req.parts else not_found()
    if label == "books":
        got = share_file(req)
        if got is not None:
            return got
    return handler(req)


class Handler(BaseHTTPRequestHandler):
    server_version = "CAOffice/1"

    def log_message(self, fmt, *args):
        pass

    def handle_one(self, method):
        try:
            url = urllib.parse.urlsplit(self.path)
            host = (self.headers.get("Host") or "").split(":", 1)[0].strip().lower()
            data = {}
            if method == "POST":
                try:
                    length = int(self.headers.get("Content-Length") or 0)
                except ValueError:
                    length = -1
                if length < 0 or length > MAX_BODY:
                    return self.reply(Resp(413, page("CA office", "Too large", "<p>The form is too large.</p>")))
                ctype = (self.headers.get("Content-Type") or "").split(";", 1)[0].strip().lower()
                raw = self.rfile.read(length).decode("utf-8", "replace") if length else ""
                if raw and ctype not in ("", "application/x-www-form-urlencoded"):
                    return self.reply(Resp(415, page("CA office", "Not a form", "<p>Send the form as a web form.</p>")))
                data = urllib.parse.parse_qs(raw, keep_blank_values=True, max_num_fields=400)
            cookies = SimpleCookie()
            try:
                cookies.load(self.headers.get("Cookie") or "")
            except Exception:  # noqa: BLE001 - a cookie that cannot be read is no cookie
                cookies = SimpleCookie()
            req = Req(method, host, url.path, urllib.parse.parse_qs(url.query, max_num_fields=100), data, cookies)
            with LOCK:
                resp = dispatch(req)
        except Exception:  # noqa: BLE001 - one bad request never stops the app
            traceback.print_exc()
            resp = Resp(500, page("CA office", "Something went wrong", "<p>The app could not do that.</p>"))
        self.reply(resp)

    def reply(self, resp):
        body = resp.body.encode("utf-8") if isinstance(resp.body, str) else resp.body
        self.send_response(resp.status)
        self.send_header("Content-Type", resp.ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for k, v in resp.headers.items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_GET(self):
        self.handle_one("GET")

    def do_HEAD(self):
        self.handle_one("GET")

    def do_POST(self):
        self.handle_one("POST")


def main(argv=None):
    global STATE_PATH
    parser = argparse.ArgumentParser(description="CA office: five mock apps for eval suites")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--fixture", default="")
    parser.add_argument("--state", required=True)
    parser.add_argument("--host", default="0.0.0.0")
    args = parser.parse_args(argv)
    STATE_PATH = os.path.abspath(args.state)
    load_fixture(args.fixture)
    save_state()
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.daemon_threads = True
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
