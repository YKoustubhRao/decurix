"""Simple local UI backend for testing the document-ingestion / schema-detection /
audit / code-generation pipeline against a local Ollama instance.

Uses Ollama's NATIVE /api/chat endpoint, not its OpenAI-compatible shim - the
shim silently caps context at ~4096 tokens and ignores num_ctx, while the
native endpoint honors options.num_ctx and takes page images as raw base64
via a per-message "images" list (not OpenAI's content/image_url format).

Each group picks an ingestion mode (content / image / image_content) and a
model at ingest time; that choice is stored on the group and reused for every
later call (ask/schema/audit/codegen) so the conversation stays on one model.

Conversation memory: every ingest, schema run, question, and table audit is
appended to the group's persisted `context_messages` list, so later calls (and
the codegen loop) see the model's own prior schema output and any corrections
the user made - not just the original documents.
"""
import base64
import json
import os
import re
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

import pdfplumber
import psutil
import pymupdf
import requests
from bs4 import BeautifulSoup, Comment
from fastapi import FastAPI, UploadFile
from fastapi.responses import StreamingResponse
from fastapi.staticfiles import StaticFiles

HERE = Path(__file__).parent
GROUPS_DIR = HERE / "groups"
GROUPS_DIR.mkdir(exist_ok=True)
# Same convention as flowera_agent/thebot's soul: OLLAMA_URL is set on the
# container to the control plane's host (http://host.docker.internal:11434);
# outside a container (e.g. running this standalone) it just falls back to
# the local machine's own Ollama.
OLLAMA = os.environ.get("OLLAMA_URL", "http://127.0.0.1:11434")
DEFAULT_TEXT_MODEL = "qwen2.5-coder:14b"
DEFAULT_VISION_MODEL = "devstral-small-2:24b"
INGEST_MODES = ("content", "image", "image_content")
IMG_TOKEN_ESTIMATE = 1600  # rough per-page-image cost, for the pre-ingest size guard
PDF_RENDER_ZOOM = 2.0  # ~144 DPI, matches text sharpness needed for small table numbers
NUM_CTX_LADDER = [8192, 16384, 32768, 65536, 131072, 262144, 393216]
PYTHON = sys.executable
CODEGEN_MAX_ITER = 10
SANDBOX_TIMEOUT_S = 30

SYSTEM_PROMPT = (
    "You are a careful data-extraction assistant. You will be given the full text of "
    "one or more documents. Only state facts that are actually present in the "
    "documents - if something is not present, say so explicitly rather than guessing "
    "or inventing a plausible-sounding value. Remember everything from earlier in this "
    "conversation, including schemas you proposed yourself and any corrections the user "
    "gives you about mistakes you made - do not repeat a corrected mistake."
)

SCHEMA_PROMPT = """Based ONLY on the documents above, propose a set of structured \
database tables that could hold the data in them. Only propose a table/column for \
something you can actually see in the documents - do not assume standard fields you \
can't verify. For EACH table, output (in this order, as you go - do not wait until \
the end): a "### {table_name}" heading on its own line, then description, grain, a \
column list (name/type/nullable/description), and the evidence (what you saw, and \
where) that justifies it. Write your reasoning and tables as you go, in normal text \
- not JSON. Start each table's heading with exactly "### " so it can be parsed."""

FILL_DATA_PROMPT = """Based ONLY on the documents above, extract the actual data for the \
table below - every real row/record you can find in the documents. Output a JSON array \
of row objects using exactly the column names from the schema. If a column's value isn't \
present for a given row, use null - never invent, guess, or carry over a value from a \
different row/document. Output ONLY the JSON array: no markdown code fences, no \
explanation, no other text before or after it.

TABLE SCHEMA:
### {name}
{text}"""

CODEGEN_TOOLS_BLURB = (
    "You have a Python environment with these libraries available: BeautifulSoup (bs4), "
    "pandas, pdfplumber, camelot (PDF table extraction), docling. Import only what you "
    "need. Your code will be executed exactly as written with `python script.py`, with "
    "its working directory set to the folder containing the group's original uploaded "
    "files (listed below) - open them by filename directly."
)

# in-memory live state (progress counters, codegen control flags) - meta.json on
# disk is the source of truth for anything that must survive a server restart
LIVE = {}
CODEGEN_CONTROL = {}  # gid -> {"pause": bool}
INFLIGHT = {}  # gid -> live requests.Response to Ollama, so /cancel can abort it mid-flight
CANCELLED = set()  # gids whose current call should stop as soon as possible


class IngestCancelled(Exception):
    pass


def _recover_orphaned_groups():
    """A group left in "ingesting" when this process starts has no worker
    thread actually running for it (threads don't survive a server restart) -
    without this it would sit "ingesting" forever with nothing able to finish
    or cancel it. Runs once at import time."""
    for d in GROUPS_DIR.iterdir() if GROUPS_DIR.exists() else []:
        meta_path = d / "meta.json"
        if not meta_path.exists():
            continue
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except Exception:
            continue
        if meta.get("status") == "ingesting":
            meta["status"] = "error"
            meta["error"] = "Ingest was interrupted by a server restart - re-run ingest."
            meta_path.write_text(json.dumps(meta, indent=2), encoding="utf-8")


app = FastAPI()
_recover_orphaned_groups()


# ---------------------------------------------------------------- extraction

def extract_pdf(path: Path) -> str:
    parts = []
    with pdfplumber.open(path) as pdf:
        for page_num, page in enumerate(pdf.pages, 1):
            tables = page.find_tables()
            bboxes = [t.bbox for t in tables]

            def not_in_table(obj, bboxes=bboxes):
                return not any(
                    b[0] <= obj["x0"] < b[2] and b[1] <= obj["top"] < b[3] for b in bboxes
                )

            text = page.filter(not_in_table).extract_text() or ""
            parts.append(f"\n--- page {page_num} ---\n{text.strip()}")
            for i, table in enumerate(tables):
                rows = []
                for row in table.extract():
                    cells = [str(c).strip() if c else "" for c in row]
                    if any(cells):
                        rows.append(" | ".join(cells))
                if rows:
                    parts.append(f"\n[[TABLE p{page_num}-{i}]]\n" + "\n".join(rows) + "\n[[/TABLE]]\n")
    return "\n".join(parts)


def extract_html(path: Path) -> str:
    html = path.read_text(encoding="utf-8", errors="ignore")
    soup = BeautifulSoup(html, "html.parser")
    for tag in soup(["script", "style", "svg", "noscript", "link", "meta", "head"]):
        tag.decompose()
    for c in soup.find_all(string=lambda t: isinstance(t, Comment)):
        c.extract()
    return soup.get_text(separator=" ", strip=True)


def extract_text_file(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="ignore")


def extract_any(path: Path) -> str:
    ext = path.suffix.lower()
    if ext == ".pdf":
        return extract_pdf(path)
    if ext in (".html", ".htm"):
        return extract_html(path)
    return extract_text_file(path)


def render_pdf_pages_b64(path: Path, zoom: float = PDF_RENDER_ZOOM) -> list:
    """Rasterize each page to a JPEG (smaller payload than PNG for photo-like
    scans), base64-encoded with no data-URI prefix - that's what Ollama's
    native /api/chat "images" field expects."""
    out = []
    doc = pymupdf.open(path)
    try:
        for page in doc:
            pix = page.get_pixmap(matrix=pymupdf.Matrix(zoom, zoom))
            data = pix.tobytes("jpeg", jpg_quality=82)
            out.append(base64.b64encode(data).decode())
    finally:
        doc.close()
    return out


def build_ingest_content(meta: dict, mode: str, fdir: Path):
    """Build the combined document text and/or page images for the ingest
    message, per the group's ingestion mode. PDFs get real page renders in
    image/image_content mode; other file types have no renderer, so they
    always fall back to text even in "image" mode."""
    text_parts = []
    images = []
    image_order = []
    for fname in meta["files"]:
        path = fdir / fname
        ext = path.suffix.lower()
        use_image = mode in ("image", "image_content") and ext == ".pdf"
        if use_image:
            try:
                pages = render_pdf_pages_b64(path)
            except Exception as e:
                pages = []
                text_parts.append(f"\n===== DOCUMENT: {fname} =====\n[page render failed: {e}]")
            for i, b64 in enumerate(pages, 1):
                images.append(b64)
                image_order.append(f"{len(images)}. {fname} - page {i}")
        if mode != "image" or not use_image:
            try:
                text = extract_any(path)
            except Exception as e:
                text = f"[extraction failed: {e}]"
            text_parts.append(f"\n===== DOCUMENT: {fname} =====\n{text}")

    parts = []
    if text_parts:
        parts.append("\n".join(text_parts))
    if images:
        parts.append("\nPage images are attached, in this order:\n" + "\n".join(image_order))
    return "\n".join(parts), images


# ---------------------------------------------------------------- group storage

def group_dir(gid: str) -> Path:
    return GROUPS_DIR / gid


def load_meta(gid: str) -> dict:
    p = group_dir(gid) / "meta.json"
    return json.loads(p.read_text(encoding="utf-8"))


def save_meta(gid: str, meta: dict):
    (group_dir(gid) / "meta.json").write_text(json.dumps(meta, indent=2), encoding="utf-8")


def append_turns(gid: str, turns: list):
    """Grow the persisted conversation - this is how the model 'remembers' schema
    it self-generated and corrections the user later makes."""
    meta = load_meta(gid)
    meta.setdefault("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    meta["context_messages"].extend(turns)
    meta["context_turns"] = len(meta["context_messages"])
    save_meta(gid, meta)


def slugify(name: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-") or uuid.uuid4().hex[:8]


def parse_tables_from_schema_text(text: str) -> list:
    """Split the streamed schema text into per-table sections on '### Name' headings."""
    sections = re.split(r"(?m)^###\s+", text)
    tables = []
    for sec in sections[1:]:  # first split part is preamble before the first heading
        lines = sec.split("\n", 1)
        name = lines[0].strip()
        body = lines[1].strip() if len(lines) > 1 else ""
        tables.append({
            "id": slugify(name), "name": name, "text": body, "edited": False,
            "data": None, "data_raw": None, "data_error": None,
        })
    return tables


# ---------------------------------------------------------------- stats

def get_gpu_stats():
    try:
        out = subprocess.check_output(
            ["nvidia-smi", "--query-gpu=memory.used,memory.total,utilization.gpu",
             "--format=csv,noheader,nounits"],
            timeout=3,
        ).decode().strip()
        used, total, util = [x.strip() for x in out.split(",")]
        return {"used_mb": int(used), "total_mb": int(total), "util_pct": int(util)}
    except Exception:
        return None


def get_ram_stats():
    vm = psutil.virtual_memory()
    return {"used_gb": round(vm.used / 1e9, 2), "total_gb": round(vm.total / 1e9, 2), "pct": vm.percent}


@app.get("/api/stats")
def api_stats():
    return {"gpu": get_gpu_stats(), "ram": get_ram_stats()}


# ---------------------------------------------------------------- models

def ollama_tags() -> list:
    try:
        r = requests.get(f"{OLLAMA}/api/tags", timeout=5).json()
    except Exception:
        return []
    out = []
    for m in r.get("models", []):
        caps = m.get("capabilities") or []
        if "completion" not in caps:
            continue  # skip embedding-only models, not usable for chat
        details = m.get("details", {})
        out.append({
            "name": m["name"],
            "context_length": details.get("context_length", 32768),
            "vision": "vision" in caps,
            "param_size": details.get("parameter_size"),
        })
    return out


def model_ctx_len(model: str) -> int:
    for m in ollama_tags():
        if m["name"] == model:
            return m["context_length"]
    return 32768


def pick_num_ctx(est_tokens: int, model_max: int) -> int:
    """Round up to a clean context size with headroom for the schema/audit/
    codegen turns that will pile onto this conversation later, capped at
    whatever the model actually supports."""
    target = max(8192, int((est_tokens + 4000) * 1.3))
    for c in NUM_CTX_LADDER:
        if c >= target:
            return min(c, model_max)
    return min(target, model_max)


@app.get("/api/models")
def api_models():
    return ollama_tags()


# ---------------------------------------------------------------- groups

@app.get("/api/groups")
def api_list_groups():
    groups = []
    for d in sorted(GROUPS_DIR.iterdir()):
        if (d / "meta.json").exists():
            m = load_meta(d.name)
            groups.append({k: v for k, v in m.items() if k != "context_messages"})
    return groups


@app.post("/api/groups")
def api_create_group(payload: dict):
    gid = uuid.uuid4().hex[:12]
    d = group_dir(gid)
    (d / "files").mkdir(parents=True)
    meta = {
        "id": gid, "name": payload.get("name", "Untitled group"),
        "created_at": time.time(), "files": [], "status": "new",
        "context_char_len": 0, "context_token_estimate": 0,
        "tables": [], "context_turns": 0,
        "mode": "content", "model": None, "num_ctx": None, "image_count": 0,
    }
    save_meta(gid, meta)
    return meta


@app.post("/api/groups/{gid}/upload")
async def api_upload(gid: str, files: list[UploadFile]):
    meta = load_meta(gid)
    fdir = group_dir(gid) / "files"
    for f in files:
        dest = fdir / f.filename
        dest.write_bytes(await f.read())
        if f.filename not in meta["files"]:
            meta["files"].append(f.filename)
    meta["status"] = "new"
    save_meta(gid, meta)
    return meta


def _ingest_worker(gid: str):
    LIVE[gid] = {"phase": "extracting", "pct": 0, "detail": ""}
    meta = load_meta(gid)
    mode = meta.get("mode", "content")
    model = meta.get("model") or (DEFAULT_TEXT_MODEL if mode == "content" else DEFAULT_VISION_MODEL)
    fdir = group_dir(gid) / "files"

    LIVE[gid] = {"phase": "extracting", "pct": 10, "detail": "reading files / rendering pages..."}
    try:
        doc_text, images = build_ingest_content(meta, mode, fdir)
    except Exception as e:
        meta["status"] = "error"
        meta["error"] = f"extraction failed: {e}"
        save_meta(gid, meta)
        LIVE[gid] = {"phase": "error", "pct": 100, "detail": meta["error"]}
        return

    user_msg = (
        "Here are the documents. Read them carefully, then just reply with a short "
        "acknowledgement (one sentence) - do not summarize or analyze yet.\n" + doc_text
    )
    message = {"role": "user", "content": user_msg}
    if images:
        message["images"] = images
    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        message,
    ]

    # Fast pre-check (rough ~4 chars/token, flat cost per page image) so an
    # oversized upload fails in milliseconds with a clear message instead of
    # after a real request.
    est_tokens = (len(SYSTEM_PROMPT) + len(user_msg)) // 4 + len(images) * IMG_TOKEN_ESTIMATE
    model_max = model_ctx_len(model)
    num_ctx = pick_num_ctx(est_tokens, model_max)
    if est_tokens > num_ctx * 0.9:
        msg = (f"Estimated ~{est_tokens:,} tokens ({len(images)} page image(s)), which won't "
               f"fit even {model}'s max {model_max:,}-token context. Remove some files/pages "
               f"or split this into multiple smaller groups.")
        meta["context_char_len"] = len(doc_text)
        meta["context_token_estimate"] = est_tokens
        meta["status"] = "error"
        meta["error"] = msg
        save_meta(gid, meta)
        LIVE[gid] = {"phase": "error", "pct": 100, "detail": msg}
        return

    LIVE[gid] = {"phase": "priming", "pct": 25, "detail": f"sending to {model}..."}

    # Ollama doesn't expose per-token prompt-eval progress over HTTP the way
    # llama.cpp's /slots did, so this is a time-based approximation, not real
    # progress - it shows elapsed time so a genuinely slow (not stuck) call is
    # distinguishable from a hung one, and there's a real /cancel to bail out.
    stop_flag = {"done": False}
    start_t = time.time()

    def fake_progress():
        pct = 25
        while not stop_flag["done"]:
            time.sleep(0.5)
            pct = min(pct + 3, 92)
            elapsed = int(time.time() - start_t)
            LIVE[gid] = {"phase": "priming", "pct": pct,
                         "detail": f"waiting on {model}... ({elapsed}s elapsed)"}

    t = threading.Thread(target=fake_progress, daemon=True)
    t.start()

    error = None
    cancelled = False
    try:
        ack = "".join(_ollama_stream(gid, messages, model, num_ctx, 60))
    except IngestCancelled:
        cancelled = True
        ack = ""
    except Exception as e:
        error = str(e)
        ack = f"[priming request failed: {error}]"
    finally:
        stop_flag["done"] = True

    meta["context_char_len"] = len(doc_text)
    meta["context_token_estimate"] = est_tokens
    meta["mode"] = mode
    meta["model"] = model
    meta["num_ctx"] = num_ctx
    meta["image_count"] = len(images)

    if cancelled:
        meta["status"] = "cancelled"
        meta["error"] = "Ingest cancelled by user."
        save_meta(gid, meta)
        LIVE[gid] = {"phase": "cancelled", "pct": 100, "detail": "cancelled by user"}
        return

    if error:
        # Do NOT mark ready - the model never actually saw the documents, so
        # nothing downstream (ask/schema/codegen) has a valid context to use.
        meta["status"] = "error"
        meta["error"] = error
        save_meta(gid, meta)
        LIVE[gid] = {"phase": "error", "pct": 100, "detail": error}
        return

    messages.append({"role": "assistant", "content": ack})
    meta["context_messages"] = messages
    meta["context_turns"] = len(messages)
    meta["status"] = "ready"
    meta["error"] = None
    meta["tables"] = []
    save_meta(gid, meta)
    LIVE[gid] = {"phase": "done", "pct": 100, "detail": ack}


@app.post("/api/groups/{gid}/ingest")
def api_ingest(gid: str, payload: dict = None):
    meta = load_meta(gid)
    payload = payload or {}
    mode = payload.get("mode", meta.get("mode", "content"))
    if mode not in INGEST_MODES:
        mode = "content"
    model = payload.get("model") or meta.get("model") or (
        DEFAULT_TEXT_MODEL if mode == "content" else DEFAULT_VISION_MODEL)
    meta["mode"] = mode
    meta["model"] = model
    meta["status"] = "ingesting"
    save_meta(gid, meta)
    CANCELLED.discard(gid)
    threading.Thread(target=_ingest_worker, args=(gid,), daemon=True).start()
    return {"ok": True}


TERMINAL_INGEST_PHASES = ("done", "error", "cancelled")


@app.get("/api/groups/{gid}/ingest/progress")
def api_ingest_progress(gid: str):
    def gen():
        last = None
        while True:
            state = LIVE.get(gid, {"phase": "idle", "pct": 0, "detail": ""})
            if state != last:
                yield f"data: {json.dumps(state)}\n\n"
                last = state
            if state.get("phase") in TERMINAL_INGEST_PHASES:
                break
            time.sleep(0.3)

    return StreamingResponse(gen(), media_type="text/event-stream")


def _ollama_stream(gid: str, messages: list, model: str, num_ctx: int, num_predict: int):
    """Stream a chat completion from Ollama's native /api/chat, yielding
    content deltas. Registers the live response under `gid` so /cancel can
    forcibly close the connection - including mid prompt-eval, before any
    token has streamed back - which is the phase a long ingest actually gets
    stuck feeling unresponsive in. Does NOT clear a pending cancel itself -
    callers that represent one fresh user-initiated action should discard it
    before their first call; a multi-call loop (codegen) discards it once up
    front so a cancel mid-loop stays effective across iterations."""
    resp = requests.post(f"{OLLAMA}/api/chat", json={
        "model": model, "messages": messages, "stream": True,
        "options": {"num_ctx": num_ctx, "temperature": 0, "num_predict": num_predict},
    }, stream=True, timeout=1800)
    INFLIGHT[gid] = resp
    try:
        for line in resp.iter_lines():
            if gid in CANCELLED:
                raise IngestCancelled()
            if not line:
                continue
            try:
                chunk = json.loads(line.decode("utf-8"))
            except Exception:
                continue
            delta = chunk.get("message", {}).get("content", "")
            if delta:
                yield delta
            if chunk.get("done"):
                break
    except IngestCancelled:
        raise
    except Exception:
        if gid in CANCELLED:
            raise IngestCancelled()
        raise
    finally:
        try:
            resp.close()
        except Exception:
            pass
        INFLIGHT.pop(gid, None)


@app.post("/api/groups/{gid}/cancel")
def api_cancel(gid: str):
    """Abort whatever LLM call is currently running for this group - ingest,
    schema detection, data fill, ask, audit, or codegen - as soon as
    possible, including mid prompt-eval, by forcibly closing the live
    connection to Ollama."""
    CANCELLED.add(gid)
    resp = INFLIGHT.get(gid)
    if resp is not None:
        try:
            resp.close()
        except Exception:
            pass
    meta = load_meta(gid)
    if meta.get("status") == "ingesting":
        meta["status"] = "cancelled"
        meta["error"] = "Ingest cancelled by user."
        save_meta(gid, meta)
        LIVE[gid] = {"phase": "cancelled", "pct": 100, "detail": "cancelled by user"}
    live_cg = LIVE.get(f"codegen:{gid}")
    if live_cg and not live_cg.get("done"):
        live_cg["phase"] = "cancelled"
        live_cg["done"] = True
    return {"ok": True}


@app.post("/api/groups/{gid}/reset")
def api_reset_group(gid: str):
    """"Clear cache": aborts anything in flight for this group and wipes its
    ingested state (conversation, tables, ingest stats) back to "new" so it
    can be re-ingested clean. Uploaded files are kept."""
    CANCELLED.add(gid)
    resp = INFLIGHT.get(gid)
    if resp is not None:
        try:
            resp.close()
        except Exception:
            pass
    meta = load_meta(gid)
    meta["status"] = "new"
    meta["error"] = None
    meta["context_messages"] = []
    meta["context_turns"] = 0
    meta["context_char_len"] = 0
    meta["context_token_estimate"] = 0
    meta["tables"] = []
    meta["num_ctx"] = None
    meta["image_count"] = 0
    save_meta(gid, meta)
    LIVE.pop(gid, None)
    LIVE.pop(f"codegen:{gid}", None)
    return {"ok": True}


def _stream_chat(gid, messages, model, num_ctx, on_complete=None):
    """Stream a chat completion and re-emit it to the frontend as SSE. If
    on_complete is given, it's called with the full accumulated text once
    streaming finishes (used to persist conversation turns / parse schema
    tables after the fact) - skipped if the call was cancelled partway."""
    def gen():
        CANCELLED.discard(gid)
        full = []
        try:
            for delta in _ollama_stream(gid, messages, model, num_ctx, 4000):
                full.append(delta)
                yield f"data: {json.dumps({'delta': delta})}\n\n"
        except IngestCancelled:
            yield f"data: {json.dumps({'cancelled': True})}\n\n"
            return
        text = "".join(full)
        if on_complete:
            try:
                on_complete(text)
            except Exception as e:
                yield f"data: {json.dumps({'warning': f'post-processing failed: {e}'})}\n\n"
        yield f"data: {json.dumps({'done': True})}\n\n"

    return StreamingResponse(gen(), media_type="text/event-stream")


@app.post("/api/groups/{gid}/ask")
def api_ask(gid: str, payload: dict):
    meta = load_meta(gid)
    question = payload["question"]
    messages = meta.get("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    messages = messages + [{"role": "user", "content": question}]
    model = meta.get("model") or DEFAULT_TEXT_MODEL
    num_ctx = meta.get("num_ctx") or 32768

    def persist(answer):
        append_turns(gid, [
            {"role": "user", "content": question},
            {"role": "assistant", "content": answer},
        ])

    return _stream_chat(gid, messages, model, num_ctx, on_complete=persist)


@app.post("/api/groups/{gid}/schema")
def api_schema(gid: str):
    meta = load_meta(gid)
    messages = meta.get("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    messages = messages + [{"role": "user", "content": SCHEMA_PROMPT}]
    model = meta.get("model") or DEFAULT_TEXT_MODEL
    num_ctx = meta.get("num_ctx") or 32768

    def persist(schema_text):
        tables = parse_tables_from_schema_text(schema_text)
        m = load_meta(gid)
        m["tables"] = tables
        m.setdefault("context_messages", messages[:-1])
        m["context_messages"].extend([
            {"role": "user", "content": SCHEMA_PROMPT},
            {"role": "assistant", "content": schema_text},
        ])
        m["context_turns"] = len(m["context_messages"])
        save_meta(gid, m)

    return _stream_chat(gid, messages, model, num_ctx, on_complete=persist)


# ---------------------------------------------------------------- tables (data fill + audit)

@app.get("/api/groups/{gid}/tables")
def api_list_tables(gid: str):
    return load_meta(gid).get("tables", [])


@app.get("/api/groups/{gid}/tables/{tid}")
def api_get_table(gid: str, tid: str):
    tables = load_meta(gid).get("tables", [])
    for t in tables:
        if t["id"] == tid:
            return t
    return {"error": "not found"}


def _extract_json_array(text: str):
    """Schema output is asked to be a bare JSON array, but models sometimes
    wrap it in a ```json fence anyway - strip that before parsing."""
    m = re.search(r"```(?:json)?\s*(\[.*\])\s*```", text, re.DOTALL)
    raw = m.group(1) if m else text.strip()
    return json.loads(raw)


@app.post("/api/groups/{gid}/tables/{tid}/fill")
def api_fill_table_data(gid: str, tid: str):
    """The schema alone is just a proposed shape - this asks the model to
    actually go read the ingested documents again and pull out the real rows
    for this table, streamed live. Triggered automatically when the table is
    opened in the UI, and re-runnable on demand."""
    meta = load_meta(gid)
    table = next((t for t in meta.get("tables", []) if t["id"] == tid), None)
    if table is None:
        return {"error": "not found"}

    prompt = FILL_DATA_PROMPT.format(name=table["name"], text=table["text"])
    messages = meta.get("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    messages = messages + [{"role": "user", "content": prompt}]
    model = meta.get("model") or DEFAULT_TEXT_MODEL
    num_ctx = meta.get("num_ctx") or 32768

    def persist(raw_text):
        m = load_meta(gid)
        for t in m.get("tables", []):
            if t["id"] == tid:
                t["data_raw"] = raw_text
                try:
                    t["data"] = _extract_json_array(raw_text)
                    t["data_error"] = None
                except Exception as e:
                    t["data"] = None
                    t["data_error"] = f"couldn't parse the model's output as a JSON array: {e}"
        m.setdefault("context_messages", messages[:-1])
        m["context_messages"].extend([
            {"role": "user", "content": prompt},
            {"role": "assistant", "content": raw_text},
        ])
        m["context_turns"] = len(m["context_messages"])
        save_meta(gid, m)

    return _stream_chat(gid, messages, model, num_ctx, on_complete=persist)


@app.post("/api/groups/{gid}/tables/{tid}/audit")
def api_audit_table(gid: str, tid: str, payload: dict):
    """User confirms a correction to the DATA the model extracted for this
    table (not the schema - the schema is just the shape; the data is what
    can actually be wrong row-by-row). The correction is appended as a real
    conversation turn, then the model is asked to self-reflect on having
    gotten it wrong - streamed live, and that reflection is itself appended
    so the model won't repeat the mistake."""
    meta = load_meta(gid)
    tables = meta.get("tables", [])
    table = next((t for t in tables if t["id"] == tid), None)
    if table is None:
        return {"error": "not found"}
    old_data = table.get("data")
    new_data = payload["new_data"]
    table["data"] = new_data
    table["edited"] = True
    save_meta(gid, meta)

    correction_msg = (
        f"I'm auditing the DATA you extracted for the '{table['name']}' table. Here is "
        f"what you originally extracted:\n\n{json.dumps(old_data, indent=2)}\n\nHere is "
        f"the CORRECTED, verified data after my review:\n\n{json.dumps(new_data, indent=2)}"
        f"\n\nSpecifically state which value(s) were wrong, then explain WHY you think you "
        f"extracted it incorrectly (misread a number, wrong row/column alignment, confused "
        f"two documents or dates, hallucinated a plausible-looking value, etc.) - be honest "
        f"and specific rather than generic. Then confirm you'll use the corrected data "
        f"going forward. Keep it to 3-4 sentences."
    )
    messages = meta.get("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    messages = messages + [{"role": "user", "content": correction_msg}]
    model = meta.get("model") or DEFAULT_TEXT_MODEL
    num_ctx = meta.get("num_ctx") or 32768

    def persist(reflection):
        append_turns(gid, [
            {"role": "user", "content": correction_msg},
            {"role": "assistant", "content": reflection},
        ])

    return _stream_chat(gid, messages, model, num_ctx, on_complete=persist)


# ---------------------------------------------------------------- sandbox

@app.post("/api/sandbox/run")
def api_sandbox_run(payload: dict):
    code = payload["code"]
    gid = payload.get("group_id")
    workdir = group_dir(gid) / "files" if gid else HERE
    script_path = (group_dir(gid) if gid else HERE) / "_sandbox_run.py"
    script_path.write_text(code, encoding="utf-8")
    try:
        proc = subprocess.run(
            [PYTHON, str(script_path)], cwd=str(workdir),
            capture_output=True, text=True, timeout=SANDBOX_TIMEOUT_S,
            encoding="utf-8", errors="replace",
        )
        return {"stdout": proc.stdout, "stderr": proc.stderr, "returncode": proc.returncode}
    except subprocess.TimeoutExpired as e:
        return {"stdout": e.stdout or "", "stderr": (e.stderr or "") + f"\n[TIMEOUT after {SANDBOX_TIMEOUT_S}s]",
                "returncode": -1}


# ---------------------------------------------------------------- codegen loop

def _extract_code_block(text: str) -> str:
    m = re.search(r"```python\s*(.*?)```", text, re.DOTALL)
    if m:
        return m.group(1)
    m = re.search(r"```\s*(.*?)```", text, re.DOTALL)
    return m.group(1) if m else text


def _file_preview_html(path: Path, max_chars: int) -> str:
    """Tag-structure-preserving excerpt (unlike extract_html, which throws away
    ids/classes) - the model needs real selectors to write BeautifulSoup code
    against, not just prose text."""
    html = path.read_text(encoding="utf-8", errors="ignore")
    soup = BeautifulSoup(html, "html.parser")
    for tag in soup(["script", "style", "svg", "noscript", "link", "meta"]):
        tag.decompose()
    for c in soup.find_all(string=lambda t: isinstance(t, Comment)):
        c.extract()
    for tag in soup.find_all(True):
        for attr in list(tag.attrs):
            if attr not in ("class", "id"):
                del tag[attr]
    text = re.sub(r"\n\s*\n+", "\n", str(soup))
    return text[:max_chars]


def _file_preview_any(path: Path, max_chars: int) -> str:
    ext = path.suffix.lower()
    if ext in (".html", ".htm"):
        return _file_preview_html(path, max_chars)
    if ext == ".pdf":
        return extract_pdf(path)[:max_chars]
    return extract_text_file(path)[:max_chars]


def _declared_columns(table_text: str) -> list:
    """Column names the schema itself promised, e.g. lines like '- `ASIN` (...)'.
    A script that just omits a hard field instead of setting it null must not be
    scored as 100% filled - it has to be checked against what was promised, not
    just against whatever keys it happened to emit."""
    return re.findall(r"^-\s*`([A-Za-z0-9_]+)`", table_text, re.MULTILINE)


def _non_null_fraction(stdout: str, expected_columns: list):
    """Fraction of the schema's declared columns that are actually populated
    across all rows (a column missing from the dict entirely counts as null,
    same as an explicit None - a script can't game this by only setting keys
    it found values for). Returns None if stdout isn't parseable JSON."""
    try:
        data = json.loads(stdout)
    except Exception:
        return None
    rows = data if isinstance(data, list) else [data]
    if not rows:
        return 0.0
    cols = expected_columns or None
    total = populated = 0
    for row in rows:
        if not isinstance(row, dict):
            continue
        keys = cols if cols else row.keys()
        for k in keys:
            v = row.get(k)
            total += 1
            if v not in (None, "", []):
                populated += 1
    return (populated / total) if total else 0.0


def _codegen_worker(gid: str, table_id: str):
    CODEGEN_CONTROL[gid] = {"pause": False}
    meta = load_meta(gid)
    table = next((t for t in meta.get("tables", []) if t["id"] == table_id), None)
    if table is None:
        LIVE[f"codegen:{gid}"] = {"phase": "error", "detail": "table not found", "iterations": []}
        return

    files = meta["files"]
    fdir = group_dir(gid) / "files"
    per_file_budget = max(1500, 9000 // max(len(files), 1))
    previews = []
    for fname in files:
        try:
            snippet = _file_preview_any(fdir / fname, per_file_budget)
        except Exception as e:
            snippet = f"[preview failed: {e}]"
        previews.append(f"----- {fname} (first {per_file_budget} chars, real ids/classes kept) -----\n{snippet}")
    previews_text = "\n\n".join(previews)

    base_prompt = f"""{CODEGEN_TOOLS_BLURB}

Below is a PREVIEW of each real file's actual structure (only the first part of \
each - the full files are longer, but the ids/classes shown are genuine and will \
recur in the rest of each file). Base your selectors on what you actually see \
here, not on generic/typical Amazon markup you might recall - the real page may \
differ from the common template.

{previews_text}

Write ONE Python script that parses the data for this table from the real files \
listed above (open them by filename), and prints the resulting rows as JSON to \
stdout (a list of row objects). Do not invent data - if a row/field genuinely \
can't be found, omit it or use null rather than guessing.

TABLE TO PARSE:
### {table['name']}
{table['text']}

Return ONLY the script in a single ```python code block, no other text."""

    context = meta.get("context_messages", [{"role": "system", "content": SYSTEM_PROMPT}])
    model = meta.get("model") or DEFAULT_TEXT_MODEL
    num_ctx = meta.get("num_ctx") or 32768
    iterations = []
    LIVE[f"codegen:{gid}"] = {"phase": "running", "detail": "", "iterations": iterations, "done": False}
    CANCELLED.discard(gid)  # once, up front - a cancel mid-loop must survive across iterations

    messages = context + [{"role": "user", "content": base_prompt}]

    for i in range(1, CODEGEN_MAX_ITER + 1):
        if CODEGEN_CONTROL.get(gid, {}).get("pause"):
            LIVE[f"codegen:{gid}"]["phase"] = "paused"
            LIVE[f"codegen:{gid}"]["done"] = True
            return

        iter_state = {"n": i, "phase": "generating", "code": "", "stdout": "", "stderr": "", "returncode": None}
        iterations.append(iter_state)
        LIVE[f"codegen:{gid}"]["detail"] = f"iteration {i}: generating code..."

        try:
            answer = "".join(_ollama_stream(gid, messages, model, num_ctx, 3000))
        except IngestCancelled:
            iter_state["phase"] = "cancelled"
            LIVE[f"codegen:{gid}"]["phase"] = "cancelled"
            LIVE[f"codegen:{gid}"]["done"] = True
            return
        except Exception as e:
            iter_state["stderr"] = f"[LLM request failed: {e}]"
            iter_state["phase"] = "failed"
            break

        code = _extract_code_block(answer)
        iter_state["code"] = code
        messages.append({"role": "assistant", "content": answer})

        if CODEGEN_CONTROL.get(gid, {}).get("pause"):
            iter_state["phase"] = "paused-before-run"
            LIVE[f"codegen:{gid}"]["phase"] = "paused"
            LIVE[f"codegen:{gid}"]["done"] = True
            return

        iter_state["phase"] = "running"
        LIVE[f"codegen:{gid}"]["detail"] = f"iteration {i}: running in sandbox..."
        result = api_sandbox_run({"code": code, "group_id": gid})
        iter_state["stdout"] = result["stdout"]
        iter_state["stderr"] = result["stderr"]
        iter_state["returncode"] = result["returncode"]

        ran_clean = result["returncode"] == 0 and not result["stderr"].strip()
        expected_cols = _declared_columns(table["text"])
        fill_frac = _non_null_fraction(result["stdout"], expected_cols) if ran_clean else None

        if ran_clean and fill_frac is not None and fill_frac >= 0.75:
            iter_state["phase"] = "success"
            iter_state["fill_fraction"] = fill_frac
            LIVE[f"codegen:{gid}"]["phase"] = "success"
            LIVE[f"codegen:{gid}"]["done"] = True
            m = load_meta(gid)
            for t in m.get("tables", []):
                if t["id"] == table_id:
                    t["generated_code"] = code
            save_meta(gid, m)
            return

        if ran_clean and fill_frac is not None:
            # Ran without crashing, but the selectors don't actually match the
            # real page - most fields came back null. That's a real failure,
            # not success, even though returncode == 0.
            iter_state["phase"] = "error"
            iter_state["fill_fraction"] = fill_frac
            fix_prompt = (
                f"That script ran without crashing, but only {fill_frac*100:.0f}% of "
                f"the schema's declared columns are actually populated across the "
                f"output rows (a column your dict never sets at all counts the same "
                f"as an explicit null here - always include every declared column key "
                f"in each row, using null when you genuinely can't find it):\n\n"
                f"{result['stdout'][:2000]}\n\n"
                f"A low fill rate usually means your selectors don't match the real "
                f"file structure, even though the code itself doesn't error. Look "
                f"again at the file preview you were given above and use the ACTUAL "
                f"ids/classes shown there, not ones you recall from typical Amazon "
                f"pages elsewhere - they may be named differently on this specific "
                f"page. Return ONLY the complete corrected script in a single "
                f"```python code block, no other text."
            )
        elif ran_clean:
            # ran clean but stdout wasn't valid JSON at all
            iter_state["phase"] = "error"
            fix_prompt = (
                f"That script ran without crashing, but its stdout isn't valid JSON:\n\n"
                f"{result['stdout'][:2000]}\n\n"
                f"Make sure the script's ONLY stdout output is a single "
                f"`json.dumps(...)` call producing a JSON array of row objects - no "
                f"print() debugging statements mixed in. Return ONLY the complete "
                f"corrected script in a single ```python code block, no other text."
            )
        else:
            iter_state["phase"] = "error"
            fix_prompt = (
                f"That script failed when run for real. Here is the exact output:\n\n"
                f"STDOUT:\n{result['stdout']}\n\nSTDERR:\n{result['stderr']}\n\n"
                f"Return code: {result['returncode']}\n\n"
                f"Fix the bug. Think about whether this is a code bug versus a wrong "
                f"assumption about the file/field, and fix only what's actually broken. "
                f"Return ONLY the complete corrected script in a single ```python code "
                f"block, no other text."
            )
        messages.append({"role": "user", "content": fix_prompt})

    LIVE[f"codegen:{gid}"]["phase"] = LIVE[f"codegen:{gid}"].get("phase", "exhausted")
    if LIVE[f"codegen:{gid}"]["phase"] == "running":
        LIVE[f"codegen:{gid}"]["phase"] = "exhausted"
    LIVE[f"codegen:{gid}"]["done"] = True


@app.post("/api/groups/{gid}/codegen/start")
def api_codegen_start(gid: str, payload: dict):
    table_id = payload["table_id"]
    threading.Thread(target=_codegen_worker, args=(gid, table_id), daemon=True).start()
    return {"ok": True}


@app.post("/api/groups/{gid}/codegen/pause")
def api_codegen_pause(gid: str):
    CODEGEN_CONTROL.setdefault(gid, {})["pause"] = True
    return {"ok": True}


@app.get("/api/groups/{gid}/codegen/stream")
def api_codegen_stream(gid: str):
    def gen():
        last_json = None
        while True:
            state = LIVE.get(f"codegen:{gid}", {"phase": "idle", "iterations": [], "done": False})
            cur = json.dumps(state)
            if cur != last_json:
                yield f"data: {cur}\n\n"
                last_json = cur
            if state.get("done"):
                break
            time.sleep(0.4)

    return StreamingResponse(gen(), media_type="text/event-stream")


app.mount("/", StaticFiles(directory=str(HERE / "static"), html=True), name="static")
