#!/usr/bin/env python3
# Copyright 2025-2026 Patrick J. Scruggs
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Check the selected modules of every tracked Go module against OSV.

The query is for exact selected versions, so OSV owns Go advisory range
evaluation. A checksum or an unselected go.mod graph edge is not a selection.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


API = "https://api.osv.dev/v1"
MAX_BATCH = 100
MAX_PAGES = 20

# These are supported package-closure targets, not claims that binaries were tested.
PACKAGE_TARGETS = (
    {"goos": "linux", "goarch": "amd64", "goamd64": "v1", "goarm64": None,
     "cgo": "0", "tags": []},
    {"goos": "linux", "goarch": "amd64", "goamd64": "v1", "goarm64": None,
     "cgo": "1", "tags": []},
    {"goos": "linux", "goarch": "arm64", "goamd64": None, "goarm64": "v8.0",
     "cgo": "0", "tags": []},
)
OPENPGP_ADVISORY = "GO-2026-5932"
OPENPGP_MODIFIED = "2026-07-10T05:44:31.101996029Z"
OPENPGP_SHA256 = "9357dd8ad55a6647c5a18fca61887c547549786d960a41de5f1038c979f573b1"
OPENPGP_MODULE = "golang.org/x/crypto"
OPENPGP_IMPORTS = (
    "golang.org/x/crypto/openpgp",
    "golang.org/x/crypto/openpgp/armor",
    "golang.org/x/crypto/openpgp/clearsign",
    "golang.org/x/crypto/openpgp/elgamal",
    "golang.org/x/crypto/openpgp/errors",
    "golang.org/x/crypto/openpgp/packet",
    "golang.org/x/crypto/openpgp/s2k",
)


class PolicyError(Exception):
    """An incomplete inventory or advisory answer cannot authorize a merge."""


def tracked_modules(root: Path) -> list[Path]:
    result = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        check=True, capture_output=True,
    )
    modules = sorted(
        root / os.fsdecode(item)
        for item in result.stdout.split(b"\0")
        if item and Path(os.fsdecode(item)).name == "go.mod"
    )
    if not modules or not all(path.is_file() for path in modules):
        raise PolicyError("Tracked Go module inventory is empty or incomplete")
    return modules


def json_stream(source: str) -> list[dict]:
    decoder = json.JSONDecoder()
    offset = 0
    values = []
    while offset < len(source):
        while offset < len(source) and source[offset].isspace():
            offset += 1
        if offset == len(source):
            break
        value, offset = decoder.raw_decode(source, offset)
        if not isinstance(value, dict):
            raise PolicyError("Go returned a non-object module entry")
        values.append(value)
    if not values:
        raise PolicyError("Go returned an empty selected module list")
    return values


def selected_modules(module_dir: Path) -> list[dict]:
    env = _base_go_env("local")
    result = subprocess.run(
        ["go", "-C", str(module_dir), "list", "-m", "-json", "all"],
        capture_output=True, text=True, encoding="utf-8", env=env,
    )
    if result.returncode:
        raise PolicyError(f"Cannot resolve {module_dir}: {result.stderr.strip()}")
    values = json_stream(result.stdout)
    if sum(entry.get("Main") is True for entry in values) != 1:
        raise PolicyError(f"Expected one main module in {module_dir}")
    return values


def _base_go_env(toolchain: str) -> dict[str, str]:
    env = dict(os.environ)
    for key in ("GOROOT", "GOOS", "GOARCH", "CGO_ENABLED", "GOAMD64", "GOARM64"):
        env.pop(key, None)
    env.update({"GOWORK": "off", "GO111MODULE": "on", "GOENV": "off",
                "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": toolchain,
                "GOEXPERIMENT": ""})
    return env


def _native_go_version() -> str:
    try:
        result = subprocess.run(["go", "env", "GOVERSION"], check=True,
                                capture_output=True, text=True, encoding="utf-8",
                                env=_base_go_env("local"), cwd=tempfile.gettempdir(),
                                timeout=60)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise PolicyError("Cannot identify the configured current Go compiler") from error
    version = result.stdout.strip()
    if not re.fullmatch(r"go1\.\d+\.\d+", version):
        raise PolicyError("Configured current Go compiler version is unsupported")
    return version


def _latest_stable_go_version() -> str:
    request = urllib.request.Request("https://go.dev/dl/?mode=json",
                                     headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            raw = response.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise PolicyError("Official Go release list exceeded its size bound")
        releases = json.loads(raw)
    except (OSError, TimeoutError, urllib.error.URLError, json.JSONDecodeError) as error:
        raise PolicyError("Cannot retrieve the official Go stable release list") from error
    if not isinstance(releases, list):
        raise PolicyError("Official Go release list is malformed")
    stable_versions = []
    for item in releases:
        if not isinstance(item, dict) or item.get("stable") is not True:
            continue
        version = item.get("version")
        if isinstance(version, str) and re.fullmatch(r"go1\.\d+\.\d+", version):
            match = re.fullmatch(r"go1\.(\d+)\.(\d+)", version)
            stable_versions.append((int(match.group(1)), int(match.group(2)), version))
    if stable_versions:
        return max(stable_versions)[2]
    raise PolicyError("Official Go release list has no recognized stable release")


def _normalize_go_floor(value: object, scope: str) -> str:
    if not isinstance(value, str):
        raise PolicyError(f"Go compatibility floor is missing in {scope}")
    match = re.fullmatch(r"(\d+)\.(\d+)(?:\.(\d+))?", value)
    if not match:
        raise PolicyError(f"Go compatibility floor is malformed in {scope}")
    major, minor, patch = match.groups()
    return f"go{major}.{minor}.{patch or '0'}"


def _resolve_go_binary(version: str) -> Path:
    if not re.fullmatch(r"go1\.\d+\.\d+", version):
        raise PolicyError("Refusing to resolve an unpinned Go compiler")
    env = _base_go_env(version)
    try:
        root = subprocess.run(["go", "env", "GOROOT"], check=True,
                              capture_output=True, text=True, encoding="utf-8",
                              env=env, cwd=tempfile.gettempdir(), timeout=180).stdout.strip()
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise PolicyError(f"Cannot provision required Go compiler {version}") from error
    goroot = Path(root).resolve()
    executable = goroot / "bin" / ("go.exe" if os.name == "nt" else "go")
    if not executable.is_file():
        raise PolicyError(f"Pinned Go compiler executable is missing: {version}")
    verify_env = _base_go_env("local")
    verify_env["GOROOT"] = str(goroot)
    try:
        actual = subprocess.run([str(executable), "env", "GOVERSION"], check=True,
                                capture_output=True, text=True, encoding="utf-8",
                                env=verify_env, cwd=tempfile.gettempdir(),
                                timeout=60).stdout.strip()
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise PolicyError(f"Cannot verify pinned Go compiler {version}") from error
    if actual != version:
        raise PolicyError(f"Pinned Go compiler mismatch: requested {version}, got {actual}")
    return executable


def _run_go(go_binary: Path, module_dir: Path, args: list[str],
            profile: dict | None = None) -> str:
    env = _base_go_env("local")
    env["GOROOT"] = str(go_binary.parent.parent)
    if profile is not None:
        env.update({"GOOS": profile["goos"], "GOARCH": profile["goarch"],
                    "CGO_ENABLED": profile["cgo"],
                    "GOAMD64": profile["goamd64"] or "",
                    "GOARM64": profile["goarm64"] or ""})
    try:
        result = subprocess.run([str(go_binary), "-C", str(module_dir), *args], check=False,
                                capture_output=True, text=True, encoding="utf-8", env=env,
                                timeout=180)
    except subprocess.TimeoutExpired as error:
        raise PolicyError(f"Go command exceeded its time bound in {module_dir}") from error
    if result.returncode:
        detail = result.stderr.strip()[-2000:]
        raise PolicyError(f"Cannot inventory package closure in {module_dir}: {detail}")
    return result.stdout


def _package_source_digest(root: Path, module_files: list[Path]) -> str:
    root = root.resolve()
    files: set[Path] = set()
    for manifest in module_files:
        files.update(_module_go_sources(manifest))
        files.add(manifest.resolve())
        sum_file = manifest.with_name("go.sum")
        if sum_file.is_file():
            files.add(sum_file.resolve())
    digest = hashlib.sha256()
    for path in sorted(files):
        try:
            relative = path.relative_to(root).as_posix()
        except ValueError as error:
            raise PolicyError("Package source input escapes the audited root") from error
        digest.update(relative.encode("utf-8") + b"\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
    return digest.hexdigest()


def _module_go_sources(manifest: Path) -> set[Path]:
    module_root = manifest.parent.resolve()
    nested_manifests = {path.resolve() for path in module_root.rglob("go.mod")
                        if path.resolve() != manifest.resolve()}
    nested_roots = {path.parent for path in nested_manifests}
    return {source.resolve() for source in module_root.rglob("*.go")
            if not any(parent in source.resolve().parents for parent in nested_roots) and
            ".git" not in source.resolve().parts and "vendor" not in source.resolve().parts}


def package_closure_evidence(root: Path, module_files: list[Path],
                             inventory_sha256: str) -> dict:
    """Capture package closures at each module floor and current stable toolchain."""
    if not module_files or not all(path.is_file() for path in module_files):
        raise PolicyError("Package closure module inventory is empty or incomplete")
    root = root.resolve()
    scopes = [path.parent.resolve().relative_to(root).as_posix() or "."
              for path in module_files]
    if len(scopes) != len(set(scopes)):
        raise PolicyError("Package closure module scopes are duplicated")
    if not re.fullmatch(r"[a-f0-9]{64}", inventory_sha256):
        raise PolicyError("Selected inventory digest is malformed")
    source_before = _package_source_digest(root, module_files)
    current = _native_go_version()
    latest = _latest_stable_go_version()
    binaries = {version: _resolve_go_binary(version)
                for version in sorted({current, latest})}
    scope_floors: dict[str, str] = {}
    scope_tool_roots: dict[str, list[str]] = {}
    tool_only_scopes = []
    profiles = []
    for manifest, scope in zip(module_files, scopes, strict=True):
        manifest_hash = hashlib.sha256(manifest.read_bytes()).hexdigest()
        try:
            module_edit = json.loads(_run_go(binaries[current], manifest.parent,
                                             ["mod", "edit", "-json"]))
        except json.JSONDecodeError as error:
            raise PolicyError(f"Malformed go.mod metadata in {scope}") from error
        if not isinstance(module_edit, dict):
            raise PolicyError(f"Malformed go.mod metadata in {scope}")
        tools = module_edit.get("Tool", [])
        if not isinstance(tools, list) or any(not isinstance(tool, dict) or
                                               not isinstance(tool.get("Path"), str) or
                                               not tool["Path"] for tool in tools):
            raise PolicyError(f"Malformed Go tool roots in {scope}")
        tool_paths = sorted({tool["Path"] for tool in tools})
        if len(tool_paths) != len(tools):
            raise PolicyError(f"Duplicate Go tool roots in {scope}")
        scope_tool_roots[scope] = tool_paths
        floor = _normalize_go_floor(module_edit.get("Go"), scope)
        scope_floors[scope] = floor
        tool_only = bool(tools) and not _module_go_sources(manifest)
        if tool_only:
            tool_only_scopes.append(scope)
            compiler_axes = _compiler_axes(floor, current, latest, True)
        else:
            compiler_axes = _compiler_axes(floor, current, latest, False)
        test_patterns = [] if tool_only else ["./..."]
        for profile in PACKAGE_TARGETS:
            for compiler_axis in compiler_axes:
                version = compiler_axis["compiler"]
                if version not in binaries:
                    binaries[version] = _resolve_go_binary(version)
                compiler_profile = {**profile, **compiler_axis}
                packages = []
                if test_patterns:
                    raw = _run_go(binaries[version], manifest.parent,
                                  ["list", "-buildvcs=false", "-deps", "-test", "-json",
                                   *test_patterns], compiler_profile)
                    test_packages = json_stream(raw)
                    if not any(package.get("DepOnly") is not True
                               for package in test_packages):
                        raise PolicyError(f"Package closure has no main-root test packages in {scope}")
                    packages.extend(test_packages)
                if tool_paths:
                    raw = _run_go(binaries[version], manifest.parent,
                                  ["list", "-buildvcs=false", "-deps", "-json",
                                   *tool_paths], compiler_profile)
                    tool_packages = json_stream(raw)
                    tool_roots = {package.get("ImportPath") for package in tool_packages
                                  if package.get("DepOnly") is not True}
                    if not set(tool_paths).issubset(tool_roots):
                        raise PolicyError(f"Go tool closure omits declared roots in {scope}")
                    packages.extend(tool_packages)
                if not packages:
                    raise PolicyError(f"Package closure has no roots in {scope}")
                imports = []
                for package in packages:
                    if package.get("Error") or package.get("Incomplete"):
                        raise PolicyError(f"Incomplete package closure in {scope}")
                    import_path = package.get("ImportPath")
                    if not isinstance(import_path, str) or not import_path:
                        raise PolicyError(f"Malformed package closure in {scope}")
                    imports.append(import_path)
                imports = sorted(set(imports))
                if not imports:
                    raise PolicyError(f"Empty package closure in {scope}")
                identity = {"scope": scope, "manifest_sha256": manifest_hash,
                            "inventory_sha256": inventory_sha256,
                            "source_sha256": source_before,
                            "test_patterns": test_patterns, "tool_paths": tool_paths,
                            "imports": imports, **compiler_profile}
                digest = hashlib.sha256(json.dumps(
                    identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
                profiles.append({**identity, "sha256": digest})
    source_after = _package_source_digest(root, module_files)
    if source_before != source_after:
        raise PolicyError("Go source or module manifests changed during package inventory")
    return {"schema": 1, "scopes": sorted(scopes),
            "scope_go_floors": scope_floors,
            "scope_tool_roots": scope_tool_roots,
            "tool_only_scopes": sorted(tool_only_scopes),
            "current_compiler": current, "latest_stable_compiler": latest,
            "inventory_sha256": inventory_sha256, "source_sha256": source_before,
            "profiles": profiles}


def inventory(root: Path, module_files: list[Path],
              known_modules: list[Path] | None = None) -> tuple[list[dict], list[dict]]:
    known = {path.parent.resolve() for path in (
        known_modules if known_modules is not None else tracked_modules(root))}
    selected: list[dict] = []
    queries: set[tuple[str, str]] = set()
    for manifest in module_files:
        scope = manifest.parent.resolve().relative_to(root.resolve()).as_posix() or "."
        for entry in selected_modules(manifest.parent):
            if entry.get("Error") or not isinstance(entry.get("Path"), str):
                raise PolicyError(f"Malformed selected module in {scope}")
            if entry.get("Main"):
                continue
            replacement = entry.get("Replace")
            actual = replacement if replacement is not None else entry
            path, version = actual.get("Path"), actual.get("Version")
            if not isinstance(path, str) or not path:
                raise PolicyError(f"Missing selected module path in {scope}")
            if replacement is not None and not version:
                local = (manifest.parent / path).resolve()
                if local not in known:
                    raise PolicyError(f"Uninventoried local replacement in {scope}: {path}")
                selected.append({"scope": scope, "path": entry["Path"],
                                 "version": entry.get("Version"),
                                 "local_replacement": local.relative_to(root.resolve()).as_posix()})
                continue
            if not isinstance(version, str) or not version.startswith("v"):
                raise PolicyError(f"Unversioned selected module in {scope}: {path}")
            selected.append({"scope": scope, "path": entry["Path"],
                             "version": entry.get("Version"),
                             "selected_path": path, "selected_version": version})
            queries.add((path, version))
    if not selected:
        raise PolicyError("No dependency modules were selected")
    return sorted(selected, key=lambda row: (row["scope"], row["path"])), [
        {"package": {"ecosystem": "Go", "name": path}, "version": version}
        for path, version in sorted(queries)
    ]


def request_json(path: str, payload: dict | None = None) -> dict:
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(
        API + path, data=data,
        headers={"Accept": "application/json", "Content-Type": "application/json"},
    )
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                answer = json.load(response)
            if not isinstance(answer, dict):
                raise PolicyError("OSV returned a non-object response")
            return answer
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as error:
            status = getattr(error, "code", None)
            if attempt == 2 or status not in {None, 429, 500, 502, 503, 504}:
                raise PolicyError(f"OSV request failed: {error}") from error
            time.sleep(2**attempt)
    raise AssertionError("unreachable")


def query_osv(queries: list[dict]) -> dict[tuple[str, str], list[dict]]:
    found: dict[tuple[str, str], list[dict]] = {}
    for start in range(0, len(queries), MAX_BATCH):
        pending = queries[start:start + MAX_BATCH]
        seen_tokens: set[tuple[str, str, str]] = set()
        for _ in range(MAX_PAGES):
            answer = request_json("/querybatch", {"queries": pending})
            results = answer.get("results")
            if not isinstance(results, list) or len(results) != len(pending):
                raise PolicyError("OSV batch response is incomplete")
            next_page = []
            for query, result in zip(pending, results, strict=True):
                if not isinstance(result, dict) or not isinstance(result.get("vulns", []), list):
                    raise PolicyError("OSV batch result is malformed")
                key = (query["package"]["name"], query["version"])
                records = result.get("vulns", [])
                if any(not isinstance(record, dict) or not record.get("id") or
                       not record.get("modified") for record in records):
                    raise PolicyError("OSV returned an incomplete advisory reference")
                found.setdefault(key, []).extend(records)
                token = result.get("next_page_token")
                if token:
                    if not isinstance(token, str) or (key[0], key[1], token) in seen_tokens:
                        raise PolicyError("OSV pagination is malformed or repeated")
                    seen_tokens.add((key[0], key[1], token))
                    next_page.append({**query, "page_token": token})
            if not next_page:
                break
            pending = next_page
        else:
            raise PolicyError("OSV pagination exceeded its bound")
    return found


def advisory_records(matches: dict[tuple[str, str], list[dict]]) -> dict[str, dict]:
    def same_instant(left: str, right: str) -> bool:
        try:
            return datetime.fromisoformat(left.replace("Z", "+00:00")) == datetime.fromisoformat(
                right.replace("Z", "+00:00"))
        except ValueError as error:
            raise PolicyError("OSV advisory modification timestamp is malformed") from error

    expected: dict[str, str] = {}
    for records in matches.values():
        for item in records:
            previous = expected.setdefault(item["id"], item["modified"])
            if not same_instant(previous, item["modified"]):
                raise PolicyError(f"Advisory changed during scan: {item['id']}")
    details = {}
    for advisory_id, modified in sorted(expected.items()):
        record = request_json("/vulns/" + urllib.parse.quote(advisory_id, safe=""))
        if record.get("id") != advisory_id or not isinstance(record.get("modified"), str) or \
                not same_instant(record["modified"], modified):
            raise PolicyError(f"Advisory changed during scan: {advisory_id}")
        if not isinstance(record.get("affected"), list) or not record["affected"]:
            raise PolicyError(f"Advisory has no affected package data: {advisory_id}")
        details[advisory_id] = record
    return details


def report_for(root: Path, module_files: list[Path], selected: list[dict],
               matches: dict[tuple[str, str], list[dict]], details: dict[str, dict],
               fetched_at: str) -> dict:
    findings = []
    for row in selected:
        if "selected_path" not in row:
            continue
        key = (row["selected_path"], row["selected_version"])
        for item in matches[key]:
            record = details[item["id"]]
            if record.get("withdrawn"):
                continue
            findings.append({"scope": row["scope"], "module": row["path"],
                             "selected_path": key[0], "version": key[1],
                             "advisory": item["id"], "modified": record["modified"]})
    findings = list({(item["scope"], item["module"], item["selected_path"],
                      item["version"], item["advisory"]): item
                     for item in findings}.values())
    findings.sort(key=lambda item: (item["scope"], item["module"], item["advisory"]))
    canonical = json.dumps(selected, sort_keys=True, separators=(",", ":")).encode()
    advisory_provenance = {}
    for advisory_id, record in sorted(details.items()):
        content = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
        advisory_provenance[advisory_id] = {
            "modified": record["modified"],
            "sha256": hashlib.sha256(content).hexdigest(),
            "aliases": sorted(record.get("aliases", [])),
            "withdrawn": record.get("withdrawn"),
            "affected": record["affected"],
            "raw_record": record,
        }
    return {"schema": 2, "result": "affected" if findings else "clean",
            "advisory_fetched_at": fetched_at,
            "advisories": advisory_provenance,
            "inventory_sha256": hashlib.sha256(canonical).hexdigest(),
            "modules": [path.parent.resolve().relative_to(root.resolve()).as_posix() or "."
                        for path in module_files],
            "selected": selected, "findings": findings}


def _reviewed_openpgp_record(candidate: dict) -> bool:
    provenance = candidate.get("advisories", {}).get(OPENPGP_ADVISORY)
    if not isinstance(provenance, dict):
        return False
    record = provenance.get("raw_record")
    if not isinstance(record, dict) or record.get("id") != OPENPGP_ADVISORY or \
            record.get("modified") != OPENPGP_MODIFIED:
        return False
    canonical = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    record_hash = hashlib.sha256(canonical).hexdigest()
    if record_hash != OPENPGP_SHA256 or provenance.get("sha256") != record_hash or \
            provenance.get("modified") != record.get("modified") or \
            provenance.get("affected") != record.get("affected") or \
            provenance.get("aliases") != sorted(record.get("aliases", [])) or \
            provenance.get("withdrawn") != record.get("withdrawn") or record.get("withdrawn"):
        return False
    affected = record.get("affected")
    if not isinstance(affected, list) or len(affected) != 1:
        return False
    item = affected[0]
    if not isinstance(item, dict) or item.get("package") != {
            "ecosystem": "Go", "name": OPENPGP_MODULE,
            "purl": "pkg:golang/golang.org/x/crypto"}:
        return False
    ranges = item.get("ranges")
    if ranges != [{"type": "SEMVER", "events": [{"introduced": "0"}]}]:
        return False
    imports = item.get("ecosystem_specific", {}).get("imports")
    if not isinstance(imports, list):
        return False
    paths = [entry.get("path") for entry in imports
             if isinstance(entry, dict) and set(entry) == {"path"}]
    return len(paths) == len(imports) and sorted(paths) == list(OPENPGP_IMPORTS)


def _target_key(target: dict) -> tuple[str, str, str, str | None, str | None, tuple[str, ...]]:
    return (target["goos"], target["goarch"], target["cgo"], target["goamd64"],
            target["goarm64"], tuple(target["tags"]))


def _verified_advisory(candidate: dict, advisory_id: str) -> dict | None:
    """Verify retained OSV provenance before using any package or alias facts."""
    provenance = candidate.get("advisories", {}).get(advisory_id)
    if not isinstance(provenance, dict):
        return None
    record = provenance.get("raw_record")
    if not isinstance(record, dict) or record.get("id") != advisory_id or record.get("withdrawn"):
        return None
    aliases = record.get("aliases", [])
    if not isinstance(aliases, list) or any(not isinstance(alias, str) for alias in aliases):
        return None
    content = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
    if provenance.get("sha256") != hashlib.sha256(content).hexdigest() or \
            provenance.get("modified") != record.get("modified") or \
            provenance.get("affected") != record.get("affected") or \
            provenance.get("aliases") != sorted(aliases) or \
            provenance.get("withdrawn") != record.get("withdrawn"):
        return None
    return record


def _affected_imports(candidate: dict, finding: dict) -> tuple[list[str], str | None]:
    """Require complete Go package facts; other databases need a reciprocal Go alias."""
    advisory_id = finding["advisory"]
    record = _verified_advisory(candidate, advisory_id)
    if record is None:
        return [], "retained advisory provenance does not verify"
    if advisory_id == OPENPGP_ADVISORY:
        if not _reviewed_openpgp_record(candidate):
            return [], "reviewed advisory identity or affected-package facts changed"
        if finding["module"] != OPENPGP_MODULE or finding["selected_path"] != OPENPGP_MODULE:
            return [], "finding does not select the reviewed Go module"
        return list(OPENPGP_IMPORTS), None
    records = [record] if re.fullmatch(r"GO-\d{4}-\d+", advisory_id) else []
    if not records:
        for alias in record.get("aliases", []):
            if not re.fullmatch(r"GO-\d{4}-\d+", alias):
                continue
            linked = _verified_advisory(candidate, alias)
            # The Go record must also be an exact-version finding in this scope.
            if linked and advisory_id in linked.get("aliases", []) and any(
                    item.get("advisory") == alias and all(item.get(field) == finding.get(field)
                    for field in ("scope", "module", "selected_path", "version"))
                    for item in candidate["findings"]):
                records.append(linked)
    if not records:
        return [], "complete Go affected-package metadata or reciprocal alias is unavailable"
    paths = set()
    for go_record in records:
        affected = go_record.get("affected")
        if not isinstance(affected, list):
            return [], "Go affected-package metadata is malformed"
        matching = [item for item in affected if isinstance(item, dict) and
                    item.get("package", {}).get("ecosystem") == "Go" and
                    item.get("package", {}).get("name") == finding["selected_path"]]
        if not matching:
            return [], "Go advisory does not describe the selected module"
        for item in matching:
            specific = item.get("ecosystem_specific")
            imports = specific.get("imports") if isinstance(specific, dict) else None
            if not isinstance(imports, list) or not imports:
                return [], "Go advisory has no complete affected-package inventory"
            for entry in imports:
                path = entry.get("path") if isinstance(entry, dict) else None
                if not isinstance(path, str) or not re.fullmatch(r"[A-Za-z0-9._~+/-]+", path) or \
                        any(part in ("", ".", "..", "...") for part in path.split("/")) or \
                        not (path == finding["selected_path"] or
                             path.startswith(finding["selected_path"] + "/")):
                    return [], "Go advisory has an unsupported affected-package path"
                paths.add(path)
    return sorted(paths), None


def _compiler_axes(floor: str, current: str, latest: str,
                   tool_only: bool) -> list[dict]:
    if tool_only:
        roles = {current: ["current"]}
        if latest != current:
            roles.setdefault(latest, []).append("latest_stable")
        return [{"compiler": version, "compiler_roles": sorted(version_roles)}
                for version, version_roles in sorted(roles.items())]
    roles: dict[str, list[str]] = {}
    roles.setdefault(floor, []).append("module_floor")
    roles.setdefault(current, []).append("current")
    if latest != current:
        roles.setdefault(latest, []).append("latest_stable")
    return [{"compiler": version, "compiler_roles": sorted(version_roles)}
            for version, version_roles in sorted(roles.items())]


def _verified_profiles(candidate: dict) -> tuple[dict[tuple[object, ...], dict], str | None]:
    evidence = candidate.get("package_profiles")
    if not isinstance(evidence, dict) or evidence.get("schema") != 1:
        return {}, "package closure evidence is missing or unsupported"
    scopes = candidate.get("modules")
    current = evidence.get("current_compiler")
    latest = evidence.get("latest_stable_compiler")
    profiles = evidence.get("profiles")
    if not isinstance(scopes, list) or not scopes or sorted(set(scopes)) != sorted(scopes) or \
            evidence.get("scopes") != sorted(scopes) or \
            not isinstance(current, str) or not re.fullmatch(r"go1\.\d+\.\d+", current) or \
            not isinstance(latest, str) or not re.fullmatch(r"go1\.\d+\.\d+", latest) or \
            not isinstance(profiles, list):
        return {}, "package closure scope or compiler evidence is malformed"
    if evidence.get("inventory_sha256") != candidate.get("inventory_sha256") or \
            not re.fullmatch(r"[a-f0-9]{64}", str(evidence.get("source_sha256", ""))):
        return {}, "package closure evidence is not bound to the candidate inventory"
    floors = evidence.get("scope_go_floors")
    tool_roots = evidence.get("scope_tool_roots")
    tool_only = evidence.get("tool_only_scopes")
    if not isinstance(floors, dict) or set(floors) != set(scopes) or \
            any(not isinstance(floor, str) or not re.fullmatch(r"go1\.\d+\.\d+", floor)
                for floor in floors.values()) or \
            not isinstance(tool_roots, dict) or set(tool_roots) != set(scopes) or \
            any(not isinstance(roots, list) or roots != sorted(set(roots)) or
                any(not isinstance(path, str) or not path for path in roots)
                for roots in tool_roots.values()) or \
            not isinstance(tool_only, list) or tool_only != sorted(set(tool_only)) or \
            not set(tool_only).issubset(scopes):
        return {}, "package closure Go-floor or tool-module evidence is malformed"
    if any(scope != ".github/tools" for scope in tool_only):
        return {}, "unreviewed tool-only module scope cannot omit its declared Go floor"
    if any(not tool_roots[scope] for scope in tool_only):
        return {}, "Tool-only scope has no declared executable roots"
    expected: set[tuple[object, ...]] = set()
    for scope in scopes:
        for target in PACKAGE_TARGETS:
            for compiler_axis in _compiler_axes(floors[scope], current, latest,
                                                scope in tool_only):
                expected.add((scope, *_target_key(target), compiler_axis["compiler"],
                              tuple(compiler_axis["compiler_roles"])))
    indexed: dict[tuple[object, ...], dict] = {}
    for item in profiles:
        if not isinstance(item, dict):
            return {}, "package closure profile is malformed"
        try:
            target = {field: item[field] for field in
                      ("goos", "goarch", "cgo", "goamd64", "goarm64", "tags")}
            roles = item["compiler_roles"]
            key = (item["scope"], *_target_key(target), item["compiler"], tuple(roles))
        except (KeyError, TypeError):
            return {}, "package closure profile is incomplete"
        if key not in expected or key in indexed or not isinstance(roles, list) or \
                roles != sorted(set(roles)):
            return {}, "package closure profile coverage is duplicated or unsupported"
        imports, digest, manifest_hash = (item.get("imports"), item.get("sha256"),
                                          item.get("manifest_sha256"))
        test_patterns, item_tool_roots = item.get("test_patterns"), item.get("tool_paths")
        expected_test_patterns = [] if item["scope"] in tool_only else ["./..."]
        if not isinstance(imports, list) or not imports or \
                any(not isinstance(path, str) or not path for path in imports) or \
                imports != sorted(set(imports)) or \
                not isinstance(manifest_hash, str) or not re.fullmatch(r"[a-f0-9]{64}", manifest_hash) or \
                test_patterns != expected_test_patterns or item_tool_roots != tool_roots[item["scope"]]:
            return {}, "package closure import or manifest evidence is malformed"
        identity = {"scope": item["scope"], "manifest_sha256": manifest_hash,
                    "compiler": item["compiler"], "compiler_roles": roles,
                    "inventory_sha256": evidence["inventory_sha256"],
                    "source_sha256": evidence["source_sha256"],
                    **target, "test_patterns": test_patterns,
                    "tool_paths": item_tool_roots,
                    "imports": imports}
        expected_digest = hashlib.sha256(json.dumps(
            identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        if digest != expected_digest:
            return {}, "package closure profile digest does not match its contents"
        indexed[key] = item
    if set(indexed) != expected:
        return {}, "package closure evidence does not cover every module, target, and compiler role"
    return indexed, None


def _applicability(candidate: dict) -> list[dict]:
    profiles, profile_error = _verified_profiles(candidate)
    evidence = []
    findings = candidate.get("findings")
    if not isinstance(findings, list) or any(not isinstance(item, dict) for item in findings):
        raise PolicyError("Candidate advisory findings are malformed")
    scopes = candidate.get("modules")
    selected = candidate.get("selected")
    if not isinstance(scopes, list) or any(not isinstance(scope, str) for scope in scopes) or \
            sorted(set(scopes)) != sorted(scopes) or \
            not isinstance(selected, list) or any(not isinstance(item, dict) or
                item.get("scope") not in scopes or not isinstance(item.get("path"), str)
                for item in selected):
        raise PolicyError("Candidate module or selected package inventory is malformed")
    selected_identities = {(item.get("scope"), item.get("path"),
                            item.get("selected_path"), item.get("selected_version"))
                           for item in selected if isinstance(item, dict)}
    for finding in findings:
        if finding.get("scope") not in scopes or (finding.get("scope"), finding.get("module"),
                finding.get("selected_path"), finding.get("version")) not in selected_identities:
            raise PolicyError("Candidate advisory finding is not backed by selected inventory")
        provenance = candidate.get("advisories", {}).get(finding.get("advisory"))
        if not isinstance(provenance, dict) or finding.get("modified") != provenance.get("modified"):
            raise PolicyError("Candidate finding and retained advisory record disagree")
        finding_identity = {key: finding.get(key) for key in
                            ("scope", "module", "selected_path", "version", "advisory", "modified")}
        affected_imports, reason = _affected_imports(candidate, finding)
        profile_refs = []
        if reason is None and profile_error:
            reason = profile_error
        if reason is None:
            for key, item in sorted(profiles.items()):
                profile_refs.append({"scope": key[0], "goos": key[1], "goarch": key[2],
                                     "cgo": key[3], "goamd64": key[4],
                                     "goarm64": key[5], "tags": list(key[6]),
                                     "compiler": key[7], "compiler_roles": list(key[8]),
                                     "sha256": item["sha256"]})
            present = sorted({path for item in profiles.values()
                              for path in item["imports"] if path in affected_imports})
            if present:
                reason = "affected package is present in a supported package closure: " + \
                    ", ".join(present)
        evidence.append({"finding": finding_identity,
                         "decision": "admissible_with_exception" if reason is None else "blocking",
                         "reason": ("all affected imports are absent from every supported "
                                   "module/profile package closure" if reason is None else reason),
                         "affected_imports": affected_imports,
                         "status": "not_affected" if reason is None else "under_investigation",
                         "justification": "vulnerable_code_not_present" if reason is None else None,
                         "profile_evidence": profile_refs})
    return evidence


def _comparison_blockers(comparison: dict) -> list[dict]:
    candidate = comparison.get("candidate")
    if not isinstance(candidate, dict):
        raise PolicyError("Selected graph comparison has no candidate evidence")
    fetched = candidate.get("advisory_fetched_at")
    try:
        fetched_at = datetime.fromisoformat(fetched.replace("Z", "+00:00"))
        now = datetime.now(timezone.utc)
        if fetched_at.tzinfo is None or fetched_at > now + timedelta(minutes=5) or \
                now - fetched_at > timedelta(hours=1):
            raise ValueError("stale or future OSV timestamp")
    except (AttributeError, TypeError, ValueError) as error:
        raise PolicyError("Candidate OSV evidence is missing, stale, or future dated") from error
    expected_evidence = _applicability(candidate)
    if candidate.get("applicability") != expected_evidence:
        raise PolicyError("Candidate applicability evidence is absent or does not verify")
    by_identity = {json.dumps(item["finding"], sort_keys=True): item
                   for item in expected_evidence}
    finding_identities = {json.dumps({key: item.get(key) for key in
                           ("scope", "module", "selected_path", "version", "advisory", "modified")},
                           sort_keys=True) for item in candidate["findings"]}
    comparison_identities = []
    for field in ("introduced", "persistent"):
        rows = comparison.get(field)
        if not isinstance(rows, list):
            raise PolicyError(f"Incomplete selected graph comparison: {field}")
        comparison_identities.extend(json.dumps({key: item.get(key) for key in
            ("scope", "module", "selected_path", "version", "advisory", "modified")},
            sort_keys=True) for item in rows if isinstance(item, dict))
        if any(not isinstance(item, dict) for item in rows):
            raise PolicyError("Comparison contains a malformed raw finding")
    if len(comparison_identities) != len(set(comparison_identities)) or \
            set(comparison_identities) != finding_identities:
        raise PolicyError("Introduced/persistent findings do not cover the complete candidate inventory")
    blockers = []
    for field in ("introduced", "persistent"):
        findings = comparison[field]
        for finding in findings:
            identity = {key: finding.get(key) for key in
                        ("scope", "module", "selected_path", "version", "advisory", "modified")}
            decision = by_identity.get(json.dumps(identity, sort_keys=True))
            if decision is None:
                raise PolicyError("Comparison finding is not backed by candidate evidence")
            if decision["decision"] != "admissible_with_exception":
                blockers.append(finding)
    return blockers


def blocking_findings(report: dict) -> list[dict]:
    """Return every raw introduced/persistent finding that lacks reviewed applicability proof."""
    if report.get("schema") == 2 and isinstance(report.get("modules"), list) and \
            "candidate" not in report:
        fetched = report.get("advisory_fetched_at")
        try:
            fetched_at = datetime.fromisoformat(fetched.replace("Z", "+00:00"))
            now = datetime.now(timezone.utc)
            if fetched_at.tzinfo is None or fetched_at > now + timedelta(minutes=5) or \
                    now - fetched_at > timedelta(hours=1):
                raise ValueError("stale or future OSV timestamp")
        except (AttributeError, TypeError, ValueError) as error:
            raise PolicyError("Snapshot OSV evidence is missing, stale, or future dated") from error
        expected = _applicability(report)
        if report.get("applicability") != expected:
            raise PolicyError("Snapshot applicability evidence is absent or does not verify")
        return [item["finding"] for item in expected
                if item["decision"] != "admissible_with_exception"]
    generated = report.get("generated")
    tracked = report.get("tracked")
    if isinstance(tracked, dict):
        if not isinstance(generated, dict):
            raise PolicyError("Aggregate graph report is missing generated evidence")
        _verify_aggregate_findings(report, tracked, generated)
        return blocking_findings(tracked) + blocking_findings(generated)
    if isinstance(report.get("candidate"), dict):
        if isinstance(generated, dict) and isinstance(generated.get("candidate"), dict):
            prefix = ".e2e/generated/"
            tracked_part = {**report,
                            "introduced": [item for item in report.get("introduced", [])
                                           if not item.get("scope", "").startswith(prefix)],
                            "persistent": [item for item in report.get("persistent", [])
                                           if not item.get("scope", "").startswith(prefix)]}
            _verify_aggregate_findings(report, tracked_part, generated)
            return (_comparison_blockers(tracked_part) +
                    blocking_findings(generated))
        return _comparison_blockers(report)
    raise PolicyError("Selected graph comparison evidence is incomplete")


def _verify_aggregate_findings(report: dict, tracked: dict, generated: dict) -> None:
    """Ensure aggregate raw findings are exactly the two retained source comparisons."""
    prefix = ".e2e/generated/"
    for field in ("introduced", "persistent"):
        tracked_items, generated_items = tracked.get(field), generated.get(field)
        combined = report.get(field)
        if not isinstance(tracked_items, list) or not isinstance(generated_items, list) or \
                not isinstance(combined, list):
            raise PolicyError(f"Aggregate selected graph comparison is incomplete: {field}")
        if any(not isinstance(item, dict) or not isinstance(item.get("scope"), str)
               for item in [*tracked_items, *generated_items]):
            raise PolicyError(f"Aggregate selected graph findings are malformed: {field}")
        expected = [*tracked_items, *({**item, "scope": prefix + item["scope"]}
                                      for item in generated_items)]
        if combined != expected:
            raise PolicyError("Aggregate selected graph findings do not match retained evidence")


def evaluate(root: Path, module_files: list[Path],
             known_modules: list[Path] | None = None) -> dict:
    selected, queries = inventory(root, module_files, known_modules)
    matches = query_osv(queries)
    details = advisory_records(matches)
    report = report_for(root, module_files, selected, matches, details,
                        datetime.now(timezone.utc).isoformat())
    if report["findings"]:
        report["package_profiles"] = package_closure_evidence(
            root, module_files, report["inventory_sha256"])
    report["applicability"] = _applicability(report)
    return report


def require_clean_comparison(report: dict) -> None:
    """Reject affected selections except the reviewed package-absence case."""
    findings = blocking_findings(report)
    if findings:
        details = ", ".join(
            f"{item.get('scope', '?')} {item.get('module', '?')} "
            f"{item.get('advisory', '?')}"
            for item in findings
        )
        raise PolicyError(f"Selected graph contains blocking affected modules: {details}")


def compare_explicit(base_root: Path, base_files: list[Path], candidate_root: Path,
                     candidate_files: list[Path],
                     base_known: list[Path] | None = None,
                     candidate_known: list[Path] | None = None) -> dict:
    if not all(path.is_file() for path in [*base_files, *candidate_files]):
        raise PolicyError("A requested module is missing from base or candidate")
    base_selected, base_queries = inventory(base_root, base_files, base_known)
    candidate_selected, candidate_queries = inventory(candidate_root, candidate_files,
                                                     candidate_known)
    union = {(query["package"]["name"], query["version"]): query
             for query in [*base_queries, *candidate_queries]}
    matches = query_osv([union[key] for key in sorted(union)])
    details = advisory_records(matches)
    fetched_at = datetime.now(timezone.utc).isoformat()
    base = report_for(base_root, base_files, base_selected, matches, details, fetched_at)
    candidate = report_for(candidate_root, candidate_files, candidate_selected,
                           matches, details, fetched_at)
    if candidate["findings"]:
        candidate["package_profiles"] = package_closure_evidence(
            candidate_root, candidate_files, candidate["inventory_sha256"])
    candidate["applicability"] = _applicability(candidate)

    def indexed(report: dict) -> dict[tuple[str, str, str], dict]:
        return {(item["scope"], item["module"], item["advisory"]): item
                for item in report["findings"]}

    before, after = indexed(base), indexed(candidate)
    shared = after.keys() & before.keys()
    changed = {key for key in shared if
               (after[key]["selected_path"], after[key]["version"]) !=
               (before[key]["selected_path"], before[key]["version"])}
    return {"schema": 1, "advisory_fetched_at": fetched_at,
            "base": base, "candidate": candidate,
            "introduced": [after[key] for key in sorted((after.keys() - before.keys()) | changed)],
            "resolved": [before[key] for key in sorted(before.keys() - after.keys())],
            "persistent": [after[key] for key in sorted(shared - changed)]}


def compare(base_root: Path, candidate_root: Path,
            module_scopes: list[Path] | None = None) -> dict:
    base_files = ([base_root / scope / "go.mod" for scope in module_scopes]
                  if module_scopes else tracked_modules(base_root))
    candidate_files = ([candidate_root / scope / "go.mod" for scope in module_scopes]
                       if module_scopes else tracked_modules(candidate_root))
    return compare_explicit(base_root, base_files, candidate_root, candidate_files)


def compare_git(root: Path, base: str, scopes: list[Path] | None = None) -> dict:
    if not re.fullmatch(r"[a-f0-9]{40}", base):
        raise PolicyError("Comparison requires an exact base commit SHA")
    with tempfile.TemporaryDirectory(prefix="selected-graph-base-") as temporary:
        base_root = Path(temporary) / "base"
        subprocess.run(["git", "-C", str(root), "worktree", "add", "--detach",
                        str(base_root), base], check=True, capture_output=True, text=True)
        try:
            return compare(base_root, root, scopes)
        finally:
            subprocess.run(["git", "-C", str(root), "worktree", "remove",
                            "--force", str(base_root)], check=True,
                           capture_output=True, text=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--module", action="append", type=Path)
    parser.add_argument("--base", help="Compare with an exact base commit")
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    try:
        root = args.root.resolve()
        modules = [root / item / "go.mod" for item in args.module] if args.module else tracked_modules(root)
        if not all(path.is_file() for path in modules):
            raise PolicyError("A requested Go module has no go.mod")
        if args.base:
            if not all(path.is_file() for path in modules):
                raise PolicyError("A requested Go module has no go.mod")
            if not all(path.resolve().is_relative_to(root) for path in modules):
                raise PolicyError("Requested module is outside the candidate checkout")
            scopes = ([path.parent.resolve().relative_to(root) for path in modules]
                      if args.module else None)
            report = compare_git(root, args.base, scopes)
        else:
            report = evaluate(root, modules)
    except (PolicyError, OSError, subprocess.CalledProcessError, json.JSONDecodeError) as error:
        print(f"Selected graph policy indeterminate: {error}", file=sys.stderr)
        return 2
    if args.report:
        args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    candidate = report["candidate"] if args.base else report
    print(f"Selected graph policy: {candidate['result']}; "
          f"{len(candidate['modules'])} modules; {len(candidate['findings'])} findings; "
          f"inventory {candidate['inventory_sha256']}")
    if args.base:
        print(f"  Introduced advisories: {len(report['introduced'])}; "
              f"resolved advisories: {len(report['resolved'])}; "
              f"persistent advisories: {len(report['persistent'])}")
    for item in candidate["findings"]:
        print(f"  {item['scope']}: {item['selected_path']}@{item['version']} "
              f"{item['advisory']}")
    try:
        blockers = blocking_findings(report)
    except PolicyError as error:
        print(f"Selected package policy indeterminate: {error}", file=sys.stderr)
        return 2
    print(f"  Blocking findings: {len(blockers)}; "
          f"package-absence exceptions: {len(candidate['findings']) - len(blockers)}")
    return 1 if blockers else 0


if __name__ == "__main__":
    raise SystemExit(main())
