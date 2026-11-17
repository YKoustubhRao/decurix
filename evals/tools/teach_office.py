"""
teach_office: demonstrates case 1 of an office suite on a teach desktop, as a person does it at the screen, so the
worker can be taught that work once (docs/evals.md, "Teaching once, then the suite from the taught worker").

It drives the ca-office mock's pages through the desktop's own VNC server (bench/screen/person.py), so the demonstration
recorder takes every click and key for a person's; where to click comes from the page itself (`gui observe --coords`,
a look, which is not an action and is not recorded), found by each control's visible label. The values it types are
the case's own: read from its scripted solution (case.json "oracle", whose lines carry the bills' figures) and its
facts (the OTP the person has). Notes (the ticket's Reason) say what a person would say while doing it.

Before it runs: the control plane is up, the desktop is running, and `evals/run.py teach` has started the case's mock
in that desktop and mapped the suite's hosts (then close the desktop's browser once). Then:

    py -3 evals/tools/teach_office.py --suite evals/suites/office-books-v1 --desktop <id>
    py -3 evals/tools/teach_office.py --suite evals/suites/office-gstr1-books-v1 --desktop <id>
    py -3 evals/tools/teach_office.py --suite evals/suites/office-gstr1-chat-v1 --desktop <id>

It opens a ticket (Record), runs the demonstration, ends the ticket and prints the recording's session id and the
/learn line to send in the desktop's chat; the lesson's questions are the person's to answer. --dry-run prints the
steps and changes nothing. Needs the Docker CLI on this machine (as the dev loop's `docker exec`).
"""

import argparse
import json
import os
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
PERSON = os.path.join(REPO, "bench", "screen", "person.py")

# The part that runs inside the desktop, after person.py's text: a person's hands (Person) and eyes (`gui observe`).
RUNNER = r'''
import json, os, re, subprocess, sys, time

ENV = dict(os.environ, DISPLAY=":1", DECURIX_GUI_TEXT_CHARS="20000")
# A control's state between its name and its place may hold a comma ("[row 4, unchecked]"): a checkbox in a table row
# was never found when the pattern refused one.
ITEM = re.compile(r'(e\d+) (?:([a-z][a-z ]*?) )?"([^"]*)"[^@\n]*?@(-?\d+),(-?\d+)')
BOTTOM = 70
FIELDS = ("textbox", "searchbox", "spinbutton", "password text", "text", "entry", "combobox")


def look():
    # A look takes 20 s on a loaded host and longer right after the desktop starts: one over 60 s stopped a demonstration.
    out = subprocess.run(["bash", "-lc", "gui observe --coords"], capture_output=True, text=True, env=ENV, timeout=240).stdout
    size = re.search(r"Screen (\d+)x(\d+)", out)
    w, h = (int(size.group(1)), int(size.group(2))) if size else (1280, 720)
    items = [(m.group(2) or "", " ".join(m.group(3).split()), int(m.group(4)), int(m.group(5))) for m in ITEM.finditer(out)]
    return items, w, h, out


def find(roles, name, me, tries=14):
    """The centre of the control with this visible name (and one of these roles), scrolled into view."""
    want = " ".join(name.split())
    last = None  # where the control was on the look before: a press waits until the page stops moving
    for attempt in range(tries):
        items, w, h, out = look()
        active = re.search(r'Active window: "([^"]*)"', out)
        if active and "Chromium" not in active.group(1) and "Chromium" in out.split("\n", 2)[1]:
            # A Return that landed on the desktop opened the file manager and took the keyboard (found live: a form's
            # values were typed into Thunar): a person closes it and goes back to the browser.
            print(f"the active window is {active.group(1)!r}; going back to the browser", flush=True)
            subprocess.run(["bash", "-lc", "pkill -x Thunar; sleep 1; gui front Chromium"], env=ENV, timeout=60)
            time.sleep(1)
            continue
        hits = [(x, y) for role, n, x, y in items if n == want and (not roles or not role or role in roles)]
        hits = hits or [(x, y) for role, n, x, y in items if n.startswith(want) and (not roles or not role or role in roles)]
        if hits:
            x, y = hits[0]
            # The desktop's panel covers the screen's bottom edge: a control drawn under it is scrolled up first, or the
            # press lands on the panel (found live: "Save" at y=690 opened the file manager from the panel's launcher).
            if 110 <= y <= h - BOTTOM:
                # A wheel scroll animates after the look that followed it: a press at the position read mid-scroll
                # hit the field a row above (found live: "Total number" typed into "Sr. no. to"). Press only where two
                # looks in a row agree.
                if last == (x, y):
                    return x, y
                last = (x, y)
                time.sleep(0.7)
                continue
            last = None
            me.click(w // 2, h // 2, button=5 if y > h - BOTTOM else 4)  # the wheel, toward it
            me.click(w // 2, h // 2, button=5 if y > h - BOTTOM else 4)
            time.sleep(0.6)
            continue
        if attempt in (4, 9):
            me.click(w // 2, h // 2, button=5)  # not drawn yet, or further down
        time.sleep(0.8)
    raise SystemExit(f"no control named {name!r} ({'/'.join(roles) or 'any role'}) on the screen:\n{out[-3000:]}")


def value_of(roles, name):
    """What the named field shows now (its value= in a look), or None when the look does not say."""
    want = " ".join(name.split())
    out = look()[3]
    for m in re.finditer(r'e\d+ (?:([a-z][a-z ]*?) )?"([^"]*)"([^@\n]*)', out):
        role, n, rest = m.group(1) or "", " ".join(m.group(2).split()), m.group(3)
        if n == want and (not role or role in roles):
            v = re.search(r'value="([^"]*)"', rest)
            # A filled field shows value="..."; one found without it is empty. A secret field reads back withheld:
            # not a value to compare.
            if not v:
                return ""
            return v.group(1).replace(",", "").strip() if "[withheld]" not in v.group(1) else None
    return None


def tab_of(out):
    """The browser tab's title and address in a look ('' when none): it changes when a page answers a press."""
    m = re.search(r'== Chromium tab ("[^"]*" \S+) ==', out)
    return m.group(1) if m else ""


def settled(roles, name, ok, tries=7):
    """The field's value once it shows what was entered, or what it shows after ~10 s: on a loaded host the keys sent
    over VNC land seconds late, and a look taken at once read an empty field that was filled a moment later."""
    have = None
    for _ in range(tries):
        time.sleep(1.5)
        have = value_of(roles, name)
        if have is None or ok(have):
            return have
    return have


def run(steps):
    me = Person(pace=0.4)
    form = []  # the fills and choices since the last press: what a person enters again when the page refuses the form
    queue = list(steps)
    redo = 0
    while queue:
        st = queue.pop(0)
        kind = st[0]
        print("step:", json.dumps(st), flush=True)
        if kind in ("fill", "choose", "tick"):
            form.append(st)
        elif kind == "click" and "button" not in st[1]:
            form = []
        if kind == "open":
            # With no browser window open, a person starts one from the panel first: ctrl+l alone goes nowhere (found
            # live on a fresh teach desktop: the address was typed into nothing and the next step found no page).
            items, w, h, out = look()
            if "has no open tab" in out or 'Active window: "(none)"' in out:
                panel = [(x, y) for role, n, x, y in items if n == "Web Browser"]
                if not panel:
                    raise SystemExit("no browser is open and the panel has no Web Browser button:\n" + out[-2000:])
                me.click(*panel[0])
                for _ in range(30):
                    time.sleep(1)
                    if "has no open tab" not in look()[3]:
                        break
                me.wait(2)
            # A person looks where the address took them and types it again when it went wrong: on a loaded host the
            # first characters of an address were lost ("http:/" gone - a file:// page; "h" gone - a web search).
            host = st[1].split("://", 1)[-1].split("/", 1)[0]
            for attempt in range(3):
                me.key("ctrl+l")
                me.wait(1.5)
                me.type(st[1])
                me.wait(0.5)
                me.key("Return")
                me.wait(3)
                # A slow host draws the tab late: look again for up to half a minute before typing the address again
                # (found live: three retypes in nine seconds while the page was still loading, and the demo gave up).
                for _ in range(10):
                    tab = re.search(r'== Chromium tab "[^"]*" (\S+) ==', look()[3])
                    if tab:
                        break
                    time.sleep(3)
                if tab and tab.group(1).split("://", 1)[-1].startswith(host):
                    break
                print(f"the address went to {tab.group(1) if tab else 'no tab'}; typing it again", flush=True)
            else:
                raise SystemExit(f"could not open {st[1]}")
        elif kind == "click":
            if "button" in st[1] and form and not st[-1:] == ["checked"]:
                # A person looks over the whole form before pressing: a late click left keys in the field before it
                # (found live: "372881.35" typed into a focused list chose rate 3; the amount was then typed again and
                # the row was saved at 3%).
                drifted = []
                for f in form:
                    if f[0] == "fill":
                        have = value_of(FIELDS, f[1])
                        if have is not None and have != f[2]:
                            drifted.append(f)
                    elif f[0] == "choose":
                        have = value_of(("combobox", "listbox", "popup button", "menu button"), f[1])
                        if have is not None and not (have.startswith(f[2]) or f[2] in have):
                            drifted.append(f)
                if drifted and redo < 3:
                    redo += 1
                    print("before pressing: " + ", ".join(repr(f[1]) for f in drifted) + " changed; entering them again",
                          flush=True)
                    queue = drifted + [st] + queue
                    form = [f for f in form if f not in drifted]
                    continue
            at = find(st[1], st[2], me)
            before = look()[3] if "button" in st[1] else ""
            me.click(*at)
            me.wait(1.5)
            if "button" in st[1]:
                # A person waits for the page to answer a press before the next one: on a loaded host the press lands
                # seconds late, and a link pressed meanwhile took the page away before the form was sent (found live:
                # a table's Save showed nothing, the next link was pressed, and the row was never stored).
                for _ in range(10):
                    items, w, h, now = look()
                    if tab_of(now) != tab_of(before) or any(role in ("alert", "status") for role, n, x, y in items):
                        break
                    time.sleep(1.5)
                # A refused form shows its problem in a role=alert line: a person reads it; a demonstration that
                # carried on past it taught a save that never happened (found live: a table saved nothing).
                alerts = [n for role, n, x, y in items if role == "alert" and n.strip()]
                if alerts and redo:
                    # The alert seen may be the refused page's, still shown while the press is on its way.
                    time.sleep(4)
                    alerts = [n for role, n, x, y in look()[0] if role == "alert" and n.strip()]
                if alerts and form and redo < 2:
                    # A person reads the problem, enters the form's values again and presses once more.
                    redo += 1
                    print(f"the page refused {st[2]!r}: {' | '.join(alerts)}; entering the form again", flush=True)
                    queue = [x for x in form if x[0] != "tick"] + [st] + queue
                    form = []
                    continue
                if alerts:
                    raise SystemExit(f"the page refused {st[2]!r}: {' | '.join(alerts)}")
                form, redo = [], 0
        elif kind == "fill":
            # A person reads the field back and types it again when a loaded host dropped a character.
            for attempt in range(3):
                me.click(*find(FIELDS, st[1], me))
                me.key("ctrl+a")
                if st[2]:
                    me.type(st[2])
                    # The browser offers what was typed in that field before in a list drawn over the field below;
                    # a person closes it, or the next press picks an old entry (found live, a demonstration taken
                    # again on the same browser: the next field's press filled this one from its history).
                    me.key("Escape")
                else:
                    me.key("BackSpace")
                have = settled(FIELDS, st[1], lambda v: v == st[2])
                if have is None or have == st[2]:
                    break
                print(f"the field {st[1]!r} reads {have!r}, not {st[2]!r}; typing it again", flush=True)
        elif kind == "choose":
            # Type-ahead into a native list resets when the keys arrive slowly (a loaded host chose "3" for "18"):
            # a person looks at what the field shows and chooses again.
            roles = ("combobox", "listbox", "popup button", "menu button")
            for attempt in range(3):
                me.click(*find(roles, st[1], me))
                me.wait(0.6)
                me.type(st[2], each=0.02)
                me.key("Return")
                have = settled(roles, st[1], lambda v: v.startswith(st[2]) or st[2] in v)
                if have is None or have.startswith(st[2]) or st[2] in have:
                    break
                print(f"the list {st[1]!r} shows {have!r}, not {st[2]!r}; choosing again", flush=True)
        elif kind == "tick":
            me.click(*find(("checkbox", "radio", "radio button", "check box"), st[1], me))
        elif kind == "wait":
            me.wait(st[1])
    me.close()


run(json.loads(sys.stdin.readline()))
'''


def oracle_posts(case):
    """The case's scripted solution as (url, fields): the values a person types, from the case files."""
    out = []
    for step in case["oracle"]:
        if not isinstance(step, str) or not step.startswith("curl "):
            continue
        words = shlex.split(step.replace("$MOCK_ROUTE", "").replace('"$HOME/.oracle-cookies"', "jar"))
        fields, url = [], ""
        for i, w in enumerate(words):
            if w == "--data-urlencode":
                k, v = words[i + 1].split("=", 1)
                fields.append((k, v))
            elif w.startswith("http://"):
                url = w
        out.append((url, fields))
    return out


def by_path(posts, part):
    return [dict(f) for u, f in posts if part in urllib.parse.urlsplit(u).path]


def otp_of(case):
    for f in case["facts"]:
        if f["id"] == "otp":
            return f["answer"].rstrip(".").split()[-1]
    return ""


def month_words(period):
    y, m = period.split("-")
    return ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November",
            "December"][int(m) - 1] + " " + y


# --- the three demonstrations: segments of steps, a person's note before each ---------------------------------------

def books_demo(case, fixture):
    posts = oracle_posts(case)
    company = next(c for c in fixture["collections"]["companies"] if c["id"] == "1")["name"]
    mail = next(m for m in fixture["collections"]["mails"] if m["subject"].startswith("Sales tax bills"))
    d, m, y = by_path(posts, "/vouchers/new")[0]["date"].split("-")
    month = month_words(f"{y}-{m}")  # the Month list holds every month of the company's period: the year decides
    segs = [("The client's sales bills come by mail, one Word file a bill; I count them, and leave the bank statement "
             "mail from the same client alone.",
             [("open", "http://mail.example.test"), ("click", ("link",), mail["subject"]), ("wait", 2)]),
            ("Bookwell opens on the company used last, which may be another client's: change to the client's own company "
             "by its name and GSTIN, whatever year its name says, as long as its period holds the bills' dates.",
             [("open", "http://books.example.test"), ("click", ("link",), "Change company"), ("click", ("link",), company),
              ("click", ("link",), "Ledgers"), ("click", ("link",), "Ledger vouchers: Sales@18%"),
              ("choose", "Month", month), ("click", ("button",), "Show"), ("wait", 2)])]
    steps = []
    for led in by_path(posts, "/ledgers/new"):
        steps += [("click", ("link",), "Create ledger"), ("fill", "Name", led["name"]), ("choose", "Under", led["under"]),
                  ("fill", "Mailing name", led["mailing_name"]), ("fill", "Address", led["address"]),
                  ("choose", "State", led["state"]), ("fill", "Pincode", led["pincode"]),
                  ("choose", "Registration type", led["registration"]), ("fill", "GSTIN/UIN", led["gstin"]),
                  ("click", ("button",), "Save ledger"), ("wait", 1.5)]
    if steps:
        segs.append(("A customer with no ledger yet gets one under Sundry Debtors, named as on the bill, with its "
                     "address, state and GSTIN from the bill (Regular when it has a GSTIN).", steps))
    steps = []
    for v in by_path(posts, "/vouchers/new"):
        steps += [("click", ("link",), "Create voucher"), ("choose", "Voucher type", "Sales"),
                  ("fill", "Voucher No.", v["number"]), ("fill", "Date (DD-MM-YYYY)", v["date"]),
                  ("choose", "Party A/c name", v["party"]), ("choose", "Sales ledger", v["sales_ledger"]),
                  ("fill", "Sales amount (taxable value)", v["taxable"]), ("fill", "CGST amount", v["cgst"]),
                  ("fill", "SGST amount", v["sgst"]), ("fill", "IGST amount", v["igst"]),
                  ("click", ("button",), "Save voucher"), ("wait", 1.5)]
    segs.append(("Each bill is a Sales voucher: its number is the bill's, its date the bill's full date, the party the "
                 "customer named in the bill's box (not the file's name), the basic price to the sales ledger and the "
                 "taxes as the bill gives them.", steps))
    segs.append(("Last, the month's sales ledger against the bills: one voucher a bill, nothing twice.",
                 [("click", ("link",), "Ledgers"), ("click", ("link",), "Ledger vouchers: Sales@18%"),
                  ("choose", "Month", month), ("click", ("button",), "Show"), ("wait", 3)]))
    return segs


def gst_books_demo(case, fixture):
    posts = oracle_posts(case)
    export = by_path(posts, "/gstr1/export")[0]
    imp = by_path(posts, "/import")[0]
    period = export["month"]
    fp_url = next(u for u, _ in posts if "/import" in u)
    client_id = urllib.parse.urlsplit(fp_url).path.split("/")[2]
    client = next(c for c in fixture["collections"]["fp_clients"] if c["id"] == client_id)
    validate = [f for u, f in posts if u.endswith("/receivers") and ("action", "validate") in f]
    words = month_words(period)
    month = words.split()[0]
    q = {"10": "Quarter 3 (Oct - Dec)", "11": "Quarter 3 (Oct - Dec)", "12": "Quarter 3 (Oct - Dec)",
         "01": "Quarter 4 (Jan - Mar)", "02": "Quarter 4 (Jan - Mar)", "03": "Quarter 4 (Jan - Mar)",
         "04": "Quarter 1 (Apr - Jun)", "05": "Quarter 1 (Apr - Jun)", "06": "Quarter 1 (Apr - Jun)",
         "07": "Quarter 2 (Jul - Sep)", "08": "Quarter 2 (Jul - Sep)", "09": "Quarter 2 (Jul - Sep)"}[period[5:]]
    fy = period[:4] + "-" + f"{(int(period[:4]) + 1) % 100:02d}" if int(period[5:]) >= 4 else f"{int(period[:4]) - 1}-{period[2:4]}"
    segs = [("The return comes from the books: check the month's GSTR-1 there (every voucher included), then export it "
             "as JSON into this client's own month folder - the dialog remembers the last folder used, often another "
             "client's.",
             [("open", "http://books.example.test"), ("click", ("link",), "GSTR-1"), ("choose", "Period", words),
              ("click", ("button",), "Show"), ("click", ("link",), f"Export GSTR-1 for {words}"),
              ("fill", "Folder path", export["folder"]), ("click", ("button",), "Export"), ("wait", 2)]),
            ("In FilePrep, open the client by its name and GSTIN (similar names exist), the same period, and import "
             "that file with Overwrite, so the period holds exactly what the books say.",
             [("open", "http://fileprep.example.test"), ("fill", "Search", client["name"].split()[0]),
              ("click", ("button",), "Search"), ("click", ("link",), client["name"]),
              ("choose", "Financial year", fy), ("choose", "Return period", month), ("click", ("button",), "OK"),
              ("click", ("link",), "Import from JSON"), ("fill", "Folder", imp["folder"]), ("click", ("button",), "Show files"),
              ("tick", imp["file"]), ("tick", "Overwrite"), ("click", ("button",), "Import"), ("wait", 2)])]
    steps = [("click", ("link",), "Validate GSTIN")]
    for f in validate:
        for k, v in f:
            if k == "gstin":
                steps.append(("tick", f"Select {v}"))
    steps += [("click", ("button",), "Validate selected"), ("wait", 1.5)]
    for f in validate:
        for k, v in f:
            if k == "gstin":
                steps.append(("tick", f"Select {v}"))
    steps += [("click", ("button",), "Update names from TaxNet"), ("wait", 1.5)]
    segs.append(("Every B2B receiver must be validated against TaxNet's register before saving; the names are taken "
                 "from the register. A cancelled GSTIN stops the filing.", steps))
    segs.append(("Then save to the portal, and open TaxNet from FilePrep: it signs in as the client.",
                 [("click", ("link",), client["name"]), ("choose", "Financial year", fy), ("choose", "Return period", month),
                  ("click", ("button",), "OK"), ("click", ("button",), "Save to portal"), ("wait", 2),
                  ("click", ("link",), client["name"]), ("click", ("link",), f"Open TaxNet as {client['name']}"),
                  ("click", ("button",), "Sign in"), ("wait", 2)]))
    segs.append(("On the portal pick the period with care: the quarter opens on the current one. Generate the summary, "
                 "compare it with the books, and file with EVC; the OTP goes to the client's phone, so ask for it.",
                 [("click", ("link",), "File returns"), ("choose", "Financial year", fy), ("choose", "Quarter", q),
                  ("choose", "Period", month), ("click", ("button",), "Search"),
                  ("click", ("link",), f"Prepare online: GSTR-1 {words}"), ("click", ("button",), "Generate summary"),
                  ("click", ("link",), "Proceed to file / summary"), ("wait", 2), ("click", ("link",), "File statement"),
                  ("tick", "I/We hereby solemnly affirm"), ("choose", "Authorised signatory", by_path(posts, "/file-evc")[0]["signatory"]),
                  ("click", ("button",), "File with EVC"), ("fill", "Enter the one-time password (OTP)", otp_of(case)),
                  ("click", ("button",), "Verify"), ("wait", 3)]))
    return segs


def gst_chat_demo(case, fixture):
    posts = oracle_posts(case)
    login = next(u for u, _ in posts if u.endswith("/login"))
    period = urllib.parse.urlsplit(next(u for u, _ in posts if "/gstr1/" in u)).path.split("/")[2]
    words = month_words(period)
    month = words.split()[0]
    g = case["request"].split("GSTIN ")[1].split(")")[0]
    client = next(c for c in fixture["collections"]["fp_clients"] if c["gstin"] == g)
    thread = next(t for t in fixture["collections"]["chat_threads"] if t["client"] == client["name"])
    q = "Quarter 3 (Oct - Dec)" if period[5:] in ("10", "11", "12") else "Quarter 4 (Jan - Mar)"
    fy = f"{period[:4]}-{(int(period[:4]) + 1) % 100:02d}" if int(period[5:]) >= 4 else f"{int(period[:4]) - 1}-{period[2:4]}"
    b2cs = by_path(posts, "/b2cs")
    hsn = by_path(posts, "/hsn")
    docs = by_path(posts, "/docs")
    segs = [("The client sends the month's invoices on chat as PDFs; read each (buyer, GSTIN or none, place of supply, "
             "taxable value and tax) - they are also in the case folder with the client's computation workbook.",
             [("open", "http://chat.example.test"), ("click", ("link",), thread["title"]), ("wait", 3)]),
            ("TaxNet is opened from FilePrep, signed in as the client; pick the period with care (the quarter opens on "
             "the current one).",
             [("open", "http://fileprep.example.test"), ("fill", "Search", client["name"].split()[0]), ("click", ("button",), "Search"),
              ("click", ("link",), client["name"]), ("click", ("link",), f"Open TaxNet as {client['name']}"),
              ("click", ("button",), "Sign in"), ("click", ("link",), "File returns"), ("choose", "Financial year", fy),
              ("choose", "Quarter", q), ("choose", "Period", month), ("click", ("button",), "Search"),
              ("click", ("link",), f"Prepare online: GSTR-1 {words}"), ("wait", 2)])]
    steps = []
    for r in b2cs:
        steps += [("click", ("link",), "7 - B2C (Others)"), ("choose", "Place of supply", f"{r['pos']}-"),
                  ("choose", "Rate (%)", r["rt"]), ("fill", "Taxable value", r["txval"]), ("click", ("button",), "Save"),
                  ("click", ("link",), f"GSTR-1 {words}")]
    segs.append(("No buyer has a GSTIN and all are in the state: they go to Table 7, one row per place of supply and "
                 "rate, the sum of the invoices' taxable values; the portal works out the tax.", steps))
    steps = []
    for r in hsn:
        steps += [("click", ("link",), "12 - HSN-wise summary of outward supplies"), ("click", ("link",), f"{r['tab']} supplies"),
                  ("fill", "HSN or SAC code", r["hsn"]), ("choose", "UQC", r["uqc"]), ("fill", "Total quantity", r["qty"]),
                  ("fill", "Total taxable value", r["txval"]), ("choose", "Rate (%)", r["rt"]),
                  ("fill", "Integrated tax", r["iamt"]), ("fill", "Central tax", r["camt"]), ("fill", "State/UT tax", r["samt"]),
                  ("fill", "Cess", r["csamt"]), ("click", ("button",), "Add"), ("click", ("link",), f"GSTR-1 {words}")]
    for d in docs:
        steps += [("click", ("link",), "13 - Documents Issued"), ("fill", "Sr. no. from", d["from"]), ("fill", "Sr. no. to", d["to"]),
                  ("fill", "Total number", d["total"]), ("fill", "Cancelled", d["cancelled"]), ("click", ("button",), "Save"),
                  ("click", ("link",), f"GSTR-1 {words}")]
    segs.append(("The HSN summary goes in its B2C tab (services: UQC NA, quantity 0, the taxes typed as the invoices add "
                 "up); the documents issued are the month's first and last invoice numbers, cancelled ones counted.", steps))
    signatory = by_path(posts, "/file-evc")[0]["signatory"]
    segs.append(("Generate the summary, check it against the invoices, then file with EVC: the OTP goes to the client's "
                 "phone, so I ask the client for it.",
                 [("click", ("button",), "Generate summary"), ("click", ("link",), "Proceed to file / summary"), ("wait", 2),
                  ("click", ("link",), "File statement"), ("tick", "I/We hereby solemnly affirm"),
                  ("choose", "Authorised signatory", signatory), ("click", ("button",), "File with EVC"),
                  ("fill", "Enter the one-time password (OTP)", otp_of(case)), ("click", ("button",), "Verify"), ("wait", 3)]))
    del login
    return segs


DEMOS = {"office-books-v1": books_demo, "office-gstr1-books-v1": gst_books_demo, "office-gstr1-chat-v1": gst_chat_demo}


# --- the control plane and the desktop -------------------------------------------------------------------------------

def call(cp, path, method="POST", **fields):
    data = urllib.parse.urlencode(fields).encode() if method == "POST" else None
    url = cp + path + ("" if method == "POST" else "?" + urllib.parse.urlencode(fields))
    with urllib.request.urlopen(urllib.request.Request(url, data=data, method=method), timeout=120) as r:
        body = r.read().decode("utf-8", "replace")
    try:
        return json.loads(body)
    except ValueError:
        return {"raw": body}


def in_desktop(desktop, steps):
    """Run a segment's steps in the desktop as its person (person.py over its own VNC server)."""
    with open(PERSON, encoding="utf-8") as f:
        script = f.read() + "\n" + RUNNER
    payload = json.dumps(steps) + "\n"
    # The script is the program; its first stdin line after it is the steps (python3 - reads the program to EOF, so
    # the program is passed with -c instead).
    proc = subprocess.run(["docker", "exec", "-i", "-u", "headless", f"desktop_{desktop}", "python3", "-c", script],
                          input=payload, text=True, capture_output=True, timeout=1800)
    sys.stdout.write(proc.stdout)
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr)
        raise SystemExit(f"the demonstration stopped (exit {proc.returncode})")


def ticket(cp, desktop):
    return call(cp, "/api/desktops/ticket", method="GET", id=desktop)


def main(argv):
    ap = argparse.ArgumentParser(description="Demonstrate an office suite's case 1 as a person, recorded for teaching.")
    ap.add_argument("--suite", required=True)
    ap.add_argument("--desktop", required=True)
    ap.add_argument("--case", default="", help="defaults to the suite's first case")
    ap.add_argument("--cp", default=os.environ.get("DECURIX_URL", "http://127.0.0.1:8080"))
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args(argv)
    with open(os.path.join(args.suite, "suite.json"), encoding="utf-8") as f:
        suite = json.load(f)
    case_id = args.case or suite["cases"][0]
    root = os.path.join(args.suite, "cases", case_id)
    with open(os.path.join(root, "case.json"), encoding="utf-8") as f:
        case = json.load(f)
    with open(os.path.join(root, "fixture.json"), encoding="utf-8") as f:
        fixture = json.load(f)
    segments = DEMOS[suite["name"]](case, fixture)
    if args.dry_run:
        for note, steps in segments:
            print("NOTE:", note)
            for st in steps:
                print("   ", json.dumps(st))
        return 0
    title = f"Demonstration: {case['request'].split('.')[0]}"
    call(args.cp, "/api/desktops/record", id=args.desktop, task=title)
    for _ in range(60):
        if (ticket(args.cp, args.desktop).get("ticket") or {}).get("title"):
            break
        time.sleep(1)
    # The ticket shows before its task has been handed to the desktop; begin is refused (409, busy) until then
    for attempt in range(30):
        try:
            call(args.cp, "/api/desktops/ticket/action", id=args.desktop, action="begin")
            break
        except urllib.error.HTTPError as e:
            if e.code != 409 or attempt == 29:
                raise
            time.sleep(2)
    for note, steps in segments:
        # The Reason a person types pauses the recording while it is written; it carries on after (as the toolbar does).
        try:
            call(args.cp, "/api/desktops/ticket/action", id=args.desktop, action="note", text=note)
        except (urllib.error.HTTPError, TimeoutError, OSError) as e:
            # A loaded desktop can take longer than the control plane waits (504) while still writing the note:
            # go on once the ticket shows it paused for it
            if isinstance(e, urllib.error.HTTPError) and e.code != 504:
                raise
            for _ in range(60):
                if (ticket(args.cp, args.desktop).get("ticket") or {}).get("paused"):
                    break
                time.sleep(3)
            else:
                raise
        call(args.cp, "/api/desktops/ticket/action", id=args.desktop, action="resume")
        in_desktop(args.desktop, steps)
    ended = call(args.cp, "/api/desktops/ticket/action", id=args.desktop, action="end")
    session = (ended.get("ticket") or {}).get("session_id") or ended.get("session_id") or ""
    print(json.dumps(ended, indent=1)[:2000])
    name = {"office-books-v1": "Enter a client's sales bills", "office-gstr1-books-v1": "File GSTR-1 from the books",
            "office-gstr1-chat-v1": "File GSTR-1 from the client's invoices"}[suite["name"]]
    print(f"\nRecorded {session or '(see the recordings page)'}. Teach it in the desktop's chat:\n"
          f"  /learn {session or '<session>'} name=\"{name}\"\nand answer its questions as the person would.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
