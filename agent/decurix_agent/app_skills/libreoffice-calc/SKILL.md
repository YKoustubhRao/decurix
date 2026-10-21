---
name: libreoffice-calc
description: LibreOffice Calc, this desktop's spreadsheet app (gui open excel opens it when there is no Excel (Host)) - sorting, formatting, totals, charts, saving as .xlsx
applies-to: calc, libreoffice calc, excel
---

Which spreadsheet. A file on this desktop (~/work, an attachment in the inbox) is worked in Calc: gui open FILE.
"Excel (Host)" is the person's own Excel and cannot open this desktop's files.

Opening
- gui open FILE opens a .csv, .xlsx or .ods in Calc. A CSV first shows "Text Import": check that the columns split
  (Separated by: Comma) and press OK.
- An attachment is the person's original: save the result under the name and folder the request gives (~/work for
  "my work folder"), never over the attachment.

Going to cells and selecting
- The Name Box is reached with Ctrl+Shift+F5: type a cell or range (A20, A1:D1) and Enter - the cursor or selection
  goes there. Ctrl+Home goes to A1.
- Shift+Space selects the cursor's row, Ctrl+Space its column, Ctrl+Shift+End to the last used cell.
- gui type "text" with no ref types into the cell under the cursor; Enter moves down, Tab right. A formula starts
  with "=". gui observe lists the visible cells with their values to read back.
- Rows of values (a header row, a table, a column of formulas) go in with ONE command, never a cell per command:
  gui type CELLREF "Item\tQty\nTea\t4" from the block's top-left cell (a Tab between cells, \n between rows). Every
  cell is read back; exit 4 names the cells that do not hold what was typed (AutoInput completing a word, a number
  read as a date, a formula showing #NAME?). A formula with quoted text goes verbatim on stdin:
  gui type CELLREF - <<'EOF' then the lines (real Tabs between cells) then EOF.

Common tasks
- Bold a header row: Ctrl+Home, Shift+Space, Ctrl+B.
- Sort: put the cursor in the column to sort by, then Data > Sort Descending (or Sort Ascending) - Calc sorts the
  whole table under its header. For a dialog: Data > Sort..., Sort Key 1 = the column, Descending, OK.
- A total: go to the first empty cell under the column (Name Box), type =SUM(<first>:<last>) over the whole column
  of data, Enter. A label goes in the first column of that row.
- A column of one formula: type it ONCE in the first row's cell, select the run to the table's LAST row with the Name
  Box, gui key ctrl+d (fill down), then read the last row back; the total's range covers the same rows.
- A chart: select the range with its header row (Name Box, e.g. "C1:D<last>"), then Insert > Chart... ; the wizard's first page
  (Column, Normal) is a column chart: press Finish. Escape twice leaves the chart edit mode.
- Freeze the header row: select A2 (Name Box), then View > Freeze Cells > Freeze Rows and Columns (it freezes what is
  above and left of the cursor - so A2 freezes row 1 only).
- Number format, borders, column width: Format > Cells... and Format > Columns > Optimal Width.

Saving
- File > Save As (Ctrl+Shift+S): type the whole path with the extension in "Name" (~/work/report.xlsx), choose the
  file type "Excel 2007-365 (.xlsx)" with gui select when the dialog has the box, Save, then "Use Excel 2007-365!"
  when asked to keep the format.
- A PDF: File > Export as PDF..., then Export and a path - or, from the shell, office combine OUT.pdf IN.xlsx.
- Read the result back with office read FILE before reporting it done: the file, not the screen, is what is handed in.
