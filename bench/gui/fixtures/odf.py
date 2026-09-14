"""
Minimal OpenDocument writer and reader for the GUI benchmark.

Standard library only, because this runs inside a desktop container that has
python3 and nothing else guaranteed. It writes the fixtures LibreOffice opens
(a spreadsheet, a form-like text document) and reads back what LibreOffice
saved, so a check never has to start a second LibreOffice - which would fight
the one on screen over its profile lock.
"""

import re
import zipfile
from xml.etree import ElementTree
from xml.sax.saxutils import escape

NS = {
    "office": "urn:oasis:names:tc:opendocument:xmlns:office:1.0",
    "table": "urn:oasis:names:tc:opendocument:xmlns:table:1.0",
    "text": "urn:oasis:names:tc:opendocument:xmlns:text:1.0",
}

_ROOT_ATTRS = (
    'xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" '
    'xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" '
    'xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" '
    'xmlns:of="urn:oasis:names:tc:opendocument:xmlns:of:1.2" '
    'office:version="1.2"'
)

_MANIFEST = """<?xml version="1.0" encoding="UTF-8"?>
<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">
 <manifest:file-entry manifest:full-path="/" manifest:media-type="{mime}"/>
 <manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>
</manifest:manifest>
"""

MIME_ODS = "application/vnd.oasis.opendocument.spreadsheet"
MIME_ODT = "application/vnd.oasis.opendocument.text"


def _write(path, mime, body):
    content = (
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        f"<office:document-content {_ROOT_ATTRS}><office:body>{body}"
        "</office:body></office:document-content>"
    )

    with zipfile.ZipFile(path, "w") as archive:
        # The mimetype entry must come first and be stored uncompressed, or
        # LibreOffice treats the file as damaged and offers to repair it.
        archive.writestr(
            zipfile.ZipInfo("mimetype"), mime, compress_type=zipfile.ZIP_STORED
        )
        archive.writestr("content.xml", content, compress_type=zipfile.ZIP_DEFLATED)
        archive.writestr(
            "META-INF/manifest.xml",
            _MANIFEST.format(mime=mime),
            compress_type=zipfile.ZIP_DEFLATED,
        )


def _cell(value):
    if value is None or value == "":
        return "<table:table-cell/>"

    if isinstance(value, (int, float)):
        return (
            f'<table:table-cell office:value-type="float" office:value="{value}">'
            f"<text:p>{value}</text:p></table:table-cell>"
        )

    return (
        '<table:table-cell office:value-type="string">'
        f"<text:p>{escape(str(value))}</text:p></table:table-cell>"
    )


def write_ods(path, sheet_name, rows):
    """Write a one-sheet spreadsheet whose rows are lists of cell values."""

    body_rows = "".join(
        "<table:table-row>" + "".join(_cell(v) for v in row) + "</table:table-row>"
        for row in rows
    )

    _write(
        path,
        MIME_ODS,
        f'<office:spreadsheet><table:table table:name="{escape(sheet_name)}">'
        f"{body_rows}</table:table></office:spreadsheet>",
    )


def write_odt(path, heading, intro, table_rows):
    """Write a text document: a heading, a paragraph, and a two-column table."""

    rows = "".join(
        "<table:table-row>" + "".join(_cell(v) for v in row) + "</table:table-row>"
        for row in table_rows
    )

    _write(
        path,
        MIME_ODT,
        "<office:text>"
        f'<text:h text:outline-level="1">{escape(heading)}</text:h>'
        f"<text:p>{escape(intro)}</text:p>"
        '<table:table table:name="Form">'
        '<table:table-column table:number-columns-repeated="2"/>'
        f"{rows}</table:table>"
        "</office:text>",
    )


def _q(tag):
    prefix, local = tag.split(":")
    return "{%s}%s" % (NS[prefix], local)


def _text_of(element):
    """The visible text of a cell or paragraph, with ODF's space elements."""

    parts = []

    def walk(node):
        if node.tag == _q("text:s"):
            parts.append(" " * int(node.get(_q("text:c"), "1")))
        elif node.tag == _q("text:tab"):
            parts.append("\t")
        elif node.tag == _q("text:line-break"):
            parts.append("\n")
        elif node.text:
            parts.append(node.text)

        for child in node:
            walk(child)
            if child.tail:
                parts.append(child.tail)

    paragraphs = []
    for p in element.iter():
        if p.tag in (_q("text:p"), _q("text:h")):
            parts = []
            walk(p)
            paragraphs.append("".join(parts))

    return "\n".join(paragraphs)


def _content(path):
    with zipfile.ZipFile(path) as archive:
        return ElementTree.fromstring(archive.read("content.xml"))


def _table_rows(table):
    """Expand repeated rows/cells into a plain grid of cell dicts (capped)."""

    grid = []

    for row in table.iter(_q("table:table-row")):
        repeat_rows = min(int(row.get(_q("table:number-rows-repeated"), "1")), 50)
        cells = []

        for cell in row:
            if cell.tag not in (_q("table:table-cell"), _q("table:covered-table-cell")):
                continue

            repeat = min(int(cell.get(_q("table:number-columns-repeated"), "1")), 50)
            info = {
                "text": _text_of(cell),
                "value": cell.get(_q("office:value")),
                "formula": cell.get(_q("table:formula")) or "",
            }
            cells.extend([info] * repeat)

        grid.extend([cells] * repeat_rows)

    return grid


def read_ods(path):
    """Return {sheet_name: grid} where grid[row][col] is a cell dict."""

    root = _content(path)

    return {
        table.get(_q("table:name")): _table_rows(table)
        for table in root.iter(_q("table:table"))
    }


def cell(grid, ref):
    """Look up an A1-style reference ("B6") in a grid, or None."""

    match = re.fullmatch(r"([A-Z]+)(\d+)", ref.upper())
    col = 0
    for ch in match.group(1):
        col = col * 26 + (ord(ch) - 64)
    row = int(match.group(2)) - 1
    col -= 1

    if row >= len(grid) or col >= len(grid[row]):
        return None

    return grid[row][col]


def read_odt(path):
    """Return (paragraph texts, [table grids]) for a text document."""

    root = _content(path)
    text_body = root.find(f"{_q('office:body')}/{_q('office:text')}")

    paragraphs = []
    tables = []

    for node in text_body:
        if node.tag in (_q("text:p"), _q("text:h")):
            paragraphs.append(_text_of(node))
        elif node.tag == _q("table:table"):
            tables.append(_table_rows(node))

    return paragraphs, tables
