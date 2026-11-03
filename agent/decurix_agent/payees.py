"""
Where a payment's bank details come from.

Found live (2026-10-20, round 5, procure-to-pay): an email "URGENT: Change of bank account for this payment run", sent
from a lookalike of the vendor's domain, asked for "all payments due to us in this week's payment run" to go to a new
account. The worker's payment proposal paid that vendor's two bills to the account in the email, not the one in the
vendor master, and asked the person to approve the run. Nothing flagged it: one approval, and real money goes to a
fraudster.

The rule, decided in code where it is precise: a bank account, IFSC/SWIFT/IBAN or UPI id that a produced payment file
pays to must be in a record input (a master: a workbook or CSV the person gave) for the same payee. One found only in a
message (an email, a chat, a note) is never used - the work is not done, and the person is told loudly - unless the
request itself gives messages as the source of payment details (the planner's reading, declare_message_details) and no
record holds an account for that payee: then the person approves it, shown the account and the message it came from
(message_payments). A message that would change an account the records already hold is a change request, always. A
message whose sender's domain differs from the one on record for the party it claims to be is reported as suspicious,
used or not. Nothing here knows any application, vendor or bank.
"""

import email
import email.policy
import os
import re
from pathlib import Path

from decurix_agent import attachments, config
from decurix_agent.logging_utils import log
from decurix_agent.stage_facts import _number as _money

RECORD_KINDS = {".xlsx", ".xlsm", ".csv", ".tsv"}
MESSAGE_KINDS = {".eml", ".msg", ".txt"}

ACCOUNT = re.compile(r"(?<![\d])\d{9,18}(?![\d])")
IFSC = re.compile(r"\b[A-Z]{4}0[A-Z0-9]{6}\b")
IBAN = re.compile(r"\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b")
SWIFT = re.compile(r"\b[A-Z]{6}[A-Z0-9]{2}(?:[A-Z0-9]{3})?\b")
UPI = re.compile(r"\b[\w.-]{2,}@[a-z]{2,}\b(?![.\w])")
# Words of an account or a bank identifier (a cell that holds one is not a payee's name). Which columns hold where
# money goes is bank_columns'.
ID_COLUMN = re.compile(r"\b(?:a/?c|acc(?:oun)?t|acc|account\s*(?:no|number|#)?|ifsc|iban|swift|bic|upi|vpa|routing|sort\s*code)\b",
                       re.I)
# A column naming who is paid.
PAYEE_COLUMN = re.compile(r"\b(?:vendor|supplier|payee|beneficiary|party|customer|employee|name|account\s*name)\b", re.I)
MAX_ROWS = 5000

# Review 2026-11-03: any table with an "account" column and a column of figures was taken for a payment file, so a
# sales report with customers' bank accounts, a ledger's "GL Account" or a stock table's "Stock Account" drew "PAYEES
# NOT CHECKED". A payment file is a table of money going out to bank details: a column of bank details and a column of
# what is paid.
# A column of bank details by its own name.
_BANK_DETAIL = re.compile(r"\b(?:ifsc|iban|swift|bic|micr|upi|vpa|routing|sort\s*code)\b", re.I)
# An account column that is a ledger's, not a bank's: "GL Account", "Account Head", "Stock Account", "Account Name".
_LEDGER_ACCOUNT = re.compile(r"\b(?:gl|g/l|ledger|head|group|code|name|type|category|class|stock|expense|income|revenue"
                             r"|sales|purchases?|cost|control|sub|parent|nature|description|holder|opening|closing"
                             r"|balance)\b", re.I)
# ("Acc" too: round-4 recheck, a column headed "Acc" was no account, so a wrong one went unchecked.)
_ACCOUNT_WORD = re.compile(r"\b(?:a/?c|acc(?:oun)?t|acc|account)\b", re.I)
# Who an account is for, or a bank, in its own label: "Bank Account", "Beneficiary A/c".
_BANK_ACCOUNT = re.compile(r"\b(?:bank|beneficiary|payee|vendor|supplier|employee|party|creditor)\b", re.I)
_ACCOUNT_NUMBER = re.compile(r"\b(?:no|number|num|#)\b|#", re.I)
# A column of what is paid out, by its last word ("Amount Payable (Rs)", "Net Pay", "Total") or by a word of paying
# anywhere in it ("Payment Value") - never a measure of something else ("Total Sales", "Taxable Value", "GST Amount").
_PAID_HEAD = {"amount", "amt", "payable", "payment", "pay", "paid", "transfer", "refund", "remittance", "disbursement",
              "net", "total"}
_PAID_WORD = re.compile(r"\b(?:pay|payment|payable|paid|transfer|remit(?:tance)?|refund|disburse(?:ment)?)\b", re.I)
# A column of money going back out to whoever is in the table: a customer refunded is a customer paid.
_OUTGOING = re.compile(r"\b(?:refunds?|refunded|disburse(?:d|ments?)?|payouts?|reimburse(?:d|ments?)?|remit(?:ted|tances?)?"
                       r"|transfer(?:red|s)?)\b", re.I)
_NOT_PAID = re.compile(r"\b(?:sales|revenue|turnover|taxable|tax|gst|cgst|sgst|igst|cess|stock|opening|closing|limit"
                       r"|budget)\b", re.I)


def _paid_column(label):
    """Whether a column's label names an amount paid out."""

    text = re.sub(r"\([^)]*\)|\[[^\]]*\]", " ", str(label or "")).lower()
    words = re.findall(r"[a-z]+", text)
    if not words or _NOT_PAID.search(text):
        return False
    # Money going back out counts too (round-5 recheck: "Employee, Acc, Reimbursement" was no payment file).
    return words[-1] in _PAID_HEAD or _PAID_WORD.search(text) is not None or _OUTGOING.search(text) is not None


def bank_columns(header):
    """The columns of a table's header that hold bank details: an IFSC, IBAN, SWIFT, UPI... column by its name, and an
    account column that is a bank's - its label says so ("Bank Account", "Beneficiary A/c"), or the table has a
    column of bank details or a bank's name beside it, or it is an account number ("Account No") in a table that names
    a payee. A ledger's account ("GL Account", "Account Head", "Account Name") is never one."""

    labels = [str(h or "") for h in header]
    has_bank = any(_BANK_DETAIL.search(h) or re.search(r"\b(?:bank|branch)\b", h, re.I) for h in labels)
    has_payee = any(PAYEE_COLUMN.search(h) or re.search(r"\bholder\b", h, re.I) for h in labels)
    # Someone who is paid, not only billed: a customer's account beside sales figures is a report's, not a payment's.
    # A customer beside a refund is paid (round-4 recheck: "Customer, Account, Refund" was no payment file).
    pays_someone = any((PAYEE_COLUMN.search(h) or re.search(r"\bholder\b", h, re.I))
                       and not re.search(r"\bcustomers?\b", h, re.I) for h in labels) or (
        has_payee and any(_OUTGOING.search(h) for h in labels))
    cols = []
    for i, h in enumerate(labels):
        if _BANK_DETAIL.search(h):
            cols.append(i)
        elif _ACCOUNT_WORD.search(h) and not _LEDGER_ACCOUNT.search(h):
            if (_BANK_ACCOUNT.search(h) or has_bank or (has_payee and _ACCOUNT_NUMBER.search(h))
                    or (pays_someone and _bare_account(h))):
                cols.append(i)
    return cols


_BARE_ACCOUNT_WORDS = {"a", "c", "ac", "acc", "acct", "account", "no", "number", "num"}


def _bare_account(label):
    """Whether a label is an account and nothing else ("A/c", "Acct", "Account", "Account No"). Beside a column of who
    is paid it is that payee's bank account (round-3 recheck: "Payee, A/c, Amount" and "Party, Account, Amount" were no
    longer read as payment files, so a wrong account went through unchecked)."""

    words = re.findall(r"[a-z]+", str(label or "").lower())
    return bool(words) and all(w in _BARE_ACCOUNT_WORDS for w in words)


def payment_table(header):
    """(bank columns, payee column or None) when a table's header is a payment file's - bank details and an amount
    paid out - else None."""

    cols = bank_columns(header)
    if not cols or not any(_paid_column(h) for i, h in enumerate(header) if i not in cols):
        return None
    payee_col = next((i for i, h in enumerate(header) if PAYEE_COLUMN.search(h or "") and i not in cols
                      and not _ACCOUNT_WORD.search(h or "")), None)
    if payee_col is None:
        payee_col = next((i for i, h in enumerate(header) if PAYEE_COLUMN.search(h or "") and i not in cols), None)
    return cols, payee_col


# Words of a name that say what kind of body it is, not which one: "ABC Traders Pvt Ltd" is "ABC Traders".
_LEGAL_FORM = {"pvt", "private", "ltd", "limited", "llp", "inc", "llc", "co", "company", "corp", "corporation", "the",
               "and"}


def _name_tokens(text):
    """A party's name as whole lower-case words, its legal form ("Pvt Ltd", "M/s"), "&" and a bracket's words (a
    branch, a city) left out."""

    text = re.sub(r"(?i)\bm/s\b\.?", " ", str(text or ""))
    text = re.sub(r"\([^)]*\)|\[[^\]]*\]", " ", text)
    return [w for w in re.findall(r"[a-z0-9]+", text.lower()) if w not in _LEGAL_FORM]


def names_payee(payee, row):
    """Whether a record's row (its cells joined by " | ") names the payee: one of its cells is the payee's name, word
    for word, its legal form aside ("M/s Shree Ganesh Packaging Pvt Ltd" is "Shree Ganesh Packaging"). Found in review
    (2026-11-02): matched as a substring, "Ram" was on record wherever "Ramesh" was; and (2026-11-03) as a run of
    words, "Ram" was on record wherever "Sita Ram Traders" was and "ABC Pvt Ltd" wherever "ABC Traders" was - another
    party's account passed for the payee's. A name written another way is not this payee: it is asked about, never
    passed."""

    wanted = _name_tokens(payee)
    if not wanted:
        return False
    return any(_name_tokens(cell) == wanted for cell in str(row or "").split(" | "))


def _named_in_text(party, text):
    """Whether free text (a message) names a party: its whole words, in order, somewhere in it."""

    wanted = _name_tokens(party)
    if not wanted:
        return False
    words = _name_tokens(text)
    n = len(wanted)
    return any(words[i:i + n] == wanted for i in range(len(words) - n + 1))


def _identifiers(text, column=""):
    """Account-like identifiers in a cell: digit runs of 9-18 digits, IFSC, IBAN, SWIFT/BIC (in such a column), UPI."""

    text = str(text if text is not None else "")
    found = set(IFSC.findall(text)) | set(IBAN.findall(text)) | set(ACCOUNT.findall(text.replace(" ", "")))
    if re.search(r"swift|bic", column or "", re.I):
        found |= set(SWIFT.findall(text))
    if re.search(r"upi|vpa", column or "", re.I):
        found |= set(UPI.findall(text))
    return found


def _cell_text(value):
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return "" if value is None else str(value)


def _tables(path, report=False):
    """[(header, [row, ...])] of a workbook's sheets or a CSV, every cell as text; [] when it cannot be read."""

    from decurix_agent import sources
    try:
        rows = sources._rows(path)
    except Exception as exc:  # noqa: BLE001 - a file that cannot be read gives nothing
        if report:
            log(f"payees: could not read {path}: {exc}")
        return []
    tables, current = [], []
    for row in rows + [None]:
        if row is None:
            if current:
                tables.append(current)
            current = []
            continue
        current.append([_cell_text(c) for c in row])
    out = []
    for table in tables:
        header_at = next((i for i, row in enumerate(table[:10]) if sum(1 for c in row if c.strip()) >= 2), None)
        if header_at is None:
            continue
        out.append((table[header_at], table[header_at + 1:header_at + 1 + MAX_ROWS]))
    return out


def record_identifiers(path):
    """{identifier: {row text}} of a record input (a master): every account-like id in its id columns, with its row."""

    out = {}
    for header, rows in _tables(path, report=True):
        id_cols = bank_columns(header)
        if not id_cols:
            continue
        for row in rows:
            text = " | ".join(c for c in row if c)
            for i in id_cols:
                if i < len(row):
                    for ident in _identifiers(row[i], header[i]):
                        out.setdefault(ident, set()).add(text)
    return out


def _message(path):
    """(subject, sender address, sender display name, body text) of a message input."""

    path = Path(path)
    if path.suffix.lower() == ".eml":
        try:
            msg = email.message_from_bytes(path.read_bytes(), policy=email.policy.default)
            body = msg.get_body(preferencelist=("plain", "html"))
            text = body.get_content() if body is not None else ""
            name, address = email.utils.parseaddr(str(msg.get("From") or ""))
            return str(msg.get("Subject") or path.name), address.lower(), name, text
        except Exception as exc:  # noqa: BLE001
            log(f"payees: could not read {path}: {exc}")
            return path.name, "", "", ""
    try:
        return path.name, "", "", path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return path.name, "", "", ""


def inputs(request, made=(), roots=None):
    """
    The request's inputs, by kind: (records, messages) as paths. Its attachments, and the files already in the work
    folder (and the shared folder) that this task did not make - a request often says only "the files are in the X
    folder" - each a master (workbook, CSV) or a message (an email, a chat or a note).
    """

    candidates, made_paths, began = _candidates(request, made, roots)
    records, messages, seen = [], [], set()
    for path in candidates:
        try:
            key = str(path.resolve()).lower()
            if key in seen or key in made_paths or not path.is_file():
                continue
            if began is not None and path.stat().st_mtime > began + 1:
                continue  # written during this task: an output, not an input
        except OSError:
            continue
        seen.add(key)
        suffix = path.suffix.lower()
        if suffix in RECORD_KINDS:
            records.append(str(path))
        elif suffix in MESSAGE_KINDS:
            messages.append(str(path))
    return records, messages


def _candidates(request, made, roots):
    """(the request's attachments and the files under its folders, the made paths as keys, when the task began)."""

    from decurix_agent import produced
    made_paths = {str(Path(str(m)).resolve()).lower() for m in made}
    began = produced.since()
    candidates = [Path(p) for p in attachments.files(request or "")]
    for root in roots if roots is not None else named_folders(request, _default_roots()):
        candidates += list(produced._walk(root))
    return candidates, made_paths, began


def _message_identifiers(messages):
    """{identifier: subject of the first message whose body holds it}."""

    in_messages = {}
    for message in messages:
        subject, *_rest, body = _message(message)
        for ident in _identifiers(body):
            in_messages.setdefault(ident, subject)
    return in_messages


def _payment_rows(path):
    """[(payee, identifier, where)] for the rows of a produced payment file that pay to an identifier."""

    out = []
    for header, rows in _tables(path, report=True):
        table = payment_table(header)
        if table is None:
            continue
        id_cols, payee_col = table
        for n, row in enumerate(rows, 2):
            payee = (row[payee_col] if payee_col is not None and payee_col < len(row) else
                     next((c for c in row if re.search(r"[A-Za-z]{3}", c or "") and not ID_COLUMN.search(c)), ""))
            for i in id_cols:
                if i < len(row):
                    for ident in _identifiers(row[i], header[i]):
                        out.append((payee.strip(), ident, f"{Path(path).name} row {n}"))
    return out


# Requests whose planner said messages are the source of payment details (normalised request text -> True).
_MESSAGE_DETAILS = {}


def _request_key(request):
    return " ".join(str(request or "").split())[:4000]


def declare_message_details(request, given):
    """Note whether this request gives messages as the source of payment details (long_task, from the planner)."""

    if given:
        _MESSAGE_DETAILS[_request_key(request)] = True
    else:
        _MESSAGE_DETAILS.pop(_request_key(request), None)


def mismatches(request, made, found=None):
    """
    Sentences for each payment a produced file makes to an identifier that is not on record for its payee - loudly, in
    plain words. [] when no produced file holds payments, or the inputs hold no master with such identifiers (nothing
    to check against: decided only where it is precise - and said, as not checked, by unchecked). A payment to details
    from a message the request gives as
    their source, for a payee no record holds an account for, is not here: it is message_payments', for approval.
    """

    return _payment_findings(request, made, found)[0]


def unchecked(request, made, found=None):
    """
    Sentences saying a produced file's payments could not be checked: it pays to bank details, and the inputs hold no
    record of payees' accounts (a master) to check them against. [] when no produced file holds payments or a master
    is there (mismatches then decides). Found in review (2026-11-02): with no master nothing was checked and nothing was
    said, so a payment run to any account read as checked. A payee no record confirms is "not checked", never passed.
    """

    files = [p for p in made if Path(str(p)).suffix.lower() in RECORD_KINDS]
    rows = [(path, r) for path in files for r in _payment_rows(path)]
    if not rows:
        return []
    records, _messages = found if found is not None else inputs(request, made)
    if any(record_identifiers(record) for record in records):
        return []
    names = list(dict.fromkeys(payee or "a payee" for _, (payee, _i, _w) in rows))
    where = ", ".join(dict.fromkeys(Path(str(path)).name for path, _ in rows))
    return [f"PAYEES NOT CHECKED: {where} pays {', '.join(names[:6])}{' and others' if len(names) > 6 else ''} to bank "
            "details, and no record the request gave (a vendor, employee or party master with their accounts) holds "
            "an account to check them against - where this money goes is not confirmed."]


def message_payments(request, made, found=None):
    """
    Sentences for each payment to details taken from a message, where the request itself gives messages as their
    source (declare_message_details) and no record holds an account for that payee: the person approves each, shown
    the account and the message. [] when the request gives no such source.
    """

    if not _MESSAGE_DETAILS.get(_request_key(request)):
        return []
    return _payment_findings(request, made, found)[1]


def _payment_findings(request, made, found=None):
    """(mismatches, for approval) - see mismatches and message_payments."""

    files = [p for p in made if Path(str(p)).suffix.lower() in RECORD_KINDS]
    if not files:
        return [], []
    records, messages = found if found is not None else inputs(request, made)
    on_record = {}
    for record in records:
        for ident, rows in record_identifiers(record).items():
            on_record.setdefault(ident, []).extend((Path(record).name, row) for row in rows)
    if not on_record:
        return [], []
    given = bool(_MESSAGE_DETAILS.get(_request_key(request)))
    in_messages = _message_identifiers(messages)
    def recorded_for(payee):
        """The identifiers the records hold for a payee, and the record file they are in."""
        found = [(ident, name) for ident, places in on_record.items() for name, row in places
                 if payee and names_payee(payee, row)]
        return sorted({i for i, _ in found}), sorted({n for _, n in found})

    wrong = {}
    for path in files:
        for payee, ident, where in _payment_rows(path):
            # The identifier exactly as on record (a dict key: never a part of one), for this payee by name.
            places = on_record.get(ident, [])
            if places and (not payee or any(names_payee(payee, row) for _, row in places)):
                continue
            key = (where, payee)
            entry = wrong.setdefault(key, {"idents": [], "message": "", "owner": ""})
            entry["idents"].append(ident)
            if places:
                entry["owner"] = entry["owner"] or f"{places[0][0]} has it for {places[0][1].split(' | ')[0]}"
            elif ident in in_messages:
                entry["message"] = entry["message"] or in_messages[ident]
    out, approve = [], []
    for (where, payee), entry in wrong.items():
        idents = " / ".join(dict.fromkeys(entry["idents"]))
        own, files_on_record = recorded_for(payee)
        on_file = (f"the account in {', '.join(files_on_record)} ({' / '.join(own)})" if own
                   else "an account on record for them")
        if entry["message"] and given and not own and not entry["owner"]:
            approve.append(f"PAYMENT DETAILS FROM A MESSAGE, FOR YOUR APPROVAL: {where} pays {payee or 'a payee'} to "
                           f"{idents}, taken from the message \"{entry['message']}\" (the request gives messages as the "
                           "source of these details; no record holds an account for this payee).")
        elif entry["message"]:
            out.append(f"PAYMENT TO AN ACCOUNT FROM A MESSAGE: {where} pays {payee or 'a payee'} to {idents}, taken from "
                       f"the message \"{entry['message']}\" - not {on_file}. A message never changes where money goes: "
                       "use the account on record and report the message to the person as a change request.")
        elif entry["owner"]:
            out.append(f"PAYMENT TO ANOTHER PARTY'S ACCOUNT: {where} pays {payee or 'a payee'} to {idents}; "
                       f"{entry['owner']}, not {on_file}.")
        else:
            out.append(f"PAYMENT TO AN ACCOUNT ON NO RECORD: {where} pays {payee or 'a payee'} to {idents}, which is in "
                       f"no record the request gave - not {on_file}.")
    return out, approve


def suspicious_messages(request, made=(), found=None):
    """
    Sentences for each input message that claims to be from a party on record but comes from another domain than the
    one on record for it: reported whether or not anything used it.
    """

    return [sentence for _path, _subject, sentence in _suspicious(found if found is not None else inputs(request, made))]


def _suspicious(found):
    """[(message path, subject, sentence)] for the suspicious messages among `found` (records, messages)."""

    records, messages = found
    domains = {}  # party name (lower) -> domains on record
    for record in records:
        for header, rows in _tables(record):
            mail_cols = [i for i, h in enumerate(header) if re.search(r"e-?mail", h or "", re.I)]
            name_col = next((i for i, h in enumerate(header) if PAYEE_COLUMN.search(h or "")), 0)
            for row in rows:
                if name_col >= len(row) or not row[name_col].strip():
                    continue
                for i in mail_cols:
                    if i < len(row) and "@" in row[i]:
                        domains.setdefault(row[name_col].strip().lower(), set()).add(row[i].split("@", 1)[1].strip().lower())
    out, seen = [], set()
    for message in messages:
        subject, sender, display, body = _message(message)
        if "@" not in sender or (subject, sender) in seen:
            continue  # the same message twice (a copy in another folder) is one warning
        seen.add((subject, sender))
        domain = sender.split("@", 1)[1]
        text = " ".join([display or "", body or ""]).lower()
        for party, known in domains.items():
            if party and _named_in_text(party, text) and domain not in known:
                out.append((message, subject,
                            f"The message \"{subject}\" says it is from {party.title()}, but it comes from {domain}; the "
                            f"record has {', '.join(sorted(known))} for them - treat it as suspicious and use the details "
                            "on record"))
                break
    return out


def payments_summary(request, made, found=None):
    """
    Per payee, the account a produced payment file pays to and where that account came from - for the approval of a
    payment run, so the person sees it. "" when no produced file holds payments.
    """

    files = [p for p in made if Path(str(p)).suffix.lower() in RECORD_KINDS]
    rows = [r for path in files for r in _payment_rows(path)]
    if not rows:
        return ""
    records, messages = found if found is not None else inputs(request, made)
    on_record = {}
    for record in records:
        for ident, texts in record_identifiers(record).items():
            on_record.setdefault(ident, (Path(record).name, texts))
    in_messages = _message_identifiers(messages)
    grouped = {}
    for payee, ident, _ in rows:
        if ident in on_record and (not payee or any(names_payee(payee, t) for t in on_record[ident][1])):
            source = f"from {on_record[ident][0]}"
        elif ident in in_messages:
            source = f"NOT ON RECORD - from the message \"{in_messages[ident]}\""
        else:
            source = "NOT ON RECORD"
        idents = grouped.setdefault((payee or "(no payee)", source), [])
        if ident not in idents:
            idents.append(ident)
    lines = [f"- {payee}: {' / '.join(idents)} ({source})" for (payee, source), idents in grouped.items()]
    return "Where each payment goes:\n" + "\n".join(lines)


def recent_outputs(roots=None):
    """The workbooks and CSVs written in the work folder since the task began (for the approval question's summary)."""

    from decurix_agent import produced
    began = produced.since()
    if began is None:
        return []
    out = []
    for root in roots if roots is not None else _default_roots():
        for path in produced._walk(root):
            try:
                if path.suffix.lower() in RECORD_KINDS and path.stat().st_mtime > began + 1:
                    out.append(str(path))
            except OSError:
                continue
    return out


# ============================================================
# What a bill charges, against what a result says is payable on it
#
# Found live (2026-10-20, round 5): every bill's "amount payable" was its taxable value without tax - the vendor is
# paid what the bill charges, tax included - and the room under the cash cap let one more bill in. Given to the check
# as evidence (a payable equal to a bill's taxable value, beside the bill's total), never decided in code: a line short
# received, disputed or capped is rightly paid less.
# ============================================================

BILL_KINDS = {".pdf", ".xlsx", ".xlsm", ".docx", ".txt"}
_INVOICE_COLUMN = re.compile(r"\b(?:invoice|bill)\s*(?:no|number|#|ref(?:erence)?)?\b", re.I)
_PAYABLE_COLUMN = re.compile(r"\b(?:payable|to pay|amount payable|net payable|pay(?:ment)? amount)\b", re.I)
_REDUCED = re.compile(r"short|disput|cap|exception|partial|withh|less|reject|mismatch", re.I)
_TAXABLE = re.compile(r"(?:taxable\s+(?:value|amount)|sub\s*-?total)[^\d\n]*\n?\s*(?:rs\.?|inr|\u20b9)?\s*([\d,]+(?:\.\d+)?)", re.I)
_TOTAL = re.compile(r"(?:invoice\s+total|grand\s+total|total\s+amount|amount\s+due|bill\s+total|total\s*\((?:rs|inr)\.?\))"
                    r"[^\d\n]*\n?\s*(?:\(?rs\.?\)?|inr|\u20b9)?\s*([\d,]+(?:\.\d+)?)", re.I)


def _doc_text(path):
    from decurix_agent.tools import office
    try:
        return office.read_document(path).get("text") or ""
    except Exception:  # noqa: BLE001 - a machine without the PDF tool: a plain reader, or nothing
        if str(path).lower().endswith(".pdf"):
            try:
                import pypdf
                return "\n".join(page.extract_text() or "" for page in pypdf.PdfReader(str(path)).pages)
            except Exception:  # noqa: BLE001
                return ""
        return ""


def bill_documents(request, made=(), roots=None):
    """The bills among the inputs (documents the task did not make): pdf, docx, workbook or text, as paths."""

    candidates, made_paths, began = _candidates(request, made, roots)
    out = []
    for path in candidates:
        try:
            if (path.suffix.lower() in BILL_KINDS and path.is_file()
                    and str(path.resolve()).lower() not in made_paths
                    and (began is None or path.stat().st_mtime <= began + 1)):
                out.append(str(path))
        except OSError:
            continue
    return list(dict.fromkeys(out))


def bill_evidence(request, made, bills=None):
    """
    For the check: each produced workbook's payable on a bill that equals that bill's taxable value where the bill's
    total (with its taxes) is more, and the line was not short, disputed or capped. "" when none.
    """

    files = [p for p in made if Path(str(p)).suffix.lower() in {".xlsx", ".xlsm", ".csv"}]
    if not files:
        return ""
    texts = {}
    for bill in bills if bills is not None else bill_documents(request, made):
        text = _doc_text(bill)
        if text:
            texts[bill] = text
    if not texts:
        return ""
    lines = []
    for path in files:
        for header, rows in _tables(path):
            inv = next((i for i, h in enumerate(header) if _INVOICE_COLUMN.search(h or "")), None)
            pay = next((i for i, h in enumerate(header) if _PAYABLE_COLUMN.search(h or "")), None)
            if inv is None or pay is None:
                continue
            per_bill, reduced = {}, set()
            for row in rows:
                if inv >= len(row) or pay >= len(row) or not row[inv].strip():
                    continue
                value = _money(row[pay])
                if value is None:
                    continue
                per_bill[row[inv].strip()] = per_bill.get(row[inv].strip(), 0.0) + value
                if any(_REDUCED.search(c or "") for i, c in enumerate(row) if i not in (inv, pay)):
                    reduced.add(row[inv].strip())
            for number, payable in per_bill.items():
                if number in reduced:
                    continue
                bill_text = next((t for t in texts.values() if number in t), "")
                taxable = _TAXABLE.search(bill_text)
                total = _TOTAL.search(bill_text)
                if not taxable or not total:
                    continue
                taxable, total = _money(taxable.group(1)), _money(total.group(1))
                if taxable is None or total is None or total <= taxable + 0.5:
                    continue
                if abs(payable - taxable) <= 0.5:
                    lines.append(f"{Path(path).name}: payable on {number} is {payable:,.2f}, which equals the bill's "
                                 f"taxable value; the bill's total, with its tax, is {total:,.2f}")
    if not lines:
        return ""
    return ("--- what is payable against what the bills charge (read in code: a vendor is paid what its bill charges, tax "
            "included, unless the request says otherwise)\n" + "\n".join(dict.fromkeys(lines)))


# A question asking the person to approve paying (a payment run, a transfer, a refund).
PAYMENT_APPROVAL = re.compile(r"\b(?:approv\w*|release|authori[sz]\w*|sign off)\b[\s\S]{0,120}\b(?:payments?|pay(?:ment)?\s+run|"
                              r"transfers?|refunds?|remittances?)\b|\bpayment run\b", re.I)


def annotate_approval(question, request=None, made=None):
    """
    A question that asks to approve a payment run, with where each payment goes and where each account came from
    appended (payments_summary); any other question as it is.
    """

    if not PAYMENT_APPROVAL.search(question or "") or "Where each payment goes:" in (question or ""):
        return question
    if request is None:
        from decurix_agent import long_task, state
        request = (long_task._run or {}).get("request") or state.current_task or ""
    if made is None:
        from decurix_agent import shared
        roots = [config.WORK_DIR] + ([shared.link_path()] if shared.active() else [])
        made = recent_outputs(roots)
    summary = payments_summary(request, made)
    return f"{question}\n\n{summary}" if summary else question


# Folder names a request uses for places rather than for its own files: never taken as "the folder it names" (the
# attachments' paths say "work" and "inbox"; "(IDs in documents)" is a subfolder of the folder it does name).
GENERIC_FOLDERS = {"inbox", "documents", "desktop", "downloads", "work", "outputs", "output", "files", "data", "tmp",
                   "temp", "home", "headless", "decurix-reports", "shared"}
_PATH = re.compile(r"(?:(?<![\w])/|\b[A-Za-z]:\\)\S+")


def named_folders(request, roots):
    """
    The folders under `roots` (two levels down) that the request names ("the files are in the X folder"). When it
    names none: [] for a request that brings its own files (they are its inputs, with nothing else), `roots` for one
    that brings none. A work folder holds other requests' files too, and another request's master or email is not
    this one's input. Found live (round 5, 2026-10-20): P3's folder and nine copies of P2's uploads sat beside the
    payment run's folder; then P4's report warned about P3's email - the attachments' own paths ("work", "inbox")
    had been read as folders the request named.
    """

    text = _PATH.sub(" ", request or "")
    # A folder of the person's computer ("Documents\Decurix demo\process\P4 on my PC") is not a work-folder name:
    # its parts matched the shared folder's "process", which holds every task's files (round 5 P4, 2026-10-20: the
    # report warned about P3's and P6's emails).
    text = re.sub(r"\S*\\\S*", " ", text)
    words = {w.lower() for w in re.findall(r"[\w.-]{4,}", text)} - GENERIC_FOLDERS
    found = []
    for root in roots:
        level = [Path(root)]
        for _ in range(2):
            below = []
            for folder in level:
                try:
                    below += [Path(c.path) for c in os.scandir(folder) if c.is_dir() and not c.name.startswith(".")
                              and c.name.lower() not in GENERIC_FOLDERS - {"documents"}]
                except OSError:
                    continue
            found += [f for f in below if f.name.lower() in words]
            level = below
    if found:
        return found
    return [] if attachments.files(request or "") else list(roots)


def _default_roots():
    """Where a request's files are: the work folder, and the shared folder of the person's computer when there is one."""

    from decurix_agent import shared
    try:
        return [config.WORK_DIR] + ([shared.link_path()] if shared.active() else [])
    except Exception:  # noqa: BLE001
        return [config.WORK_DIR]


# ============================================================
# Where an email goes
# ============================================================
# Found live (round 5 P3, 2026-10-20): the four dispute drafts went to addresses the model made up from the vendors'
# names (accounts@gujaratpantrysupplies.example) - the vendor master had accounts@gujaratpantry.example.in. Like a bank
# account, an address comes from the records: one found in no input is decided in code.

ADDRESS = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def _addresses(text):
    return {a.lower().rstrip(".") for a in ADDRESS.findall(text or "")}


def known_addresses(request, made=(), found=None, documents=None, pages=None):
    """
    {address: (file, party or "")} for every email address the request's inputs hold: the request itself, the record
    inputs' cells (with the row's party), the messages' headers and bodies, the input documents' text, and the system's
    own pages the tools read during this task (`pages`; by default this task's, as provenance.gather has them).
    """

    records, messages = found if found is not None else inputs(request, made)
    known = {a: ("your request", "") for a in _addresses(request)}
    for record in records:
        for header, rows in _tables(record):
            name_col = next((i for i, h in enumerate(header) if PAYEE_COLUMN.search(h or "")), 0)
            for row in rows:
                party = row[name_col].strip() if name_col < len(row) else ""
                for cell in row:
                    for a in _addresses(cell):
                        known.setdefault(a, (Path(record).name, party))
    for doc in documents if documents is not None else bill_documents(request, made):
        if Path(doc).suffix.lower() in RECORD_KINDS:
            continue
        for a in _addresses(_doc_text(doc)):
            known.setdefault(a, (Path(doc).name, ""))
    # A suspicious message (payees.suspicious_messages) vouches for no address: one found only there - its sender,
    # reply-to or an address in its body - is marked with that message's subject, and a draft to it is not done.
    flagged = {str(path): subject for path, subject, _ in _suspicious((records, messages))}
    for message in sorted(messages, key=lambda m: str(m) in flagged):
        try:
            raw = Path(message).read_bytes().decode("utf-8", errors="replace")
        except OSError:
            continue
        tainted = _sender_side(message) if str(message) in flagged else set()
        for a in _addresses(raw):
            known.setdefault(a, (Path(message).name, "", flagged[str(message)] if a in tainted else ""))
    if pages is None:
        try:
            from decurix_agent import long_task
            pages = long_task._page_notes()
        except Exception:  # noqa: BLE001 - nothing read is nothing known
            pages = []
    for text in pages or []:
        for a in _addresses(text):
            known.setdefault(a, ("the system's pages", ""))
    return known


def _sender_side(path):
    """The addresses a message's sender controls: From, Reply-To, Sender, Return-Path, and those in its body."""

    try:
        msg = email.message_from_bytes(Path(path).read_bytes(), policy=email.policy.default)
        heads = " ".join(str(msg.get(h) or "") for h in ("From", "Reply-To", "Sender", "Return-Path"))
        return _addresses(heads) | _addresses(_message(path)[3])
    except Exception:  # noqa: BLE001 - a message that cannot be parsed: everything in it is the sender's
        try:
            return _addresses(Path(path).read_bytes().decode("utf-8", errors="replace"))
        except OSError:
            return set()


def _parties(known):
    """{party: [(address, file)]} of the addresses known_addresses found on a party's row."""

    parties = {}
    for address, (where, party, *_rest) in known.items():
        if party:
            parties.setdefault(party, []).append((address, where))
    return parties


def unknown_recipients(request, drafts, made=(), known=None):
    """
    One plain sentence for each email draft recipient (To, Cc) that is in no input - with the address the records hold
    for the party the draft is about, when there is one. [] when there are no drafts, or the inputs hold no address
    at all (nothing to check against).
    """

    if not drafts:
        return []
    known = known if known is not None else known_addresses(request, made)
    if not known:
        return []
    parties = _parties(known)
    out = []
    for draft in drafts:
        subject = str(draft.get("subject") or "")
        text = f"{subject}\n{draft.get('body') or ''}".lower()
        for recipient in list(draft.get("to") or []) + list(draft.get("cc") or []):
            for address in _addresses(str(recipient)):
                entry = known.get(address)
                suspicious = entry[2] if entry and len(entry) > 2 else ""
                if entry and not suspicious:
                    continue
                about = [(p, mails) for p, mails in parties.items() if len(p) >= 4 and p.lower() in text]
                hint = "; ".join(f"{mails[0][1]} has {', '.join(sorted({m for m, _ in mails}))} for {p}"
                                 for p, mails in about[:2])
                where = (f"which comes only from the suspicious message \"{suspicious}\"" if suspicious
                         else "which is in no file you gave me")
                out.append(f"the draft \"{subject[:80]}\" goes to {address}, {where}"
                           + (f"; {hint}" if hint else "") + ". An address is looked up in the records, never made up.")
    return out


TEXT_KINDS = {".csv", ".tsv", ".xlsx", ".xlsm", ".txt", ".md"}


def unknown_addresses_in_files(request, made, known=None):
    """
    One plain sentence for each made table or text file that lists an email address found in no input - a log of
    recipients typed apart from the drafts. Found live (round 5 P1, 2026-10-20): the drafts went to the master's
    addresses, the stage's recipients CSV listed made-up ones (accounts@coromandelclinics.in). [] when the inputs hold
    no address at all.
    """

    files = [p for p in made if Path(str(p)).suffix.lower() in TEXT_KINDS]
    if not files:
        return []
    known = known if known is not None else known_addresses(request, made)
    if not known:
        return []
    parties = _parties(known)
    out = []
    for path in files:
        try:
            if Path(str(path)).suffix.lower() in RECORD_KINDS:
                rows = [" | ".join(row) for _header, body in _tables(path) for row in body]
            else:
                rows = Path(str(path)).read_text(encoding="utf-8", errors="replace").splitlines()
        except Exception:  # noqa: BLE001 - a file that cannot be read decides nothing here
            continue
        wrong = []
        for row in rows:
            for address in sorted(_addresses(row)):
                entry = known.get(address)
                if entry and not (len(entry) > 2 and entry[2]):
                    continue
                about = [(p, mails) for p, mails in parties.items() if len(p) >= 4 and p.lower() in row.lower()]
                hint = (f" ({about[0][1][0][1]} has {', '.join(sorted({m for m, _ in about[0][1]}))} for {about[0][0]})"
                        if about else "")
                wrong.append(address + hint)
        if wrong:
            out.append(f"{Path(str(path)).name} lists {'; '.join(dict.fromkeys(wrong))}, which is in no file you gave "
                       "me - write a list of recipients from what was drafted (office drafts), never typed")
    return out


# ============================================================
# A recipient given as a party of the records
# ============================================================
# Found live (round 5 P1, 2026-10-20): told to look addresses up, the model still typed real-looking ones
# (accounts@coromandelclinics.in) and lost a whole attempt each time. The draft tool takes the party and the records
# file instead, and the address comes from the file.


class PartyError(Exception):
    """A party whose address the records do not give one way: in words for the model."""


def _party_key(text):
    return re.sub(r"[^a-z0-9]+", " ", str(text or "").lower()).strip()


def party_address(name, records=None, request=""):
    """
    The one email address the records hold for a party, by its name (case and punctuation aside; a name that is part
    of one row's name when none is equal). `records` is the file to look in (a workbook or CSV), or the request's record
    inputs when None. Raises PartyError when no row, no address, or more than one address answers.
    """

    files = [str(records)] if records else inputs(request)[0]
    if records and not Path(str(records)).is_file():
        raise PartyError(f"there is no records file {records}")
    want = _party_key(name)
    if not want:
        raise PartyError("give the party's name as the records write it")
    exact, partial = {}, {}
    for path in files:
        for header, rows in _tables(path, report=True):
            name_col = next((i for i, h in enumerate(header) if PAYEE_COLUMN.search(h or "")), 0)
            for row in rows:
                who = row[name_col].strip() if name_col < len(row) else ""
                mails = sorted({a for cell in row for a in _addresses(cell)})
                key = _party_key(who)
                if not key or not mails:
                    continue
                target = exact if key == want else partial if (want in key or key in want) and len(want) >= 4 else None
                if target is not None:
                    for mail in mails:
                        target.setdefault(mail, (Path(path).name, who))
    found = exact or partial
    where = Path(str(records)).name if records else "the records you were given"
    if not found:
        raise PartyError(f"{where} has no row for \"{name}\" with an email address - check the name as the records "
                         "write it, or ask the person for the address")
    if len(found) > 1:
        options = "; ".join(f"{mail} ({who}, {file})" for mail, (file, who) in sorted(found.items()))
        raise PartyError(f"\"{name}\" answers to more than one address in {where}: {options} - give the exact name of "
                         "the row you mean, or that row's address with --to")
    return next(iter(found))


def typed_recipients_refused(request, to, subject, body, known=None):
    """
    Why a draft whose typed recipients are in no input is refused at creation, or "" (all found, or the inputs hold no
    address at all). Found live (round 5 P1): made-up addresses were caught only at the stage's check, an attempt lost
    each time.
    """

    found = unknown_recipients(request, [{"to": list(to), "subject": subject, "body": body}], known=known)
    if not found:
        return ""
    return (" ".join(found) + " Give the recipient as the party with --to-party \"<name as in the records>\" --records "
            "<the file that lists them>, or the address the records hold.")
