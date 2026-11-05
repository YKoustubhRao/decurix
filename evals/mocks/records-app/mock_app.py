"""
Records - a small, ordinary server-rendered records app for eval suites (docs/evals.md).

It speaks the practice sandbox's mock contract (docs/simulation.md): started as

    python3 mock_app.py --port 8000 --fixture fixture.json --state state.json

it serves its pages, and writes the whole state, {"collections": {name: [records]}}, to --state at start and after
every change, which is what the graders read once the practice desktop is paused. Standard library only; no external
assets.

Collections: users, customers, bills, payees, payments and tickets. Each has a list page, a page per record, a new
form and an edit form. A payment has a Pay button that leads to a confirmation page (both are committing actions, so a worker's
approval gate asks before either). Date inputs are <input type=date>, so the state holds ISO dates. Every field has a
<label for>, so the accessibility tree names it.

The fixture is {"collections": {...}, "options": {...}, "auth": {"user": ..., "password": ...}, "settings": {...}},
every key optional. With "auth", every page needs a sign-in first; without it, the app is open. "settings" changes how
the app is drawn, the way real apps differ, never what it stores:

  - "custom_selects": {collection: [field, ...]}: those choice fields are drawn as a dropdown the page makes itself (a
    button with role=combobox that opens a list of role=option entries, and a hidden input), not a native <select>.
  - "filters": {collection: [field, ...]}: the list page has a filter for each (a native <select>, "All" first), read
    from the query string, e.g. /tickets?status=Pending.
  - "page_size": N: list pages show N records a page, with a "Rows per page" choice and Previous/Next page links.
"""

import argparse
import html
import json
import os
import re
import secrets
import threading
import urllib.parse
from http.cookies import SimpleCookie
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
STATE_PATH = ""
STATE = {"collections": {}}
OPTIONS = {}
AUTH = None
SETTINGS = {}
SESSION = secrets.token_hex(16)

DEFAULT_OPTIONS = {
    "roles": ["Viewer", "Editor", "Admin"],
    "cost_centres": ["General"],
    "priorities": ["Low", "Normal", "High"],
    "statuses": ["Open", "Pending", "Closed"],
    "states": ["27-Maharashtra", "29-Karnataka", "32-Kerala", "33-Tamil Nadu"],
}
ROWS_PER_PAGE = (10, 25, 50)

# name, label, input type ("text", "email", "date", "number", "textarea" or "select:<options key>"), required
SCHEMA = {
    "users": {"title": "Users", "one": "user", "label": "name", "fields": [
        ("name", "Name", "text", True),
        ("email", "Email", "email", True),
        ("role", "Role", "select:roles", True),
    ]},
    "customers": {"title": "Customers", "one": "customer", "label": "name", "fields": [
        ("name", "Name", "text", True),
        ("email", "Email", "email", True),
        ("state", "Place of supply", "select:states", True),
    ]},
    "bills": {"title": "Bills", "one": "bill", "label": "number", "fields": [
        ("number", "Bill number", "text", True),
        ("vendor", "Vendor", "text", True),
        ("date", "Bill date", "date", True),
        ("due", "Due date", "date", False),
        ("total", "Total", "text", True),
        ("cost_centre", "Cost centre", "select:cost_centres", True),
        ("notes", "Notes", "textarea", False),
    ]},
    "payees": {"title": "Payees", "one": "payee", "label": "name", "fields": [
        ("name", "Name", "text", True),
        ("bank", "Bank", "text", False),
        ("account", "Account number", "text", True),
    ]},
    "payments": {"title": "Payments", "one": "payment", "label": "reference", "fields": [
        ("payee", "Payee", "text", True),
        ("account", "Account number", "text", True),
        ("amount", "Amount", "text", True),
        ("date", "Payment date", "date", True),
        ("reference", "Reference", "text", True),
    ]},
    "tickets": {"title": "Support tickets", "one": "ticket", "label": "subject", "fields": [
        ("subject", "Subject", "text", True),
        ("priority", "Priority", "select:priorities", True),
        ("status", "Status", "select:statuses", True),
    ]},
}

ISO_DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def esc(value):
    return html.escape("" if value is None else str(value), quote=True)


def save_state():
    tmp = STATE_PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(STATE, f, indent=1, ensure_ascii=False)
    os.replace(tmp, STATE_PATH)


def load_fixture(path):
    global AUTH
    fixture = {}
    if path and os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            text = f.read().strip()
        fixture = json.loads(text) if text else {}
    collections = fixture.get("collections") or {}
    STATE["collections"] = {name: [dict(r) for r in collections.get(name, [])] for name in SCHEMA}
    for name, records in STATE["collections"].items():
        for i, rec in enumerate(records, 1):
            rec.setdefault("id", i)
    OPTIONS.clear()
    OPTIONS.update(DEFAULT_OPTIONS)
    OPTIONS.update(fixture.get("options") or {})
    SETTINGS.clear()
    SETTINGS.update(fixture.get("settings") or {})
    auth = fixture.get("auth")
    AUTH = auth if isinstance(auth, dict) and auth.get("user") else None


def records(name):
    return STATE["collections"].setdefault(name, [])


def find(name, rid):
    for rec in records(name):
        if str(rec.get("id")) == str(rid):
            return rec
    return None


def next_id(name):
    ids = [int(r["id"]) for r in records(name) if str(r.get("id", "")).isdigit()]
    return max(ids, default=0) + 1


def label_of(name, rec):
    value = rec.get(SCHEMA[name]["label"]) or ""
    return str(value) or f"{SCHEMA[name]['one']} {rec.get('id')}"


def page(title, body):
    nav = " | ".join(f'<a href="/{n}">{esc(s["title"])}</a>' for n, s in SCHEMA.items())
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{esc(title)} - Records</title>
<style>
body{{font-family:sans-serif;margin:24px;max-width:960px}}
table{{border-collapse:collapse;margin:12px 0}} td,th{{border:1px solid #bbb;padding:6px 10px;text-align:left}}
.field{{margin:10px 0}} .field label{{display:inline-block;min-width:150px}}
input,select,textarea{{font-size:15px;padding:4px}} button{{font-size:15px;padding:6px 14px;margin-top:8px}}
.note{{color:#060}} .error{{color:#a00}} nav{{margin-bottom:16px}}
.combo{{display:inline-block;position:relative}} .combo-box{{display:inline-block;min-width:180px;border:1px solid #888;padding:4px;cursor:pointer}}
.combo ul{{position:absolute;z-index:2;background:#fff;border:1px solid #888;margin:0;padding:0;list-style:none;min-width:180px}}
.combo li{{padding:4px 8px;cursor:pointer}} .combo li:hover{{background:#def}}
</style></head>
<body><header><strong>Records</strong></header><nav aria-label="Main">{nav}</nav>
<main><h1>{esc(title)}</h1>{body}</main>{COMBO_JS}</body></html>"""


# The page's own dropdown (settings.custom_selects): the button opens the list, an entry clicked is the value.
COMBO_JS = """<script>
document.querySelectorAll("[data-combo]").forEach(function(c){
  var box = c.querySelector("[role=combobox]"), list = c.querySelector("[role=listbox]"), input = c.querySelector("input");
  function show(open){ list.hidden = !open; box.setAttribute("aria-expanded", open ? "true" : "false"); }
  box.addEventListener("click", function(){ show(list.hidden); });
  box.addEventListener("keydown", function(e){
    if(e.key === "Enter" || e.key === " " || e.key === "ArrowDown"){ e.preventDefault(); show(true); }
    if(e.key === "Escape"){ show(false); }
  });
  list.querySelectorAll("[role=option]").forEach(function(o){
    o.addEventListener("click", function(){
      input.value = o.dataset.value;
      box.textContent = o.textContent;
      list.querySelectorAll("[role=option]").forEach(function(x){ x.setAttribute("aria-selected", x === o ? "true" : "false"); });
      show(false);
      box.focus();
    });
  });
  document.addEventListener("click", function(e){ if(!c.contains(e.target)){ show(false); } });
});
</script>"""


def custom_select(name, field):
    return field in ((SETTINGS.get("custom_selects") or {}).get(name) or [])


def form_html(name, rec, action, button, errors=()):
    rows = []
    for field, label, kind, required in SCHEMA[name]["fields"]:
        value = rec.get(field, "") if rec else ""
        fid = "f-" + field
        req = " required" if required else ""
        if kind.startswith("select:") and custom_select(name, field):
            entries = "".join(
                f'<li role="option" data-value="{esc(o)}" aria-selected="{"true" if str(o) == str(value) else "false"}">{esc(o)}</li>'
                for o in OPTIONS.get(kind.split(":", 1)[1], []))
            rows.append(f'<div class="field"><span class="label" id="l-{fid}">{esc(label)}</span> <div class="combo" data-combo>'
                        f'<input type="hidden" id="{fid}" name="{field}" value="{esc(value)}">'
                        f'<div class="combo-box" role="combobox" tabindex="0" aria-haspopup="listbox" aria-expanded="false" '
                        f'aria-labelledby="l-{fid}" aria-controls="{fid}-list">{esc(value) if value else "-- Select --"}</div>'
                        f'<ul role="listbox" id="{fid}-list" aria-labelledby="l-{fid}" hidden>{entries}</ul></div></div>')
            continue
        if kind.startswith("select:"):
            opts = ['<option value="">Choose...</option>'] + [
                f'<option value="{esc(o)}"{" selected" if str(o) == str(value) else ""}>{esc(o)}</option>'
                for o in OPTIONS.get(kind.split(":", 1)[1], [])]
            control = f'<select id="{fid}" name="{field}"{req}>{"".join(opts)}</select>'
        elif kind == "textarea":
            control = f'<textarea id="{fid}" name="{field}" rows="3" cols="40">{esc(value)}</textarea>'
        else:
            control = f'<input id="{fid}" name="{field}" type="{kind}" value="{esc(value)}"{req}>'
        rows.append(f'<div class="field"><label for="{fid}">{esc(label)}</label> {control}</div>')
    errs = "".join(f'<p class="error" role="alert">{esc(e)}</p>' for e in errors)
    title = SCHEMA[name]["one"]
    return (f'{errs}<form method="post" action="{esc(action)}" aria-label="{esc(button)} {esc(title)}">'
            f'{"".join(rows)}<button type="submit">{esc(button)}</button></form>')


def validate(name, form):
    values, errors = {}, []
    for field, label, kind, required in SCHEMA[name]["fields"]:
        value = " ".join(str(form.get(field, "")).split()) if kind != "textarea" else str(form.get(field, "")).strip()
        if required and not value:
            errors.append(f"{label} is required.")
        if value and kind == "date" and not ISO_DATE.match(value):
            errors.append(f"{label} must be a date.")
        if value and kind.startswith("select:") and value not in [str(o) for o in OPTIONS.get(kind.split(":", 1)[1], [])]:
            errors.append(f"{label}: choose one of the options.")
        values[field] = value
    return values, errors


def _int(value, default):
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def list_html(name, query=None):
    spec = SCHEMA[name]
    query = query or {}
    head = "".join(f"<th>{esc(label)}</th>" for _, label, _, _ in spec["fields"])
    extra = "<th>Status</th>" if name == "payments" else ""
    kinds = {field: kind for field, _, kind, _ in spec["fields"]}
    shown = records(name)
    controls = []
    for field in (SETTINGS.get("filters") or {}).get(name) or []:
        kind = kinds.get(field, "")
        if not kind.startswith("select:"):
            continue
        label = next(l for f, l, _, _ in spec["fields"] if f == field)
        chosen = (query.get(field) or [""])[-1]
        opts = '<option value="">All</option>' + "".join(
            f'<option value="{esc(o)}"{" selected" if str(o) == chosen else ""}>{esc(o)}</option>'
            for o in OPTIONS.get(kind.split(":", 1)[1], []))
        controls.append(f'<label for="flt-{field}">{esc(label)} filter</label> '
                        f'<select id="flt-{field}" name="{field}" onchange="this.form.submit()">{opts}</select>')
        if chosen:
            shown = [r for r in shown if str(r.get(field, "")) == chosen]
    size = _int(SETTINGS.get("page_size"), 0)
    pager = ""
    if size > 0:
        size = _int((query.get("per_page") or [""])[-1], size)
        opts = "".join(f'<option value="{n}"{" selected" if n == size else ""}>{n}</option>'
                       for n in sorted(set(ROWS_PER_PAGE) | {_int(SETTINGS.get("page_size"), 10)}))
        controls.append(f'<label for="flt-per-page">Rows per page</label> '
                        f'<select id="flt-per-page" name="per_page" onchange="this.form.submit()">{opts}</select>')
        pages = max(1, (len(shown) + size - 1) // size)
        number = min(max(1, _int((query.get("page") or [""])[-1], 1)), pages)
        keep = {k: v[-1] for k, v in query.items() if k != "page" and v and v[-1]}
        link = lambda n: f"/{name}?" + urllib.parse.urlencode(dict(keep, page=n))  # noqa: E731
        pager = f"<p>Page {number} of {pages}. "
        if number > 1:
            pager += f'<a href="{esc(link(number - 1))}">Previous page</a> '
        if number < pages:
            pager += f'<a href="{esc(link(number + 1))}">Next page</a>'
        pager += "</p>"
        total = len(shown)
        shown = shown[(number - 1) * size: number * size]
    else:
        total = len(shown)
    form = (f'<form method="get" action="/{name}" aria-label="Filter {esc(spec["title"].lower())}">{" ".join(controls)} '
            f'<button type="submit">Show</button></form>') if controls else ""
    rows = []
    for rec in shown:
        cells = "".join(f"<td>{esc(rec.get(field, ''))}</td>" for field, _, _, _ in spec["fields"])
        if name == "payments":
            cells += f"<td>{esc(rec.get('status', 'unpaid'))}</td>"
        label = label_of(name, rec)
        rows.append(f'<tr>{cells}<td><a href="/{name}/{esc(rec["id"])}">Open {esc(label)}</a> '
                    f'<a href="/{name}/{esc(rec["id"])}/edit">Edit {esc(label)}</a></td></tr>')
    table = (f'<table aria-label="{esc(spec["title"])}"><thead><tr>{head}{extra}<th>Actions</th></tr></thead>'
             f'<tbody>{"".join(rows)}</tbody></table>') if rows else f"<p>No {esc(spec['title'].lower())} yet.</p>"
    return (f'<p><a href="/{name}/new">New {esc(spec["one"])}</a></p>{form}'
            f'<p>{total} {esc(spec["title"].lower())}.</p>{table}{pager}')


def detail_html(name, rec, note=""):
    spec = SCHEMA[name]
    rows = "".join(f"<tr><th>{esc(label)}</th><td>{esc(rec.get(field, ''))}</td></tr>" for field, label, _, _ in spec["fields"])
    body = (f'<p class="note" role="status">{esc(note)}</p>' if note else "") + f"<table>{rows}"
    if name == "payments":
        body += f"<tr><th>Status</th><td>{esc(rec.get('status', 'unpaid'))}</td></tr>"
    body += f'</table><p><a href="/{name}/{esc(rec["id"])}/edit">Edit {esc(label_of(name, rec))}</a> | <a href="/{name}">Back to {esc(spec["title"].lower())}</a></p>'
    if name == "payments" and rec.get("status", "unpaid") != "paid":
        body += (f'<form method="post" action="/payments/{esc(rec["id"])}/pay" aria-label="Pay">'
                 f'<button type="submit">Pay</button></form>')
    return body


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def send_html(self, text, status=200):
        data = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def redirect(self, location, cookie=None):
        self.send_response(303)
        self.send_header("Location", location)
        if cookie is not None:
            self.send_header("Set-Cookie", f"session={cookie}; Path=/; HttpOnly")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def not_found(self):
        self.send_html(page("Not found", "<p>There is no such page.</p>"), 404)

    def form(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(min(length, 1 << 20)).decode("utf-8", "replace")
        return {k: v[-1] for k, v in urllib.parse.parse_qs(raw, keep_blank_values=True).items()}

    def signed_in(self):
        if AUTH is None:
            return True
        morsel = SimpleCookie(self.headers.get("Cookie", "")).get("session")
        return morsel is not None and morsel.value == SESSION

    def route(self):
        url = urllib.parse.urlparse(self.path)
        parts = [urllib.parse.unquote(p) for p in url.path.strip("/").split("/") if p]
        return url, parts

    def do_GET(self):
        url, parts = self.route()
        query = urllib.parse.parse_qs(url.query)
        if parts == ["login"]:
            error = '<p class="error" role="alert">Wrong user name or password.</p>' if "error" in query else ""
            return self.send_html(page("Sign in", error + '<form method="post" action="/login" aria-label="Sign in">'
                                       '<div class="field"><label for="f-user">User name</label> <input id="f-user" name="user"></div>'
                                       '<div class="field"><label for="f-password">Password</label> <input id="f-password" name="password" type="password"></div>'
                                       '<button type="submit">Sign in</button></form>'))
        if not self.signed_in():
            return self.redirect("/login")
        with LOCK:
            if not parts:
                links = "".join(f'<li><a href="/{n}">{esc(s["title"])}</a></li>' for n, s in SCHEMA.items())
                return self.send_html(page("Records", f"<ul>{links}</ul>"))
            name = parts[0]
            if name not in SCHEMA:
                return self.not_found()
            spec = SCHEMA[name]
            if len(parts) == 1:
                return self.send_html(page(spec["title"], list_html(name, query)))
            if parts[1] == "new" and len(parts) == 2:
                return self.send_html(page(f"New {spec['one']}", form_html(name, {}, f"/{name}/new", f"Save {spec['one']}")))
            rec = find(name, parts[1])
            if rec is None:
                return self.not_found()
            if len(parts) == 2:
                note = "Saved." if "saved" in query else ("Paid." if "paid" in query else "")
                return self.send_html(page(f"{spec['one'].capitalize()} {label_of(name, rec)}", detail_html(name, rec, note)))
            if parts[2:] == ["edit"]:
                body = form_html(name, rec, f"/{name}/{rec['id']}/edit", "Save changes")
                if name == "users":
                    body += (f'<form method="post" action="/users/{esc(rec["id"])}/delete" aria-label="Delete user">'
                             f'<button type="submit">Delete user</button></form>')
                return self.send_html(page(f"Edit {label_of(name, rec)}", body))
        return self.not_found()

    def do_POST(self):
        url, parts = self.route()
        form = self.form()
        if parts == ["login"]:
            if AUTH is not None and form.get("user") == AUTH.get("user") and form.get("password") == AUTH.get("password"):
                return self.redirect("/", cookie=SESSION)
            return self.redirect("/login?error=1")
        if not self.signed_in():
            return self.redirect("/login")
        with LOCK:
            if not parts or parts[0] not in SCHEMA:
                return self.not_found()
            name = parts[0]
            spec = SCHEMA[name]
            if parts[1:] == ["new"]:
                values, errors = validate(name, form)
                if errors:
                    return self.send_html(page(f"New {spec['one']}", form_html(name, values, f"/{name}/new", f"Save {spec['one']}", errors)), 400)
                rec = dict(values, id=next_id(name))
                if name == "payments":
                    rec["status"] = "unpaid"
                records(name).append(rec)
                save_state()
                return self.redirect(f"/{name}/{rec['id']}?saved=1")
            rec = find(name, parts[1]) if len(parts) > 1 else None
            if rec is None:
                return self.not_found()
            if parts[2:] == ["edit"]:
                values, errors = validate(name, form)
                if errors:
                    return self.send_html(page(f"Edit {label_of(name, rec)}", form_html(name, dict(rec, **values), f"/{name}/{rec['id']}/edit", "Save changes", errors)), 400)
                rec.update(values)
                save_state()
                return self.redirect(f"/{name}/{rec['id']}?saved=1")
            if name == "users" and parts[2:] == ["delete"]:
                STATE["collections"]["users"] = [r for r in records("users") if r is not rec]
                save_state()
                return self.redirect("/users")
            if name == "payments" and parts[2:] == ["pay"]:
                if rec.get("status") == "paid":
                    return self.redirect(f"/payments/{rec['id']}")
                body = (f"<p>Pay {esc(rec.get('amount'))} to {esc(rec.get('payee'))}, account {esc(rec.get('account'))}?</p>"
                        f'<form method="post" action="/payments/{esc(rec["id"])}/pay/confirm" aria-label="Confirm payment">'
                        f'<button type="submit">Confirm payment</button></form>'
                        f'<p><a href="/payments/{esc(rec["id"])}">Cancel</a></p>')
                return self.send_html(page("Confirm payment", body))
            if name == "payments" and parts[2:] == ["pay", "confirm"]:
                rec["status"] = "paid"
                save_state()
                return self.redirect(f"/payments/{rec['id']}?paid=1")
        return self.not_found()


def main(argv=None):
    global STATE_PATH
    parser = argparse.ArgumentParser(description="Records: a mock records app for eval suites")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--fixture", default="")
    parser.add_argument("--state", required=True)
    parser.add_argument("--host", default="0.0.0.0")
    args = parser.parse_args(argv)
    STATE_PATH = args.state
    load_fixture(args.fixture)
    save_state()
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
