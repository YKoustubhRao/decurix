"""Durable, engine-independent workflows. Checkpoints are evidence, not claims.

An executor may use office tools, GUI, browser or a connector. No application names
are special to the runner. All artifacts live in a separate directory per run.
Per-run file locks serialize checkpoint changes; ownership leases prevent two
workers from claiming the same running stage. Executors must stop before recovery.
"""
import copy
import hashlib
import json
import os
import re
import socket
import threading
import time
import uuid
import zipfile
import xml.etree.ElementTree as ET
from pathlib import Path
from contextlib import contextmanager
from functools import wraps
from decurix_agent import filelock


MAX_REPORT_CHARS = 2000


def missing_inputs(spec, inputs):
    """Zero and False are supplied values; absence and empty data are not."""
    def absent(value):
        return value is None or (isinstance(value, str) and not value.strip()) or (isinstance(value, (list, dict, tuple)) and not value)
    return [key for key in spec.get("required_inputs", []) if key not in inputs or absent(inputs[key])]


def _attempt_limit(step, record):
    return step.get("max_attempts", 2) + record.get("additional_attempts", 0)


def _serialized(method):
    @wraps(method)
    def call(self, run_id, *args, **kwargs):
        with self._lock(run_id):
            return method(self, run_id, *args, **kwargs)
    return call


def _alive(pid):
    if not isinstance(pid, int) or pid <= 0:
        return False
    if pid == os.getpid():
        return True
    if os.name == "nt":
        import ctypes
        kernel = ctypes.windll.kernel32
        kernel.OpenProcess.restype = ctypes.c_void_p
        kernel.CloseHandle.argtypes = [ctypes.c_void_p]
        kernel.GetExitCodeProcess.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ulong)]
        handle = kernel.OpenProcess(0x1000, False, pid)
        if not handle:
            return kernel.GetLastError() == 5  # Access denied cannot prove the owner dead.
        code = ctypes.c_ulong()
        try:
            return bool(kernel.GetExitCodeProcess(handle, ctypes.byref(code))) and code.value == 259
        finally:
            kernel.CloseHandle(handle)
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _process_identity(pid):
    """Detect PID reuse, including PID reuse after a container/host restart."""
    try:
        if os.name != "nt":
            boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            stat = Path(f"/proc/{pid}/stat").read_text()
            return boot + ":" + stat.rsplit(")", 1)[1].split()[19]
        import ctypes
        kernel = ctypes.windll.kernel32
        kernel.OpenProcess.restype = ctypes.c_void_p
        kernel.CloseHandle.argtypes = [ctypes.c_void_p]
        kernel.GetProcessTimes.argtypes = [ctypes.c_void_p] + [ctypes.POINTER(ctypes.c_ulonglong)] * 4
        handle = kernel.OpenProcess(0x1000, False, pid)
        if not handle:
            return None
        times = [ctypes.c_ulonglong() for _ in range(4)]
        try:
            if kernel.GetProcessTimes(handle, *[ctypes.byref(item) for item in times]):
                return str(times[0].value)
        finally:
            kernel.CloseHandle(handle)
    except (OSError, ValueError, IndexError):
        pass
    return None


def validate(spec):
    if not isinstance(spec, dict):
        raise ValueError("workflow must be an object")
    steps = spec.get("steps", [])
    if not isinstance(spec.get("id"), str) or not spec["id"].strip() or not isinstance(steps, list) or not steps:
        raise ValueError("workflow needs an id and steps")
    if any(not isinstance(s, dict) for s in steps):
        raise ValueError("steps must contain objects")
    required = spec.get("required_inputs", [])
    if not isinstance(required, list) or any(not isinstance(k, str) or not k for k in required):
        raise ValueError("required_inputs must be a list of names")
    ids = [s.get("id") for s in steps]
    if any(not isinstance(i, str) or not re.fullmatch(r"[\w-]+", i) for i in ids) or len(set(ids)) != len(ids):
        raise ValueError("step ids must be unique safe names")
    seen = set()
    for s in steps:
        if not isinstance(s.get("instruction"), str) or not s["instruction"].strip() or not isinstance(s.get("checks"), list) or not s["checks"]:
            raise ValueError("every step needs an instruction and independently checkable outputs")
        dependencies = s.get("depends_on", [])
        if not isinstance(dependencies, list) or any(not isinstance(k, str) for k in dependencies):
            raise ValueError("depends_on must be a list of step ids")
        if set(dependencies) - seen:
            raise ValueError("dependencies must name earlier steps (cycles are forbidden)")
        if type(s.get("max_attempts", 2)) is not int or not 1 <= s.get("max_attempts", 2) <= 10:
            raise ValueError("max_attempts must be between 1 and 10")
        for key in ("retry_safe", "approval"):
            if key in s and type(s[key]) is not bool:
                raise ValueError(f"{key} must be boolean")
        if "skill" in s and (not isinstance(s["skill"], dict) or not isinstance(s["skill"].get("name"), str) or not s["skill"]["name"].strip() or not isinstance(s["skill"].get("params", {}), dict)):
            raise ValueError("skill needs a name and parameter object")
        if "skill" in s:
            sources = s["skill"].get("source_files", {})
            if not isinstance(sources, dict) or any(not isinstance(k, str) or not k.strip() or not (
                    isinstance(v, str) and v.strip() or isinstance(v, dict) and set(v) == {"input"}
                    and isinstance(v["input"], str) and v["input"].strip()) for k, v in sources.items()):
                raise ValueError("skill source_files must map recorded paths to paths or input bindings")
        for c in s["checks"]:
            if not isinstance(c, dict) or not isinstance(c.get("path"), str):
                raise ValueError("check needs a string path")
            if set(c) - {"path", "format", "contains", "json_equals", "columns", "min_rows", "sha256"}:
                raise ValueError("unknown artifact check field")
            p = Path(c.get("path", ""))
            if not str(c.get("path", "")) or p.is_absolute() or ".." in p.parts or ":" in str(p) or "\\" in str(p):
                raise ValueError("artifact paths must be relative and confined to this run")
            if c.get("format", "text") not in {"text", "json", "xlsx", "pptx", "docx", "ods", "odt", "odp", "pdf", "eml", "csv", "tsv", "html", "xml", "image", "binary"}:
                raise ValueError("unsupported artifact format")
            if not isinstance(c.get("contains", []), list) or any(not isinstance(v, (str, int, float)) or isinstance(v, bool) for v in c.get("contains", [])):
                raise ValueError("contains must be a list of text or numbers")
            if "json_equals" in c and (c.get("format") != "json" or not isinstance(c["json_equals"], dict)):
                raise ValueError("json_equals requires JSON format and pointer/value object")
            if "json_equals" in c and any(not isinstance(k, str) or (k and not k.startswith("/")) for k in c["json_equals"]):
                raise ValueError("json_equals keys must be JSON pointers")
            if "columns" in c and (c.get("format") not in {"csv", "tsv"} or not isinstance(c["columns"], list) or any(not isinstance(k, str) for k in c["columns"])):
                raise ValueError("columns requires CSV/TSV and a list of column names")
            if "min_rows" in c and (c.get("format") not in {"csv", "tsv"} or type(c["min_rows"]) is not int or c["min_rows"] < 0):
                raise ValueError("min_rows requires CSV/TSV and a nonnegative integer")
            if "sha256" in c and (not isinstance(c["sha256"], str) or not re.fullmatch(r"[a-f0-9]{64}", c["sha256"])):
                raise ValueError("sha256 must be a lowercase digest")
            if c.get("format") in {"binary", "image"} and c.get("contains"):
                raise ValueError("binary/image content checks require a hash or an independent verifier")
        seen.add(s["id"])
    return spec


def artifact_evidence(step, result, context):
    """Read outputs independently; hash every checked artifact for resume auditing."""
    base = Path(context["work_dir"]).resolve()
    evidence = []
    for check in step["checks"]:
        p = (base / check["path"]).resolve()
        if not p.is_relative_to(base) or not p.is_file():
            raise ValueError(f"missing/confined artifact: {check['path']}")
        data = p.read_bytes()
        if not data:
            raise ValueError(f"empty artifact: {check['path']}")
        fmt = check.get("format", "text")
        if "sha256" in check and hashlib.sha256(data).hexdigest() != check["sha256"]:
            raise ValueError("artifact SHA-256 mismatch")
        if fmt in {"binary", "image"}:
            text = ""
            if fmt == "image":
                from PIL import Image
                with Image.open(p) as picture:
                    picture.verify()
        elif fmt in {"ods", "odt", "odp"}:
            mime = {"ods": "spreadsheet", "odt": "text", "odp": "presentation"}[fmt]
            with zipfile.ZipFile(p) as archive:
                if archive.read("mimetype").decode().strip() != "application/vnd.oasis.opendocument." + mime:
                    raise ValueError("invalid OpenDocument MIME type")
                document = ET.fromstring(archive.read("content.xml"))
                office = "{urn:oasis:names:tc:opendocument:xmlns:office:1.0}"
                if document.tag != office + "document-content":
                    raise ValueError("invalid OpenDocument root")
                body = document.find(office + "body/" + office + mime)
                if body is None:
                    raise ValueError("invalid OpenDocument body")
                text = "\n".join(body.itertext())
                if not text.strip():
                    raise ValueError("OpenDocument needs visible content")
        elif fmt in {"xlsx", "pptx", "docx"}:
            required = {"xlsx": "xl/workbook.xml", "pptx": "ppt/presentation.xml", "docx": "word/document.xml"}[fmt]
            with zipfile.ZipFile(p) as z:
                if required not in z.namelist() or "[Content_Types].xml" not in z.namelist():
                    raise ValueError(f"invalid {fmt} package")
                ET.fromstring(z.read("[Content_Types].xml"))
                document = ET.fromstring(z.read(required))
                expected_root = {"xlsx": "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}workbook",
                                 "pptx": "{http://schemas.openxmlformats.org/presentationml/2006/main}presentation",
                                 "docx": "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}document"}[fmt]
                if document.tag != expected_root:
                    raise ValueError(f"invalid {fmt} document root")
                if fmt == "pptx":
                    parts = [n for n in z.namelist() if re.fullmatch(r"ppt/slides/slide\d+\.xml", n)]
                    ns = "{http://schemas.openxmlformats.org/drawingml/2006/main}"
                    text = "\n".join(node.text or "" for n in parts for node in ET.fromstring(z.read(n)).iter(ns + "t"))
                    if not parts or not text.strip():
                        raise ValueError("presentation needs populated slides")
                elif fmt == "docx":
                    ns = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
                    text = "\n".join(node.text or "" for node in document.iter(ns + "t"))
                    if not text.strip():
                        raise ValueError("document needs visible body text")
                else:
                    ns = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
                    parts = [n for n in z.namelist() if re.fullmatch(r"xl/worksheets/sheet\d+\.xml", n)]
                    if not parts:
                        raise ValueError("workbook needs worksheets")
                    strings = ET.fromstring(z.read("xl/sharedStrings.xml")) if "xl/sharedStrings.xml" in z.namelist() else ET.Element("empty")
                    texts = [node.get("name", "") for node in document.iter(ns + "sheet")]
                    shared = ["".join(node.text or "" for node in item.iter(ns + "t")) for item in strings.findall(ns + "si")]
                    for n in parts:
                        sheet = ET.fromstring(z.read(n))
                        for cell in sheet.iter(ns + "c"):
                            value = cell.find(ns + "v")
                            if cell.get("t") == "s" and value is not None:
                                index = int(value.text)
                                if not 0 <= index < len(shared):
                                    raise ValueError("invalid shared-string cell index")
                                texts.append(shared[index])
                            elif cell.get("t") == "inlineStr":
                                texts.append("".join(node.text or "" for node in cell.iter(ns + "t")))
                            elif value is not None:
                                texts.append(value.text or "")
                    text = "\n".join(texts)
        elif fmt == "pdf":
            if not data.startswith(b"%PDF-"):
                raise ValueError("invalid PDF")
            # Content checks for PDF require the installed extraction tool.
            import subprocess
            text = subprocess.run(["pdftotext", str(p), "-"], capture_output=True, text=True, check=True, timeout=30).stdout
        else:
            text = data.decode("utf-8")
            if fmt in {"csv", "tsv"}:
                import csv
                import io
                rows = list(csv.reader(io.StringIO(text), delimiter="\t" if fmt == "tsv" else ",", strict=True))
                if not rows or not rows[0] or any(len(row) != len(rows[0]) for row in rows):
                    raise ValueError("delimited file needs a header and consistent columns")
                if any(column not in rows[0] for column in check.get("columns", [])):
                    raise ValueError("delimited file missing required columns")
                if len(rows) - 1 < check.get("min_rows", 0):
                    raise ValueError("delimited file has too few data rows")
            if fmt == "xml":
                document = ET.fromstring(text)
                text = "\n".join(document.itertext())
            if fmt == "html":
                from html.parser import HTMLParser
                class VisibleText(HTMLParser):
                    def __init__(self):
                        super().__init__(convert_charrefs=True)
                        self.text, self.hidden, self.tags = [], 0, 0
                    def handle_starttag(self, tag, attrs):
                        self.tags += 1
                        if tag in {"script", "style"}:
                            self.hidden += 1
                    def handle_endtag(self, tag):
                        if tag in {"script", "style"} and self.hidden:
                            self.hidden -= 1
                    def handle_data(self, data):
                        if not self.hidden:
                            self.text.append(data)
                parser = VisibleText()
                parser.feed(text)
                if not parser.tags:
                    raise ValueError("HTML artifact needs markup")
                text = "\n".join(parser.text)
            if fmt == "json":
                parsed = json.loads(text, parse_constant=lambda value: (_ for _ in ()).throw(ValueError("non-finite JSON value")))
                for pointer, expected in check.get("json_equals", {}).items():
                    value = parsed
                    if pointer and not pointer.startswith("/"):
                        raise ValueError("JSON assertions use RFC 6901 pointers")
                    for key in pointer.split("/")[1:] if pointer else []:
                        key = key.replace("~1", "/").replace("~0", "~")
                        value = value[int(key)] if isinstance(value, list) else value[key]
                    if type(value) is not type(expected) or value != expected:
                        raise ValueError(f"JSON value mismatch at {pointer}")
            if fmt == "eml" and not all(re.search(rf"^{h}:", text, re.M | re.I) for h in ("To", "Subject")):
                raise ValueError("mail draft needs To and Subject headers")
        for needle in check.get("contains", []):
            if str(needle) not in text:
                raise ValueError(f"{check['path']} missing required content: {needle}")
        evidence.append({"path": check["path"], "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)})
    return evidence


class WorkflowRunner:
    def __init__(self, root, executor=None, verifier=None, lease_seconds=3600, lock_timeout=60):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        self.executor = executor
        self.verifier = verifier
        if not isinstance(lease_seconds, (int, float)) or lease_seconds <= 0:
            raise ValueError("lease_seconds must be positive")
        self.lease_seconds, self.lock_timeout = lease_seconds, lock_timeout
        self.owner = uuid.uuid4().hex
        self._claims = {}
        self._mutex = threading.RLock()

    @contextmanager
    def _lock(self, run_id):
        self._path(run_id)
        directory = self.root / ".locks"
        directory.mkdir(exist_ok=True)
        path = directory / (hashlib.sha256(run_id.encode()).hexdigest() + ".lock")
        with self._mutex, path.open("a+b") as handle:
            handle.seek(0, 2)
            if not handle.tell():
                handle.write(b"0")
                handle.flush()
            deadline = time.monotonic() + self.lock_timeout
            handle.seek(0)
            while not filelock.try_exclusive(handle):
                if time.monotonic() >= deadline:
                    raise TimeoutError("workflow checkpoint is locked by another worker")
                time.sleep(0.02)
            yield  # Closing the handle releases the cross-platform OS lock.

    def _path(self, run_id):
        if not isinstance(run_id, str) or not re.fullmatch(r"[\w-]+", run_id):
            raise ValueError("invalid run id")
        path = self.root / run_id / "checkpoint.json"
        if not path.resolve().is_relative_to(self.root):
            raise ValueError("workflow run directory escaped its root")
        return path

    def _save(self, state):
        p = self._path(state["run_id"])
        p.parent.mkdir(parents=True, exist_ok=True)
        temp = p.with_name(p.name + "." + uuid.uuid4().hex + ".tmp")
        with temp.open("w", encoding="utf-8") as output:
            json.dump(state, output, indent=2, allow_nan=False)
            output.flush()
            os.fsync(output.fileno())
        temp.replace(p)
        return copy.deepcopy(state)

    def create(self, spec, inputs=None, run_id=None):
        run_id = run_id or uuid.uuid4().hex
        with self._lock(run_id):
            return self._create(spec, inputs, run_id)

    def _create(self, spec, inputs, run_id):
        validate(spec)
        if inputs is not None and not isinstance(inputs, dict):
            raise ValueError("inputs must be an object")
        missing = missing_inputs(spec, inputs or {})
        if missing:
            raise ValueError("missing required inputs: " + ", ".join(missing))
        p = self._path(run_id)
        if p.exists():
            raise ValueError("run id already exists; resume it instead")
        state = {"version": 1, "run_id": run_id, "spec": copy.deepcopy(spec), "inputs": inputs or {},
                 "status": "pending", "created_at": time.time(), "steps": {
                     s["id"]: {"status": "pending", "attempts": 0, "evidence": []} for s in spec["steps"]}}
        (p.parent / "artifacts").mkdir(parents=True)
        return self._save(state)

    def status(self, run_id):
        state = json.loads(self._path(run_id).read_text(encoding="utf-8"))
        if not isinstance(state, dict) or "spec" not in state:
            raise ValueError("checkpoint must contain a workflow object")
        validate(state["spec"])
        if state.get("run_id") != run_id or state.get("version") != 1 or not isinstance(state.get("inputs"), dict):
            raise ValueError("invalid checkpoint identity or version")
        if state.get("status") not in {"pending", "running", "awaiting_approval", "awaiting_backend", "uncertain", "completed", "failed", "cancelled"}:
            raise ValueError("invalid checkpoint status")
        if not isinstance(state.get("steps"), dict) or set(state["steps"]) != {s["id"] for s in state["spec"]["steps"]}:
            raise ValueError("checkpoint step ids do not match spec")
        for step in state["spec"]["steps"]:
            record = state["steps"][step["id"]]
            if not isinstance(record, dict) or type(record.get("additional_attempts", 0)) is not int or not 0 <= record.get("additional_attempts", 0) <= 10 - step.get("max_attempts", 2) or record.get("status") not in {"pending", "running", "completed", "failed"} or type(record.get("attempts")) is not int or not 0 <= record["attempts"] <= _attempt_limit(step, record) or not isinstance(record.get("evidence"), list):
                raise ValueError("invalid checkpoint step record")
        return state

    def context(self, state):
        return {"run_id": state["run_id"], "inputs": state["inputs"],
                "work_dir": str(self._path(state["run_id"]).parent / "artifacts"),
                "outputs": {k: v["evidence"] for k, v in state["steps"].items() if v["status"] == "completed"}}

    @_serialized
    def claim(self, run_id, approvals=(), recover=False):
        state = self.status(run_id)
        if state["status"] in {"cancelled", "failed"}:
            return state, None
        context = self.context(state)
        # Re-read evidence: a previously completed file that changed is not trusted.
        for s in state["spec"]["steps"]:
            record = state["steps"][s["id"]]
            if record["status"] == "completed":
                try:
                    if artifact_evidence(s, {}, context) != record["evidence"]:
                        raise ValueError("checked artifact changed")
                except Exception as exc:
                    state["status"], state["error"] = "failed", str(exc)
                    return self._save(state), None
                continue
            if record["status"] == "running":
                lease = record.get("lease", {})
                local = lease.get("host") == socket.gethostname()
                live = local and _alive(lease.get("pid"))
                if live and lease.get("process_identity"):
                    current_identity = _process_identity(lease["pid"])
                    if current_identity is not None:
                        live = lease["process_identity"] == current_identity
                if live or (not local and (not recover or lease.get("expires_at", 0) > time.time())):
                    return state, None
                if not s.get("retry_safe", False):
                    state["status"], state["error"] = "uncertain", "Interrupted non-idempotent step; reconcile external state before a new run"
                    return self._save(state), None
            if record["attempts"] >= _attempt_limit(s, record):
                state["status"] = "failed"
                return self._save(state), None
            if s.get("approval") and f"{run_id}:{s['id']}" not in approvals:
                state["status"], state["approval_required"] = "awaiting_approval", f"{run_id}:{s['id']}"
                return self._save(state), None
            if not record["attempts"]:
                record["baseline"] = {}
                for c in s["checks"]:
                    artifact = Path(context["work_dir"]) / c["path"]
                    if artifact.is_file():
                        record["baseline"][c["path"]] = hashlib.sha256(artifact.read_bytes()).hexdigest()
            record["status"], record["attempts"] = "running", record["attempts"] + 1
            token = uuid.uuid4().hex
            record["lease"] = {"token": token, "owner": self.owner, "host": socket.gethostname(), "pid": os.getpid(), "process_identity": _process_identity(os.getpid()), "expires_at": time.time() + self.lease_seconds}
            self._claims[(run_id, s["id"])] = token
            state["status"] = "running"
            state.pop("approval_required", None)
            state.pop("backend_wait", None)
            self._save(state)
            return state, s
        state["status"] = "completed"
        return self._save(state), None

    @_serialized
    def complete(self, run_id, step_id, result=None, error=None, claim_token=None, report=None):
        state = self.status(run_id)
        if state["status"] == "cancelled":
            return state
        step = next(s for s in state["spec"]["steps"] if s["id"] == step_id)
        record = state["steps"][step_id]
        if record["status"] != "running":
            raise ValueError("step was not claimed")
        # What the stage's executor said it did: kept beside the evidence (never
        # as evidence) so the person's final report can show it.
        if isinstance(report, str) and report.strip():
            record["report"] = report.strip()[:MAX_REPORT_CHARS]
        token = claim_token or self._claims.get((run_id, step_id))
        if not token or token != record.get("lease", {}).get("token"):
            raise ValueError("stage completion requires its current claim token")
        try:
            if error:
                raise ValueError(str(error))
            context = self.context(state)
            evidence = artifact_evidence(step, result or {}, context)
            if any(record.get("baseline", {}).get(item["path"]) == item["sha256"] for item in evidence):
                raise ValueError("artifact was already present and unchanged before this step")
            if self.verifier and not self.verifier(step, result or {}, context):
                raise ValueError("independent verifier rejected output")
            record.update(status="completed", evidence=evidence)
            record.pop("error", None)
            state["status"] = "pending"
        except Exception as exc:
            record["error"] = str(exc)
            retry = step.get("retry_safe", False) and record["attempts"] < _attempt_limit(step, record)
            record["status"] = "pending" if retry else "failed"
            state["status"] = "pending" if retry else "failed"
        record.pop("lease", None)
        self._claims.pop((run_id, step_id), None)
        return self._save(state)

    def advance(self, run_id, approvals=(), max_steps=None):
        if self.executor is None:
            raise ValueError("advance requires an executor")
        if max_steps is not None and (type(max_steps) is not int or max_steps < 0):
            raise ValueError("max_steps must be nonnegative")
        count = 0
        while max_steps is None or count < max_steps:
            state, step = self.claim(run_id, approvals)
            if step is None:
                return state
            try:
                result = self.executor(copy.deepcopy(step), self.context(state))
            except Exception as exc:
                self.complete(run_id, step["id"], error=exc)
            else:
                self.complete(run_id, step["id"], result)
            count += 1
        return self.status(run_id)

    @_serialized
    def retry(self, run_id, step_id):
        """An explicit operator request grants one safe attempt, never a reset."""
        state = self.status(run_id)
        step = next((s for s in state["spec"]["steps"] if s["id"] == step_id), None)
        record = state["steps"].get(step_id)
        if not step or state["status"] != "failed" or record["status"] != "failed":
            raise ValueError("retry requires a failed workflow and failed stage")
        if not step.get("retry_safe", False):
            raise ValueError("this stage is not retry-safe; reconcile its external effects before starting new work")
        if _attempt_limit(step, record) >= 10:
            raise ValueError("stage reached its lifetime limit of 10 attempts")
        for predecessor in state["spec"]["steps"]:
            previous = state["steps"][predecessor["id"]]
            if previous["status"] == "completed" and artifact_evidence(predecessor, {}, self.context(state)) != previous["evidence"]:
                raise ValueError("checked predecessor artifact changed; retry refused")
        state.setdefault("recovery_events", []).append({"action": "retry", "step_id": step_id,
            "at": time.time(), "previous_error": record.get("error", ""), "attempts": record["attempts"]})
        record["additional_attempts"] = record.get("additional_attempts", 0) + 1
        record["status"] = "pending"
        state["status"] = "pending"
        state.pop("error", None)
        return self._save(state)

    @_serialized
    def cancel(self, run_id):
        state = self.status(run_id)
        if state["status"] in {"completed", "cancelled"}:
            return state
        state["status"] = "cancelled"
        return self._save(state)

    @_serialized
    def bind_task(self, run_id, task_id):
        state = self.status(run_id)
        state["task_id"] = str(task_id)
        return self._save(state)

    @_serialized
    def defer_backend(self, run_id):
        state = self.status(run_id)
        if state["status"] in {"cancelled", "failed", "completed", "uncertain"} or any(record["status"] == "running" for record in state["steps"].values()):
            return state
        state["status"] = "awaiting_backend"
        state["backend_wait"] = f"The next stage needs the model backend. Resume with /workflow resume {run_id} when it is ready; completed stages are saved."
        return self._save(state)
