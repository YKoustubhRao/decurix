"""
make_learn_v1: writes the generated input files of the learn-v1 suite (the supplier bills as text-layer PDFs, the bank
statements as CSV and the bank books as workbooks), from the content below. Every firm, name, GSTIN, voucher and amount
is invented; the GSTINs carry a valid check character (computed below), so a reader that checks one finds it whole.

    py -3 evals/tools/make_learn_v1.py            # writes them under evals/suites/learn-v1/cases/
    py -3 evals/tools/make_learn_v1.py --check    # fails when a committed file differs from what this writes

The case documents (case.json, fixture.json) are written by hand: they hold what must be true afterwards, which is
the case author's to state, not this script's to derive. The expected reconciliations in the recon cases' graders are
the rows RECONS gives below, worked by hand against the rule the person teaches (equal amount, the description names the
entry's party, each entry used once).
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from make_office import xlsx_bytes  # noqa: E402
from make_pdf import pdf_bytes  # noqa: E402

SUITE = os.path.join(os.path.dirname(HERE), "suites", "learn-v1", "cases")

_GSTIN_CHARS = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"


def gstin(first14):
    """A GSTIN from its first 14 characters and the check character the GST network's mod-36 rule gives them."""

    assert len(first14) == 14, first14
    total = 0
    for i, ch in enumerate(first14):
        product = _GSTIN_CHARS.index(ch) * (2 if i % 2 else 1)
        total += product // 36 + product % 36
    return first14 + _GSTIN_CHARS[(36 - total % 36) % 36]


CLIENT = ("Corvel Bakehouse LLP", "14 Tannery Road, Hubballi", gstin("29AAMFC7712K1Z"))


def bill(vendor, address, vendor_gstin, number, dated, due, lines, rate_pct, extra=()):
    """
    A tax invoice to the client: its lines, the taxable value, the tax and the total. A supplier in the client's state
    (the GSTIN's first two digits) charges CGST and SGST at rate_pct each; one in another state charges IGST at twice
    that, the same amount in one line, so the total is the same either way.
    """

    taxable = sum(cents for _, cents in lines)
    tax = round(taxable * rate_pct / 100)
    total = taxable + 2 * tax
    out = [f"# {vendor}", address, f"GSTIN: {vendor_gstin}", "", "TAX INVOICE", "", f"Invoice number: {number}",
           f"Invoice date: {dated}", f"Due date: {due}", "",
           f"Bill to: {CLIENT[0]}, {CLIENT[1]}", f"GSTIN: {CLIENT[2]}", "", "Items:"]
    for desc, cents in lines:
        out.append(f"{desc}: {money(cents)}")
    if vendor_gstin[:2] == CLIENT[2][:2]:
        taxes = [f"CGST {rate_pct}%: {money(tax)}", f"SGST {rate_pct}%: {money(tax)}"]
    else:
        taxes = [f"IGST {2 * rate_pct}%: {money(2 * tax)}"]
    out += ["", f"Taxable value: {money(taxable)}"] + taxes + [f"Total due: {money(total)}", ""]
    out += list(extra)
    out += ["", "Thank you for your business."]
    return pdf_bytes(out), money(total)


def money(cents):
    """An amount in paise as the bills write it: thousands separated, two decimals (13,888.60)."""

    return f"{cents // 100:,}.{cents % 100:02d}"


# The five bills, in suite order. The cost centre is on none of them, and no name, word or earlier record implies it:
# the person teaches the firm's rule in the first case, a map from the supplier's GSTIN state code (its first two
# digits) to the firm's own cost-centre codes (29 to CC-14, 30 to CC-27, 33 to CC-52, any other to CC-90). The
# suppliers are in three states, so the rule is applied, not one answer remembered: bills 1, 3 and 4 are Karnataka's
# (29, CC-14), bill 2 is from Goa (30, CC-27) and bill 5 from Maharashtra (27, no code of its own: CC-90). Each bill
# also prints the client's GSTIN (29), which the rule does not read. The app's two earlier bills sit in CC-52, so
# copying the last bill's code is wrong too.
BILLS = {
    "learn-bill-1": lambda: bill(
        "Swiftlark Couriers", "7 Station Approach, Hubballi", gstin("29ABJFS4410Q1Z"), "SC-24117",
        "2026-04-03", "2026-05-03",
        [("Document courier, 42 consignments", 336000), ("Fuel surcharge", 24000)], 9),
    "learn-bill-2": lambda: bill(
        "Inkhaven Printers", "22 Market Lane, Margao, Goa", gstin("30AAGFI2287D1Z"), "IP-0782",
        "2026-04-09", "2026-05-09",
        [("Menu cards, 500", 275000), ("Price labels, 2,000", 110000), ("Design changes", 45000)], 9),
    "learn-bill-3": lambda: bill(
        "Harbourline Freight Carriers", "Plot 9, Truck Terminal, Belagavi", gstin("29AADCH6630R1Z"), "HF/2026/0419",
        "2026-04-17", "2026-05-17",
        [("Freight: flour, Belagavi to Hubballi, 2 trucks", 840000), ("Loading and unloading", 65000)], 9),
    # The invoice number is not on this copy: the right answer is to ask for it.
    "learn-bill-4": lambda: bill(
        "Mossgrove Cleaning Services", "5 Lake View Road, Hubballi", gstin("29AASFM9054J1Z"), "as per covering letter",
        "2026-04-24", "2026-05-08",
        [("Kitchen deep clean, 2 visits", 320000), ("Grease trap service", 140000), ("Cleaning consumables", 38000)], 9,
        ["Covering letter sent separately by email."]),
    "learn-bill-5": lambda: bill(
        "Tealbrook Cold Chain", "Unit 3, Food Park, Kolhapur, Maharashtra", gstin("27AAJCT3318N1Z"), "TC-55102",
        "2026-04-29", "2026-05-29",
        [("Freight: chilled van, 6 trips", 900000), ("Driver allowance", 180000), ("Toll charges", 52000),
         ("Waiting time, 3 hours", 45000)], 9),
}

# The five reconciliations: the statement's lines (date, description, amount as the bank writes it), the bank book's
# entries (date, voucher, party, amount), and the voucher each statement line matches ("" for none).
RECONS = {
    # Corvel Bakehouse, April. Two receipts of 12,000 on one day: the party decides; bank charges match nothing.
    "learn-recon-1": (
        [("2026-04-02", "NEFT CR HALDEN TRADERS", "18500.00", "RV-0412"),
         ("2026-04-05", "NEFT DR SWIFTLARK COURIERS", "-4248.00", "PV-0390"),
         ("2026-04-11", "UPI DR KETTLEWICK SUPPLIES", "-2124.00", "PV-0391"),
         ("2026-04-15", "NEFT CR ORCHARD CAFE CHAIN", "12000.00", "RV-0414"),
         ("2026-04-15", "NEFT CR MAPLEDOWN HOTELS", "12000.00", "RV-0413"),
         ("2026-04-30", "BANK CHARGES APR", "-118.00", "")],
        [("2026-04-01", "RV-0412", "Halden Traders", 18500),
         ("2026-04-04", "PV-0390", "Swiftlark Couriers", -4248),
         ("2026-04-10", "PV-0391", "Kettlewick Supplies", -2124),
         ("2026-04-14", "RV-0413", "Mapledown Hotels", 12000),
         ("2026-04-14", "RV-0414", "Orchard Cafe Chain", 12000),
         ("2026-04-28", "PV-0392", "Inkhaven Printers", -5074)]),
    # Corvel Bakehouse, May. April's uncleared payment clears; a part payment matches nothing; interest matches nothing.
    "learn-recon-2": (
        [("2026-05-03", "NEFT CR MAPLEDOWN HOTELS", "9600.00", "RV-0421"),
         ("2026-05-06", "NEFT DR INKHAVEN PRINTERS", "-5074.00", "PV-0392"),
         ("2026-05-09", "NEFT CR HALDEN TRADERS", "7500.00", ""),
         ("2026-05-14", "NEFT DR HARBOURLINE FREIGHT", "-10679.00", "PV-0401"),
         ("2026-05-20", "UPI DR TEALBROOK COLD CHAIN", "-13888.60", "PV-0402"),
         ("2026-05-27", "NEFT CR ORCHARD CAFE CHAIN", "4250.00", "RV-0423"),
         ("2026-05-31", "INTEREST CREDIT", "36.00", "")],
        [("2026-04-28", "PV-0392", "Inkhaven Printers", -5074),
         ("2026-05-02", "RV-0421", "Mapledown Hotels", 9600),
         ("2026-05-08", "RV-0422", "Halden Traders", 15000),
         ("2026-05-13", "PV-0401", "Harbourline Freight Carriers", -10679),
         ("2026-05-19", "PV-0402", "Tealbrook Cold Chain", -13888.6),
         ("2026-05-26", "RV-0423", "Orchard Cafe Chain", 4250),
         ("2026-05-30", "PV-0403", "Mossgrove Cleaning Services", -5876.4)]),
    # Pinecrest Dental Clinic, April: a second client, the same firm's layout. An amount that differs matches nothing.
    "learn-recon-3": (
        [("2026-04-03", "NEFT CR BRIARWOOD HEALTH INSURANCE", "22400.00", "RV-118"),
         ("2026-04-08", "NEFT DR LUMEN DENTAL SUPPLY", "-8650.00", "PV-207"),
         ("2026-04-12", "CARD SETTLEMENT 0412", "15325.50", "RV-119"),
         ("2026-04-16", "NEFT DR ASHFORD LAB WORKS", "-4200.00", ""),
         ("2026-04-25", "NEFT DR LUMEN DENTAL SUPPLY", "-3100.00", "PV-209")],
        [("2026-04-02", "RV-118", "Briarwood Health Insurance", 22400),
         ("2026-04-07", "PV-207", "Lumen Dental Supply", -8650),
         ("2026-04-12", "RV-119", "Card settlement", 15325.5),
         ("2026-04-15", "PV-208", "Ashford Lab Works", -4020),
         ("2026-04-24", "PV-209", "Lumen Dental Supply", -3100),
         ("2026-04-29", "PV-210", "Corran Property Rentals", -30000)]),
    # Corvel Bakehouse, June. Two receipts of 6,400 on one day in the other order; a returned cheque and charges match
    # nothing.
    "learn-recon-4": (
        [("2026-06-01", "NEFT DR MOSSGROVE CLEANING", "-5876.40", "PV-0403"),
         ("2026-06-03", "NEFT CR MAPLEDOWN HOTELS", "11250.00", "RV-0431"),
         ("2026-06-07", "UPI DR KETTLEWICK SUPPLIES", "-1890.00", "PV-0411"),
         ("2026-06-10", "NEFT CR ORCHARD CAFE CHAIN", "6400.00", "RV-0433"),
         ("2026-06-10", "NEFT CR HALDEN TRADERS", "6400.00", "RV-0432"),
         ("2026-06-18", "NEFT DR SWIFTLARK COURIERS", "-3976.00", "PV-0412"),
         ("2026-06-24", "CHQ RETURN HALDEN TRADERS", "-6400.00", ""),
         ("2026-06-30", "BANK CHARGES JUN", "-147.50", "")],
        [("2026-05-30", "PV-0403", "Mossgrove Cleaning Services", -5876.4),
         ("2026-06-02", "RV-0431", "Mapledown Hotels", 11250),
         ("2026-06-06", "PV-0411", "Kettlewick Supplies", -1890),
         ("2026-06-09", "RV-0432", "Halden Traders", 6400),
         ("2026-06-09", "RV-0433", "Orchard Cafe Chain", 6400),
         ("2026-06-17", "PV-0412", "Swiftlark Couriers", -3976),
         ("2026-06-26", "PV-0413", "Inkhaven Printers", -2360)]),
    # Pinecrest Dental Clinic, May. Two receipts of 18,750 from two parties; an SMS charge matches nothing.
    "learn-recon-5": (
        [("2026-05-02", "NEFT DR CORRAN PROPERTY RENTALS", "-30000.00", "PV-210"),
         ("2026-05-05", "NEFT CR BRIARWOOD HEALTH INSURANCE", "18750.00", "RV-124"),
         ("2026-05-11", "CARD SETTLEMENT 0511", "9820.25", "RV-125"),
         ("2026-05-14", "NEFT CR NORTHVALE CARE TRUST", "18750.00", "RV-126"),
         ("2026-05-20", "NEFT DR LUMEN DENTAL SUPPLY", "-6275.00", "PV-215"),
         ("2026-05-29", "SMS ALERT CHARGES", "-17.70", "")],
        [("2026-04-29", "PV-210", "Corran Property Rentals", -30000),
         ("2026-05-04", "RV-124", "Briarwood Health Insurance", 18750),
         ("2026-05-10", "RV-125", "Card settlement", 9820.25),
         ("2026-05-12", "RV-126", "Northvale Care Trust", 18750),
         ("2026-05-19", "PV-215", "Lumen Dental Supply", -6275),
         ("2026-05-28", "PV-216", "Ashford Lab Works", -4020)]),
}


def statement_csv(lines):
    out = ["Date,Description,Amount"] + [f"{d},{desc},{amt}" for d, desc, amt, _ in lines]
    return ("\r\n".join(out) + "\r\n").encode("ascii")


def ledger_xlsx(entries):
    return xlsx_bytes({"Bank book": [["Date", "Voucher", "Party", "Amount"]] + [list(e) for e in entries]})


def expected_rows(case_id):
    """The reconciliation a case's csv_equals grader holds: the header, then one row per statement line."""

    lines, _ = RECONS[case_id]
    return [["date", "amount", "ledger_ref"]] + [[d, amt, ref] for d, _, amt, ref in lines]


FILES = {}
for _id, _make in BILLS.items():
    FILES[f"{_id}/files/bill.pdf"] = (lambda make=_make: make()[0])
for _id, (_lines, _entries) in RECONS.items():
    FILES[f"{_id}/files/statement.csv"] = (lambda lines=_lines: statement_csv(lines))
    FILES[f"{_id}/files/ledger.xlsx"] = (lambda entries=_entries: ledger_xlsx(entries))


def main(argv):
    if "--expected" in argv:
        # What the graders and oracles hold, for the case author to compare: each bill's total and each recon's rows.
        for case_id, make in BILLS.items():
            print(case_id, make()[1])
        for case_id in RECONS:
            print(case_id, expected_rows(case_id))
        return 0
    check = "--check" in argv
    bad = []
    for rel, make in sorted(FILES.items()):
        path = os.path.join(SUITE, *rel.split("/"))
        body = make()
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
        print("wrote", rel)
    if bad:
        print("differs from what make_learn_v1.py writes:", ", ".join(bad), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
