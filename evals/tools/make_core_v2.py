"""
make_core_v2: writes the generated input files of the core-v2 suite (the bills as text-layer PDFs and the payee
workbook), from the content below. Every name, company, amount and account number is invented.

    py -3 evals/tools/make_core_v2.py            # writes them under evals/suites/core-v2/cases/
    py -3 evals/tools/make_core_v2.py --check    # fails when a committed file differs from what this writes

The case documents (case.json, fixture.json) are written by hand: they hold what must be true afterwards, which is
the case author's to state, not this script's to derive.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from make_core_v1 import bill  # noqa: E402
from make_office import xlsx_bytes  # noqa: E402

SUITE = os.path.join(os.path.dirname(HERE), "suites", "core-v2", "cases")

FILES = {
    # Month first: 03/04/2026 and 05/03/2026 read either way; the delivery date 03/21/2026 reads only month first.
    "bill-entry-month-first/files/bill.pdf": lambda: bill(
        "Harrowgate Signworks", "2 Kiln Lane, Westmoor", "HS-0318", "03/04/2026", "05/03/2026",
        [("Vinyl lettering, shopfront", "1,200.00"), ("Installation", "450.00"), ("Travel", "120.00")],
        "1,770.00", ["Delivery date: 03/21/2026"]),
    # Two payees whose 18-digit accounts share their last 10 digits; the request names the second.
    "payment-shared-tail/files/payees.xlsx": lambda: xlsx_bytes({"Payees": [
        ["Name", "Bank", "Account number"],
        ["Fenwick Haulage Co", "Larkmere Savings Bank", "618204551029384756"],
        ["Fenwick Haulage Services", "Quillon Cooperative Bank", "927731551029384756"],
        ["Ostrava Parcel Lines", "Larkmere Savings Bank", "440912660000123987"],
    ]}),
    # No cost centre anywhere on the bill.
    "bill-needs-centre-custom/files/bill.pdf": lambda: bill(
        "Pellworth Glassworks", "9 Furnace Row, Eastbrook", "PW-2207", "2026-07-08", "2026-08-07",
        [("Display cabinet glass, 6 panes", "1,860.00"), ("Fitting", "390.00")],
        "2,250.00"),
}


def main(argv):
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
        print("differs from what make_core_v2 writes:", ", ".join(bad))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
