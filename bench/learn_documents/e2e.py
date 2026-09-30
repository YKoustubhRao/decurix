"""
Record once, learn, run on new documents - end to end, on a real desktop.

A document-driven workflow (a customer's PO keyed into a sales app as a
quotation, with its line items) is demonstrated once, taught with /learn by
answering the lesson's questions the way a person would, then run twice on
new POs the demonstration never saw:

  B. `/skill <name>` with the new PO's PDF named after the command (what an
     email trigger sends): the values the skill needs - customer, reference,
     one product and quantity per line - are read from the PDF by the hosted
     model (decurix_agent/binding.py); the warehouse, which the person said
     stays the same, is typed as taught.
  A. A plain request naming a third PO, to TheBot on a hosted brain: it has
     to find the taught procedure and run it with values it read itself.

Every result is read back from the app's own saved data, never taken from
the agent's report.

Needs: this tree's control plane on 127.0.0.1:8080 with a hosted brain set up
(New desktop > Model), and a running desktop created with that brain from this
tree's image:

    python3 bench/learn_documents/e2e.py office1
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request

BASE = "http://127.0.0.1:8080"
DESK = sys.argv[1] if len(sys.argv) > 1 else "office1"
CONTAINER = f"desktop_{DESK}"
HOME = "/home/headless"

APP = r"""<!doctype html><meta charset=utf-8><title>Sales</title>
<style>body{font:15px sans-serif;margin:24px}label{display:block;margin:8px 0}input{margin-left:8px}
table{border-collapse:collapse;margin:10px 0}td,th{border:1px solid #ccc;padding:4px 10px}</style>
<h1>Quotations</h1>
<div id=list></div>
<button id=new>New quotation</button>
<form id=form hidden onsubmit="return false">
  <h2>New quotation</h2>
  <label>Customer <input aria-label="Customer" id=customer></label>
  <label>Customer Reference <input aria-label="Customer Reference" id=ref></label>
  <label>Warehouse <input aria-label="Warehouse" id=wh></label>
  <table><thead><tr><th>Product</th><th>Quantity</th></tr></thead><tbody id=lines></tbody></table>
  <button id=add type=button>Add a line</button>
  <div id=editor hidden>
    <label>Product <input aria-label="Product" id=product></label>
    <label>Quantity <input aria-label="Quantity" id=qty></label>
    <button id=saveline type=button>Save line</button>
  </div>
  <button id=save type=button>Save quotation</button>
</form>
<p id=out role=status></p>
<script>
const q = () => JSON.parse(localStorage.getItem("quotations") || "[]");
let lines = [];
function render(){document.getElementById("list").innerHTML = q().map(x=>`<p>${x.id}: ${x.customer} (${x.ref})</p>`).join("");}
render();
document.getElementById("new").onclick = () => {document.getElementById("form").hidden = false; lines = [];};
document.getElementById("add").onclick = () => {document.getElementById("editor").hidden = false;
  document.getElementById("product").value = ""; document.getElementById("qty").value = "";};
document.getElementById("saveline").onclick = () => {
  const p = document.getElementById("product").value, n = document.getElementById("qty").value;
  lines.push({product: p, qty: n});
  document.getElementById("lines").insertAdjacentHTML("beforeend", `<tr><td>${p}</td><td>${n}</td></tr>`);
  document.getElementById("editor").hidden = true;};
document.getElementById("save").onclick = () => {
  const all = q(), id = "Q-" + String(all.length + 1).padStart(3, "0");
  const x = {id, customer: document.getElementById("customer").value, ref: document.getElementById("ref").value,
             warehouse: document.getElementById("wh").value, lines};
  all.push(x); localStorage.setItem("quotations", JSON.stringify(all));
  document.getElementById("out").textContent = `Saved quotation ${id} for ${x.customer}`;
  document.getElementById("form").hidden = true; render();};
</script>
"""

DEMO = {"customer": "Deco Addict", "ref": "DA-PO-7731", "warehouse": "Main",
        "lines": [("Large Cabinet", "2"), ("Office Chair Black", "5")]}

PO_B = {"customer": "Wood Corner", "ref": "WC-PO-118", "file": "WC-PO-118.pdf",
        "lines": [("Corner Desk Right Sit", "1"), ("Large Desk", "4"), ("Office Chair", "10")]}
PO_A = {"customer": "Gemini Furniture", "ref": "GF-5522", "file": "GF-5522.pdf",
        "lines": [("Cabinet with Doors", "3"), ("Desk Combination", "1")]}

failures = []


def check(name, ok, detail=""):
    print(f"  {'ok ' if ok else 'BAD'} {name}" + (f"  ({detail})" if detail and not ok else ""))
    if not ok:
        failures.append(name)


def sh(cmd, timeout=180):
    r = subprocess.run(["docker", "exec", "-u", "headless", "-e", "DISPLAY=:1", "-e", "PYTHONPATH=/opt", CONTAINER,
                        "bash", "-lc", cmd], capture_output=True, text=True, timeout=timeout)
    return r


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


def send(text):
    # By id, not position: a desktop's chat keeps its last 200 entries, so once
    # it is full a new reply does not lengthen the list.
    before = {e.get("id") for e in chat().get("entries") or []}
    api("/api/desktops/send", {"id": DESK, "content": text})
    return before


def reply_after(before, timeout=900):
    """The desktop's next reply after entry `before`, once its turn is over."""

    end = time.time() + timeout
    while time.time() < end:
        th = chat()
        replies = [e for e in th.get("entries") or [] if e.get("id") not in before and e.get("role") != "user"]
        if replies and not th.get("running"):
            return replies[-1]
        time.sleep(3)
    raise SystemExit("no reply in time")


def steps_of(entry):
    return [s.get("text") or "" for s in entry.get("steps") or []]


def saved_quotations():
    out = sh("""python3 - <<'EOF'
import json
from decurix_agent.computer import cdp
p = cdp.active_page()
try:
    print(p.evaluate("localStorage.getItem('quotations') || '[]'"))
finally:
    p.close()
EOF""").stdout.strip()
    return json.loads(out or "[]")


def po_pdf(po, workdir):
    rows = "".join(f"<tr><td>{i}</td><td>{p}</td><td>{n}</td></tr>" for i, (p, n) in enumerate(po["lines"], 1))
    html = (f"<html><body style='font-family:Arial'><h2>{po['customer'].upper()}</h2><h3>PURCHASE ORDER</h3>"
            f"<p>PO Number: <b>{po['ref']}</b> &nbsp; Date: 27/09/2026 &nbsp; Deliver to: our Pleasant Hill site</p>"
            f"<p>Supplier: YourCompany</p><table border=1 cellpadding=5 cellspacing=0>"
            f"<tr><th>#</th><th>Description</th><th>Qty</th></tr>{rows}</table>"
            f"<p>Authorised by Purchasing, {po['customer']}</p></body></html>")
    src = os.path.join(workdir, po["file"].replace(".pdf", ".html"))
    with open(src, "w") as f:
        f.write(html)
    subprocess.run(["soffice", "--headless", "--convert-to", "pdf", "--outdir", workdir, src],
                   capture_output=True, timeout=180, check=True)
    return os.path.join(workdir, po["file"])


def put(local, remote_dir):
    sh(f"mkdir -p {remote_dir}")
    subprocess.run(["docker", "cp", local, f"{CONTAINER}:{remote_dir}/"], check=True)
    subprocess.run(["docker", "exec", "-u", "root", CONTAINER, "chown", "-R", "1001:1001", remote_dir], check=True)
    return f"{remote_dir}/{os.path.basename(local)}"


def gui(cmd):
    r = sh(cmd, timeout=120)
    if r.returncode:
        raise SystemExit(f"gui failed ({cmd}): {r.stdout[-400:]} {r.stderr[-400:]}")


def matches(saved, po, warehouse):
    lines = [(l["product"], l["qty"]) for l in saved.get("lines", [])]
    return (saved.get("customer") == po["customer"] and saved.get("ref") == po["ref"]
            and saved.get("warehouse") == warehouse and lines == [tuple(l) for l in po["lines"]])


def teach(session):
    """/learn the demonstration, answering each question as a person would by reading it."""

    before = send(f'/learn {session} name="Enter a customer PO"')
    asked = []
    # What a person teaching this says when asked what the task is: the
    # study step (a hosted brain) opens with its understanding, "is that
    # right?", and may ask questions of its own, worded differently each run.
    explanation = ("Yes. It keys a customer's purchase order into the sales app as a quotation. "
                   "The customer, the customer reference, and each line's product and quantity come "
                   "from the PO and change every time; a PO can have more or fewer lines than this one. "
                   "The warehouse is always Main. It is done when the page says Saved quotation.")
    for _ in range(20):
        entry = reply_after(before)
        text = entry.get("content") or ""
        asked.append(text)
        # A question pauses the task (kind query/approval); anything else ends it.
        if entry.get("kind") not in ("query", "approval"):
            return entry, asked

        # The last question also says when the task is finished, so it is matched first.
        if "That is everything I wanted to ask" in text or "What should I call it" in text:
            answer = "yes"
        elif "Which of them change" in text:
            # Everything but the warehouse changes with each PO; find its number in the question.
            number = next((m.group(1) for m in re.finditer(r"(\d+)\) [^;]*?into textbox \"(\w+)\"", text)
                           if m.group(2) == "Warehouse"), None)
            answer = f"all change except {number}" if number else "all change"
        elif "times in a row with different text" in text:
            answer = "each"
        elif "check that before I start" in text:
            answer = "no"
        elif "finished" in text:
            answer = "the page says Saved quotation"
        elif "same every time" in text:
            answer = "changes"
        elif "is that right" in text or "What I understood" in text:
            answer = explanation
        elif text.rstrip().endswith("?"):
            # One of the model's own questions: answered from what a person knows.
            answer = explanation
        else:
            raise SystemExit(f"a question this bench does not know how to answer:\n{text}")
        print(f"  Q: {text[:150]!r}\n  A: {answer}")
        before = send(answer)
    raise SystemExit("the lesson did not end")


def main():
    work = tempfile.mkdtemp(prefix="learn-docs-")
    try:
        print("== the app, and one demonstration of keying a PO into it")
        sh(f"mkdir -p ~/work && cat > ~/work/sales.html <<'EOF'\n{APP}EOF")
        sh("python3 -c \"from decurix_agent.computer import cdp; cdp.navigate('file:///home/headless/work/sales.html')\" "
           "; python3 - <<'EOF'\nfrom decurix_agent.computer import cdp\np=cdp.active_page()\n"
           "p.evaluate(\"localStorage.clear()\")\np.close()\nEOF")

        task_id = "t" + os.urandom(8).hex()
        sh(f"printf %s {task_id} > /tmp/decurix-current-task; printf %s 'Enter a customer PO' > /tmp/decurix-current-task.request")
        gui("gui open file:///home/headless/work/sales.html")
        gui('gui click "New quotation"')
        gui(f'gui type "Customer" "{DEMO["customer"]}"')
        gui(f'gui type "Customer Reference" "{DEMO["ref"]}"')
        gui(f'gui type "Warehouse" "{DEMO["warehouse"]}"')
        for product, qty in DEMO["lines"]:
            gui('gui click "Add a line"')
            gui(f'gui type "Product" "{product}"')
            gui(f'gui type "Quantity" "{qty}"')
            gui('gui click "Save line"')
        gui('gui click "Save quotation"')
        sh("printf %s '' > /tmp/decurix-current-task")
        session = f"task-{task_id}"
        demo = saved_quotations()
        check("the demonstration saved its quotation", demo and matches(demo[-1], DEMO, "Main"), demo)

        print("\n== teaching it (/learn)")
        entry, asked = teach(session)
        check("the lesson ended with a saved skill", entry.get("kind") not in ("query", "approval", "error")
          and "Learned" in (entry.get("content") or ""), entry.get("content"))
        grouped = [q for q in asked if "Which of them change" in q]
        check("the typed values were asked about in one question", len(grouped) == 1, asked)
        skill = json.loads(sh("cat ~/.decurix/skills/enter-a-customer-po.json").stdout or "{}")
        params = {p["label"]: p for p in skill.get("params", [])}
        print("  params:", {k: (v.get("varies"), v.get("list"), v.get("example")) for k, v in params.items()})
        check("the lines became list values", params.get("Product", {}).get("list") and params.get("Quantity", {}).get("list"))
        check("the warehouse stays as taught", params.get("Warehouse", {}).get("varies") is False)

        print("\n== B: /skill with a new PO's PDF after the command (what an email trigger sends)")
        pdf_b = put(po_pdf(PO_B, work), f"{HOME}/work/inbox/mjob-e2e-b")
        count = len(saved_quotations())
        before = send(f"/skill enter-a-customer-po\n\n--- The email that started this task ---\nFrom: purchasing@woodcorner.example\n"
                      f"Subject: PO {PO_B['ref']}\n\nFiles attached to the email, saved on this desktop:\n  {pdf_b}\n--- End of the email ---")
        entry = reply_after(before)
        print("  reply:", (entry.get("content") or "")[:600])
        saved = saved_quotations()
        check("a new quotation was saved", len(saved) == count + 1, saved[-1:] if saved else saved)
        check("with the PDF's customer, reference and every line, and the taught warehouse",
              saved and matches(saved[-1], PO_B, "Main"), saved[-1:] if saved else "")
        check("the report says where each value was read", "Values read from the documents" in (entry.get("content") or ""))
        check("nothing was asked", entry.get("kind") not in ("query", "approval", "error"), entry.get("kind"))
        if entry.get("kind") in ("query", "approval"):
            # A paused run takes the next request as its answer; end it before path A.
            reply_after(send("stop"))

        print("\n== A: a plain request to TheBot, with a third PO")
        pdf_a = put(po_pdf(PO_A, work), f"{HOME}/work/inbox/mjob-e2e-a")
        count = len(saved_quotations())
        before = send(f"A customer sent us a purchase order: {pdf_a}. Enter it in our sales app as a quotation.")
        entry = reply_after(before, timeout=1200)
        print("  reply:", (entry.get("content") or "")[:600])
        steps = steps_of(entry)
        saved = saved_quotations()
        check("the engine ran the taught procedure", any("run_skill" in s or "enter-a-customer-po" in s for s in steps), steps[-15:])
        check("a new quotation was saved", len(saved) == count + 1, saved[-1:] if saved else saved)
        check("with the third PO's values", saved and matches(saved[-1], PO_A, "Main"), saved[-1:] if saved else "")
    finally:
        shutil.rmtree(work, ignore_errors=True)

    print("\nEND TO END:", "OK" if not failures else "FAILED - " + ", ".join(failures))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
