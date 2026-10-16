---
name: libreoffice-impress
description: LibreOffice Impress, this desktop's presentation app - slides, bullets, tables, saving as .pptx
applies-to: impress, libreoffice impress, powerpoint
---

Opening
- gui open impress starts a new presentation. A "Select a Template" dialog may open first: Close (or Escape) gives an
  empty presentation with a title slide.

Filling a slide
- A layout's boxes read "Click to add Title" / "Click to add Text": type into each by its own ref
  (gui type <ref> "text"); never type into whatever has focus after Tab. Several bullets go as lines of one gui type
  ("first\nsecond\nthird"). The text is read back after typing; if it came out scrambled it is typed again slowly.
- A new slide: Slide > New Slide (Ctrl+M). Its layout is set in the Layouts list of the sidebar (Properties): choose
  "Title, Content" for a title and bullets.
- A table: Insert > Table..., set "Number of columns" and "Number of rows" with gui select REF "N" (or type the number),
  Insert. Then fill the whole table with ONE command into its first cell: gui type <ref of the first cell>
  "Region\tRevenue\tNorth\t812\tSouth\t655" - a Tab between cells, row after row (the last cell of a row Tabs to the
  next row's first), and no Tab after the very last cell (one there adds a row). Typed cell by cell, the cells' refs
  move as they change. Read the table back with gui observe: each cell is listed with its text.
- Undo a slip with Ctrl+Z; an empty slide made by mistake: Slide > Delete Slide.

Saving
- File > Save As (Ctrl+Shift+S): type the whole path with .pptx in "Name" (~/work/deck.pptx), file type
  "PowerPoint 2007-365 (.pptx)" when asked, Save, then "Use PowerPoint 2007-365!" to keep the format.
- Read the file back with office read FILE: each slide's title, bullets and table rows must be there.
