---
name: microsoft-word
description: Microsoft Word (the real one, "Word (Host)" on the desktop) - writing a document and saving it as .docx on the person's computer
applies-to: microsoft word, ms word, word (host), word app, in word, using word, with word
---

Which Word. "Word (Host)" is Microsoft Word on the person's computer, shown in a window of this desktop: it opens and
saves the person's files (C:\...), never this desktop's. A document attached to the request is on this desktop:
write it in LibreOffice Writer instead (see libreoffice-writer).

How Word is worked here
- Ribbon tabs and buttons are pressed by their refs; Alt key-tips do not work here. Ctrl shortcuts do (Ctrl+B, Ctrl+S,
  Ctrl+O, F12).
- A dialog's drop-down (Save as type) is set with gui select REF "entry"; never click where an entry might be.

Starting
- From Word's start screen press button "Blank document" (or Ctrl+N).
- To open a file: Ctrl+O, then button "Browse", type the whole path into "File name" and Enter.

Writing
- The page is a tall text field: gui type <its ref> "text" with \n between paragraphs types a whole letter; each \n
  is Enter in Word. Read the page back (gui read <ref>) after typing - its first lines, its last lines.
- Formatting: select text by keyboard (Ctrl+Home, Shift+End for a line, Ctrl+Shift+End to the end) and use Ctrl+B,
  Ctrl+I; a heading style from tab "Home"'s style gallery by its ref.
- A page break is its key (gui key ctrl+Return), never "\f" typed as text: gui type reads only \n and \t as keys. office read shows a .docx's page breaks as [page break] - read the saved file back to see them.

Saving
- F12 opens Save As. In order: type the whole path and name into "File name" (C:\Users\...\reply.docx), set
  "Save as type" with gui select to "Word Document", read both boxes back, then press "Save".
- A .txt opened in Word is still text: Save As with type "Word Document" is what makes a .docx.
- The window's title then shows the new name; Word may ask about keeping a format - answer for the format asked for.
