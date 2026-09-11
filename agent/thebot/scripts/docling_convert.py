#!/usr/bin/env python3
"""
Thin CLI wrapper around docling's DocumentConverter - the only thing this
does is turn one real file (PDF, DOCX, PPTX, XLSX, HTML, images with
embedded text, etc.) into real extracted markdown, and print it as JSON so
internal/rag (Go) can consume it without needing its own PDF/table/layout
parser. Real docling output only - never a placeholder or partial result
on failure; a failure is reported as ok:false with the real error so the
Go side never treats a broken conversion as empty-but-fine.

Forced CPU-only - docling's own OCR/layout models never compete with
Ollama for GPU VRAM.

OCR is off by default (see PdfPipelineOptions below) - found live: docling's
default pipeline runs full OCR (RapidOCR, backed by torch) on EVERY PDF
regardless of whether it needs it, downloading ~30MB of models from
modelscope.cn on first use and getting OOM-killed ("signal: killed", no
docling error at all) trying to ingest a perfectly ordinary 1.8MB SEC 10-K
- a filing that, like nearly every corporate filing, already carries a
real embedded text layer and never needed OCR in the first place. A
document_timeout is set for the same reason a killed process is worse than
a reported one: a pathological file should degrade to a clean timeout
error, not an untraceable OOM kill.
"""

import io
import sys
import json
import os

os.environ.setdefault("CUDA_VISIBLE_DEVICES", "")

# Set THEBOT_DOCLING_OCR=1 to re-enable OCR for a run that genuinely needs
# it (a scanned document with no text layer at all). Off by default: OCR
# is the expensive, memory-heavy path, and the documents this pipeline
# actually ingests (filings, reports, downloaded office docs) essentially
# always have a real text layer already.
OCR_ENABLED = os.environ.get("THEBOT_DOCLING_OCR") == "1"

# Generous but bounded - real conversions of a normal-sized filing finish
# in seconds to low tens of seconds; a huge or pathological file should
# fail cleanly with a real timeout error rather than hang indefinitely or
# get killed by the OS with no diagnostic at all.
DOCUMENT_TIMEOUT_SECONDS = 120


def main():
    if len(sys.argv) != 2:
        print(json.dumps({"ok": False, "error": "usage: docling_convert.py <file>"}))
        sys.exit(1)

    path = sys.argv[1]
    try:
        from docling.document_converter import DocumentConverter, PdfFormatOption
        from docling.datamodel.base_models import InputFormat
        from docling.datamodel.pipeline_options import PdfPipelineOptions

        pipeline_options = PdfPipelineOptions(
            do_ocr=OCR_ENABLED,
            # Use the PDF's own embedded text directly rather than the
            # layout model's text detection - cheaper, and exactly right
            # for documents that (like nearly every corporate filing)
            # already carry a reliable programmatic text layer.
            force_backend_text=True,
            document_timeout=DOCUMENT_TIMEOUT_SECONDS,
        )
        converter = DocumentConverter(
            format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=pipeline_options)}
        )

        # Found live: docling's own table-matching postprocessor writes
        # real WARNING log lines directly to stdout (not stderr, and not
        # through anything that respects a logging.basicConfig(stream=...)
        # set here), corrupting the single JSON line this script's whole
        # contract depends on - the Go side's json.Unmarshal choked on
        # "2026-09-10 ... WARNING  Orphan pdf_cell 317 ..." glued in front
        # of the real JSON. Redirecting stdout for the conversion itself
        # and restoring it only to print the final result is the one fix
        # that holds regardless of what any dependency does internally.
        real_stdout = sys.stdout
        sys.stdout = io.StringIO()
        try:
            result = converter.convert(path)
        finally:
            sys.stdout = real_stdout

        if result.status.name == "FAILURE":
            errors = "; ".join(str(e) for e in result.errors) or "conversion failed with no further detail"
            print(json.dumps({"ok": False, "error": errors}))
            sys.exit(1)

        markdown = result.document.export_to_markdown()
        out = {"ok": True, "markdown": markdown}
        if result.status.name == "PARTIAL_SUCCESS":
            # Real partial content, honestly labeled - never silently
            # presented as a complete extraction.
            out["partial"] = True
            out["errors"] = [str(e) for e in result.errors]
        print(json.dumps(out))
    except Exception as exc:  # noqa: BLE001 - real conversion errors are the whole point of ok:false
        print(json.dumps({"ok": False, "error": str(exc)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
