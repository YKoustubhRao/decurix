---
name: tallyprime
description: TallyPrime ("TallyPrime (Host)" on the desktop) - companies, ledgers and vouchers, worked by keyboard
applies-to: tally, tallyprime, tally prime, tally erp
---

How TallyPrime is worked here
- TallyPrime draws its own screens: they are read from the picture, and it is worked by keyboard only. The field with
  the cursor is highlighted (yellow). gui type "text" (no ref) types into it; gui key enter moves to the next field.
- ONE field per step: gui type "value", then gui key enter, before the next value. Two typings with no Enter between
  them both land in one field (a ledger named "Acme SuppliesSundry Cr").
- Look at the picture after every key (each command's result shows it): a list, a question or an error changes what
  the next key does. Do not press keys in a row without looking.
- A field chosen from a list (Under, Party A/c name, Particulars) opens the list on the right as soon as the cursor is
  in it: type the first letters - the list narrows to the entry - and Enter takes the highlighted one.
- Escape leaves a screen. On a form with unsaved entries it asks "Quit? Yes or No": Y throws the form away, N stays.
  On the first screen it asks to quit TallyPrime - answer N. Never press Alt+F4.
- A message box ("Name already exists", "Incorrect value"): read it, Enter closes it, correct the field.
- Starting: a "License" box offers "T: Continue In Educational Mode" - press T. The Gateway of Tally then shows the
  company that is open on its left ("NAME OF COMPANY").

Create a company (with none open; or Alt+K, then C)
1. Company Name: type it, Enter. Mailing Name copies it; Enter through Address, State, Country unless given.
2. "Financial year beginning from" and "Books beginning from": type the date as 1-4-2026 for 1 April 2026; Tally
   shows 1-Apr-26.
3. Ctrl+A accepts. A Company Features screen may follow: Ctrl+A accepts it as it is.
- Educational Mode accepts entries only on the 1st, 2nd and 31st of a month (Tally's documentation): a voucher dated
  otherwise is refused - read the message and tell the person.

Create a ledger - from the Gateway of Tally, exactly these steps
1. With "Create" highlighted, Enter: "Master Creation" opens with its List of Masters.
2. gui type "Ledger", Enter: "Ledger Creation" opens; the cursor is in Name.
3. gui type the ledger's name, Enter: the cursor moves to (alias).
4. Enter (skips alias): the cursor moves to Under (it shows "Capital Account" until changed) and the List of
   Groups opens on the right.
5. gui type the group's first letters ("Sundry Cr", "Indirect Ex"): the list narrows to the group. Enter takes it.
6. Look: Under must read the whole group name. Then Ctrl+A accepts the ledger; Ledger Creation opens again, empty,
   for the next one (repeat from step 3). Escape back to the Gateway when done.
- "Name already exists": that ledger is there already - Enter, then Escape and Y to leave this form; do not make
  another.

Record a purchase - from the Gateway of Tally
1. Alt+G, gui type "Create Voucher", Enter; then F9 for Purchase (the voucher type shows at the top left).
2. F2: type the date (1-4-2026), Enter.
3. Ctrl+H (Change Mode) - choose "Accounting Invoice" for a purchase with no stock items.
4. Supplier Invoice No.: from the request when given. Party A/c name: the supplier's ledger from its list.
5. Under Particulars: the expense ledger from its list, Enter, then the Amount, Enter.
6. Narration: type it as given. Ctrl+A saves; an empty voucher opens. Popups such as "Party Details" accept with
   Ctrl+A when the request gives none of their values.

Checking
- Gateway > Day Book lists the vouchers; Gateway > Chart of Accounts > Ledgers lists ledgers with their groups.
  Read them before reporting the work done.
