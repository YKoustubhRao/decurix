# Use case: a customer's PO becomes a sales order in the ERP

## Who has this problem

A US industrial distributor whose customer-service reps re-key the purchase
orders customers email in. Field Fastener (a fastener distributor) is a public
example: "Customer orders received via email, phone or fax created the
challenge of dual entry, where Customer Service Representatives (CSRs) needed
to manually enter purchase orders into" their ERP - and they paid for a
dedicated product (Conexiom) to stop it
([case study](https://www.inddist.com/operations/article/13763824/case-study-conexiom-powers-field-fasteners-erp-system)).

Here the distributor is **Field Supply Co.** and its ERP is **ERPNext v16**
(open source, self-hosted, set up as a US company in USD). A customer's PO is a
one-page PDF; the job is a **Sales Order** with the customer's PO number, the
ship-by date, and one line per PO line at the price the PO says - not always
the list price (PO 4502 has a contract price). Decurix keys it and **saves it
as a draft**; a person reviews and submits it.

## What is here

| File | What |
|---|---|
| `erpnext/compose.yml` | frappe_docker's single-file ERPNext, on `127.0.0.1:8010`, and as `erpnext:8080` on `decurix-network` so a desktop can open it |
| `erpnext_po.py` | runs on the host, standard library only: `setup` (setup wizard, 3 customers, 6 items with list prices, the PO PDFs into `out/`), `check <PO>` (is it in ERPNext, line for line?), `list`, `reset` (delete draft orders) |

## Run it

```bash
docker compose -p erpnext -f usecases/po-entry/erpnext/compose.yml up -d   # ~3 min: creates the site
python3 usecases/po-entry/erpnext_po.py setup
```

Create a desktop, copy `out/PO-*.pdf` into its `~/Documents/Inbox`, and sign
its Chromium in to `http://erpnext:8080` once (Administrator / admin).

Record one PO (the screen's ticket toolbar, or `gui` commands under a task),
for example PO 4502: open `http://erpnext:8080/app/sales-order/new`, Delivery
Date, Customer + Enter, tick the empty first row and **Delete row**, then for
each PO line **Add row**, Item Code + Enter, Quantity, Rate; More Info >
Additional Info > Customer's Purchase Order; Ctrl+S. Teach it with `/learn`
(the three typed header values change; the line block is `each`), then:

```
/skill po-to-sales-order delivery_date="10-09-2026" customer="Northwind Machining" item_code="FSC-HHCS-1213X2;FSC-FW-12USS" quantity="12;6" rate_usd="18.40;9.75" customer_s_purchase_order="4501"
```

and `python3 usecases/po-entry/erpnext_po.py check 4501`.

## What was verified (2026-10-01, decurix-desktop on Colima, ERPNext v16.37.0)

- Recorded PO 4502 (Rockford Tool & Die, 3 lines, a contract price) once.
  `/learn` asked 4 questions and made a 17-step skill whose line block runs
  once per item.
- Ran it, with no model, for **PO 4501** (another customer, 2 lines) and
  **PO 4503** (a third customer, 1 line): both drafts matched their PO line for
  line (`check` ok: items, quantities, prices, ship-by date, PO number).
- Each run asks a person to confirm at the end: 3-4 key presses (Enter to pick
  a suggestion, Tab, Ctrl+S) leave nothing a replay can check without a model.

## What it found in Decurix (fixed on this branch)

- **A date field that throws typed text away** (`cdp.type_text` -> `core`):
  ERPNext's date picker empties its input after text arrives in one piece, so
  every "type 10-12-2026 into Delivery Date" reported success, left the field
  empty, and the order could not be saved. The text is now checked after the
  page settles and, when it was lost, typed again with real keys.
- **"Leave site?"** (`cdp._navigate_or_refuse_to_leave`): ERPNext asks it of any
  unsaved form; `open` hung for 20 s and left the dialog up. It now answers
  *stay* (nothing is thrown away) and says why.
- **Names a program makes up, in window titles** (`replay._stable_title`): a new
  form is titled "New Sales Order - new-sales-order-gwaniocbxa"; the skill
  waited for that exact title and would halt the next run at step 1.
- Found first on GnuCash (the desktop accounting app tried before ERPNext):
  nameless fields named after the label beside them (`atspi.name_fields_by_layout`),
  and a field typed the same on every loop item stays fixed (`learning._apply_loop`).

## Not done

- Reading the PO PDF into the run's values. Today a person (or an intake step)
  supplies them; `pdftotext` reads these PDFs cleanly, so a per-customer parser
  or a model could fill the `/skill` request.
- Submitting the order: left to a person on purpose. "Submit" is not in the
  approval gate's list (it would gate every web form's Submit button).
- An item code or customer that ERPNext does not know: the suggestion list
  would be empty and Enter would pick nothing; not tried.
