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

Common tasks
- Bold a header row: Ctrl+Home, Shift+Space, Ctrl+B.
- Sort: put the cursor in the column to sort by, then Data > Sort Descending (or Sort Ascending) - Calc sorts the
  whole table under its header. For a dialog: Data > Sort..., Sort Key 1 = the column, Descending, OK.
- A total: go to the first empty cell under the column (Name Box), type =SUM(D2:D19), Enter. A label goes in the
  first column of that row.
- A chart: select the range with its header row (Name Box "C1:D19"), then Insert > Chart... ; the wizard's first page
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
