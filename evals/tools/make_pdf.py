"""
make_pdf: a one-page PDF with a real text layer, from lines of text. Standard library only, and deterministic: the
same lines give the same bytes (no dates, no ids), so a suite's SHA moves only when its content does.

    py -3 evals/tools/make_pdf.py out.pdf < lines.txt

Each input line is one line of the page in 11 pt Helvetica; a line starting with "# " is a 16 pt heading. Text is
Latin-1 (WinAnsi); anything else is refused, so a bill never holds a glyph the reader cannot extract.
"""

import sys

PAGE_W, PAGE_H = 595, 842  # A4 in points
MARGIN = 64


def _escape(text):
    return text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")


def pdf_bytes(lines):
    """The PDF for these lines, as bytes."""
    ops = []
    y = PAGE_H - MARGIN
    for raw in lines:
        line = raw.rstrip("\n")
        size, text = (16, line[2:]) if line.startswith("# ") else (11, line)
        try:
            text.encode("latin-1")
        except UnicodeEncodeError:
            raise ValueError(f"not Latin-1 text: {line!r}")
        y -= size + 6
        if text.strip():
            ops.append(f"BT /F1 {size} Tf {MARGIN} {y} Td ({_escape(text)}) Tj ET")
        if y < MARGIN:
            raise ValueError("too many lines for one page")
    stream = "\n".join(ops).encode("latin-1")
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        (f"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 {PAGE_W} {PAGE_H}] "
         f"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>").encode(),
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
    ]
    out = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = []
    for i, body in enumerate(objects, 1):
        offsets.append(len(out))
        out += f"{i} 0 obj\n".encode() + body + b"\nendobj\n"
    xref = len(out)
    out += f"xref\n0 {len(objects) + 1}\n".encode()
    out += b"0000000000 65535 f \n"
    for off in offsets:
        out += f"{off:010d} 00000 n \n".encode()
    out += f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()
    return bytes(out)


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    with open(argv[1], "wb") as f:
        f.write(pdf_bytes(sys.stdin.read().splitlines()))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
