---
name: microsoft-excel
description: Microsoft Excel (the real one, "Excel (Host)" on the desktop) - opening, sorting, formatting, totals, charts, saving as a workbook
applies-to: excel, microsoft excel, excel (host)
---

Which Excel. "Excel (Host)" on the desktop is Microsoft Excel on the person's computer, shown in a window of this
desktop: it opens and saves the person's files (C:\...), never this desktop's (~/work, the inbox). A file attached to
the request is on this desktop: work on it in LibreOffice Calc (gui open FILE) - see the libreoffice-calc guide.

How Excel is worked here (each of these was tried on this desktop)
- Ribbon tabs, buttons and dialog buttons are pressed by their refs. After a tab is pressed, its result lists the
  tab's buttons with their refs: press the one you mean from that list, never a ref you did not see.
- Keys that work: Ctrl+O, Ctrl+B, Ctrl+Z, Ctrl+S, F12, Enter, Tab, Escape, arrows. The Alt key-tips, Alt+F1 and
  Ctrl+F12 do not. Never press Ctrl+F1: it collapses the ribbon (if only tab names show, press it once to restore).
- A drop-down in a dialog (Save as type, Sort by) is chosen with gui select REF "entry"; never click where an entry
  might be. The result says if the choice did not take.
- Something opened by mistake: Escape (or its Close), then gui observe.

Opening a file of the person's computer
1. gui open "Excel (Host)" (its first start can take 20-60 s: observe, never double-click the icon again).
2. On the start screen press "New blank workbook"; then Ctrl+O, press "Browse".
3. gui type the "File name" box's ref with the whole path (C:\Users\...\sales.csv), then Enter.

Reading and reaching cells
- gui observe lists the cells that hold something as rows like row "D2 = 4950" (address = what it shows). Read
  them before and after every change - that is how you know what the sheet holds.
- Go to a cell or range with the Name Box: gui type <ref of textbox "Name Box"> "D2", then gui key enter. The Enter
  is required: without it the next text goes into the Name Box itself ("A20Total"; Escape clears it).
- After that Enter, gui type "text" with no ref types into the cell; Enter moves down. Never type with no ref after
  clicking somewhere to look: the keys replace the cell under the cursor.
- Lost (columns off screen)? Name Box "A1", Enter. Never drag scrollbars, click headers, or use Ctrl+Shift+arrow.

Common tasks
- Order matters: sort first, then add the Total row under the data, then bold, chart and save.
- Sort by one column: Name Box to a cell in that column's data (D2, Enter); tab "Data"; press the sort button
  from its list: "Sort Largest to Smallest" for numbers ("Sort Z to A" for text). Excel keeps the header row. Do not
  use the "Sort..." dialog for one column. Check the rows after it.
- Bold the header: Name Box "A1:D1", Enter, Ctrl+B. With those cells selected, tab "Home" lists button "Bold" as
  [pressed] when they are bold: check it there, and never press Bold again when it is - that undoes it.
- A total: Name Box "A20", Enter, gui type "Total", Enter; Name Box "D20", Enter, gui type "=SUM(D2:D19)", Enter.
  Observe: row "A20 = Total" and row "D20 = <the sum>" must be there, and every data row unchanged.
- A chart: Name Box to the range with its header (C1:D19), Enter; tab "Insert", press "Recommended Charts", then
  "OK" (a clustered column chart is offered first). Escape leaves the chart.
- Freeze the header row: tab "View", "Freeze Panes", "Freeze Top Row". Undo a slip at once with Ctrl+Z.

Saving as a workbook
1. F12 (Save As). 2. gui type the "File name" box's ref with the whole path (C:\Users\...\report.xlsx).
3. gui select the "Save as type" box "Excel Workbook" (not 97-2003, not Macro-Enabled).
4. gui observe: both boxes must read what you set; then press the dialog's "Save" (the one listed with the dialog).
A CSV opened in Excel stays a CSV until saved this way. The window's title then shows the new name.
