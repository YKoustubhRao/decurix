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
2. "Financial year beginning from" and "Books beginning from": type dates as d-m-yyyy; Tally shows them as
   d-Mon-yy.
3. Ctrl+A accepts. A Company Features screen may follow: Ctrl+A accepts it as it is.
- Educational Mode accepts entries only on the 1st, 2nd and 31st of a month (Tally's documentation): a voucher dated
  otherwise is refused - read the message and tell the person.

Masters and vouchers
- From the Gateway, "Create" (Enter) opens Master Creation; type the master's kind ("Ledger", "Group" ...) and
  Enter to open its form. Alt+G opens Go To: type a screen's name ("Create Voucher", "Day Book") and Enter.
- In a voucher, the F-keys choose its type (shown at the top left); F2 sets its date; Ctrl+H changes its mode.
- Ctrl+A accepts a form (and any pop-up of it); the same empty form then opens for the next one. Escape back to the
  Gateway when done.
- "Name already exists": that master is there already - Enter, then Escape and Y to leave the form; do not make
  another.

Checking
- Gateway > Day Book lists the vouchers; Gateway > Chart of Accounts > Ledgers lists ledgers with their groups.
  Read them before reporting the work done.
