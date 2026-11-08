#!/usr/bin/env python3
"""Hash-locked requirements for Brainiac's Python worker (G-09, O-3.5).

For each requirement group (workers/python/requirements-<group>.txt) and each
supported target (CPython 3.11-3.14 on Windows x86_64, Linux x86_64 and
aarch64, macOS arm64), this writes workers/python/locks/<group>-cp3N-<target>.txt:
every package the group needs there, transitive ones included, pinned to one
version, with the sha256 of every wheel of that version that could install on
that target. Brainiac installs only from these files, with pip's
--require-hashes and --only-binary=:all: (internal/install, internal/worker),
and refuses an install with no lock for the machine.

It reads PyPI's own JSON indexes and nothing else: the Simple API
(application/vnd.pypi.simple.v1+json) for each project's files and their
sha256, and each chosen wheel's core metadata (PEP 658) for its dependencies.
Nothing is downloaded but metadata, and nothing is installed. Markers are
evaluated for the target, not for the machine running this, so it can run
anywhere with network access:

    py -3 -I brainiac/scripts/lock_worker.py            (Windows)
    python3 -I brainiac/scripts/lock_worker.py          (elsewhere)

The groups are resolved together on each target, so installing several
groups into one environment never asks for two versions of a package; each
group's lock holds only what its own requirements reach. Only wheels count:
a package with no wheel for a target fails that target, never an sdist
(which would run its build with unhashed build dependencies). The newest
version that satisfies every constraint and has a wheel is chosen; when a
later constraint rules a choice out, resolution starts again with it.
"""

import argparse
import email.parser
import json
import os
import re
import sys
import time
import urllib.request

try:
    from packaging.markers import Marker  # noqa: F401
    from packaging.requirements import Requirement
    from packaging.specifiers import SpecifierSet
    from packaging.utils import canonicalize_name
    from packaging.version import InvalidVersion, Version
except ImportError:  # pip's own copy
    from pip._vendor.packaging.requirements import Requirement
    from pip._vendor.packaging.specifiers import SpecifierSet
    from pip._vendor.packaging.utils import canonicalize_name
    from pip._vendor.packaging.version import InvalidVersion, Version

PYTHONS = ["3.11", "3.12", "3.13", "3.14"]

# Where each target is, for markers, and which wheel platforms install there.
# The glibc and macOS ceilings bound which version is chosen (it must have a
# wheel for a machine that old); every wheel of that version for the
# platform family is hashed, so pip on a newer machine may take a newer one.
TARGETS = {
    "win_amd64": {"sys_platform": "win32", "platform_system": "Windows", "os_name": "nt", "platform_machine": "AMD64"},
    "linux_x86_64": {"sys_platform": "linux", "platform_system": "Linux", "os_name": "posix", "platform_machine": "x86_64"},
    "linux_aarch64": {"sys_platform": "linux", "platform_system": "Linux", "os_name": "posix", "platform_machine": "aarch64"},
    "macos_arm64": {"sys_platform": "darwin", "platform_system": "Darwin", "os_name": "posix", "platform_machine": "arm64"},
}
GLIBC_MAX = (2, 35)  # Ubuntu 22.04
MACOS_MAX = (14, 0)

SIMPLE = "https://pypi.org/simple/{}/"
UA = "brainiac-lock-worker/1 (+https://decurix.computer)"


class Fetcher:
    """PyPI JSON with an on-disk cache, so a rerun costs nothing."""

    def __init__(self, cache):
        self.cache = cache
        os.makedirs(cache, exist_ok=True)

    def get(self, url, accept=None):
        key = re.sub(r"[^A-Za-z0-9._-]", "_", url)[-180:]
        path = os.path.join(self.cache, key)
        if os.path.exists(path):
            with open(path, "rb") as f:
                return f.read()
        headers = {"User-Agent": UA}
        if accept:
            headers["Accept"] = accept
        for attempt in range(4):
            try:
                with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=60) as r:
                    data = r.read()
                break
            except Exception:  # retried, then raised
                if attempt == 3:
                    raise
                time.sleep(2 ** attempt)
        with open(path, "wb") as f:
            f.write(data)
        return data

    def files(self, name):
        data = self.get(SIMPLE.format(canonicalize_name(name)), "application/vnd.pypi.simple.v1+json")
        return json.loads(data)["files"]


WHEEL = re.compile(r"^(?P<name>.+?)-(?P<ver>[^-]+)(-(?P<build>\d[^-]*))?-(?P<py>[^-]+)-(?P<abi>[^-]+)-(?P<plat>[^-]+)\.whl$")


def parse_wheel(filename):
    m = WHEEL.match(filename)
    if not m:
        return None
    try:
        v = Version(m.group("ver"))
    except InvalidVersion:
        return None
    return v, m.group("py").split("."), m.group("abi").split("."), m.group("plat").split(".")


def interp_ok(py, abi, minor):
    """A wheel's python and ABI tags install on CPython 3.<minor>."""
    if abi == "none":
        if py in ("py3", "py2.py3", "cp3%d" % minor, "py3%d" % minor):
            return True
        m = re.fullmatch(r"cp3(\d+)", py)
        return bool(m) and int(m.group(1)) <= minor
    if abi == "abi3":
        m = re.fullmatch(r"cp3(\d+)", py)
        return bool(m) and int(m.group(1)) <= minor
    return py == "cp3%d" % minor and abi == "cp3%d" % minor  # never the free-threaded cp3Nt


def plat_family(plat, target):
    """A wheel platform tag installs on the target's family, and the oldest
    machine it needs (a glibc or macOS version), or None when it does not."""
    if plat == "any":
        return ()
    if target == "win_amd64":
        return () if plat == "win_amd64" else None
    if target.startswith("linux_"):
        arch = target[len("linux_"):]
        legacy = {"manylinux1": (2, 5), "manylinux2010": (2, 12), "manylinux2014": (2, 17)}
        m = re.fullmatch(r"(manylinux1|manylinux2010|manylinux2014)_(.+)", plat)
        if m and m.group(2) == arch:
            return legacy[m.group(1)]
        m = re.fullmatch(r"manylinux_(\d+)_(\d+)_(.+)", plat)
        if m and m.group(3) == arch:
            return (int(m.group(1)), int(m.group(2)))
        return None
    if target == "macos_arm64":
        m = re.fullmatch(r"macosx_(\d+)_(\d+)_(arm64|universal2)", plat)
        if m:
            return (int(m.group(1)), int(m.group(2)))
        return None
    return None


def wheel_fits(filename, minor, target, ceiling=True):
    p = parse_wheel(filename)
    if not p:
        return False
    _, pys, abis, plats = p
    if not any(interp_ok(py, abi, minor) for py in pys for abi in abis):
        return False
    for plat in plats:
        need = plat_family(plat, target)
        if need is None:
            continue
        if not ceiling or need == ():
            return True
        limit = GLIBC_MAX if target.startswith("linux_") else MACOS_MAX
        if need <= limit:
            return True
    return False


class Project:
    def __init__(self, fetch, name):
        self.name = canonicalize_name(name)
        self.by_version = {}
        for f in fetch.files(name):
            if f.get("yanked") or not f["filename"].endswith(".whl"):
                continue
            p = parse_wheel(f["filename"])
            if p:
                self.by_version.setdefault(p[0], []).append(f)


def requires_python_ok(f, full):
    rp = f.get("requires-python")
    if not rp:
        return True
    try:
        return SpecifierSet(rp).contains(full, prereleases=True)
    except Exception:
        return True


class Resolver:
    def __init__(self, fetch):
        self.fetch = fetch
        self.projects = {}
        self.meta = {}

    def project(self, name):
        n = canonicalize_name(name)
        if n not in self.projects:
            self.projects[n] = Project(self.fetch, n)
        return self.projects[n]

    def candidates(self, name, minor, target):
        full = "3.%d.0" % minor
        out = []
        for v, files in self.project(name).by_version.items():
            fit = [f for f in files if wheel_fits(f["filename"], minor, target) and requires_python_ok(f, full)]
            if fit:
                out.append((v, fit))
        out.sort(key=lambda x: x[0], reverse=True)
        return out

    def requires(self, f):
        """Requires-Dist of one wheel, from its PEP 658 core metadata."""
        url = f["url"].split("#")[0]
        if url not in self.meta:
            if f.get("core-metadata") or f.get("data-dist-info-metadata"):
                raw = self.fetch.get(url + ".metadata")
            else:
                raise SystemExit("%s has no core metadata on PyPI; cannot read its dependencies without downloading it" % f["filename"])
            msg = email.parser.BytesParser().parsebytes(raw, headersonly=True)
            self.meta[url] = msg.get_all("Requires-Dist") or []
        return self.meta[url]

    def resolve(self, roots, minor, target):
        """Pins for every package roots reach on the target, the dependency
        edges between them, and the wheels of each pin that install there."""
        env = dict(TARGETS[target])
        env.update({"python_version": "3.%d" % minor, "python_full_version": "3.%d.0" % minor,
                    "implementation_name": "cpython", "platform_python_implementation": "CPython", "extra": ""})
        learned = {}  # name -> list of SpecifierSet found to rule out an earlier choice
        for _ in range(200):
            pins, edges, extras, conflict = {}, {}, {}, None
            specs = {k: list(v) for k, v in learned.items()}
            queue = [(Requirement(r), None) for r in roots]
            while queue and conflict is None:
                req, parent = queue.pop(0)
                if req.marker is not None:
                    want = [""] + sorted(extras.get(parent, set())) if parent else [""]
                    if not any(req.marker.evaluate(dict(env, extra=e)) for e in want):
                        continue
                name = canonicalize_name(req.name)
                if parent:
                    edges.setdefault(parent, set()).add(name)
                specs.setdefault(name, []).append(req.specifier)
                new_extras = set(req.extras) - extras.get(name, set())
                extras.setdefault(name, set()).update(req.extras)
                if name in pins:
                    if all(s.contains(pins[name][0], prereleases=True) for s in specs[name]):
                        if new_extras:
                            queue.extend(self.deps(name, pins[name][1], new_extras))
                        continue
                    conflict = (name, req.specifier)
                    break
                # A pre-release only when a constraint names one (packaging
                # lets an empty specifier match them).
                pre = any(s.prereleases for s in specs[name] if str(s))
                cands = [c for c in self.candidates(name, minor, target)
                         if (pre or not c[0].is_prerelease) and all(s.contains(c[0], prereleases=True) for s in specs[name])]
                if not cands:
                    raise LookupError("%s: no wheel of %s fits %s on CPython 3.%d (%s)" % (
                        target, name, " and ".join(str(s) or "any" for s in specs[name]), minor, target))
                pins[name] = cands[0]
                queue.extend(self.deps(name, cands[0][1], extras[name]))
            if conflict is None:
                return pins, edges
            learned.setdefault(conflict[0], []).append(conflict[1])
        raise LookupError("%s cp3%d: resolution did not settle" % (target, minor))

    def deps(self, name, files, extras):
        out = []
        for line in self.requires(files[0]):
            r = Requirement(line)
            out.append((r, name))
        return out


def closure(roots, edges):
    seen, stack = set(), list(roots)
    while stack:
        n = stack.pop()
        if n in seen:
            continue
        seen.add(n)
        stack.extend(edges.get(n, ()))
    return seen


def read_roots(path):
    roots = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                roots.append(line)
    return roots


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--worker-dir", default=os.path.join(here, "..", "workers", "python"))
    ap.add_argument("--cache", default=os.path.join(os.path.expanduser("~"), ".cache", "brainiac-lock"))
    ap.add_argument("--groups", default="asr,ocr")
    ap.add_argument("--pythons", default=",".join(PYTHONS))
    ap.add_argument("--targets", default=",".join(TARGETS))
    args = ap.parse_args()
    fetch = Fetcher(args.cache)
    res = Resolver(fetch)
    groups = [g.strip() for g in args.groups.split(",") if g.strip()]
    roots = {g: read_roots(os.path.join(args.worker_dir, "requirements-%s.txt" % g)) for g in groups}
    out_dir = os.path.join(args.worker_dir, "locks")
    os.makedirs(out_dir, exist_ok=True)
    covered, failed = [], []
    for py in args.pythons.split(","):
        minor = int(py.split(".")[1])
        for target in args.targets.split(","):
            try:
                pins, edges = res.resolve([r for g in groups for r in roots[g]], minor, target)
            except LookupError as e:
                failed.append("cp3%d %s: %s" % (minor, target, e))
                continue
            for g in groups:
                mine = closure([canonicalize_name(Requirement(r).name) for r in roots[g]
                                if Requirement(r).marker is None or Requirement(r).marker.evaluate(dict(
                                    TARGETS[target], python_version="3.%d" % minor, python_full_version="3.%d.0" % minor,
                                    implementation_name="cpython", platform_python_implementation="CPython", extra=""))], edges)
                lines = [
                    "# Hash-locked requirements-%s.txt for CPython 3.%d on %s." % (g, minor, target),
                    "# Generated by brainiac/scripts/lock_worker.py from PyPI's JSON index; do not edit.",
                    "# Brainiac installs it with pip --require-hashes --only-binary=:all:.",
                ]
                for name in sorted(mine):
                    v, _ = pins[name]
                    files = [f for f in res.project(name).by_version[v] if wheel_fits(f["filename"], minor, target, ceiling=False)]
                    hashes = sorted({f["hashes"]["sha256"] for f in files})
                    lines.append("%s==%s \\" % (name, v))
                    for i, h in enumerate(hashes):
                        lines.append("    --hash=sha256:%s%s" % (h, " \\" if i < len(hashes) - 1 else ""))
                path = os.path.join(out_dir, "%s-cp3%d-%s.txt" % (g, minor, target))
                with open(path, "w", encoding="utf-8", newline="\n") as f:
                    f.write("\n".join(lines) + "\n")
            covered.append("cp3%d %s (%s)" % (minor, target, ", ".join(
                "%s %s" % (n, pins[n][0]) for n in sorted(pins) if n in ("numpy", "onnxruntime", "opencv-python-headless", "sherpa-onnx"))))
    print("Locked:")
    for c in covered:
        print("  " + c)
    if failed:
        print("Not locked (Brainiac refuses to install the worker there):")
        for f in failed:
            print("  " + f)
    return 0 if covered else 1


if __name__ == "__main__":
    sys.exit(main())
