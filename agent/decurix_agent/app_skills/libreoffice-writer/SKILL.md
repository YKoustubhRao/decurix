---
name: libreoffice-writer
description: LibreOffice Writer, this desktop's word processor - letters and documents, styles, saving as .docx and PDF
applies-to: writer, libreoffice writer, word processor
---

Opening
- gui open writer starts an empty document; gui open FILE opens a .docx, .odt or .txt.

Writing
- The page is one document element: gui type <its ref> "text" with lines separated by \n types a whole letter at
  once (each \n is a new paragraph). gui read <ref> shows what the document holds with line numbers - read it after
  typing.
- To change part of a text use the app: Edit > Find & Replace (Ctrl+H) with Replace All, or select and retype.
- A paragraph style (Heading 1, ...) is set with gui select on the style box of the toolbar while the cursor is in that
  paragraph. Bold/italic: select the words, Ctrl+B / Ctrl+I.
- A table: Table > Insert Table... (Ctrl+F12), columns and rows, Insert; cells are typed into by their refs.
- A page break is its key (gui key ctrl+Return), never "\f" typed as text: gui type reads only \n and \t as keys. office read shows a .docx's page breaks as [page break] - read the saved file back to see them.

Saving
- File > Save As (Ctrl+Shift+S): the whole path with .docx in "Name", Save, then "Use Word 2007-365!" to keep the
  format.
- A PDF: File > Export as PDF..., Export, then the path - or office combine OUT.pdf IN.docx from the shell.
- Read the result back with office read FILE before reporting it done.
