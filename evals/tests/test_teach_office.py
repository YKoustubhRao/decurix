"""The three teaching demonstrations (evals/tools/teach_office.py) can be done on the ca-office mock as written: a small
browser stand-in follows each step by the visible label it names (a link's text, a button's text, a field's <label>, a
list's option as typed) on the suite's case 1, and the work ends done in the mock's state. The live run drives the same
steps through the desktop's screen. Run: py -3 -m unittest discover -s evals/tests"""

import html.parser
import importlib.util
import json
import os
import sys
import unittest
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
EVALS = os.path.dirname(HERE)
sys.path.insert(0, HERE)

from test_ca_office_mock import Mock  # noqa: E402

spec = importlib.util.spec_from_file_location("teach_office", os.path.join(EVALS, "tools", "teach_office.py"))
teach = importlib.util.module_from_spec(spec)
spec.loader.exec_module(teach)


def norm(text):
    return " ".join(str(text).split())


class Page(html.parser.HTMLParser):
    """What a person sees of a page: links, forms (fields, lists, boxes, buttons) and labels."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.links, self.forms, self.labels = [], [], {}
        self._a = self._label = self._button = self._option = self._textarea = None
        self._form = None
        self._select = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "a":
            self._a = [a.get("href", ""), ""]
        elif tag == "form":
            self._form = {"action": a.get("action", ""), "method": a.get("method", "get").lower(), "fields": [], "buttons": []}
            self.forms.append(self._form)
        elif tag == "label":
            self._label = [a.get("for", ""), ""]
        elif tag == "input" and self._form is not None:
            self._form["fields"].append({"kind": a.get("type", "text"), "name": a.get("name", ""), "id": a.get("id", ""),
                                         "value": a.get("value", ""), "checked": "checked" in a})
        elif tag == "textarea" and self._form is not None:
            self._textarea = {"kind": "textarea", "name": a.get("name", ""), "id": a.get("id", ""), "value": ""}
            self._form["fields"].append(self._textarea)
        elif tag == "select" and self._form is not None:
            self._select = {"kind": "select", "name": a.get("name", ""), "id": a.get("id", ""), "value": None, "options": []}
            self._form["fields"].append(self._select)
        elif tag == "option" and self._select is not None:
            self._option = [a.get("value", ""), "", "selected" in a]
        elif tag == "button" and self._form is not None:
            self._button = {"name": a.get("name", ""), "value": a.get("value", ""), "formaction": a.get("formaction", ""), "text": ""}
            self._form["buttons"].append(self._button)

    def handle_endtag(self, tag):
        if tag == "a" and self._a:
            self.links.append((norm(self._a[1]), self._a[0]))
            self._a = None
        elif tag == "label" and self._label:
            self.labels[self._label[0]] = norm(self._label[1])
            self._label = None
        elif tag == "option" and self._option:
            v, t, sel = self._option
            self._select["options"].append((v, norm(t)))
            if sel or self._select["value"] is None:
                self._select["value"] = v
            self._option = None
        elif tag == "select":
            self._select = None
        elif tag == "textarea":
            self._textarea = None
        elif tag == "button":
            self._button = None
        elif tag == "form":
            self._form = None

    def handle_data(self, data):
        for part in (self._a, self._label, self._option):
            if part:
                part[1] += data
        if self._button is not None:
            self._button["text"] += data
        if self._textarea is not None:
            self._textarea["value"] += data


class Browser:
    """Follows a demonstration's steps by label, as the screen-driven person does, against the mock."""

    def __init__(self, mock):
        self.mock, self.host, self.path, self.page = mock, "", "/", None

    def go(self, url, method="GET", fields=None):
        u = urllib.parse.urlsplit(url)
        host = u.hostname or self.host
        path = (u.path or "/") + (("?" + u.query) if u.query else "")
        for _ in range(6):
            status, loc, body = self.mock.request(method, host, path, body=fields if method == "POST" else None)
            if status in (301, 302, 303) and loc:
                lu = urllib.parse.urlsplit(loc)
                host, path = lu.hostname or host, (lu.path or "/") + (("?" + lu.query) if lu.query else "")
                method, fields = "GET", None
                continue
            break
        self.host, self.path = host, path
        self.status = status
        self.page = Page()
        self.page.feed(body.decode("utf-8", "replace"))

    def field(self, label, kinds):
        want = norm(label)
        for form in self.page.forms:
            for f in form["fields"]:
                text = self.page.labels.get(f["id"], "")
                if f["kind"] in kinds and text and (text == want or text.startswith(want)):
                    return form, f
        raise AssertionError(f"{self.host}{self.path}: no {'/'.join(kinds)} labelled {label!r}")

    def step(self, st):
        kind = st[0]
        if kind == "open":
            self.go(st[1])
        elif kind == "click" and "link" in st[1]:
            hrefs = [h for t, h in self.page.links if t == norm(st[2])] or [h for t, h in self.page.links if t.startswith(norm(st[2]))]
            assert hrefs, f"{self.host}{self.path}: no link {st[2]!r}"
            self.go(hrefs[0])
        elif kind == "click":
            for form in self.page.forms:
                for b in form["buttons"]:
                    if norm(b["text"]) == norm(st[2]):
                        return self.submit(form, b)
            raise AssertionError(f"{self.host}{self.path}: no button {st[2]!r}")
        elif kind == "fill":
            _, f = self.field(st[1], ("text", "textarea", "password", "date", "search"))
            f["value"] = st[2]
        elif kind == "choose":
            _, f = self.field(st[1], ("select",))
            match = [v for v, t in f["options"] if t.lower().startswith(st[2].lower())]
            assert match, f"{self.host}{self.path}: the list {st[1]!r} has no option typed {st[2]!r}: {f['options']}"
            f["value"] = match[0]
        elif kind == "tick":
            form, f = self.field(st[1], ("checkbox", "radio"))
            if f["kind"] == "radio":
                for other in form["fields"]:
                    if other["kind"] == "radio" and other["name"] == f["name"]:
                        other["checked"] = False
            f["checked"] = True

    def submit(self, form, button):
        fields = []
        for f in form["fields"]:
            if f["kind"] in ("checkbox", "radio") and not f["checked"]:
                continue
            if f["name"]:
                fields.append((f["name"], f["value"] or ""))
        if button["name"]:
            fields.append((button["name"], button["value"]))
        action = button["formaction"] or form["action"] or self.path
        if form["method"] == "get":
            self.go(action.split("?")[0] + "?" + urllib.parse.urlencode(fields))
        else:
            self.go(action, "POST", fields)
        assert self.status < 400, f"{action}: the form was refused ({self.status})"


class Demonstrations(unittest.TestCase):
    def demo(self, suite):
        root = os.path.join(EVALS, "suites", suite)
        with open(os.path.join(root, "suite.json"), encoding="utf-8") as f:
            first = json.load(f)["cases"][0]
        with open(os.path.join(root, "cases", first, "case.json"), encoding="utf-8") as f:
            case = json.load(f)
        with open(os.path.join(root, "cases", first, "fixture.json"), encoding="utf-8") as f:
            fixture = json.load(f)
        mock = Mock(fixture)
        self.addCleanup(mock.close)
        b = Browser(mock)
        for _, steps in teach.DEMOS[suite](case, fixture):
            for st in steps:
                if st[0] != "wait":
                    b.step(st)
        return mock, case

    def test_books(self):
        mock, case = self.demo("office-books-v1")
        new = [v for v in mock.coll("vouchers") if v["company"] == "1" and v["date"].startswith("2026-10")]
        self.assertEqual(sorted(v["number"] for v in new), ["1043", "1044", "1045", "1046", "1047"])
        self.assertTrue(any(l["name"] == "Kadri Heights Builders LLP" and l["under"] == "Sundry Debtors" for l in mock.coll("ledgers")))

    def test_return_from_books(self):
        mock, case = self.demo("office-gstr1-books-v1")
        self.assertTrue(any(f["folder"].endswith("Sahyadri Lift Services/2026-27/7. OCTOBER") for f in mock.coll("share_files")))
        self.assertEqual(next(r for r in mock.coll("tn_returns") if r["period"] == "2026-10")["status"], "Filed")

    def test_return_from_chat(self):
        mock, case = self.demo("office-gstr1-chat-v1")
        ret = next(r for r in mock.coll("tn_returns") if r["period"] == "2026-10" and r["status"] != "")
        self.assertEqual(ret["status"], "Filed")
        self.assertEqual(len(mock.coll("tn_b2cs")), 1)
        self.assertEqual(len(mock.coll("tn_hsn")), 1)


if __name__ == "__main__":
    unittest.main()
